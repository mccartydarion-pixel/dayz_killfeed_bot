package repository

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The Stadium (docs/STADIUM.md): one arena configuration and build state per installation
// (stadiums), the installation's server binding, the owner's linked player and the latest
// ADM-recorded positions the altitude comes from (player_location_events, read-only).

// Stadium statuses.
const (
	StadiumDraft   = "DRAFT"
	StadiumBuilt   = "BUILT"
	StadiumRemoved = "REMOVED"
)

// Stadium is one stadiums row.
type Stadium struct {
	InstallationID int64
	OrganizationID int64
	Params         json.RawMessage
	Status         string
	FileSHA256     *string
	ObjectCount    int
	BuiltAt        *time.Time
	RemovedAt      *time.Time
	LastOutcome    json.RawMessage
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// StadiumTarget is the installation's game server as a build needs it.
type StadiumTarget struct {
	InstallationID   int64
	OrganizationID   int64
	GuildID          int64
	ServerID         int64
	NitradoServiceID string
}

// StadiumPosition is one ADM-recorded position with its altitude.
type StadiumPosition struct {
	PlayerID   int64
	PlayerName string
	X, Z       float64
	AltitudeY  float64
	ObservedAt time.Time
}

type StadiumRepository struct{ pool *pgxpool.Pool }

func NewStadiumRepository(pool *pgxpool.Pool) *StadiumRepository {
	return &StadiumRepository{pool: pool}
}

const stadiumCols = "installation_id, organization_id, params, status, file_sha256, object_count, built_at, removed_at, last_outcome, created_at, updated_at"

func scanStadium(row pgx.Row) (*Stadium, error) {
	var s Stadium
	err := row.Scan(&s.InstallationID, &s.OrganizationID, &s.Params, &s.Status, &s.FileSHA256, &s.ObjectCount, &s.BuiltAt, &s.RemovedAt, &s.LastOutcome, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Get returns the installation's stadium, or nil when none was ever saved.
func (r *StadiumRepository) Get(ctx context.Context, organizationID, installationID int64) (*Stadium, error) {
	return scanStadium(r.pool.QueryRow(ctx, `SELECT `+stadiumCols+` FROM stadiums WHERE installation_id=$1 AND organization_id=$2`, installationID, organizationID))
}

// Upsert inserts or replaces the installation's stadium row (every column but the timestamps
// comes from s; updated_at is set now).
func (r *StadiumRepository) Upsert(ctx context.Context, s Stadium) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO stadiums (installation_id, organization_id, params, status, file_sha256, object_count, built_at, removed_at, last_outcome)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (installation_id) DO UPDATE SET
    params = EXCLUDED.params, status = EXCLUDED.status, file_sha256 = EXCLUDED.file_sha256, object_count = EXCLUDED.object_count,
    built_at = EXCLUDED.built_at, removed_at = EXCLUDED.removed_at, last_outcome = EXCLUDED.last_outcome, updated_at = NOW()
WHERE stadiums.organization_id = EXCLUDED.organization_id`,
		s.InstallationID, s.OrganizationID, s.Params, s.Status, s.FileSHA256, s.ObjectCount, s.BuiltAt, s.RemovedAt, s.LastOutcome)
	return err
}

// Target resolves the installation's server binding; nil when the installation is not in the
// organization or has no game server.
func (r *StadiumRepository) Target(ctx context.Context, organizationID, installationID int64) (*StadiumTarget, error) {
	var t StadiumTarget
	err := r.pool.QueryRow(ctx, `
SELECT i.id, i.organization_id, c.guild_id, gs.id, COALESCE(gs.provider_service_id, '')
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN game_servers gs ON gs.id = i.game_server_id
WHERE i.id = $1 AND i.organization_id = $2`, installationID, organizationID).Scan(&t.InstallationID, &t.OrganizationID, &t.GuildID, &t.ServerID, &t.NitradoServiceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// InstallationMapKey is the map configured for the installation (shop_delivery_settings.map_key,
// the key the live map uses too); "" when none is set.
func (r *StadiumRepository) InstallationMapKey(ctx context.Context, installationID int64) (string, error) {
	var key string
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(map_key, '') FROM shop_delivery_settings WHERE installation_id=$1`, installationID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return key, err
}

// LinkedPlayer is the player a website user's VERIFIED DayZ link names on the guild (0 when none).
func (r *StadiumRepository) LinkedPlayer(ctx context.Context, guildID int64, discordUserID string) (int64, error) {
	var playerID int64
	err := r.pool.QueryRow(ctx, `SELECT player_id FROM player_links WHERE guild_id=$1 AND discord_user_id=$2 AND status='VERIFIED'`, guildID, discordUserID).Scan(&playerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return playerID, err
}

const stadiumPositionCols = "e.player_id, e.gamertag, e.x, e.z, e.y, e.observed_at"

func scanStadiumPosition(row pgx.Row) (*StadiumPosition, error) {
	var p StadiumPosition
	err := row.Scan(&p.PlayerID, &p.PlayerName, &p.X, &p.Z, &p.AltitudeY, &p.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// LatestPosition is the player's newest ADM position on the server that carries an altitude and
// was observed at or after since; nil when there is none. The altitude is the recorded one,
// never computed.
func (r *StadiumRepository) LatestPosition(ctx context.Context, guildID, serverID, playerID int64, since time.Time) (*StadiumPosition, error) {
	return scanStadiumPosition(r.pool.QueryRow(ctx, `
SELECT `+stadiumPositionCols+` FROM player_location_events e
WHERE e.guild_id=$1 AND e.server_id=$2 AND e.player_id=$3 AND e.y IS NOT NULL AND e.source='ADM' AND e.observed_at >= $4
ORDER BY e.observed_at DESC, e.id DESC LIMIT 1`, guildID, serverID, playerID, since))
}

// LatestPositionByName is LatestPosition for a player named by gamertag (case-insensitive), which
// must have been seen on this server.
func (r *StadiumRepository) LatestPositionByName(ctx context.Context, guildID, serverID int64, name string, since time.Time) (*StadiumPosition, error) {
	return scanStadiumPosition(r.pool.QueryRow(ctx, `
SELECT `+stadiumPositionCols+` FROM player_location_events e
WHERE e.guild_id=$1 AND e.server_id=$2 AND lower(e.gamertag)=lower($3) AND e.y IS NOT NULL AND e.source='ADM' AND e.observed_at >= $4
ORDER BY e.observed_at DESC, e.id DESC LIMIT 1`, guildID, serverID, strings.TrimSpace(name), since))
}
