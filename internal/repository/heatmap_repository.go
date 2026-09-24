package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HeatmapRepository backs Champion Phase 5 (docs/HEATMAPS.md): grid-aggregated heatmap queries
// against already-persisted data (kills, deaths, player_location_events, zone_intrusions). Every
// method aggregates in SQL (GROUP BY grid cell) - none of them ever load raw events into Go, per
// the task's explicit "database GROUP BY grid cell, not load-then-aggregate-in-memory" instruction.
//
// Coordinate association: kills/deaths carry no coordinate columns of their own. Every coordinate
// is recovered via an exact join to the player_location_events row written from the SAME ADM line:
// since Live Sync phase 2 (migration 0051) kills and deaths store the line's physical source
// (canonical ADM file + end-of-line byte offset), exactly as the location row does, so the join is
// (server, source_file, source_offset, player). The pre-phase-2 join on observed_at = event_time
// never matched anything - event_time is NULL because ADM lines carry no date (docs/
// CHAMPION_LIVE_SYNC.md finding A8) - and is kept only for any legacy row that has both. A kill or
// death with no matching location row (the line carried no position) is excluded, never fabricated.
//
// Time window: event_time when present, else the row's persistence time (created_at) - the same
// COALESCE(event_time, created_at) rule the faction statistics already use. The two identities are
// separate UNION ALL branches so each keeps an index-friendly join (the source branch matches
// uq_player_location_events_source exactly; the legacy branch is the original plan).
type HeatmapRepository struct{ pool *pgxpool.Pool }

func NewHeatmapRepository(pool *pgxpool.Pool) *HeatmapRepository {
	return &HeatmapRepository{pool: pool}
}

// HeatmapCell is one aggregated grid cell: every event whose floor(coord/resolution) landed on
// (CellX, CellZ), counted once per contributing row (never raw events).
type HeatmapCell struct {
	CellX, CellZ int64
	Count        int64
}

