package repository

import (
	"context"
	"log/slog"
	"time"
)

// Daily and hourly activity rollups (docs/RETENTION.md). Each rollup statement runs immediately
// BEFORE the player_server_activity statement it mirrors and reads the same pre-update row, so it
// adds exactly the seconds that statement is about to add to the running total. They are separate
// statements on purpose: a rollup failure is logged and ignored and can never fail - or roll back -
// the presence write the killfeed pipeline depends on.
//
// One persistence worker per server issues these calls sequentially, so the read-then-update pair is
// not raced by another writer for the same server.

// rollupDeltaCap bounds the seconds one observation may add to a day. Presence is checkpointed every
// 30 seconds, so a larger gap is a lapse in observation, not playtime (the same 300-second bound
// Checkpoint and CheckpointConnected already apply to the running total).
const rollupDeltaCap = 300

const rollupDailyUpsert = `
ON CONFLICT (server_id, player_id, day) DO UPDATE SET
    observed_seconds = player_daily_activity.observed_seconds + EXCLUDED.observed_seconds,
    sessions = player_daily_activity.sessions + EXCLUDED.sessions,
    first_seen_at = LEAST(player_daily_activity.first_seen_at, EXCLUDED.first_seen_at),
    last_seen_at = GREATEST(player_daily_activity.last_seen_at, EXCLUDED.last_seen_at),
    source = 'OBSERVED'`

// rollupConnect records presence on the connect's UTC day and counts a session when the player was
// not already connected (a repeated "is connected" line for an online player is a refresh).
func (r *ActivityRepository) rollupConnect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) {
	const q = `
INSERT INTO player_daily_activity(guild_id, server_id, player_id, day, observed_seconds, sessions, first_seen_at, last_seen_at)
SELECT $1, $2, $3, ($4::timestamptz AT TIME ZONE 'UTC')::date, 0,
       CASE WHEN COALESCE((SELECT currently_connected FROM player_server_activity WHERE guild_id=$1 AND server_id=$2 AND player_id=$3), FALSE) THEN 0 ELSE 1 END,
       $4::timestamptz, $4::timestamptz` + rollupDailyUpsert
	if _, err := r.pool.Exec(ctx, q, guildID, serverID, playerID, at); err != nil {
		slog.Warn("component=retention", "event", "daily_rollup_failed", "op", "connect", "server_id", serverID, "err", err.Error())
	}
}

// rollupPlayer adds the seconds one connected player has accrued since their last observation.
func (r *ActivityRepository) rollupPlayer(ctx context.Context, guildID, serverID, playerID int64, at time.Time, op string) {
	const q = `
INSERT INTO player_daily_activity(guild_id, server_id, player_id, day, observed_seconds, sessions, first_seen_at, last_seen_at)
SELECT guild_id, server_id, player_id, ($4::timestamptz AT TIME ZONE 'UTC')::date,
       CASE WHEN last_observed_at IS NOT NULL AND $4::timestamptz >= last_observed_at
                 AND EXTRACT(EPOCH FROM ($4::timestamptz - last_observed_at)) <= $5
            THEN EXTRACT(EPOCH FROM ($4::timestamptz - last_observed_at))::BIGINT ELSE 0 END,
       0, $4::timestamptz, $4::timestamptz
FROM player_server_activity
WHERE guild_id=$1 AND server_id=$2 AND player_id=$3 AND currently_connected` + rollupDailyUpsert
	if _, err := r.pool.Exec(ctx, q, guildID, serverID, playerID, at, rollupDeltaCap); err != nil {
		slog.Warn("component=retention", "event", "daily_rollup_failed", "op", op, "server_id", serverID, "err", err.Error())
	}
}

// rollupConnected is the per-server form of rollupPlayer for the 30-second checkpoint. The same
// pass also feeds server_hourly_activity: connected players at this sample (peak) and the seconds
// they accrued.
func (r *ActivityRepository) rollupConnected(ctx context.Context, guildID, serverID int64, at time.Time) {
	const q = `
WITH d AS (
    SELECT player_id,
           CASE WHEN last_observed_at IS NOT NULL AND $3::timestamptz >= last_observed_at
                     AND EXTRACT(EPOCH FROM ($3::timestamptz - last_observed_at)) <= $4
                THEN EXTRACT(EPOCH FROM ($3::timestamptz - last_observed_at))::BIGINT ELSE 0 END AS delta
    FROM player_server_activity
    WHERE guild_id=$1 AND server_id=$2 AND currently_connected
), daily AS (
    INSERT INTO player_daily_activity(guild_id, server_id, player_id, day, observed_seconds, sessions, first_seen_at, last_seen_at)
    SELECT $1, $2, player_id, ($3::timestamptz AT TIME ZONE 'UTC')::date, delta, 0, $3::timestamptz, $3::timestamptz FROM d` + rollupDailyUpsert + `
)
INSERT INTO server_hourly_activity(guild_id, server_id, hour, peak_players, player_seconds, samples)
SELECT $1, $2, date_trunc('hour', $3::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', COUNT(*)::int, COALESCE(SUM(delta), 0), 1 FROM d
ON CONFLICT (server_id, hour) DO UPDATE SET
    peak_players = GREATEST(server_hourly_activity.peak_players, EXCLUDED.peak_players),
    player_seconds = server_hourly_activity.player_seconds + EXCLUDED.player_seconds,
    samples = server_hourly_activity.samples + 1`
	if _, err := r.pool.Exec(ctx, q, guildID, serverID, at, rollupDeltaCap); err != nil {
		slog.Warn("component=retention", "event", "daily_rollup_failed", "op", "checkpoint_connected", "server_id", serverID, "err", err.Error())
	}
}

// BackfillDailyPresence recovers presence-only days for one server from data that already exists:
// kills, deaths and (inside the location retention window) location events. It never writes seconds
// or sessions and never touches a day that is already recorded, so it is safe to run on every start.
// Returns the number of days added.
func (r *ActivityRepository) BackfillDailyPresence(ctx context.Context, guildID, serverID int64) (int64, error) {
	const q = `
WITH seen AS (
    SELECT killer_player_id AS player_id, COALESCE(event_time, created_at) AS at FROM kills
     WHERE guild_id=$1 AND server_id=$2 AND killer_player_id IS NOT NULL
    UNION ALL
    SELECT victim_player_id, COALESCE(event_time, created_at) FROM kills
     WHERE guild_id=$1 AND server_id=$2 AND victim_player_id IS NOT NULL
    UNION ALL
    SELECT player_id, COALESCE(event_time, created_at) FROM deaths WHERE guild_id=$1 AND server_id=$2
    UNION ALL
    SELECT player_id, observed_at FROM player_location_events WHERE guild_id=$1 AND server_id=$2
)
INSERT INTO player_daily_activity(guild_id, server_id, player_id, day, observed_seconds, sessions, first_seen_at, last_seen_at, source)
SELECT $1, $2, player_id, (at AT TIME ZONE 'UTC')::date, 0, 0, MIN(at), MAX(at), 'BACKFILL'
FROM seen GROUP BY player_id, (at AT TIME ZONE 'UTC')::date
ON CONFLICT (server_id, player_id, day) DO NOTHING`
	tag, err := r.pool.Exec(ctx, q, guildID, serverID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
