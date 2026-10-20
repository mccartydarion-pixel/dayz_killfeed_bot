package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/ranked"
)

// Ranked bonuses (docs/RANKED_BONUSES.md): optional extra RP on top of a ranked kill, each switched
// on per server. A bonus is decided with the award, inside its transaction, from when the kill
// happened. Bonuses are not multiplied by double RP. Kills that earn nothing (cooldown, out of
// order) earn no bonus.

const (
	BonusBounty     = "BOUNTY"
	BonusUnderdog   = "UNDERDOG"
	BonusRevenge    = "REVENGE"
	BonusDailyFirst = "DAILY_FIRST"

	WantedTop    = "TOP"
	WantedStreak = "STREAK"

	// bonusRepeatGap stops a pair of friends farming a bonus off each other: the #1 bounty, the
	// underdog bonus on the same victim and revenge on the same victim pay at most once an hour.
	bonusRepeatGap = time.Hour
)

var ErrRankedBonusInvalid = errors.New("bonus settings are out of range")

// RankedBonusSettings are one server's switches and amounts. Percent is of the season's RP per kill.
type RankedBonusSettings struct {
	BountyEnabled     bool       `json:"bountyEnabled"`
	BountyStreak      int        `json:"bountyStreak"`
	BountyPercent     int        `json:"bountyPercent"`
	UnderdogEnabled   bool       `json:"underdogEnabled"`
	UnderdogPercent   int        `json:"underdogPercent"`
	RevengeEnabled    bool       `json:"revengeEnabled"`
	RevengePercent    int        `json:"revengePercent"`
	RevengeMinutes    int        `json:"revengeMinutes"`
	DailyFirstEnabled bool       `json:"dailyFirstEnabled"`
	DailyFirstPercent int        `json:"dailyFirstPercent"`
	RankUpCards       bool       `json:"rankUpCards"`
	RankUpDMs         bool       `json:"rankUpDms"`
	WeeklyRecap       bool       `json:"weeklyRecap"`
	UpdatedAt         *time.Time `json:"updatedAt"`
}

// DefaultRankedBonusSettings is everything off with sensible amounts ready.
func DefaultRankedBonusSettings() RankedBonusSettings {
	return RankedBonusSettings{BountyStreak: 5, BountyPercent: 100, UnderdogPercent: 50, RevengePercent: 25, RevengeMinutes: 30, DailyFirstPercent: 50}
}

// Validate keeps the amounts inside what the table allows.
func (s RankedBonusSettings) Validate() error {
	pct := func(v int) bool { return v >= 10 && v <= 500 }
	if s.BountyStreak < 3 || s.BountyStreak > 20 || !pct(s.BountyPercent) || !pct(s.UnderdogPercent) || !pct(s.RevengePercent) ||
		!pct(s.DailyFirstPercent) || s.RevengeMinutes < 5 || s.RevengeMinutes > 120 {
		return ErrRankedBonusInvalid
	}
	return nil
}

// AnyKillBonus reports whether any bonus changes the RP of a kill.
func (s RankedBonusSettings) AnyKillBonus() bool {
	return s.BountyEnabled || s.UnderdogEnabled || s.RevengeEnabled || s.DailyFirstEnabled
}

const bonusSettingsCols = `bounty_enabled,bounty_streak,bounty_percent,underdog_enabled,underdog_percent,revenge_enabled,revenge_percent,revenge_minutes,
daily_first_enabled,daily_first_percent,rankup_cards,rankup_dms,weekly_recap,updated_at`

func loadBonusSettings(ctx context.Context, q querier, serverID int64) (RankedBonusSettings, error) {
	s := DefaultRankedBonusSettings()
	var updated time.Time
	err := q.QueryRow(ctx, `SELECT `+bonusSettingsCols+` FROM ranked_bonus_settings WHERE server_id=$1`, serverID).Scan(
		&s.BountyEnabled, &s.BountyStreak, &s.BountyPercent, &s.UnderdogEnabled, &s.UnderdogPercent, &s.RevengeEnabled, &s.RevengePercent,
		&s.RevengeMinutes, &s.DailyFirstEnabled, &s.DailyFirstPercent, &s.RankUpCards, &s.RankUpDMs, &s.WeeklyRecap, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultRankedBonusSettings(), nil
	}
	if err != nil {
		return s, err
	}
	s.UpdatedAt = &updated
	return s, nil
}