func aggregateRows(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]HeatmapCell, error) {
	var out []HeatmapCell
	for rows.Next() {
		var c HeatmapCell
		if err := rows.Scan(&c.CellX, &c.CellZ, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AggregateKills aggregates PvP kills by the killer's position at the moment of the kill (task
// section 12: "use the coordinate associated with the kill event"). Only genuine kills rows
// (kills table, never a raw player_location_events KILL row on its own) are counted, so this can
// never overcount relative to the kill feed itself. limit bounds the returned cell count (task
// section 20: "protect API from pathological results") - callers pass maxCells+1 and treat a
// full result as "too many cells, reject."
func (r *HeatmapRepository) AggregateKills(ctx context.Context, guildID, serverID int64, from, to time.Time, resolution int, limit int) ([]HeatmapCell, error) {
	rows, err := r.pool.Query(ctx, `
SELECT FLOOR(m.x / $5)::BIGINT, FLOOR(m.z / $5)::BIGINT, COUNT(*)
FROM (
  -- Sourced rows (Live Sync phase 2): the location row written from the same ADM line.
  SELECT ple.x, ple.z
  FROM kills k
  JOIN player_location_events ple
    ON ple.server_id = k.server_id AND ple.source_file = k.source_file AND ple.source_offset = k.source_offset
    AND ple.player_id = k.killer_player_id AND ple.event_type = 'KILL' AND ple.guild_id = k.guild_id
  WHERE k.guild_id = $1 AND k.server_id = $2 AND k.source_file IS NOT NULL
    AND COALESCE(k.event_time, k.created_at) >= $3 AND COALESCE(k.event_time, k.created_at) < $4
  UNION ALL
  -- Legacy rows: the original exact-timestamp identity (only rows that actually have event_time).
  SELECT ple.x, ple.z
  FROM kills k
  JOIN player_location_events ple
    ON ple.guild_id = k.guild_id AND ple.server_id = k.server_id AND ple.player_id = k.killer_player_id
    AND ple.event_type = 'KILL' AND ple.observed_at = k.event_time
  WHERE k.guild_id = $1 AND k.server_id = $2 AND k.source_file IS NULL
    AND k.event_time >= $3 AND k.event_time < $4
) m
GROUP BY 1, 2
LIMIT $6`, guildID, serverID, from, to, resolution, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return aggregateRows(rows)
}

// AggregateDeaths aggregates PvP deaths by the victim's position (task section 13). Only genuine
// deaths rows are counted, so RESPAWN/UNCONSCIOUS/CONNECT/DISCONNECT location events (which never
// have a corresponding deaths row) can never be miscounted as a death.
func (r *HeatmapRepository) AggregateDeaths(ctx context.Context, guildID, serverID int64, from, to time.Time, resolution int, limit int) ([]HeatmapCell, error) {
	rows, err := r.pool.Query(ctx, `
SELECT FLOOR(m.x / $5)::BIGINT, FLOOR(m.z / $5)::BIGINT, COUNT(*)
FROM (
  -- Sourced rows (Live Sync phase 2): the location row written from the same ADM line.
  SELECT ple.x, ple.z
  FROM deaths d
  JOIN player_location_events ple
    ON ple.server_id = d.server_id AND ple.source_file = d.source_file AND ple.source_offset = d.source_offset
    AND ple.player_id = d.player_id AND ple.event_type = 'DEATH' AND ple.guild_id = d.guild_id
  WHERE d.guild_id = $1 AND d.server_id = $2 AND d.source_file IS NOT NULL
    AND COALESCE(d.event_time, d.created_at) >= $3 AND COALESCE(d.event_time, d.created_at) < $4
  UNION ALL
  -- Legacy rows: the original exact-timestamp identity (only rows that actually have event_time).
  SELECT ple.x, ple.z
  FROM deaths d
  JOIN player_location_events ple
    ON ple.guild_id = d.guild_id AND ple.server_id = d.server_id AND ple.player_id = d.player_id
    AND ple.event_type = 'DEATH' AND ple.observed_at = d.event_time
  WHERE d.guild_id = $1 AND d.server_id = $2 AND d.source_file IS NULL
    AND d.event_time >= $3 AND d.event_time < $4
) m
GROUP BY 1, 2
LIMIT $6`, guildID, serverID, from, to, resolution, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return aggregateRows(rows)
}

// AggregateActivity aggregates raw location pings into a player-presence heatmap, deliberately
// NOT one contribution per raw location row (task section 11: ADM can emit many closely-spaced
// position events per player - counting each one would show ADM's own logging density, not real
// player activity). The documented sampling policy: at most one contribution per (player, minute,
// grid cell) - implemented as SELECT DISTINCT on (player_id, minute-truncated observed_at, cell)
// before the COUNT(*), so a player pinging the same cell 50 times in one minute contributes
// exactly once to that cell for that minute.
func (r *HeatmapRepository) AggregateActivity(ctx context.Context, guildID, serverID int64, from, to time.Time, resolution int, limit int) ([]HeatmapCell, error) {
	rows, err := r.pool.Query(ctx, `
WITH sampled AS (
  SELECT DISTINCT player_id, date_trunc('minute', observed_at) AS minute_bucket,
         FLOOR(x / $5)::BIGINT AS cell_x, FLOOR(z / $5)::BIGINT AS cell_z
  FROM player_location_events
  WHERE guild_id = $1 AND server_id = $2 AND observed_at >= $3 AND observed_at < $4
)
SELECT cell_x, cell_z, COUNT(*) FROM sampled GROUP BY cell_x, cell_z
LIMIT $6`, guildID, serverID, from, to, resolution, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return aggregateRows(rows)
}

// AggregateIntrusions aggregates zone intrusions by their entry coordinate (task section 14: "use
// intrusion entry location... one intrusion entry should contribute once", never once per location
// update while a player remains inside - satisfied structurally, since zone_intrusions itself only
// ever has one row per OUTSIDE->INSIDE transition, internal/killfeed's intrusion engine invariant).
// zone_intrusions carries no coordinate of its own (task's "Do NOT TOUCH: zone intrusion behavior"
// ruled out adding one) - the entry coordinate is recovered the same way kill/death coordinates
// are: an exact join to player_location_events on (player_id, server_id, observed_at=entered_at).
// Unlike the kill/death joins this one cannot also filter by a specific event_type (the location
// event that produced the transition could have been any type carrying a position), so a LATERAL
// join with a deterministic tiebreaker (lowest id) is used for the rare case of two location event
// types sharing the exact same timestamp.
func (r *HeatmapRepository) AggregateIntrusions(ctx context.Context, installationID int64, zoneID *int64, from, to time.Time, resolution int, limit int) ([]HeatmapCell, error) {
	rows, err := r.pool.Query(ctx, `
SELECT FLOOR(loc.x / $4)::BIGINT, FLOOR(loc.z / $4)::BIGINT, COUNT(*)
FROM zone_intrusions zi
JOIN LATERAL (
  SELECT x, z FROM player_location_events ple
  WHERE ple.player_id = zi.player_id AND ple.server_id = zi.server_id AND ple.observed_at = zi.entered_at
  ORDER BY ple.id LIMIT 1
) loc ON true
WHERE zi.installation_id = $1 AND zi.entered_at >= $2 AND zi.entered_at < $3
  AND ($5::BIGINT IS NULL OR zi.zone_id = $5)
GROUP BY 1, 2
LIMIT $6`, installationID, from, to, resolution, zoneID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return aggregateRows(rows)
}
