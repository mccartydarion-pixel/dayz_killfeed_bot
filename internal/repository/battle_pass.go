package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/progression"
)

// The battle pass (docs/PROGRESSION.md): a season of levels earned with XP from kills, time played
// and challenges. Every level can carry a reward on the free track and one on the premium track,
// which players unlock with Champion Points (paid to the owner, like the perk store). Rewards are
// granted by the scheduler as soon as a level is reached.

// Ledger types written only here.
const (
	TxPassPurchase = "PASS_PURCHASE" // debit: a player bought a season's premium track
	TxPassSale     = "PASS_SALE"     // credit: the owner received that payment
)

var (
	ErrBattlePassInvalid      = errors.New("battle pass settings are out of range")
	ErrBattlePassOpen         = errors.New("this server already has a battle pass season running")
	ErrBattlePassNotFound     = errors.New("battle pass season not found")
	ErrBattlePassNoPremium    = errors.New("this season has no premium track")
	ErrBattlePassClosed       = errors.New("this season is not running")
	ErrBattlePassHasPremium   = errors.New("you already have the premium track")
	ErrCosmeticNotOwned       = errors.New("you do not own that title or badge")
	ErrBattlePassRewardsEmpty = errors.New("a season needs at least one reward")
)

// BattlePassSeason is one season and its rules.
type BattlePassSeason struct {
	ID                int64                `json:"id"`
	ServerID          int64                `json:"-"`
	GuildID           int64                `json:"-"`
	InstallationID    int64                `json:"-"`
	Name              string               `json:"name"`
	StartsAt          time.Time            `json:"startsAt"`
	EndsAt            time.Time            `json:"endsAt"`
	EndedAt           *time.Time           `json:"endedAt"`
	Levels            int                  `json:"levels"`
	XPPerLevel        int64                `json:"xpPerLevel"`
	PremiumPrice      int64                `json:"premiumPrice"`
	XPKill            int                  `json:"xpKill"`
	XPKillDailyCap    int                  `json:"xpKillDailyCap"`
	XPHour            int                  `json:"xpHour"`
	XPHoursDailyCap   int                  `json:"xpHoursDailyCap"`
	XPDailyChallenge  int                  `json:"xpDailyChallenge"`
	XPWeeklyChallenge int                  `json:"xpWeeklyChallenge"`
	Rewards           []progression.Reward `json:"rewards"`
	AnnouncedAt       *time.Time           `json:"-"`
	EndAnnouncedAt    *time.Time           `json:"-"`
}

// Running reports whether the season counts XP at t.
func (s BattlePassSeason) Running(t time.Time) bool {
	return s.EndedAt == nil && !t.Before(s.StartsAt) && t.Before(s.EndsAt)
}

// Validate checks the season's numbers and its reward track.
func (s *BattlePassSeason) Validate() error {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" || utf8.RuneCountInString(s.Name) > 60 || !s.EndsAt.After(s.StartsAt) || s.EndsAt.Sub(s.StartsAt) > 366*24*time.Hour ||
		s.Levels < progression.MinLevels || s.Levels > progression.MaxLevels ||
		s.XPPerLevel < progression.MinXPPerLevel || s.XPPerLevel > progression.MaxXPPerLevel ||
		s.PremiumPrice < 0 || s.PremiumPrice > progression.MaxPremiumPrice ||
		s.XPKill < 0 || s.XPKill > progression.MaxXPPerSource || s.XPKillDailyCap < 0 || s.XPKillDailyCap > progression.MaxDailyKillCap ||
		s.XPHour < 0 || s.XPHour > progression.MaxXPPerSource || s.XPHoursDailyCap < 0 || s.XPHoursDailyCap > progression.MaxDailyHours ||
		s.XPDailyChallenge < 0 || s.XPDailyChallenge > progression.MaxXPPerSource || s.XPWeeklyChallenge < 0 || s.XPWeeklyChallenge > progression.MaxXPPerSource {
		return ErrBattlePassInvalid
	}
	for i := range s.Rewards {
		s.Rewards[i].Text = strings.TrimSpace(s.Rewards[i].Text)
		s.Rewards[i].Label = strings.TrimSpace(s.Rewards[i].Label)
		if s.Rewards[i].Kind != progression.RewardPoints {
			s.Rewards[i].Amount = 0
		}
	}
	if len(s.Rewards) == 0 {
		return ErrBattlePassRewardsEmpty
	}
	if err := progression.ValidateRewards(s.Rewards, s.Levels); err != nil {
		return ErrBattlePassInvalid
	}
	return nil
}

