package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	WarPending   = "PENDING"
	WarActive    = "ACTIVE"
	WarEnded     = "ENDED"
	WarCancelled = "CANCELLED"
)

// ErrWarFactionNotInGuild means a war was asked for between factions that are not two different
// Faction Hub factions of the same installation.
var ErrWarFactionNotInGuild = errors.New("both factions must be different factions of the same server")

// ErrWarAlreadyOpen means the two factions already have a pending or active war.
var ErrWarAlreadyOpen = errors.New("these factions already have a pending or active war")

// ErrWarNotChanged means the war was not in a state the change applies to (or does not exist).
var ErrWarNotChanged = errors.New("the war is not in a state that allows this")

// War is a war between two Faction Hub factions. The challenger is side A. A side's faction id is
// 0 once that faction is dissolved; its label stays.
type War struct {
	ID, GuildID, InstallationID, SeasonID int64
	FactionAID, FactionBID                int64
	FactionALabel, FactionBLabel          string
	Status                                string
	StartedAt, EndedAt                    *time.Time
	ScoreA, ScoreB                        int64
	// WinnerSide is "A", "B" or "" (no winner yet, or a draw).
	WinnerSide string
}

// WinnerLabel names the winning faction, or "" when there is none.
func (w War) WinnerLabel() string {
	switch w.WinnerSide {
	case "A":
		return w.FactionALabel
	case "B":
		return w.FactionBLabel
	}
	return ""
}

type PostgresWarRepository struct{ pool *pgxpool.Pool }

func NewPostgresWarRepository(pool *pgxpool.Pool) *PostgresWarRepository {
	return &PostgresWarRepository{pool: pool}
}

// CanonicalPair normalizes faction order to prevent A/B and B/A duplicates.
func CanonicalPair(a, b int64) (int64, int64, error) {
	if a <= 0 || b <= 0 || a == b {
		return 0, 0, fmt.Errorf("factions must be distinct")
	}
	if a > b {
		return b, a, nil
	}
	return a, b, nil
}

// WarClassification classifies faction combat from kill-time snapshots.
func WarClassification(killerFaction, victimFaction *int64) string {
	if killerFaction == nil || victimFaction == nil {
		return "NO_FACTION"
	}
	if *killerFaction == *victimFaction {
		return "TEAM_KILL"
	}
	return "ENEMY_FACTION_KILL"
}

// warColumns are the columns scanWar reads. Wars of the retired Discord faction system
// (installation_id IS NULL) are never read.
const warColumns = `w.id,w.guild_id,w.installation_id,COALESCE(w.season_id,0),COALESCE(w.hub_faction_a_id,0),COALESCE(w.hub_faction_b_id,0),
 w.faction_a_label,w.faction_b_label,w.status,w.started_at,w.ended_at,w.faction_a_score,w.faction_b_score,COALESCE(w.winner_side,'')`

func scanWar(row pgx.Row) (*War, error) {
	var w War
	if err := row.Scan(&w.ID, &w.GuildID, &w.InstallationID, &w.SeasonID, &w.FactionAID, &w.FactionBID,
		&w.FactionALabel, &w.FactionBLabel, &w.Status, &w.StartedAt, &w.EndedAt, &w.ScoreA, &w.ScoreB, &w.WinnerSide); err != nil {
		return nil, err
	}
	return &w, nil
}

// Challenge opens a pending war: challenger (side A) against opponent (side B), both factions of
// installationID.
func (r *PostgresWarRepository) Challenge(ctx context.Context, installationID, seasonID, challenger, opponent, creatorPlayerID int64) (*War, error) {
	if installationID <= 0 || challenger <= 0 || opponent <= 0 || challenger == opponent {
		return nil, ErrWarFactionNotInGuild
	}
	w, err := scanWar(r.pool.QueryRow(ctx, `INSERT INTO faction_wars AS w(guild_id,installation_id,season_id,hub_faction_a_id,hub_faction_b_id,faction_a_label,faction_b_label,status,created_by_player_id)
SELECT c.guild_id,i.id,NULLIF($2,0),a.id,b.id,'['||a.tag||'] '||a.name,'['||b.tag||'] '||b.name,$5,NULLIF($6,0)
FROM installations i
JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id
JOIN hub_factions a ON a.id=$3 AND a.installation_id=i.id
JOIN hub_factions b ON b.id=$4 AND b.installation_id=i.id
WHERE i.id=$1
RETURNING `+warColumns, installationID, seasonID, challenger, opponent, WarPending, creatorPlayerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWarFactionNotInGuild
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_open_hub_war_pair" {
		return nil, ErrWarAlreadyOpen
	}
	return w, err
}

func changed(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWarNotChanged
	}
	return nil
}

// AcceptWar starts a pending war.
func (r *PostgresWarRepository) AcceptWar(ctx context.Context, guildID, warID int64) error {
	return changed(r.pool.Exec(ctx, `UPDATE faction_wars SET status=$1,started_at=NOW() WHERE guild_id=$2 AND id=$3 AND status=$4 AND installation_id IS NOT NULL`, WarActive, guildID, warID, WarPending))
}

// DeclineWar cancels a pending war.
func (r *PostgresWarRepository) DeclineWar(ctx context.Context, guildID, warID int64) error {
	return changed(r.pool.Exec(ctx, `UPDATE faction_wars SET status=$1,ended_at=NOW() WHERE guild_id=$2 AND id=$3 AND status=$4 AND installation_id IS NOT NULL`, WarCancelled, guildID, warID, WarPending))
}

