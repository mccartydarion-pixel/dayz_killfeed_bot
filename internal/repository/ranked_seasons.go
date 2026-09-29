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

type ServerRankedSeason struct {
	ID int64 `json:"id"`
	ServerID int64 `json:"serverId"`
	Platform string `json:"platform"`
	Status string `json:"status"`
	RPPerKill int64 `json:"rpPerKill"`
	Thresholds ranked.Thresholds `json:"thresholds"`
	StartsAt time.Time `json:"startsAt"`
}

// ActiveServerSeason returns the selected server's active rules or nil before
// its first season. Both guild and server IDs are required for tenant scope.
func (r *RankedRepository) ActiveServerSeason(ctx context.Context, guildID, serverID int64) (*ServerRankedSeason, error) {
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 { return nil, fmt.Errorf("guild and server IDs are required") }
	var s ServerRankedSeason
	var values []int64
	err := r.pool.QueryRow(ctx, `SELECT s.id,s.platform,s.rp_per_kill,s.thresholds,s.starts_at
FROM ranked_seasons s JOIN game_servers gs ON gs.id=s.server_id AND gs.guild_id=$1
WHERE s.scope='SERVER' AND s.status='ACTIVE' AND s.server_id=$2`, guildID, serverID).Scan(&s.ID, &s.Platform, &s.RPPerKill, &values, &s.StartsAt)
	if errors.Is(err, pgx.ErrNoRows) { return nil, nil }
	if err != nil { return nil, fmt.Errorf("load active server ranked season: %w", err) }
	if len(values) != 7 { return nil, fmt.Errorf("invalid stored ranked thresholds") }
	copy(s.Thresholds[:], values)
	if err = s.Thresholds.Validate(); err != nil { return nil, err }
	s.ServerID, s.Status = serverID, "ACTIVE"
	return &s, nil
}

// StartServerSeason opens a local season, or atomically archives the active
// season and starts a new one when reset is explicitly requested. The server
// row lock serializes concurrent starts and prevents a reset from crossing
// tenant boundaries. Archived awards are never changed.
func (r *RankedRepository) StartServerSeason(ctx context.Context, guildID, serverID, rpPerKill int64, thresholds ranked.Thresholds, reset bool, now time.Time) (ServerRankedSeason, error) {
	var season ServerRankedSeason
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 || rpPerKill <= 0 || now.IsZero() {
		return season, fmt.Errorf("guild, server, RP per kill, and start time are required")
	}
	if err := thresholds.Validate(); err != nil { return season, err }
	tx, err := r.pool.Begin(ctx)
	if err != nil { return season, fmt.Errorf("begin ranked season: %w", err) }
	defer tx.Rollback(ctx)
	var platform string
	err = tx.QueryRow(ctx, `SELECT platform FROM game_servers WHERE id=$1 AND guild_id=$2 AND status='ACTIVE' AND platform IN ('PLAYSTATION','XBOX') FOR UPDATE`, serverID, guildID).Scan(&platform)
	if errors.Is(err, pgx.ErrNoRows) { return season, ErrRankedIneligible }
	if err != nil { return season, fmt.Errorf("load ranked server: %w", err) }
	var activeID int64
	var activeStart time.Time
	err = tx.QueryRow(ctx, `SELECT id,starts_at FROM ranked_seasons WHERE scope='SERVER' AND server_id=$1 AND status='ACTIVE' FOR UPDATE`, serverID).Scan(&activeID, &activeStart)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) { return season, fmt.Errorf("load active ranked season: %w", err) }
	if err == nil {
		if !reset { return season, ErrRankedSeasonConflict }
		if !now.After(activeStart) { return season, fmt.Errorf("new season must start after the current season") }
		if _, err = tx.Exec(ctx, `UPDATE ranked_seasons SET status='ARCHIVED',ends_at=$2 WHERE id=$1 AND status='ACTIVE'`, activeID, now); err != nil {
			return season, fmt.Errorf("archive ranked season: %w", err)
		}
	} else if reset {
		return season, ErrRankedIneligible
	}
	values := make([]int64, len(thresholds))
	copy(values, thresholds[:])
	err = tx.QueryRow(ctx, `INSERT INTO ranked_seasons(scope,platform,server_id,status,rp_per_kill,thresholds,starts_at)
VALUES('SERVER',$1,$2,'ACTIVE',$3,$4,$5) RETURNING id`, platform, serverID, rpPerKill, values, now).Scan(&season.ID)
	if err != nil { return ServerRankedSeason{}, fmt.Errorf("insert ranked season: %w", err) }
	if err = tx.Commit(ctx); err != nil { return ServerRankedSeason{}, fmt.Errorf("commit ranked season: %w", err) }
	season.ServerID, season.Platform, season.Status, season.RPPerKill, season.Thresholds, season.StartsAt = serverID, platform, "ACTIVE", rpPerKill, thresholds, now
	return season, nil
}