// DefaultBattlePassSeason is a 30-day, 30-level season with the default track, starting at now.
func DefaultBattlePassSeason(now time.Time) BattlePassSeason {
	start := now.UTC().Truncate(time.Hour)
	return BattlePassSeason{Name: "Season 1", StartsAt: start, EndsAt: start.AddDate(0, 0, 30), Levels: 30, XPPerLevel: 1000, PremiumPrice: 1000,
		XPKill: 100, XPKillDailyCap: 20, XPHour: 150, XPHoursDailyCap: 6, XPDailyChallenge: 300, XPWeeklyChallenge: 1000,
		Rewards: progression.DefaultRewards(30)}
}

type BattlePassRepository struct{ pool *pgxpool.Pool }

func NewBattlePassRepository(pool *pgxpool.Pool) *BattlePassRepository {
	return &BattlePassRepository{pool: pool}
}

const bpSeasonCols = `id,server_id,guild_id,installation_id,name,starts_at,ends_at,ended_at,levels,xp_per_level,premium_price,xp_kill,xp_kill_daily_cap,
xp_hour,xp_hours_daily_cap,xp_daily_challenge,xp_weekly_challenge,rewards,announced_at,end_announced_at`

func scanBPSeason(row pgx.Row) (BattlePassSeason, error) {
	var s BattlePassSeason
	var raw []byte
	err := row.Scan(&s.ID, &s.ServerID, &s.GuildID, &s.InstallationID, &s.Name, &s.StartsAt, &s.EndsAt, &s.EndedAt, &s.Levels, &s.XPPerLevel, &s.PremiumPrice,
		&s.XPKill, &s.XPKillDailyCap, &s.XPHour, &s.XPHoursDailyCap, &s.XPDailyChallenge, &s.XPWeeklyChallenge, &raw, &s.AnnouncedAt, &s.EndAnnouncedAt)
	if err != nil {
		return s, err
	}
	s.Rewards = []progression.Reward{}
	err = json.Unmarshal(raw, &s.Rewards)
	return s, err
}