// BonusSettings returns the server's settings (defaults before they are first saved).
func (r *RankedRepository) BonusSettings(ctx context.Context, serverID int64) (RankedBonusSettings, error) {
	return loadBonusSettings(ctx, r.pool, serverID)
}

// SaveBonusSettings stores the server's settings. Turning rank-up announcements on starts them
// from the players' current tiers, so ranks reached while they were off are not announced late.
func (r *RankedRepository) SaveBonusSettings(ctx context.Context, serverID int64, s RankedBonusSettings, by string, now time.Time) (RankedBonusSettings, error) {
	if serverID <= 0 {
		return s, ErrRankedBonusInvalid
	}
	if err := s.Validate(); err != nil {
		return s, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return s, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	before, err := loadBonusSettings(ctx, tx, serverID)
	if err != nil {
		return s, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ranked_bonus_settings(server_id,`+bonusSettingsCols+`,updated_by)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
ON CONFLICT (server_id) DO UPDATE SET bounty_enabled=EXCLUDED.bounty_enabled,bounty_streak=EXCLUDED.bounty_streak,bounty_percent=EXCLUDED.bounty_percent,
underdog_enabled=EXCLUDED.underdog_enabled,underdog_percent=EXCLUDED.underdog_percent,revenge_enabled=EXCLUDED.revenge_enabled,
revenge_percent=EXCLUDED.revenge_percent,revenge_minutes=EXCLUDED.revenge_minutes,daily_first_enabled=EXCLUDED.daily_first_enabled,
daily_first_percent=EXCLUDED.daily_first_percent,rankup_cards=EXCLUDED.rankup_cards,rankup_dms=EXCLUDED.rankup_dms,
weekly_recap=EXCLUDED.weekly_recap,updated_at=EXCLUDED.updated_at,updated_by=EXCLUDED.updated_by`,
		serverID, s.BountyEnabled, s.BountyStreak, s.BountyPercent, s.UnderdogEnabled, s.UnderdogPercent, s.RevengeEnabled, s.RevengePercent,
		s.RevengeMinutes, s.DailyFirstEnabled, s.DailyFirstPercent, s.RankUpCards, s.RankUpDMs, s.WeeklyRecap, now, by); err != nil {
		return s, err
	}
	if (s.RankUpCards || s.RankUpDMs) && !(before.RankUpCards || before.RankUpDMs) {
		if _, err = tx.Exec(ctx, `DELETE FROM ranked_tier_watch WHERE season_id IN (SELECT id FROM ranked_seasons WHERE server_id=$1 AND status='ACTIVE')`, serverID); err != nil {
			return s, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return s, err
	}
	s.UpdatedAt = &now
	return s, nil
}

// RankedBonus is one bonus on an award.
type RankedBonus struct {
	Kind string `json:"kind"`
	RP   int64  `json:"rp"`
	// Detail is the reason in a few words ("#1 on the server", "6-kill streak").
	Detail string `json:"detail,omitempty"`
}

func bonusRP(rpPerKill int64, percent int) int64 {
	if v := rpPerKill * int64(percent) / 100; v > 0 {
		return v
	}
	return 1
}

// bonusContext is what scoring a kill's bonuses needs from the award.
type bonusContext struct {
	seasonID, serverID, guildID int64
	rpPerKill                   int64
	thresholds                  ranked.Thresholds
	killer, victim              string
	at                          time.Time
}

// seasonRP is a player's season RP from awards decided before this one.
func seasonRP(ctx context.Context, q querier, seasonID int64, player string) (int64, error) {
	var rp int64
	err := q.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0)::BIGINT FROM ranked_awards WHERE season_id=$1 AND attacker_key=$2 AND outcome='AWARDED'`, seasonID, player).Scan(&rp)
	return rp, err
}

// rankedStreakAt is how many ranked kills a player made since they last died on the server
// (killed by a player or anything else) before `at`.
func rankedStreakAt(ctx context.Context, q querier, seasonID, guildID, serverID int64, player string, at time.Time) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT COUNT(*)::INT FROM ranked_awards a
WHERE a.season_id=$1 AND a.attacker_key=$2 AND a.outcome='AWARDED' AND a.event_time<$5
AND a.event_time>COALESCE(GREATEST(
  (SELECT MAX(event_time) FROM ranked_awards WHERE season_id=$1 AND victim_key=$2 AND event_time<$5),
  (SELECT MAX(event_time) FROM deaths WHERE guild_id=$3 AND server_id=$4 AND player_id=$2::BIGINT AND event_time<$5)), '-infinity')`,
		seasonID, player, guildID, serverID, at).Scan(&n)
	return n, err
}

// hasBonus reports whether a matching award already carries the bonus since `since`.
func hasBonus(ctx context.Context, q querier, seasonID int64, kind, attacker, victim string, since, until time.Time) (bool, error) {
	var found bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ranked_awards WHERE season_id=$1 AND outcome='AWARDED'
AND bonuses @> jsonb_build_array(jsonb_build_object('kind',$2::TEXT))
AND ($3='' OR attacker_key=$3) AND ($4='' OR victim_key=$4) AND event_time>=$5 AND event_time<$6)`,
		seasonID, kind, attacker, victim, since, until).Scan(&found)
	return found, err
}

