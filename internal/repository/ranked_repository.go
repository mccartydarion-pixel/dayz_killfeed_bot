package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/ranked"
)

var ErrRankedIneligible = errors.New("kill is not eligible for server ranked points")

// RankedRepository records server season awards.
type RankedRepository struct{ pool *pgxpool.Pool }

func NewRankedRepository(pool *pgxpool.Pool) *RankedRepository { return &RankedRepository{pool: pool} }

type RankedAward struct {
	SeasonID int64
	KillID   int64
	Outcome  string
	Amount   int64
}

type ServerStanding struct {
	PlayerID  int64
	Name      string
	RP        int64
	Tier      ranked.Tier
	NextTier  ranked.Tier
	Remaining int64
}

// ServerStandings reads the active local season only. All displayed totals
// come from immutable awarded rows; archived seasons remain intact separately.
func (r *RankedRepository) ServerStandings(ctx context.Context, serverID int64, limit int) ([]ServerStanding, error) {
	if r == nil || r.pool == nil || serverID <= 0 || limit < 1 || limit > 250 {
		return nil, fmt.Errorf("valid server ID and limit (1..250) are required")
	}
	var seasonID int64
	var values []int64
	err := r.pool.QueryRow(ctx, `SELECT id,thresholds FROM ranked_seasons
WHERE scope='SERVER' AND status='ACTIVE' AND server_id=$1`, serverID).Scan(&seasonID, &values)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRankedIneligible
	}
	if err != nil {
		return nil, fmt.Errorf("load server ranked season: %w", err)
	}
	if len(values) != 7 {
		return nil, fmt.Errorf("invalid stored ranked thresholds")
	}
	var thresholds ranked.Thresholds
	copy(thresholds[:], values)
	if err := thresholds.Validate(); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT p.id,p.display_name,SUM(a.amount)::bigint AS rp
FROM ranked_awards a JOIN ranked_seasons s ON s.id=a.season_id
JOIN players p ON p.id::text=a.attacker_key AND p.guild_id=(SELECT guild_id FROM game_servers WHERE id=s.server_id)
WHERE a.season_id=$1 AND a.outcome='AWARDED'
GROUP BY p.id,p.display_name ORDER BY rp DESC,p.id ASC LIMIT $2`, seasonID, limit)
	if err != nil {
		return nil, fmt.Errorf("query server standings: %w", err)
	}
	defer rows.Close()
	var out []ServerStanding
	for rows.Next() {
		var entry ServerStanding
		if err := rows.Scan(&entry.PlayerID, &entry.Name, &entry.RP); err != nil {
			return nil, err
		}
		entry.Tier, entry.NextTier, entry.Remaining, err = thresholds.Progress(entry.RP)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// RecordServerKill processes a previously persisted kill in an active local
// season. The award and five-minute pair decision are committed together.
// Replays return the existing decision without awarding again. Delayed kills
// older than the pair's latest awarded event are marked OUT_OF_ORDER rather
// than retroactively changing RP already displayed to players.
func (r *RankedRepository) RecordServerKill(ctx context.Context, seasonID, killID int64) (RankedAward, error) {
	var result RankedAward
	if r == nil || r.pool == nil || seasonID <= 0 || killID <= 0 {
		return result, fmt.Errorf("ranked season and kill IDs are required")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("begin ranked award: %w", err)
	}
	defer tx.Rollback(ctx)
	// Archived seasons reject new awards, but a replay of an already decided
	// kill must still return its immutable outcome after the reset.
	result = RankedAward{SeasonID: seasonID, KillID: killID}
	err = tx.QueryRow(ctx, `SELECT outcome,amount FROM ranked_awards WHERE season_id=$1 AND kill_id=$2`, seasonID, killID).Scan(&result.Outcome, &result.Amount)
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RankedAward{}, fmt.Errorf("read ranked replay: %w", err)
	}

	var serverID, guildID, killerID, victimID, rp int64
	var fingerprint string
	var eventTime time.Time
	err = tx.QueryRow(ctx, `
SELECT s.server_id,k.guild_id,k.killer_player_id,k.victim_player_id,s.rp_per_kill,k.event_fingerprint,k.event_time
FROM ranked_seasons s JOIN game_servers gs ON gs.id=s.server_id
JOIN kills k ON k.id=$2 AND k.server_id=s.server_id AND k.guild_id=gs.guild_id
WHERE s.id=$1 AND s.scope='SERVER' AND s.status='ACTIVE' AND s.platform=gs.platform
  AND k.killer_player_id IS NOT NULL AND k.victim_player_id IS NOT NULL
  AND k.killer_player_id<>k.victim_player_id AND k.event_time IS NOT NULL
  AND k.event_time>=s.starts_at AND (s.ends_at IS NULL OR k.event_time<s.ends_at)
FOR SHARE OF s`, seasonID, killID).Scan(&serverID, &guildID, &killerID, &victimID, &rp, &fingerprint, &eventTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrRankedIneligible
	}
	if err != nil {
		return result, fmt.Errorf("load ranked kill: %w", err)
	}
	_ = guildID // checked by the query; identities below are guild-scoped player row IDs.

	// Serialize this attacker/victim pair for the season across workers. The
	// transaction lock is held through the insert, so concurrent repeat kills
	// cannot both pass the cooldown query.
	attacker := strconv.FormatInt(killerID, 10)
	victim := strconv.FormatInt(victimID, 10)
	lockKey := fmt.Sprintf("ranked:%d:%s:%s", seasonID, attacker, victim)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return result, fmt.Errorf("lock ranked pair: %w", err)
	}

	result = RankedAward{SeasonID: seasonID, KillID: killID}
	err = tx.QueryRow(ctx, `SELECT outcome,amount FROM ranked_awards WHERE season_id=$1 AND kill_id=$2`, seasonID, killID).Scan(&result.Outcome, &result.Amount)
	if err == nil {
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RankedAward{}, fmt.Errorf("read ranked replay: %w", err)
	}

	var previous time.Time
	err = tx.QueryRow(ctx, `SELECT event_time FROM ranked_awards
WHERE season_id=$1 AND attacker_key=$2 AND victim_key=$3 AND outcome='AWARDED'
ORDER BY event_time DESC LIMIT 1`, seasonID, attacker, victim).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return RankedAward{}, fmt.Errorf("read ranked cooldown: %w", err)
	}
	result.Outcome, result.Amount = "AWARDED", rp
	if err == nil {
		switch {
		case eventTime.Before(previous):
			result.Outcome, result.Amount = "OUT_OF_ORDER", 0
		case !ranked.EligibleRepeat(eventTime, previous):
			result.Outcome, result.Amount = "COOLDOWN", 0
		}
	}
	// The source key identifies this server's persisted event.
	sourceKey := fmt.Sprintf("%d:%s", serverID, fingerprint)
	var inserted int64
	err = tx.QueryRow(ctx, `INSERT INTO ranked_awards
(season_id,kill_id,source_key,attacker_key,victim_key,event_time,outcome,amount)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING RETURNING id`,
		seasonID, killID, sourceKey, attacker, victim, eventTime, result.Outcome, result.Amount).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return RankedAward{}, ErrDuplicate
	}
	if err != nil {
		return RankedAward{}, fmt.Errorf("insert ranked award: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return RankedAward{}, fmt.Errorf("commit ranked award: %w", err)
	}
	return result, nil
}
