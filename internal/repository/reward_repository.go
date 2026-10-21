package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/rewards"
)

// RewardRepository stores reward rules and finds who has earned them. Payouts go through the
// economy ledger (SYSTEM_REWARD, source_key "reward:..."), so each one is paid at most once.
type RewardRepository struct {
	pool    *pgxpool.Pool
	economy *EconomyRepository
}

func NewRewardRepository(pool *pgxpool.Pool) *RewardRepository {
	return &RewardRepository{pool: pool, economy: NewEconomyRepository(pool)}
}

var ErrRewardRuleNotFound = errors.New("reward rule not found")

const rewardRuleCols = `id,kind,tier,points,min_hours,min_days,places,enabled,created_by,created_at`

func scanRewardRule(row pgx.Row) (rewards.Rule, error) {
	var r rewards.Rule
	err := row.Scan(&r.ID, &r.Kind, &r.Tier, &r.Points, &r.MinHours, &r.MinDays, &r.Places, &r.Enabled, &r.CreatedBy, &r.CreatedAt)
	return r, err
}

func (r *RewardRepository) ListRules(ctx context.Context, guildID int64) ([]rewards.Rule, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+rewardRuleCols+` FROM reward_rules WHERE guild_id=$1 ORDER BY kind, points, id`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []rewards.Rule{}
	for rows.Next() {
		rule, err := scanRewardRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, rows.Err()
}

// UpsertRule creates the rule, or updates the guild's existing rule of the same kind (and tier).
// created_at is kept on update: it is the line before which history is never paid.
func (r *RewardRepository) UpsertRule(ctx context.Context, guildID int64, rule rewards.Rule, by string) (rewards.Rule, error) {
	return scanRewardRule(r.pool.QueryRow(ctx, `
INSERT INTO reward_rules(guild_id,kind,tier,points,min_hours,min_days,places,enabled,created_by)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (guild_id,kind,tier) DO UPDATE SET points=EXCLUDED.points,min_hours=EXCLUDED.min_hours,min_days=EXCLUDED.min_days,
  places=EXCLUDED.places,enabled=EXCLUDED.enabled,updated_at=NOW()
RETURNING `+rewardRuleCols, guildID, rule.Kind, rule.Tier, rule.Points, rule.MinHours, rule.MinDays, rule.Places, rule.Enabled, by))
}

func (r *RewardRepository) DeleteRule(ctx context.Context, guildID, id int64) (rewards.Rule, error) {
	rule, err := scanRewardRule(r.pool.QueryRow(ctx, `DELETE FROM reward_rules WHERE guild_id=$1 AND id=$2 RETURNING `+rewardRuleCols, guildID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return rule, ErrRewardRuleNotFound
	}
	return rule, err
}

// RewardPayout is one automatic reward paid.
type RewardPayout struct {
	PlayerID    int64     `json:"playerId"`
	PlayerName  string    `json:"playerName"`
	Amount      int64     `json:"amount"`
	Reference   string    `json:"reference"`
	Description string    `json:"description"`
	PaidAt      time.Time `json:"paidAt"`
}

func (r *RewardRepository) RecentPayouts(ctx context.Context, guildID int64, limit int) ([]RewardPayout, error) {
	rows, err := r.pool.Query(ctx, `SELECT pt.player_id,p.display_name,pt.amount,pt.source_key,COALESCE(pt.description,''),pt.created_at
FROM point_transactions pt JOIN players p ON p.id=pt.player_id
WHERE pt.guild_id=$1 AND pt.reason_type=$2 AND pt.source_key LIKE 'reward:%'
ORDER BY pt.id DESC LIMIT $3`, guildID, TxSystemReward, clampLimit(limit, 50, 200))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RewardPayout{}
	for rows.Next() {
		var p RewardPayout
		if err := rows.Scan(&p.PlayerID, &p.PlayerName, &p.Amount, &p.Reference, &p.Description, &p.PaidAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// paidSet is the players already paid under one reference.
func (r *RewardRepository) paidSet(ctx context.Context, guildID int64, ref string) (map[int64]bool, error) {
	rows, err := r.pool.Query(ctx, `SELECT player_id FROM point_transactions WHERE guild_id=$1 AND reason_type=$2 AND source_key=$3`, guildID, TxSystemReward, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// RankHolder is a player's current tier in one server's active Ranked season.
type RankHolder struct {
	SeasonID, PlayerID int64
	Tier               ranked.Tier
}

// RankHolders computes every ranked player's tier in the guild's active server seasons.
func (r *RewardRepository) RankHolders(ctx context.Context, guildID int64) ([]RankHolder, error) {
	rows, err := r.pool.Query(ctx, `
SELECT s.id,s.thresholds,p.id,SUM(a.amount)::bigint
FROM ranked_seasons s JOIN game_servers gs ON gs.id=s.server_id AND gs.guild_id=$1
JOIN ranked_awards a ON a.season_id=s.id AND a.outcome='AWARDED'
JOIN players p ON p.id::text=a.attacker_key AND p.guild_id=$1
WHERE s.scope='SERVER' AND s.status='ACTIVE'
GROUP BY s.id,s.thresholds,p.id`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RankHolder{}
	for rows.Next() {
		var h RankHolder
		var values []int64
		var rp int64
		if err := rows.Scan(&h.SeasonID, &values, &h.PlayerID, &rp); err != nil {
			return nil, err
		}
		if len(values) != 7 {
			continue
		}
		var th ranked.Thresholds
		copy(th[:], values)
		tier, _, _, err := th.Progress(rp)
		if err != nil {
			continue
		}
		h.Tier = tier
		out = append(out, h)
	}
	return out, rows.Err()
}

// WeeklyActive is every player observed at least minHours across at least minDays in [start,end).
func (r *RewardRepository) WeeklyActive(ctx context.Context, guildID int64, start, end time.Time, minHours, minDays int) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `SELECT player_id FROM player_daily_activity
WHERE guild_id=$1 AND day >= $2::date AND day < $3::date
GROUP BY player_id HAVING SUM(observed_seconds) >= $4 AND COUNT(DISTINCT day) >= $5`, guildID, start, end, int64(minHours)*3600, minDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SeasonPlace is a finishing position in an ended stats season.
type SeasonPlace struct {
	SeasonID, PlayerID int64
	Place              int
}

// SeasonTop returns the top `places` killers of every stats season that ended at or after since.
func (r *RewardRepository) SeasonTop(ctx context.Context, guildID int64, since time.Time, places int) ([]SeasonPlace, error) {
	rows, err := r.pool.Query(ctx, `
SELECT season_id,killer_player_id,place FROM (
  SELECT k.season_id,k.killer_player_id,ROW_NUMBER() OVER (PARTITION BY k.season_id ORDER BY COUNT(*) DESC,k.killer_player_id) AS place
  FROM kills k JOIN seasons s ON s.id=k.season_id AND s.guild_id=$1
  WHERE k.guild_id=$1 AND s.status='ENDED' AND s.ends_at >= $2
  GROUP BY k.season_id,k.killer_player_id
) ranked WHERE place <= $3 ORDER BY season_id,place`, guildID, since, places)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SeasonPlace{}
	for rows.Next() {
		var p SeasonPlace
		if err := rows.Scan(&p.SeasonID, &p.PlayerID, &p.Place); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RewardGrant is one payout to make.
type RewardGrant struct {
	PlayerID    int64
	Amount      int64
	Reference   string
	Description string
}

// Pay credits every grant not already paid under its reference and returns how many were new.
func (r *RewardRepository) Pay(ctx context.Context, guildID int64, grants []RewardGrant) (int, error) {
	paidByRef := map[string]map[int64]bool{}
	paid := 0
	for _, g := range grants {
		set, ok := paidByRef[g.Reference]
		if !ok {
			var err error
			if set, err = r.paidSet(ctx, guildID, g.Reference); err != nil {
				return paid, err
			}
			paidByRef[g.Reference] = set
		}
		if set[g.PlayerID] || g.Amount <= 0 {
			continue
		}
		e, err := r.economy.Credit(ctx, LedgerParams{GuildID: guildID, PlayerID: g.PlayerID, Type: TxSystemReward, Amount: g.Amount,
			ReferenceID: g.Reference, Description: g.Description, CreatedBy: "SYSTEM"})
		if err != nil {
			return paid, fmt.Errorf("pay reward %s: %w", g.Reference, err)
		}
		set[g.PlayerID] = true
		if !e.Duplicate {
			paid++
		}
	}
	return paid, nil
}