// EndWar ends an active war and records its score and winning side: each side scores its
// members' kills attributed to the war.
func (r *PostgresWarRepository) EndWar(ctx context.Context, guildID, warID int64) error {
	return changed(r.pool.Exec(ctx, `WITH scores AS (
 SELECT COUNT(k.id) FILTER (WHERE k.killer_hub_faction_id=w.hub_faction_a_id) AS a,
        COUNT(k.id) FILTER (WHERE k.killer_hub_faction_id=w.hub_faction_b_id) AS b
 FROM faction_wars w LEFT JOIN kills k ON k.war_id=w.id
 WHERE w.guild_id=$1 AND w.id=$2)
UPDATE faction_wars w SET status=$3,ended_at=NOW(),faction_a_score=s.a,faction_b_score=s.b,
 winner_side=CASE WHEN s.a>s.b THEN 'A' WHEN s.b>s.a THEN 'B' END
FROM scores s WHERE w.guild_id=$1 AND w.id=$2 AND w.status=$4 AND w.installation_id IS NOT NULL`, guildID, warID, WarEnded, WarActive))
}

// CancelWar cancels a pending or active war (staff).
func (r *PostgresWarRepository) CancelWar(ctx context.Context, guildID, warID int64) error {
	return changed(r.pool.Exec(ctx, `UPDATE faction_wars SET status=$1,ended_at=NOW() WHERE guild_id=$2 AND id=$3 AND status IN ($4,$5) AND installation_id IS NOT NULL`, WarCancelled, guildID, warID, WarPending, WarActive))
}

// ActiveWarOnServer returns the active war between two Hub factions of the installation on
// serverID, or 0 when they are not at war.
func (r *PostgresWarRepository) ActiveWarOnServer(ctx context.Context, serverID, factionA, factionB int64) (int64, error) {
	if serverID <= 0 || factionA <= 0 || factionB <= 0 || factionA == factionB {
		return 0, nil
	}
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT w.id FROM faction_wars w
JOIN installations i ON i.id=w.installation_id AND i.game_server_id=$1
WHERE w.status=$4 AND ((w.hub_faction_a_id=$2 AND w.hub_faction_b_id=$3) OR (w.hub_faction_a_id=$3 AND w.hub_faction_b_id=$2))
ORDER BY w.id LIMIT 1`, serverID, factionA, factionB, WarActive).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (r *PostgresWarRepository) GetWar(ctx context.Context, guildID, warID int64) (*War, error) {
	return scanWar(r.pool.QueryRow(ctx, `SELECT `+warColumns+` FROM faction_wars w WHERE w.guild_id=$1 AND w.id=$2 AND w.installation_id IS NOT NULL`, guildID, warID))
}

func (r *PostgresWarRepository) GetActiveWars(ctx context.Context, guildID int64) ([]War, error) {
	return r.listWars(ctx, `SELECT `+warColumns+` FROM faction_wars w WHERE w.guild_id=$1 AND w.status='ACTIVE' AND w.installation_id IS NOT NULL ORDER BY w.started_at`, guildID)
}

func (r *PostgresWarRepository) GetWarHistory(ctx context.Context, guildID int64, limit int) ([]War, error) {
	return r.listWars(ctx, `SELECT `+warColumns+` FROM faction_wars w WHERE w.guild_id=$1 AND w.installation_id IS NOT NULL ORDER BY w.created_at DESC,w.id DESC LIMIT $2`, guildID, limit)
}

func (r *PostgresWarRepository) GetEndedWars(ctx context.Context, guildID int64, limit int) ([]War, error) {
	return r.listWars(ctx, `SELECT `+warColumns+` FROM faction_wars w WHERE w.guild_id=$1 AND w.status='ENDED' AND w.installation_id IS NOT NULL ORDER BY w.ended_at DESC,w.id DESC LIMIT $2`, guildID, limit)
}

func (r *PostgresWarRepository) listWars(ctx context.Context, q string, args ...any) ([]War, error) {
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []War
	for rows.Next() {
		w, err := scanWar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// GetWarScore counts each side's kills attributed to the war so far.
func (r *PostgresWarRepository) GetWarScore(ctx context.Context, guildID, warID int64) (int64, int64, error) {
	var a, b int64
	err := r.pool.QueryRow(ctx, `SELECT COUNT(k.id) FILTER (WHERE k.killer_hub_faction_id=w.hub_faction_a_id),COUNT(k.id) FILTER (WHERE k.killer_hub_faction_id=w.hub_faction_b_id)
FROM faction_wars w LEFT JOIN kills k ON k.war_id=w.id WHERE w.guild_id=$1 AND w.id=$2 AND w.installation_id IS NOT NULL GROUP BY w.id`, guildID, warID).Scan(&a, &b)
	return a, b, err
}

func (r *PostgresWarRepository) GetWarTopKiller(ctx context.Context, guildID, warID int64) (int64, int64, error) {
	var player, count int64
	err := r.pool.QueryRow(ctx, `SELECT killer_player_id,COUNT(*) FROM kills WHERE guild_id=$1 AND war_id=$2 AND killer_player_id IS NOT NULL GROUP BY killer_player_id ORDER BY COUNT(*) DESC,killer_player_id LIMIT 1`, guildID, warID).Scan(&player, &count)
	return player, count, err
}

func (r *PostgresWarRepository) GetWarLongestKill(ctx context.Context, guildID, warID int64) (int64, float64, error) {
	var player int64
	var distance float64
	err := r.pool.QueryRow(ctx, `SELECT killer_player_id,distance FROM kills WHERE guild_id=$1 AND war_id=$2 AND killer_player_id IS NOT NULL AND distance IS NOT NULL ORDER BY distance DESC,id LIMIT 1`, guildID, warID).Scan(&player, &distance)
	return player, distance, err
}
