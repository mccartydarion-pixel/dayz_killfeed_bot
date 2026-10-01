package repository

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CardRepository reads the figures on a Champion Card and stores its share links
// (docs/CHAMPION_CARD.md). Every figure is scoped to one installation's own server, with the same
// counting conventions as the player stats API (PlayerServerRepository.KillStats).
type CardRepository struct{ pool *pgxpool.Pool }

func NewCardRepository(pool *pgxpool.Pool) *CardRepository { return &CardRepository{pool: pool} }

// CardStats is one player's card figures on one server.
type CardStats struct {
	PlayerName         string
	ServerName         string
	Kills              int
	Deaths             int
	Headshots          int
	LongestKillMeters  float64
	PlaytimeSeconds    int64
	LongestLifeSeconds *int64
	Rank               *int // by kills; nil with no kills
	RankedPlayers      int
}

// Stats returns the card figures, or (nil, nil) when the player does not exist in the guild.
func (r *CardRepository) Stats(ctx context.Context, guildID, serverID, playerID int64) (*CardStats, error) {
	const q = `
SELECT p.display_name, COALESCE(gs.display_name, ''),
  (SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3)::int,
  (SELECT COUNT(*) FROM deaths WHERE guild_id=$1 AND server_id=$2 AND player_id=$3)::int,
  (SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3 AND headshot)::int,
  (SELECT COALESCE(MAX(distance), 0) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3)::float8,
  COALESCE((SELECT total_observed_seconds FROM player_server_activity WHERE guild_id=$1 AND server_id=$2 AND player_id=$3), 0),
  (SELECT MAX(playtime_seconds) FROM player_lives WHERE guild_id=$1 AND server_id=$2 AND player_id=$3),
  (SELECT COUNT(DISTINCT killer_player_id) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id IS NOT NULL)::int
FROM players p LEFT JOIN game_servers gs ON gs.id=$2 AND gs.guild_id=$1
WHERE p.guild_id=$1 AND p.id=$3`
	var s CardStats
	err := r.pool.QueryRow(ctx, q, guildID, serverID, playerID).Scan(&s.PlayerName, &s.ServerName, &s.Kills, &s.Deaths, &s.Headshots,
		&s.LongestKillMeters, &s.PlaytimeSeconds, &s.LongestLifeSeconds, &s.RankedPlayers)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("card stats: %w", err)
	}
	if s.Kills > 0 {
		var rank int
		if err := r.pool.QueryRow(ctx, `
SELECT 1 + COUNT(*) FROM (SELECT killer_player_id FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id IS NOT NULL
    GROUP BY killer_player_id HAVING COUNT(*) > $3) ahead`, guildID, serverID, s.Kills).Scan(&rank); err != nil {
			return nil, fmt.Errorf("card rank: %w", err)
		}
		s.Rank = &rank
	}
	return &s, nil
}

// Faction returns the Faction Hub faction of the player's VERIFIED link holder on one installation.
func (r *CardRepository) Faction(ctx context.Context, installationID, guildID, playerID int64) (name, tag string, ok bool, err error) {
	err = r.pool.QueryRow(ctx, `
SELECT f.name, f.tag FROM player_links pl
JOIN app_users u ON u.discord_user_id = pl.discord_user_id
JOIN hub_faction_members m ON m.user_id = u.id AND m.installation_id = $1
JOIN hub_factions f ON f.id = m.faction_id
WHERE pl.guild_id=$2 AND pl.player_id=$3 AND pl.status='VERIFIED'`, installationID, guildID, playerID).Scan(&name, &tag)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	return name, tag, err == nil, err
}

// InstallationForServer returns an installation backed by serverID (0 when none is).
func (r *CardRepository) InstallationForServer(ctx context.Context, serverID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM installations WHERE game_server_id=$1 ORDER BY id LIMIT 1`, serverID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// CardShare is a public share link for one player's card on one installation.
type CardShare struct {
	Token          string
	InstallationID int64
	GuildID        int64
	ServerID       int64
	PlayerID       int64
	CreatedAt      time.Time
}

func newShareToken() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// EnsureShare returns the player's active share on the installation, creating it if needed. A
// player has at most one active share per installation, so sharing twice yields the same link.
func (r *CardRepository) EnsureShare(ctx context.Context, installationID, guildID, serverID, playerID int64) (*CardShare, error) {
	token, err := newShareToken()
	if err != nil {
		return nil, err
	}
	s := CardShare{InstallationID: installationID, GuildID: guildID, ServerID: serverID, PlayerID: playerID}
	err = r.pool.QueryRow(ctx, `
INSERT INTO player_card_shares(token, installation_id, guild_id, server_id, player_id) VALUES($1,$2,$3,$4,$5)
ON CONFLICT (installation_id, player_id) WHERE revoked_at IS NULL DO UPDATE SET server_id = EXCLUDED.server_id
RETURNING token, created_at`, token, installationID, guildID, serverID, playerID).Scan(&s.Token, &s.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("ensure card share: %w", err)
	}
	return &s, nil
}

// ActiveShare returns the player's active share on the installation, or (nil, nil).
func (r *CardRepository) ActiveShare(ctx context.Context, installationID, playerID int64) (*CardShare, error) {
	return r.share(ctx, `installation_id=$1 AND player_id=$2`, installationID, playerID)
}

// ResolveShare returns the active share addressed by token, or (nil, nil).
func (r *CardRepository) ResolveShare(ctx context.Context, token string) (*CardShare, error) {
	if token == "" {
		return nil, nil
	}
	return r.share(ctx, `token=$1`, token)
}

func (r *CardRepository) share(ctx context.Context, where string, args ...any) (*CardShare, error) {
	var s CardShare
	err := r.pool.QueryRow(ctx, `SELECT token, installation_id, guild_id, server_id, player_id, created_at FROM player_card_shares WHERE revoked_at IS NULL AND `+where, args...).
		Scan(&s.Token, &s.InstallationID, &s.GuildID, &s.ServerID, &s.PlayerID, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// RevokeShare ends the player's active share; the old link stops resolving immediately.
func (r *CardRepository) RevokeShare(ctx context.Context, installationID, playerID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE player_card_shares SET revoked_at=NOW() WHERE installation_id=$1 AND player_id=$2 AND revoked_at IS NULL`, installationID, playerID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
