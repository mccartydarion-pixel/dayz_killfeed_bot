package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/ranked"
)

var ErrRankedSeasonConflict = errors.New("server already has an active ranked season")

// ErrRankedServerIneligible: the selected server is not an active
// PlayStation/Xbox server of this guild, so no Ranked season can open on it.
var ErrRankedServerIneligible = errors.New("server is not an active PlayStation or Xbox server of this guild")

// ErrRankedNoActiveSeason: a reset was requested but no season is active.
var ErrRankedNoActiveSeason = errors.New("server has no active ranked season to reset")

type ServerRankedSeason struct {
	ID        int64  `json:"id"`
	ServerID  int64  `json:"serverId"`
	Platform  string `json:"platform"`
	Status    string `json:"status"`
	RPPerKill int64  `json:"rpPerKill"`
	// SameVictimCooldownMinutes is the season's current wait before the same
	// attacker earns RP from the same victim again (0 = no wait). Unlike the
	// other rules it can change mid-season (ChangeActiveSeasonCooldown).
	SameVictimCooldownMinutes int               `json:"sameVictimCooldownMinutes"`
	Thresholds                ranked.Thresholds `json:"thresholds"`
	StartsAt                  time.Time         `json:"startsAt"`
}

// ActiveServerSeason returns the selected server's active rules or nil before
// its first season. Both guild and server IDs are required for tenant scope.
func (r *RankedRepository) ActiveServerSeason(ctx context.Context, guildID, serverID int64) (*ServerRankedSeason, error) {
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 {
		return nil, fmt.Errorf("guild and server IDs are required")
	}
	var s ServerRankedSeason
	var values []int64
	err := r.pool.QueryRow(ctx, `SELECT s.id,s.platform,s.rp_per_kill,s.same_victim_cooldown_minutes,s.thresholds,s.starts_at
FROM ranked_seasons s JOIN game_servers gs ON gs.id=s.server_id AND gs.guild_id=$1
WHERE s.scope='SERVER' AND s.status='ACTIVE' AND s.server_id=$2`, guildID, serverID).Scan(&s.ID, &s.Platform, &s.RPPerKill, &s.SameVictimCooldownMinutes, &values, &s.StartsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load active server ranked season: %w", err)
	}
	if len(values) != 7 {
		return nil, fmt.Errorf("invalid stored ranked thresholds")
	}
	copy(s.Thresholds[:], values)
	if err = s.Thresholds.Validate(); err != nil {
		return nil, err
	}
	s.ServerID, s.Status = serverID, "ACTIVE"
	return &s, nil
}

