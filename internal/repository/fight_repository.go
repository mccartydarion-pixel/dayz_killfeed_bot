package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FightRepository reads what a fight replay is built from (docs/FIGHT_REPLAY.md): persisted kills
// with the positions logged on their ADM line, and the participants' persisted location samples.
// Nothing is written and nothing is interpolated.
type FightRepository struct{ pool *pgxpool.Pool }

func NewFightRepository(pool *pgxpool.Pool) *FightRepository { return &FightRepository{pool: pool} }

// FightKill is one PvP kill with the positions its ADM line carried (nil when it carried none).
type FightKill struct {
	ID         int64
	At         time.Time
	KillerID   int64
	VictimID   int64
	KillerName string
	VictimName string
	Weapon     string
	Distance   *float64
	Headshot   bool
	Longshot   bool
	KillerX    *float64
	KillerZ    *float64
	VictimX    *float64
	VictimZ    *float64
}

// fightKillLocation joins the location row written from the same ADM line as the kill, for one
// side of it: by source identity for sourced rows, by exact timestamp for legacy ones - the same
// identity the heatmap uses (HeatmapRepository.AggregateKills).
const fightKillLocation = `LEFT JOIN LATERAL (
    SELECT ple.x, ple.z FROM player_location_events ple
    WHERE ple.server_id = k.server_id AND ple.player_id = %s AND ple.event_type = 'KILL'
      AND ((k.source_file IS NOT NULL AND ple.source_file = k.source_file AND ple.source_offset = k.source_offset)
        OR (k.source_file IS NULL AND ple.source_file IS NULL AND ple.observed_at = k.event_time))
    LIMIT 1) %s ON TRUE`

// Kills returns the server's PvP kills in [from, to), oldest first. A self-kill is not PvP.
func (r *FightRepository) Kills(ctx context.Context, guildID, serverID int64, from, to time.Time, limit int) ([]FightKill, error) {
	return r.kills(ctx, guildID, serverID, from, to, limit, "ORDER BY 2, k.id")
}

// RecentKills is Kills newest first: the live map's feed (docs/LIVE_MAP.md) wants the latest
// kills of the window, not the earliest.
func (r *FightRepository) RecentKills(ctx context.Context, guildID, serverID int64, from, to time.Time, limit int) ([]FightKill, error) {
	return r.kills(ctx, guildID, serverID, from, to, limit, "ORDER BY 2 DESC, k.id DESC")
}

func (r *FightRepository) kills(ctx context.Context, guildID, serverID int64, from, to time.Time, limit int, order string) ([]FightKill, error) {
	q := `
SELECT k.id, COALESCE(k.event_time, k.created_at), k.killer_player_id, k.victim_player_id, COALESCE(kp.display_name, ''), COALESCE(vp.display_name, ''),
       COALESCE(k.weapon_display, k.weapon_raw, ''), k.distance, k.headshot, k.longshot, kl.x, kl.z, vl.x, vl.z
FROM kills k
LEFT JOIN players kp ON kp.id = k.killer_player_id
LEFT JOIN players vp ON vp.id = k.victim_player_id
` + fmt.Sprintf(fightKillLocation, "k.killer_player_id", "kl") + `
` + fmt.Sprintf(fightKillLocation, "k.victim_player_id", "vl") + `
WHERE k.guild_id=$1 AND k.server_id=$2 AND COALESCE(k.event_time, k.created_at) >= $3 AND COALESCE(k.event_time, k.created_at) < $4
  AND k.killer_player_id IS NOT NULL AND k.victim_player_id IS NOT NULL AND k.killer_player_id <> k.victim_player_id
` + order + ` LIMIT $5`
	rows, err := r.pool.Query(ctx, q, guildID, serverID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FightKill{}
	for rows.Next() {
		var k FightKill
		if err := rows.Scan(&k.ID, &k.At, &k.KillerID, &k.VictimID, &k.KillerName, &k.VictimName, &k.Weapon, &k.Distance, &k.Headshot, &k.Longshot,
			&k.KillerX, &k.KillerZ, &k.VictimX, &k.VictimZ); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// KillTime returns when one kill of the server happened, or (zero, false).
func (r *FightRepository) KillTime(ctx context.Context, guildID, serverID, killID int64) (time.Time, bool, error) {
	var at *time.Time
	err := r.pool.QueryRow(ctx, `SELECT MAX(COALESCE(event_time, created_at)) FROM kills WHERE guild_id=$1 AND server_id=$2 AND id=$3`, guildID, serverID, killID).Scan(&at)
	if err != nil || at == nil {
		return time.Time{}, false, err
	}
	return *at, true, nil
}

// TrackPoint is one persisted position sample of one player.
type TrackPoint struct {
	PlayerID  int64
	At        time.Time
	X, Z      float64
	EventType string
}

// Tracks returns the persisted location samples of playerIDs on the server in [from, to], oldest
// first, at most limit rows.
func (r *FightRepository) Tracks(ctx context.Context, serverID int64, playerIDs []int64, from, to time.Time, limit int) ([]TrackPoint, error) {
	if len(playerIDs) == 0 {
		return []TrackPoint{}, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT player_id, observed_at, x, z, event_type FROM player_location_events
WHERE server_id=$1 AND player_id = ANY($2) AND observed_at >= $3 AND observed_at <= $4
ORDER BY observed_at, id LIMIT $5`, serverID, playerIDs, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrackPoint{}
	for rows.Next() {
		var p TrackPoint
		if err := rows.Scan(&p.PlayerID, &p.At, &p.X, &p.Z, &p.EventType); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
