package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/ranked"
)

type ServerRankedProgress struct {
	SeasonID int64 `json:"seasonId"`
	ServerID int64 `json:"serverId"`
	StartsAt time.Time `json:"startsAt"`
	RP int64 `json:"rp"`
	Tier ranked.Tier `json:"tier"`
	NextTier ranked.Tier `json:"nextTier,omitempty"`
	Remaining int64 `json:"remainingRp"`
	TierStartRP int64 `json:"tierStartRp"`
	NextTierRP *int64 `json:"nextTierRp"`
	ServerPosition *int64 `json:"serverPosition,omitempty"`
}

// ServerPlayerProgress reports only the verified caller's local season. The
// handler must establish the player-to-installation relationship first.
func (r *RankedRepository) ServerPlayerProgress(ctx context.Context, guildID, serverID, playerID int64) (ServerRankedProgress, error) {
	var p ServerRankedProgress
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 || playerID <= 0 {
		return p, fmt.Errorf("guild, server, and player IDs are required")
	}
	var values []int64
	err := r.pool.QueryRow(ctx, `SELECT s.id,s.starts_at,s.thresholds FROM ranked_seasons s
JOIN game_servers gs ON gs.id=s.server_id AND gs.guild_id=$2
JOIN players pl ON pl.id=$3 AND pl.guild_id=gs.guild_id
WHERE s.scope='SERVER' AND s.status='ACTIVE' AND s.server_id=$1`, serverID, guildID, playerID).Scan(&p.SeasonID, &p.StartsAt, &values)
	if errors.Is(err, pgx.ErrNoRows) { return p, ErrRankedIneligible }
	if err != nil { return p, fmt.Errorf("load player ranked season: %w", err) }
	if len(values) != 7 { return p, fmt.Errorf("invalid stored ranked thresholds") }
	var thresholds ranked.Thresholds
	copy(thresholds[:], values)
	if err = thresholds.Validate(); err != nil { return p, err }
	p.ServerID = serverID
	key := strconv.FormatInt(playerID, 10)
	err = r.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0)::bigint FROM ranked_awards
WHERE season_id=$1 AND attacker_key=$2 AND outcome='AWARDED'`, p.SeasonID, key).Scan(&p.RP)
	if err != nil { return p, fmt.Errorf("load player RP: %w", err) }
	p.Tier, p.NextTier, p.Remaining, err = thresholds.Progress(p.RP)
	if err != nil { return p, err }
	for _, value := range thresholds {
		if p.RP < value { p.NextTierRP = &value; break }
		p.TierStartRP = value
	}
	if p.RP > 0 {
		var position int64
		err = r.pool.QueryRow(ctx, `WITH totals AS (
SELECT attacker_key::bigint AS player_id,SUM(amount)::bigint AS rp FROM ranked_awards
WHERE season_id=$1 AND outcome='AWARDED' GROUP BY attacker_key)
SELECT COUNT(*)+1 FROM totals WHERE rp>$2 OR (rp=$2 AND player_id<$3)`, p.SeasonID, p.RP, playerID).Scan(&position)
		if err != nil { return p, fmt.Errorf("load player server position: %w", err) }
		p.ServerPosition = &position
	}
	return p, nil
}