func (r *BattlePassRepository) one(ctx context.Context, q string, args ...any) (*BattlePassSeason, error) {
	s, err := scanBPSeason(r.pool.QueryRow(ctx, `SELECT `+bpSeasonCols+` FROM battle_pass_seasons `+q, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// OpenSeason is the server's season that has not ended (it may not have started yet), nil when none.
func (r *BattlePassRepository) OpenSeason(ctx context.Context, serverID int64) (*BattlePassSeason, error) {
	return r.one(ctx, `WHERE server_id=$1 AND ended_at IS NULL`, serverID)
}

// LatestSeason is the server's open season, else its most recent one.
func (r *BattlePassRepository) LatestSeason(ctx context.Context, serverID int64) (*BattlePassSeason, error) {
	return r.one(ctx, `WHERE server_id=$1 ORDER BY (ended_at IS NULL) DESC, starts_at DESC, id DESC LIMIT 1`, serverID)
}

func (r *BattlePassRepository) Season(ctx context.Context, serverID, seasonID int64) (*BattlePassSeason, error) {
	return r.one(ctx, `WHERE server_id=$1 AND id=$2`, serverID, seasonID)
}

// CreateSeason starts a season; ErrBattlePassOpen while another is open.
func (r *BattlePassRepository) CreateSeason(ctx context.Context, s BattlePassSeason, by string) (BattlePassSeason, error) {
	if err := s.Validate(); err != nil {
		return s, err
	}
	raw, err := json.Marshal(s.Rewards)
	if err != nil {
		return s, err
	}
	err = r.pool.QueryRow(ctx, `INSERT INTO battle_pass_seasons(guild_id,server_id,installation_id,name,starts_at,ends_at,levels,xp_per_level,premium_price,
xp_kill,xp_kill_daily_cap,xp_hour,xp_hours_daily_cap,xp_daily_challenge,xp_weekly_challenge,rewards,created_by)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id`,
		s.GuildID, s.ServerID, s.InstallationID, s.Name, s.StartsAt, s.EndsAt, s.Levels, s.XPPerLevel, s.PremiumPrice, s.XPKill, s.XPKillDailyCap,
		s.XPHour, s.XPHoursDailyCap, s.XPDailyChallenge, s.XPWeeklyChallenge, raw, by).Scan(&s.ID)
	if isUniqueViolation(err) {
		return s, ErrBattlePassOpen
	}
	return s, err
}

// UpdateSeason changes an open season's rules and track. What was already granted stays granted.
func (r *BattlePassRepository) UpdateSeason(ctx context.Context, s BattlePassSeason) (BattlePassSeason, error) {
	if err := s.Validate(); err != nil {
		return s, err
	}
	raw, err := json.Marshal(s.Rewards)
	if err != nil {
		return s, err
	}
	tag, err := r.pool.Exec(ctx, `UPDATE battle_pass_seasons SET name=$3,starts_at=$4,ends_at=$5,levels=$6,xp_per_level=$7,premium_price=$8,xp_kill=$9,
xp_kill_daily_cap=$10,xp_hour=$11,xp_hours_daily_cap=$12,xp_daily_challenge=$13,xp_weekly_challenge=$14,rewards=$15,updated_at=NOW()
WHERE id=$1 AND server_id=$2 AND ended_at IS NULL`, s.ID, s.ServerID, s.Name, s.StartsAt, s.EndsAt, s.Levels, s.XPPerLevel, s.PremiumPrice,
		s.XPKill, s.XPKillDailyCap, s.XPHour, s.XPHoursDailyCap, s.XPDailyChallenge, s.XPWeeklyChallenge, raw)
	if err != nil {
		return s, err
	}
	if tag.RowsAffected() == 0 {
		return s, ErrBattlePassNotFound
	}
	return s, nil
}

// EndSeason closes an open season now.
func (r *BattlePassRepository) EndSeason(ctx context.Context, serverID, seasonID int64, now time.Time) error {
	tag, err := r.pool.Exec(ctx, `UPDATE battle_pass_seasons SET ended_at=$3,updated_at=$3 WHERE id=$1 AND server_id=$2 AND ended_at IS NULL`, seasonID, serverID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrBattlePassNotFound
	}
	return nil
}

// GuildSeasons returns the guild's open seasons, after closing the ones whose end has passed.
// Seasons that ended in the last day are returned too, so their last XP and grants are settled.
func (r *BattlePassRepository) GuildSeasons(ctx context.Context, guildID int64, now time.Time) ([]BattlePassSeason, error) {
	if _, err := r.pool.Exec(ctx, `UPDATE battle_pass_seasons SET ended_at=ends_at,updated_at=$2 WHERE guild_id=$1 AND ended_at IS NULL AND ends_at<=$2`, guildID, now); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+bpSeasonCols+` FROM battle_pass_seasons WHERE guild_id=$1 AND starts_at<=$2
AND (ended_at IS NULL OR ended_at>$2::TIMESTAMPTZ-INTERVAL '1 day') ORDER BY id`, guildID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BattlePassSeason
	for rows.Next() {
		s, err := scanBPSeason(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// seasonEnd is when a season stops counting XP: its end, or when it was ended early.
func seasonEnd(s BattlePassSeason) time.Time {
	if s.EndedAt != nil && s.EndedAt.Before(s.EndsAt) {
		return *s.EndedAt
	}
	return s.EndsAt
}

// IngestXP turns the season's recent kills and playtime into XP: kills from `since` (whole UTC days,
// so the daily cap is counted the same way every pass) and the hours played on those days. Only one
// kill of the same victim an hour counts, and each day counts at most the season's caps.
func (r *BattlePassRepository) IngestXP(ctx context.Context, s BattlePassSeason, since, now time.Time) error {
	from := since.UTC().Truncate(24 * time.Hour)
	if from.Before(s.StartsAt) {
		from = s.StartsAt
	}
	to := seasonEnd(s)
	if now.Before(to) {
		to = now
	}
	if !to.After(from) {
		return nil
	}
	if s.XPKill > 0 && s.XPKillDailyCap > 0 {
		if _, err := r.pool.Exec(ctx, `INSERT INTO battle_pass_xp(season_id,player_id,source,ref,xp,earned_at)
SELECT $1,killer,'KILL','kill:'||id,$6,at FROM (
  SELECT d.id,d.killer,d.at,ROW_NUMBER() OVER (PARTITION BY d.killer,date_trunc('day',d.at AT TIME ZONE 'UTC') ORDER BY d.at,d.id) AS rn FROM (
    SELECT DISTINCT ON (k.killer_player_id,k.victim_player_id,date_trunc('hour',COALESCE(k.event_time,k.created_at)))
      k.id,k.killer_player_id AS killer,COALESCE(k.event_time,k.created_at) AS at
    FROM kills k WHERE k.guild_id=$2 AND k.server_id=$3 AND COALESCE(k.event_time,k.created_at)>=GREATEST($4::TIMESTAMPTZ,date_trunc('day',$4::TIMESTAMPTZ AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')
      AND COALESCE(k.event_time,k.created_at)<$5 AND k.killer_player_id IS NOT NULL AND k.victim_player_id IS NOT NULL AND k.killer_player_id<>k.victim_player_id
    ORDER BY k.killer_player_id,k.victim_player_id,date_trunc('hour',COALESCE(k.event_time,k.created_at)),COALESCE(k.event_time,k.created_at),k.id) d
  WHERE d.at>=$8) x
WHERE rn<=$7 ON CONFLICT DO NOTHING`, s.ID, s.GuildID, s.ServerID, from.Truncate(24*time.Hour), to, s.XPKill, s.XPKillDailyCap, s.StartsAt); err != nil {
			return fmt.Errorf("kill xp: %w", err)
		}
	}
	if s.XPHour > 0 && s.XPHoursDailyCap > 0 {
		if _, err := r.pool.Exec(ctx, `INSERT INTO battle_pass_xp(season_id,player_id,source,ref,xp,earned_at)
SELECT $1,d.player_id,'PLAYTIME','play:'||d.day::TEXT||':'||h,$6,d.last_seen_at
FROM player_daily_activity d CROSS JOIN LATERAL generate_series(1,LEAST((d.observed_seconds/3600)::INT,$7)) h
WHERE d.guild_id=$2 AND d.server_id=$3 AND d.day>=($4::TIMESTAMPTZ AT TIME ZONE 'UTC')::DATE AND d.day<=($5::TIMESTAMPTZ AT TIME ZONE 'UTC')::DATE
ON CONFLICT DO NOTHING`, s.ID, s.GuildID, s.ServerID, from, to, s.XPHour, s.XPHoursDailyCap); err != nil {
			return fmt.Errorf("playtime xp: %w", err)
		}
	}
	return nil
}

// AddXP records one piece of XP (a challenge); it counts once per reference.
func (r *BattlePassRepository) AddXP(ctx context.Context, seasonID, playerID int64, source, ref string, xp int, at time.Time) error {
	if xp <= 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO battle_pass_xp(season_id,player_id,source,ref,xp,earned_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
		seasonID, playerID, source, ref, xp, at)
	return err
}

// GrantLevels records every reward the season's players have reached and not been given yet: the
// free track for everyone, the premium track for players who bought it.
func (r *BattlePassRepository) GrantLevels(ctx context.Context, seasonID int64, now time.Time) (int, error) {
	tag, err := r.pool.Exec(ctx, `WITH s AS (SELECT * FROM battle_pass_seasons WHERE id=$1),
lv AS (SELECT x.player_id,LEAST((SELECT levels FROM s)::BIGINT,SUM(x.xp)/(SELECT xp_per_level FROM s)) AS level FROM battle_pass_xp x WHERE x.season_id=$1 GROUP BY x.player_id),
rw AS (SELECT (e->>'level')::INT AS level,e->>'track' AS track,e->>'kind' AS kind,COALESCE((e->>'amount')::BIGINT,0) AS amount,
  COALESCE(e->>'text','') AS text,COALESCE(e->>'label','') AS label FROM s CROSS JOIN LATERAL jsonb_array_elements(s.rewards) e)
INSERT INTO battle_pass_grants(season_id,player_id,level,track,kind,amount,text,label,granted_at)
SELECT $1,lv.player_id,rw.level,rw.track,rw.kind,rw.amount,rw.text,rw.label,$2 FROM lv JOIN rw ON rw.level<=lv.level
WHERE rw.track='FREE' OR EXISTS (SELECT 1 FROM battle_pass_premium p WHERE p.season_id=$1 AND p.player_id=lv.player_id)
ON CONFLICT DO NOTHING`, seasonID, now)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// BattlePassGrant is a reward given to a player.
type BattlePassGrant struct {
	SeasonID   int64      `json:"-"`
	SeasonName string     `json:"-"`
	GuildID    int64      `json:"-"`
	ServerID   int64      `json:"-"`
	PlayerID   int64      `json:"-"`
	Level      int        `json:"level"`
	Track      string     `json:"track"`
	Kind       string     `json:"kind"`
	Amount     int64      `json:"amount,omitempty"`
	Text       string     `json:"text,omitempty"`
	Label      string     `json:"label,omitempty"`
	GrantedAt  time.Time  `json:"grantedAt"`
	PaidAt     *time.Time `json:"-"`
}

const bpGrantCols = `g.season_id,s.name,s.guild_id,s.server_id,g.player_id,g.level,g.track,g.kind,g.amount,g.text,g.label,g.granted_at,g.paid_at`

func (r *BattlePassRepository) grants(ctx context.Context, q string, args ...any) ([]BattlePassGrant, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+bpGrantCols+` FROM battle_pass_grants g JOIN battle_pass_seasons s ON s.id=g.season_id `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BattlePassGrant{}
	for rows.Next() {
		var g BattlePassGrant
		if err := rows.Scan(&g.SeasonID, &g.SeasonName, &g.GuildID, &g.ServerID, &g.PlayerID, &g.Level, &g.Track, &g.Kind, &g.Amount, &g.Text, &g.Label, &g.GrantedAt, &g.PaidAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UnpaidGrants returns the season's grants not delivered yet.
func (r *BattlePassRepository) UnpaidGrants(ctx context.Context, seasonID int64, limit int) ([]BattlePassGrant, error) {
	return r.grants(ctx, `WHERE g.season_id=$1 AND g.paid_at IS NULL ORDER BY g.granted_at,g.player_id,g.level LIMIT $2`, seasonID, limit)
}

// DeliverGrant marks a grant delivered; a title or badge is added to the player's cosmetics in the
// same transaction. Points are credited by the caller first (idempotently) through the ledger.
func (r *BattlePassRepository) DeliverGrant(ctx context.Context, g BattlePassGrant, now time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if g.Kind == progression.RewardTitle || g.Kind == progression.RewardBadge {
		if _, err := tx.Exec(ctx, `INSERT INTO player_cosmetics(guild_id,server_id,player_id,kind,value,label,source,granted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (server_id,player_id,kind,value) DO NOTHING`, g.GuildID, g.ServerID, g.PlayerID, g.Kind, g.Text, g.Label,
			fmt.Sprintf("%s · level %d", g.SeasonName, g.Level), now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE battle_pass_grants SET paid_at=$5 WHERE season_id=$1 AND player_id=$2 AND level=$3 AND track=$4`,
		g.SeasonID, g.PlayerID, g.Level, g.Track, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PlayerGrants returns what the player has been given this season.
func (r *BattlePassRepository) PlayerGrants(ctx context.Context, seasonID, playerID int64) ([]BattlePassGrant, error) {
	return r.grants(ctx, `WHERE g.season_id=$1 AND g.player_id=$2 ORDER BY g.level,g.track`, seasonID, playerID)
}

// BattlePassProgress is one player's standing in a season.
type BattlePassProgress struct {
	PlayerID int64            `json:"-"`
	Name     string           `json:"name"`
	XP       int64            `json:"xp"`
	Level    int              `json:"level"`
	Premium  bool             `json:"premium"`
	Title    string           `json:"title,omitempty"`
	Badge    string           `json:"badge,omitempty"`
	Sources  map[string]int64 `json:"sources,omitempty"`
}

// PlayerProgress returns the player's XP (by source), level and premium status.
func (r *BattlePassRepository) PlayerProgress(ctx context.Context, s BattlePassSeason, playerID int64) (BattlePassProgress, error) {
	p := BattlePassProgress{PlayerID: playerID, Sources: map[string]int64{}}
	rows, err := r.pool.Query(ctx, `SELECT source,SUM(xp)::BIGINT FROM battle_pass_xp WHERE season_id=$1 AND player_id=$2 GROUP BY source`, s.ID, playerID)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var src string
		var xp int64
		if err := rows.Scan(&src, &xp); err != nil {
			rows.Close()
			return p, err
		}
		p.Sources[src] = xp
		p.XP += xp
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return p, err
	}
	p.Level = progression.Level(p.XP, s.XPPerLevel, s.Levels)
	err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM battle_pass_premium WHERE season_id=$1 AND player_id=$2)`, s.ID, playerID).Scan(&p.Premium)
	return p, err
}

// Leaderboard ranks the season's players by XP.
func (r *BattlePassRepository) Leaderboard(ctx context.Context, s BattlePassSeason, limit int) ([]BattlePassProgress, error) {
	rows, err := r.pool.Query(ctx, `SELECT x.player_id,COALESCE(p.display_name,''),SUM(x.xp)::BIGINT,
EXISTS(SELECT 1 FROM battle_pass_premium pr WHERE pr.season_id=$1 AND pr.player_id=x.player_id),COALESCE(c.title,''),COALESCE(c.badge,'')
FROM battle_pass_xp x LEFT JOIN players p ON p.id=x.player_id LEFT JOIN player_cosmetic_choice c ON c.server_id=$2 AND c.player_id=x.player_id
WHERE x.season_id=$1 GROUP BY x.player_id,p.display_name,c.title,c.badge ORDER BY SUM(x.xp) DESC,x.player_id LIMIT $3`, s.ID, s.ServerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BattlePassProgress{}
	for rows.Next() {
		var p BattlePassProgress
		if err := rows.Scan(&p.PlayerID, &p.Name, &p.XP, &p.Premium, &p.Title, &p.Badge); err != nil {
			return nil, err
		}
		p.Level = progression.Level(p.XP, s.XPPerLevel, s.Levels)
		out = append(out, p)
	}
	return out, rows.Err()
}

// BattlePassStats summarises a season for the owner.
type BattlePassStats struct {
	Players      int   `json:"players"`
	Premium      int   `json:"premium"`
	PremiumPaid  int64 `json:"premiumPaid"`
	MaxedOut     int   `json:"maxedOut"`
	PointsGiven  int64 `json:"pointsGiven"`
	RewardsGiven int   `json:"rewardsGiven"`
}

func (r *BattlePassRepository) Stats(ctx context.Context, s BattlePassSeason) (BattlePassStats, error) {
	var st BattlePassStats
	err := r.pool.QueryRow(ctx, `SELECT
(SELECT COUNT(DISTINCT player_id) FROM battle_pass_xp WHERE season_id=$1)::INT,
(SELECT COUNT(*) FROM battle_pass_premium WHERE season_id=$1)::INT,
(SELECT COALESCE(SUM(price),0) FROM battle_pass_premium WHERE season_id=$1)::BIGINT,
(SELECT COUNT(*) FROM (SELECT player_id FROM battle_pass_xp WHERE season_id=$1 GROUP BY player_id HAVING SUM(xp)>=$2::BIGINT*$3::BIGINT) m)::INT,
(SELECT COALESCE(SUM(amount),0) FROM battle_pass_grants WHERE season_id=$1 AND kind='POINTS' AND paid_at IS NOT NULL)::BIGINT,
(SELECT COUNT(*) FROM battle_pass_grants WHERE season_id=$1 AND paid_at IS NOT NULL)::INT`, s.ID, s.Levels, s.XPPerLevel).
		Scan(&st.Players, &st.Premium, &st.PremiumPaid, &st.MaxedOut, &st.PointsGiven, &st.RewardsGiven)
	return st, err
}

// BuyPremium charges the player the season's premium price (paid to the owner, as the perk store
// does) and unlocks the premium track. One purchase per player and season.
func (r *BattlePassRepository) BuyPremium(ctx context.Context, scope PerkScope, seasonID, playerID int64, now time.Time) (BattlePassSeason, int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return BattlePassSeason{}, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	s, err := scanBPSeason(tx.QueryRow(ctx, `SELECT `+bpSeasonCols+` FROM battle_pass_seasons WHERE id=$1 AND server_id=$2 FOR UPDATE`, seasonID, scope.ServerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return s, 0, ErrBattlePassNotFound
	}
	if err != nil {
		return s, 0, err
	}
	if s.PremiumPrice <= 0 {
		return s, 0, ErrBattlePassNoPremium
	}
	if s.EndedAt != nil || !now.Before(s.EndsAt) {
		return s, 0, ErrBattlePassClosed
	}
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM battle_pass_premium WHERE season_id=$1 AND player_id=$2)`, s.ID, playerID).Scan(&owned); err != nil {
		return s, 0, err
	}
	if owned {
		return s, 0, ErrBattlePassHasPremium
	}
	ref := fmt.Sprintf("pass:%d", s.ID)
	desc := "Battle pass premium · " + s.Name
	entry, err := applyLedger(ctx, tx, LedgerParams{GuildID: scope.GuildID, PlayerID: playerID, ServerID: scope.ServerID, Type: TxPassPurchase,
		Amount: s.PremiumPrice, CreatedBy: "SYSTEM", ReferenceID: ref, Description: desc}, true)
	if err != nil {
		return s, 0, err
	}
	var owner *int64
	if id, ok, err := ownerPlayer(ctx, tx, scope); err != nil {
		return s, 0, err
	} else if ok {
		if _, err := applyLedger(ctx, tx, LedgerParams{GuildID: scope.GuildID, PlayerID: id, ServerID: scope.ServerID, Type: TxPassSale,
			Amount: s.PremiumPrice, CreatedBy: "SYSTEM", ReferenceID: fmt.Sprintf("%s:%d", ref, playerID), Description: desc}, false); err != nil {
			return s, 0, err
		}
		owner = &id
		if id == playerID {
			if err := tx.QueryRow(ctx, `SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2`, scope.GuildID, playerID).Scan(&entry.BalanceAfter); err != nil {
				return s, 0, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO battle_pass_premium(season_id,player_id,price,owner_player_id,bought_at) VALUES($1,$2,$3,$4,$5)`,
		s.ID, playerID, s.PremiumPrice, owner, now); err != nil {
		return s, 0, err
	}
	return s, entry.BalanceAfter, tx.Commit(ctx)
}

// Cosmetic is a title or badge a player owns.
type Cosmetic struct {
	Kind      string    `json:"kind"`
	Value     string    `json:"value"`
	Label     string    `json:"label,omitempty"`
	Source    string    `json:"source"`
	GrantedAt time.Time `json:"grantedAt"`
}

// CosmeticChoice is the title and badge a player shows ("" for none).
type CosmeticChoice struct {
	Title string `json:"title"`
	Badge string `json:"badge"`
}

func (r *BattlePassRepository) Cosmetics(ctx context.Context, serverID, playerID int64) ([]Cosmetic, CosmeticChoice, error) {
	var choice CosmeticChoice
	rows, err := r.pool.Query(ctx, `SELECT kind,value,label,source,granted_at FROM player_cosmetics WHERE server_id=$1 AND player_id=$2 ORDER BY kind DESC,granted_at`, serverID, playerID)
	if err != nil {
		return nil, choice, err
	}
	out := []Cosmetic{}
	for rows.Next() {
		var c Cosmetic
		if err := rows.Scan(&c.Kind, &c.Value, &c.Label, &c.Source, &c.GrantedAt); err != nil {
			rows.Close()
			return nil, choice, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, choice, err
	}
	err = r.pool.QueryRow(ctx, `SELECT title,badge FROM player_cosmetic_choice WHERE server_id=$1 AND player_id=$2`, serverID, playerID).Scan(&choice.Title, &choice.Badge)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return out, choice, err
}

// SetCosmetics chooses what the player shows; each must be one they own ("" shows nothing).
func (r *BattlePassRepository) SetCosmetics(ctx context.Context, guildID, serverID, playerID int64, c CosmeticChoice, now time.Time) error {
	c.Title, c.Badge = strings.TrimSpace(c.Title), strings.TrimSpace(c.Badge)
	for kind, v := range map[string]string{progression.RewardTitle: c.Title, progression.RewardBadge: c.Badge} {
		if v == "" {
			continue
		}
		var owned bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM player_cosmetics WHERE guild_id=$1 AND server_id=$2 AND player_id=$3 AND kind=$4 AND value=$5)`,
			guildID, serverID, playerID, kind, v).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return ErrCosmeticNotOwned
		}
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO player_cosmetic_choice(server_id,player_id,title,badge,updated_at) VALUES($1,$2,$3,$4,$5)
ON CONFLICT (server_id,player_id) DO UPDATE SET title=EXCLUDED.title,badge=EXCLUDED.badge,updated_at=EXCLUDED.updated_at`, serverID, playerID, c.Title, c.Badge, now)
	return err
}

// ClaimAnnouncement reserves the season's start (end=false) or end card; false when taken.
func (r *BattlePassRepository) ClaimAnnouncement(ctx context.Context, seasonID int64, end bool, now time.Time) (bool, error) {
	col := "announced_at"
	if end {
		col = "end_announced_at"
	}
	tag, err := r.pool.Exec(ctx, `UPDATE battle_pass_seasons SET `+col+`=$2 WHERE id=$1 AND `+col+` IS NULL`, seasonID, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