// StartServerSeason opens a local season, or atomically archives the active
// season and starts a new one when reset is explicitly requested. The server
// row lock serializes concurrent starts and prevents a reset from crossing
// tenant boundaries. Archived awards are never changed. The same-victim wait
// (minutes, 0 to 120) starts here and is the one rule that can change later.
func (r *RankedRepository) StartServerSeason(ctx context.Context, guildID, serverID, rpPerKill int64, thresholds ranked.Thresholds, sameVictimCooldownMinutes int, reset bool, now time.Time) (ServerRankedSeason, error) {
	var season ServerRankedSeason
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 || rpPerKill <= 0 || now.IsZero() {
		return season, fmt.Errorf("guild, server, RP per kill, and start time are required")
	}
	if err := thresholds.Validate(); err != nil {
		return season, err
	}
	if !ranked.ValidSameVictimCooldownMinutes(sameVictimCooldownMinutes) {
		return season, fmt.Errorf("same-victim wait must be 0 to %d minutes", ranked.MaxSameVictimCooldownMinutes)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return season, fmt.Errorf("begin ranked season: %w", err)
	}
	defer tx.Rollback(ctx)
	var platform string
	// Eligibility is the server's active flag (what the ADM workers run on),
	// its owning guild and a console platform. game_servers.status is a
	// display label - production writes CONNECTED (/server select) or
	// ONLINE/OFFLINE (SaaS setup), never ACTIVE - so it must not gate this.
	err = tx.QueryRow(ctx, `SELECT platform FROM game_servers WHERE id=$1 AND guild_id=$2 AND active AND platform IN ('PLAYSTATION','XBOX') FOR UPDATE`, serverID, guildID).Scan(&platform)
	if errors.Is(err, pgx.ErrNoRows) {
		return season, ErrRankedServerIneligible
	}
	if err != nil {
		return season, fmt.Errorf("load ranked server: %w", err)
	}
	var activeID int64
	var activeStart time.Time
	err = tx.QueryRow(ctx, `SELECT id,starts_at FROM ranked_seasons WHERE scope='SERVER' AND server_id=$1 AND status='ACTIVE' FOR UPDATE`, serverID).Scan(&activeID, &activeStart)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return season, fmt.Errorf("load active ranked season: %w", err)
	}
	if err == nil {
		if !reset {
			return season, ErrRankedSeasonConflict
		}
		if !now.After(activeStart) {
			return season, fmt.Errorf("new season must start after the current season")
		}
		if _, err = tx.Exec(ctx, `UPDATE ranked_seasons SET status='ARCHIVED',ends_at=$2 WHERE id=$1 AND status='ACTIVE'`, activeID, now); err != nil {
			return season, fmt.Errorf("archive ranked season: %w", err)
		}
	} else if reset {
		return season, ErrRankedNoActiveSeason
	}
	values := make([]int64, len(thresholds))
	copy(values, thresholds[:])
	err = tx.QueryRow(ctx, `INSERT INTO ranked_seasons(scope,platform,server_id,status,rp_per_kill,thresholds,starts_at,same_victim_cooldown_minutes)
VALUES('SERVER',$1,$2,'ACTIVE',$3,$4,$5,$6) RETURNING id`, platform, serverID, rpPerKill, values, now, sameVictimCooldownMinutes).Scan(&season.ID)
	if err != nil {
		return ServerRankedSeason{}, fmt.Errorf("insert ranked season: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ranked_season_cooldown_changes(season_id,cooldown_minutes,effective_from,changed_by) VALUES($1,$2,$3,'season-start')`, season.ID, sameVictimCooldownMinutes, now); err != nil {
		return ServerRankedSeason{}, fmt.Errorf("record ranked season wait: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ServerRankedSeason{}, fmt.Errorf("commit ranked season: %w", err)
	}
	season.ServerID, season.Platform, season.Status, season.RPPerKill, season.Thresholds, season.StartsAt = serverID, platform, "ACTIVE", rpPerKill, thresholds, now
	season.SameVictimCooldownMinutes = sameVictimCooldownMinutes
	return season, nil
}

// ChangeActiveSeasonCooldown changes the same-victim wait of the server's ACTIVE season and nothing
// else. The change takes effect at `now`: RecordServerKill judges each kill by the wait that was in
// force at the kill's own event time, so decisions already made are untouched and a kill that
// happened before the change keeps the old wait even when it is processed or reconciled later.
// The season row lock serializes this with award transactions (they hold FOR SHARE on it).
func (r *RankedRepository) ChangeActiveSeasonCooldown(ctx context.Context, guildID, serverID int64, minutes int, changedBy string, now time.Time) (season ServerRankedSeason, previous int, err error) {
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 || now.IsZero() {
		return season, 0, fmt.Errorf("guild, server and change time are required")
	}
	if !ranked.ValidSameVictimCooldownMinutes(minutes) {
		return season, 0, fmt.Errorf("same-victim wait must be 0 to %d minutes", ranked.MaxSameVictimCooldownMinutes)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return season, 0, fmt.Errorf("begin ranked wait change: %w", err)
	}
	defer tx.Rollback(ctx)
	var values []int64
	err = tx.QueryRow(ctx, `SELECT s.id,s.platform,s.rp_per_kill,s.same_victim_cooldown_minutes,s.thresholds,s.starts_at
FROM ranked_seasons s JOIN game_servers gs ON gs.id=s.server_id AND gs.guild_id=$1
WHERE s.scope='SERVER' AND s.status='ACTIVE' AND s.server_id=$2 FOR UPDATE OF s`, guildID, serverID).Scan(&season.ID, &season.Platform, &season.RPPerKill, &previous, &values, &season.StartsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerRankedSeason{}, 0, ErrRankedNoActiveSeason
	}
	if err != nil {
		return ServerRankedSeason{}, 0, fmt.Errorf("load active ranked season: %w", err)
	}
	if len(values) != 7 {
		return ServerRankedSeason{}, 0, fmt.Errorf("invalid stored ranked thresholds")
	}
	copy(season.Thresholds[:], values)
	season.ServerID, season.Status, season.SameVictimCooldownMinutes = serverID, "ACTIVE", previous
	if minutes == previous {
		return season, previous, nil // nothing to change; no history row
	}
	// A season without history (created outside StartServerSeason) first gets its old value from
	// its start, otherwise kills before `now` would fall back to the column's new value.
	if _, err = tx.Exec(ctx, `INSERT INTO ranked_season_cooldown_changes(season_id,cooldown_minutes,effective_from,changed_by)
SELECT $1,$2,$3,'season-start' WHERE NOT EXISTS (SELECT 1 FROM ranked_season_cooldown_changes WHERE season_id=$1)`, season.ID, previous, season.StartsAt); err != nil {
		return ServerRankedSeason{}, 0, fmt.Errorf("seed ranked wait history: %w", err)
	}
	// Never earlier than the latest recorded change, so the history stays ordered if clocks step back.
	if _, err = tx.Exec(ctx, `INSERT INTO ranked_season_cooldown_changes(season_id,cooldown_minutes,effective_from,changed_by)
SELECT $1,$2,GREATEST($3::timestamptz,(SELECT MAX(effective_from) FROM ranked_season_cooldown_changes WHERE season_id=$1)),$4`, season.ID, minutes, now, changedBy); err != nil {
		return ServerRankedSeason{}, 0, fmt.Errorf("record ranked wait change: %w", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE ranked_seasons SET same_victim_cooldown_minutes=$2 WHERE id=$1`, season.ID, minutes); err != nil {
		return ServerRankedSeason{}, 0, fmt.Errorf("update ranked season wait: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return ServerRankedSeason{}, 0, fmt.Errorf("commit ranked wait change: %w", err)
	}
	season.SameVictimCooldownMinutes = minutes
	return season, previous, nil
}