// scoreBonuses decides the bonuses of an awarded kill. It runs inside the award transaction, under
// the killer's lock.
func scoreBonuses(ctx context.Context, q querier, s RankedBonusSettings, c bonusContext) ([]RankedBonus, error) {
	out := []RankedBonus{}
	if !s.AnyKillBonus() {
		return out, nil
	}
	if s.BountyEnabled {
		var top string
		err := q.QueryRow(ctx, `SELECT attacker_key FROM ranked_awards WHERE season_id=$1 AND outcome='AWARDED'
GROUP BY attacker_key ORDER BY SUM(amount) DESC, attacker_key::BIGINT ASC LIMIT 1`, c.seasonID).Scan(&top)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		streak, err := rankedStreakAt(ctx, q, c.seasonID, c.guildID, c.serverID, c.victim, c.at)
		if err != nil {
			return nil, err
		}
		switch {
		case streak >= s.BountyStreak:
			out = append(out, RankedBonus{Kind: BonusBounty, RP: bonusRP(c.rpPerKill, s.BountyPercent), Detail: fmt.Sprintf("was on a %d-kill streak", streak)})
		case top == c.victim:
			claimed, err := hasBonus(ctx, q, c.seasonID, BonusBounty, "", c.victim, c.at.Add(-bonusRepeatGap), c.at.Add(bonusRepeatGap))
			if err != nil {
				return nil, err
			}
			if !claimed {
				out = append(out, RankedBonus{Kind: BonusBounty, RP: bonusRP(c.rpPerKill, s.BountyPercent), Detail: "was #1 on the server"})
			}
		}
	}
	if s.UnderdogEnabled {
		killerRP, err := seasonRP(ctx, q, c.seasonID, c.killer)
		if err != nil {
			return nil, err
		}
		victimRP, err := seasonRP(ctx, q, c.seasonID, c.victim)
		if err != nil {
			return nil, err
		}
		killerTier, _, _, err := c.thresholds.Progress(killerRP)
		if err != nil {
			return nil, err
		}
		victimTier, _, _, err := c.thresholds.Progress(victimRP)
		if err != nil {
			return nil, err
		}
		if victimTier.Level() > killerTier.Level() {
			again, err := hasBonus(ctx, q, c.seasonID, BonusUnderdog, c.killer, c.victim, c.at.Add(-bonusRepeatGap), c.at.Add(bonusRepeatGap))
			if err != nil {
				return nil, err
			}
			if !again {
				out = append(out, RankedBonus{Kind: BonusUnderdog, RP: bonusRP(c.rpPerKill, s.UnderdogPercent), Detail: "was " + tierWord(victimTier)})
			}
		}
	}
	if s.RevengeEnabled {
		var lastAttacker string
		var lastAt time.Time
		err := q.QueryRow(ctx, `SELECT attacker_key,event_time FROM ranked_awards WHERE season_id=$1
AND ((attacker_key=$2 AND victim_key=$3) OR (attacker_key=$3 AND victim_key=$2)) AND event_time<$4
ORDER BY event_time DESC LIMIT 1`, c.seasonID, c.killer, c.victim, c.at).Scan(&lastAttacker, &lastAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil && lastAttacker == c.victim && c.at.Sub(lastAt) <= time.Duration(s.RevengeMinutes)*time.Minute {
			again, err := hasBonus(ctx, q, c.seasonID, BonusRevenge, c.killer, c.victim, c.at.Add(-bonusRepeatGap), c.at)
			if err != nil {
				return nil, err
			}
			if !again {
				out = append(out, RankedBonus{Kind: BonusRevenge, RP: bonusRP(c.rpPerKill, s.RevengePercent), Detail: "had killed them"})
			}
		}
	}
	if s.DailyFirstEnabled {
		day := c.at.UTC().Truncate(24 * time.Hour)
		done, err := hasBonus(ctx, q, c.seasonID, BonusDailyFirst, c.killer, "", day, day.Add(24*time.Hour))
		if err != nil {
			return nil, err
		}
		if !done {
			out = append(out, RankedBonus{Kind: BonusDailyFirst, RP: bonusRP(c.rpPerKill, s.DailyFirstPercent), Detail: "first kill of the day"})
		}
	}
	return out, nil
}

// tierWord is "Gold" for GOLD.
func tierWord(t ranked.Tier) string {
	if len(t) < 2 {
		return "higher-ranked"
	}
	return string(t[:1]) + strings.ToLower(string(t[1:]))
}

// TierName is the display name of a tier ("Gold").
func TierName(t ranked.Tier) string { return tierWord(t) }

// --- wanted players --------------------------------------------------------------------------------

// WantedPlayer is a player with a bounty on them right now.
type WantedPlayer struct {
	PlayerID int64  `json:"playerId"`
	Name     string `json:"name"`
	Reason   string `json:"reason"`
	Streak   int    `json:"streak"`
	RP       int64  `json:"rp"`
	Bounty   int64  `json:"bountyRp"`
}

// CurrentWanted lists who has a bounty on the season's server now: the #1 player (unless their
// bounty was claimed in the last hour) and anyone on a streak of at least the setting.
func (r *RankedRepository) CurrentWanted(ctx context.Context, season ServerRankedSeason, guildID int64, s RankedBonusSettings, now time.Time) ([]WantedPlayer, error) {
	if !s.BountyEnabled {
		return []WantedPlayer{}, nil
	}
	rows, err := r.pool.Query(ctx, `WITH totals AS (
  SELECT attacker_key,SUM(amount)::BIGINT rp FROM ranked_awards WHERE season_id=$1 AND outcome='AWARDED' GROUP BY attacker_key),
top AS (SELECT attacker_key FROM totals ORDER BY rp DESC, attacker_key::BIGINT ASC LIMIT 1),
streaks AS (
  SELECT t.attacker_key,(SELECT COUNT(*)::INT FROM ranked_awards a WHERE a.season_id=$1 AND a.attacker_key=t.attacker_key AND a.outcome='AWARDED'
    AND a.event_time>COALESCE(GREATEST(
      (SELECT MAX(event_time) FROM ranked_awards WHERE season_id=$1 AND victim_key=t.attacker_key),
      (SELECT MAX(event_time) FROM deaths WHERE guild_id=$2 AND server_id=$3 AND player_id=t.attacker_key::BIGINT)),'-infinity')) AS streak
  FROM totals t)
SELECT t.attacker_key::BIGINT,COALESCE(p.display_name,''),t.rp,st.streak,(t.attacker_key IN (SELECT attacker_key FROM top))
FROM totals t JOIN streaks st ON st.attacker_key=t.attacker_key LEFT JOIN players p ON p.id=t.attacker_key::BIGINT
WHERE st.streak>=$4 OR t.attacker_key IN (SELECT attacker_key FROM top)
ORDER BY st.streak DESC, t.rp DESC LIMIT 20`, season.ID, guildID, season.ServerID, s.BountyStreak)
	if err != nil {
		return nil, err
	}
	type cand struct {
		w     WantedPlayer
		isTop bool
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.w.PlayerID, &c.w.Name, &c.w.RP, &c.w.Streak, &c.isTop); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []WantedPlayer{}
	for _, c := range cands {
		w := c.w
		w.Bounty = bonusRP(season.RPPerKill, s.BountyPercent)
		switch {
		case w.Streak >= s.BountyStreak:
			w.Reason = WantedStreak
		case c.isTop && w.RP > 0:
			claimed, err := hasBonus(ctx, r.pool, season.ID, BonusBounty, "", strconv.FormatInt(w.PlayerID, 10), now.Add(-bonusRepeatGap), now.Add(bonusRepeatGap))
			if err != nil {
				return nil, err
			}
			if claimed {
				continue
			}
			w.Reason = WantedTop
		default:
			continue
		}
		out = append(out, w)
	}
	return out, nil
}

// SyncWanted records the season's current wanted players and returns the ones who are new (their
// card is due). Bounties that ended are closed, so the same player can be wanted again later.
func (r *RankedRepository) SyncWanted(ctx context.Context, seasonID int64, current []WantedPlayer, now time.Time) ([]WantedPlayer, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	ids := make([]int64, 0, len(current))
	reasons := make([]string, 0, len(current))
	for _, w := range current {
		ids = append(ids, w.PlayerID)
		reasons = append(reasons, w.Reason)
	}
	if _, err = tx.Exec(ctx, `UPDATE ranked_wanted SET cleared_at=$2 WHERE season_id=$1 AND cleared_at IS NULL
AND (player_id,reason) NOT IN (SELECT * FROM UNNEST($3::BIGINT[],$4::TEXT[]))`, seasonID, now, ids, reasons); err != nil {
		return nil, err
	}
	var fresh []WantedPlayer
	for _, w := range current {
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO ranked_wanted(season_id,player_id,reason,since) VALUES($1,$2,$3,$4)
ON CONFLICT (season_id,player_id,reason) WHERE cleared_at IS NULL DO NOTHING RETURNING id`, seasonID, w.PlayerID, w.Reason, now).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		fresh = append(fresh, w)
	}
	return fresh, tx.Commit(ctx)
}

// BountyClaim is an awarded kill that carried a bounty and whose card is due.
type BountyClaim struct {
	AwardID    int64
	ServerID   int64
	KillerName string
	VictimName string
	BountyRP   int64
	TotalRP    int64
	Detail     string
	At         time.Time
}

// DueBountyClaims returns the guild's bounty kills from the last day that were not announced yet.
func (r *RankedRepository) DueBountyClaims(ctx context.Context, guildID int64, now time.Time) ([]BountyClaim, error) {
	rows, err := r.pool.Query(ctx, `SELECT a.id,s.server_id,COALESCE(pk.display_name,''),COALESCE(pv.display_name,''),
(b->>'rp')::BIGINT,a.amount,COALESCE(b->>'detail',''),a.event_time
FROM ranked_awards a JOIN ranked_seasons s ON s.id=a.season_id JOIN game_servers gs ON gs.id=s.server_id AND gs.guild_id=$1
CROSS JOIN LATERAL jsonb_array_elements(a.bonuses) b
LEFT JOIN players pk ON pk.id=a.attacker_key::BIGINT LEFT JOIN players pv ON pv.id=a.victim_key::BIGINT
WHERE a.bounty_announced_at IS NULL AND a.bonus_rp>0 AND a.created_at>$2::TIMESTAMPTZ-INTERVAL '1 day' AND b->>'kind'='BOUNTY'
ORDER BY a.id LIMIT 20`, guildID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BountyClaim
	for rows.Next() {
		var c BountyClaim
		if err := rows.Scan(&c.AwardID, &c.ServerID, &c.KillerName, &c.VictimName, &c.BountyRP, &c.TotalRP, &c.Detail, &c.At); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkBountyClaimsAnnounced stamps the guild's awards whose bounty card went out (or was skipped).
// Awards with other bonuses are stamped too, so the due query only ever looks at fresh rows.
func (r *RankedRepository) MarkBountyClaimsAnnounced(ctx context.Context, awardIDs []int64, now time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE ranked_awards SET bounty_announced_at=$2 WHERE id=ANY($1)`, awardIDs, now)
	return err
}

// MarkOtherBonusesSeen stamps the guild's awards that carry bonuses but no bounty, so they leave
// the partial index the due query reads.
func (r *RankedRepository) MarkOtherBonusesSeen(ctx context.Context, guildID int64, now time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE ranked_awards a SET bounty_announced_at=$2 FROM ranked_seasons s, game_servers gs
WHERE s.id=a.season_id AND gs.id=s.server_id AND gs.guild_id=$1 AND a.bounty_announced_at IS NULL AND a.bonus_rp>0
AND NOT a.bonuses @> '[{"kind":"BOUNTY"}]'`, guildID, now)
	return err
}

// --- rank-ups --------------------------------------------------------------------------------------

// RankUp is a player who reached a higher tier than they were last told about.
type RankUp struct {
	PlayerID      int64
	Name          string
	DiscordUserID string
	From, To      ranked.Tier
	RP            int64
	Position      int
}

// RankUpMinLevel is the lowest tier announced: reaching Rookie takes a kill or two, so it is not news.
const RankUpMinLevel = 2

// DueRankUps compares every player's tier with the last one recorded, records the new ones and
// returns the players who went up to Bronze or higher. The first pass of a season only records.
func (r *RankedRepository) DueRankUps(ctx context.Context, season ServerRankedSeason, guildID int64, now time.Time, limit int) ([]RankUp, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ranked_tiers:'||$1::BIGINT::TEXT,0))`, season.ID); err != nil {
		return nil, err
	}
	var seeded bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ranked_tier_watch WHERE season_id=$1)`, season.ID).Scan(&seeded); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT a.attacker_key::BIGINT,COALESCE(p.display_name,''),SUM(a.amount)::BIGINT,COALESCE(t.tier,'UNRANKED'),
COALESCE((SELECT discord_user_id FROM player_links l WHERE l.guild_id=$2 AND l.player_id=a.attacker_key::BIGINT AND l.status='VERIFIED' LIMIT 1),'')
FROM ranked_awards a LEFT JOIN players p ON p.id=a.attacker_key::BIGINT
LEFT JOIN ranked_player_tiers t ON t.season_id=a.season_id AND t.player_id=a.attacker_key::BIGINT
WHERE a.season_id=$1 AND a.outcome='AWARDED' GROUP BY a.attacker_key,p.display_name,t.tier
ORDER BY SUM(a.amount) DESC, a.attacker_key::BIGINT ASC`, season.ID, guildID)
	if err != nil {
		return nil, err
	}
	var ups []RankUp
	type change struct {
		player int64
		tier   ranked.Tier
	}
	var changes []change
	position := 0
	for rows.Next() {
		var u RankUp
		var stored string
		if err := rows.Scan(&u.PlayerID, &u.Name, &u.RP, &stored, &u.DiscordUserID); err != nil {
			rows.Close()
			return nil, err
		}
		position++
		tier, _, _, err := season.Thresholds.Progress(u.RP)
		if err != nil {
			rows.Close()
			return nil, err
		}
		u.From, u.To, u.Position = ranked.Tier(stored), tier, position
		if tier.Level() > u.From.Level() {
			changes = append(changes, change{u.PlayerID, tier})
			if seeded && tier.Level() >= RankUpMinLevel && len(ups) < limit {
				ups = append(ups, u)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range changes {
		if _, err := tx.Exec(ctx, `INSERT INTO ranked_player_tiers(season_id,player_id,tier,updated_at) VALUES($1,$2,$3,$4)
ON CONFLICT (season_id,player_id) DO UPDATE SET tier=EXCLUDED.tier,updated_at=EXCLUDED.updated_at`, season.ID, c.player, string(c.tier), now); err != nil {
			return nil, err
		}
	}
	if !seeded {
		if _, err := tx.Exec(ctx, `INSERT INTO ranked_tier_watch(season_id,seeded_at) VALUES($1,$2) ON CONFLICT DO NOTHING`, season.ID, now); err != nil {
			return nil, err
		}
	}
	return ups, tx.Commit(ctx)
}

// --- weekly recap ----------------------------------------------------------------------------------

// RecapWeekStart is the Monday 00:00 UTC that starts the week containing t.
func RecapWeekStart(t time.Time) time.Time {
	day := t.UTC().Truncate(24 * time.Hour)
	offset := (int(day.Weekday()) + 6) % 7 // Monday = 0
	return day.AddDate(0, 0, -offset)
}

// RankedRecap is one server's ranked week.
type RankedRecap struct {
	WeekStart   time.Time
	Kills       int
	Climbers    []RPBoostLeader
	Bounty      *BountyClaim
	BestStreak  int
	StreakName  string
	Revenges    int
	RevengeName string
	RevengeMost int
}

// ClaimWeeklyRecap reserves the recap of the week that started at weekStart; false when it was
// already taken.
func (r *RankedRepository) ClaimWeeklyRecap(ctx context.Context, serverID int64, weekStart, now time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `INSERT INTO ranked_weekly_recaps(server_id,week_start,posted_at) VALUES($1,$2::DATE,$3) ON CONFLICT DO NOTHING`, serverID, weekStart.Format("2006-01-02"), now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// WeeklyRecap gathers the ranked week [weekStart, weekStart+7d) of the season.
func (r *RankedRepository) WeeklyRecap(ctx context.Context, season ServerRankedSeason, weekStart time.Time) (RankedRecap, error) {
	rc := RankedRecap{WeekStart: weekStart}
	end := weekStart.AddDate(0, 0, 7)
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::INT FROM ranked_awards WHERE season_id=$1 AND outcome='AWARDED' AND event_time>=$2 AND event_time<$3`,
		season.ID, weekStart, end).Scan(&rc.Kills); err != nil {
		return rc, err
	}
	if rc.Kills == 0 {
		return rc, nil
	}
	leaders, err := r.RPBoostLeaders(ctx, RPBoost{ServerID: season.ServerID, StartsAt: weekStart, EndsAt: end}, 3)
	if err != nil {
		return rc, err
	}
	rc.Climbers = leaders
	var b BountyClaim
	err = r.pool.QueryRow(ctx, `SELECT a.id,COALESCE(pk.display_name,''),COALESCE(pv.display_name,''),(x->>'rp')::BIGINT,COALESCE(x->>'detail','')
FROM ranked_awards a CROSS JOIN LATERAL jsonb_array_elements(a.bonuses) x
LEFT JOIN players pk ON pk.id=a.attacker_key::BIGINT LEFT JOIN players pv ON pv.id=a.victim_key::BIGINT
WHERE a.season_id=$1 AND a.outcome='AWARDED' AND a.event_time>=$2 AND a.event_time<$3 AND x->>'kind'='BOUNTY'
ORDER BY (x->>'rp')::BIGINT DESC, a.event_time LIMIT 1`, season.ID, weekStart, end).Scan(&b.AwardID, &b.KillerName, &b.VictimName, &b.BountyRP, &b.Detail)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return rc, err
	}
	if err == nil {
		rc.Bounty = &b
	}
	// Longest run of ranked kills without a ranked death, among kills made this week.
	err = r.pool.QueryRow(ctx, `WITH ev AS (
  SELECT attacker_key AS player,event_time,1 AS kill FROM ranked_awards WHERE season_id=$1 AND outcome='AWARDED' AND event_time>=$2 AND event_time<$3
  UNION ALL SELECT victim_key,event_time,0 FROM ranked_awards WHERE season_id=$1 AND event_time>=$2 AND event_time<$3),
runs AS (SELECT player,kill,SUM(1-kill) OVER (PARTITION BY player ORDER BY event_time,kill ROWS UNBOUNDED PRECEDING) AS life FROM ev)
SELECT COALESCE(p.display_name,''),COUNT(*)::INT FROM runs LEFT JOIN players p ON p.id=runs.player::BIGINT
WHERE kill=1 GROUP BY runs.player,p.display_name,life ORDER BY COUNT(*) DESC LIMIT 1`, season.ID, weekStart, end).Scan(&rc.StreakName, &rc.BestStreak)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return rc, err
	}
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::INT FROM ranked_awards WHERE season_id=$1 AND outcome='AWARDED' AND event_time>=$2 AND event_time<$3
AND bonuses @> '[{"kind":"REVENGE"}]'`, season.ID, weekStart, end).Scan(&rc.Revenges); err != nil {
		return rc, err
	}
	if rc.Revenges > 0 {
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(p.display_name,''),COUNT(*)::INT FROM ranked_awards a LEFT JOIN players p ON p.id=a.attacker_key::BIGINT
WHERE a.season_id=$1 AND a.outcome='AWARDED' AND a.event_time>=$2 AND a.event_time<$3 AND a.bonuses @> '[{"kind":"REVENGE"}]'
GROUP BY a.attacker_key,p.display_name ORDER BY COUNT(*) DESC, MIN(a.event_time) LIMIT 1`, season.ID, weekStart, end).Scan(&rc.RevengeName, &rc.RevengeMost); err != nil {
			return rc, err
		}
	}
	return rc, nil
}

// ActiveSeasonsForGuild lists the guild's servers with an active ranked season.
func (r *RankedRepository) ActiveSeasonsForGuild(ctx context.Context, guildID int64) ([]ServerRankedSeason, error) {
	rows, err := r.pool.Query(ctx, `SELECT s.server_id FROM ranked_seasons s JOIN game_servers gs ON gs.id=s.server_id AND gs.guild_id=$1
WHERE s.scope='SERVER' AND s.status='ACTIVE' ORDER BY s.server_id`, guildID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ServerRankedSeason, 0, len(ids))
	for _, id := range ids {
		s, err := r.ActiveServerSeason(ctx, guildID, id)
		if err != nil {
			return nil, err
		}
		if s != nil {
			out = append(out, *s)
		}
	}
	return out, nil
}
