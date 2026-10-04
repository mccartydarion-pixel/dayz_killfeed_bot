package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/progression"
)

// Daily and weekly challenges (docs/PROGRESSION.md). Each server's day and week get a set of
// challenges drawn once (progression.Generate); progress is measured from the kills, playtime and
// lives the bot already records, so nothing is counted twice and a late kill still counts.

var ErrChallengeSettingsInvalid = errors.New("challenge settings are out of range")

type ChallengeSettings struct {
	Enabled      bool       `json:"enabled"`
	DailyCount   int        `json:"dailyCount"`
	WeeklyCount  int        `json:"weeklyCount"`
	DailyPoints  int64      `json:"dailyPoints"`
	WeeklyPoints int64      `json:"weeklyPoints"`
	Announce     bool       `json:"announce"`
	UpdatedAt    *time.Time `json:"updatedAt"`
}

func DefaultChallengeSettings() ChallengeSettings {
	return ChallengeSettings{DailyCount: 3, WeeklyCount: 2, DailyPoints: 50, WeeklyPoints: 250}
}

func (s ChallengeSettings) Validate() error {
	if s.DailyCount < 1 || s.DailyCount > 5 || s.WeeklyCount < 0 || s.WeeklyCount > 4 ||
		s.DailyPoints < 0 || s.DailyPoints > 1_000_000 || s.WeeklyPoints < 0 || s.WeeklyPoints > 1_000_000 {
		return ErrChallengeSettingsInvalid
	}
	return nil
}

type ChallengeRepository struct{ pool *pgxpool.Pool }

func NewChallengeRepository(pool *pgxpool.Pool) *ChallengeRepository {
	return &ChallengeRepository{pool: pool}
}

func (r *ChallengeRepository) Settings(ctx context.Context, serverID int64) (ChallengeSettings, error) {
	s := DefaultChallengeSettings()
	var updated time.Time
	err := r.pool.QueryRow(ctx, `SELECT enabled,daily_count,weekly_count,daily_points,weekly_points,announce,updated_at FROM challenge_settings WHERE server_id=$1`, serverID).
		Scan(&s.Enabled, &s.DailyCount, &s.WeeklyCount, &s.DailyPoints, &s.WeeklyPoints, &s.Announce, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultChallengeSettings(), nil
	}
	if err != nil {
		return s, err
	}
	s.UpdatedAt = &updated
	return s, nil
}

func (r *ChallengeRepository) SaveSettings(ctx context.Context, serverID int64, s ChallengeSettings, by string, now time.Time) (ChallengeSettings, error) {
	if err := s.Validate(); err != nil {
		return s, err
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO challenge_settings(server_id,enabled,daily_count,weekly_count,daily_points,weekly_points,announce,updated_at,updated_by)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (server_id) DO UPDATE SET enabled=EXCLUDED.enabled,daily_count=EXCLUDED.daily_count,
weekly_count=EXCLUDED.weekly_count,daily_points=EXCLUDED.daily_points,weekly_points=EXCLUDED.weekly_points,announce=EXCLUDED.announce,
updated_at=EXCLUDED.updated_at,updated_by=EXCLUDED.updated_by`, serverID, s.Enabled, s.DailyCount, s.WeeklyCount, s.DailyPoints, s.WeeklyPoints, s.Announce, now, by)
	if err != nil {
		return s, err
	}
	s.UpdatedAt = &now
	return s, nil
}

// ChallengeServer is a server with challenges switched on.
type ChallengeServer struct {
	GuildID, ServerID int64
	Settings          ChallengeSettings
	HotZones          bool
	Territory         bool
}

// EnabledServers lists the guild's servers with challenges on, and which optional features they run.
func (r *ChallengeRepository) EnabledServers(ctx context.Context, guildID int64) ([]ChallengeServer, error) {
	rows, err := r.pool.Query(ctx, `SELECT gs.id,c.daily_count,c.weekly_count,c.daily_points,c.weekly_points,c.announce,
COALESCE((SELECT s.hot_zones_enabled FROM installations i JOIN installation_feature_settings s ON s.installation_id=i.id WHERE i.game_server_id=gs.id LIMIT 1),FALSE),
COALESCE((SELECT t.enabled FROM territory_settings t WHERE t.server_id=gs.id),FALSE)
FROM challenge_settings c JOIN game_servers gs ON gs.id=c.server_id WHERE gs.guild_id=$1 AND c.enabled ORDER BY gs.id`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChallengeServer
	for rows.Next() {
		cs := ChallengeServer{GuildID: guildID}
		cs.Settings.Enabled = true
		if err := rows.Scan(&cs.ServerID, &cs.Settings.DailyCount, &cs.Settings.WeeklyCount, &cs.Settings.DailyPoints, &cs.Settings.WeeklyPoints,
			&cs.Settings.Announce, &cs.HotZones, &cs.Territory); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// ChallengeSet is one server's challenges of a day or a week.
type ChallengeSet struct {
	ID          int64                   `json:"id"`
	ServerID    int64                   `json:"-"`
	Period      string                  `json:"period"`
	StartsAt    time.Time               `json:"startsAt"`
	EndsAt      time.Time               `json:"endsAt"`
	Challenges  []progression.Challenge `json:"challenges"`
	AnnouncedAt *time.Time              `json:"-"`
}

func scanChallengeSet(row pgx.Row) (ChallengeSet, error) {
	var s ChallengeSet
	var raw []byte
	err := row.Scan(&s.ID, &s.ServerID, &s.Period, &s.StartsAt, &s.EndsAt, &raw, &s.AnnouncedAt)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(raw, &s.Challenges)
	return s, err
}

const challengeSetCols = `id,server_id,period,starts_at,ends_at,challenges,announced_at`

// EnsureSet returns the server's set for the period containing `at`, drawing it the first time.
func (r *ChallengeRepository) EnsureSet(ctx context.Context, serverID int64, period string, at time.Time, count int, f progression.Features) (ChallengeSet, error) {
	start := progression.PeriodStart(period, at)
	set, err := scanChallengeSet(r.pool.QueryRow(ctx, `SELECT `+challengeSetCols+` FROM challenge_sets WHERE server_id=$1 AND period=$2 AND starts_at=$3`, serverID, period, start))
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return set, err
	}
	raw, err := json.Marshal(progression.Generate(serverID, period, start, count, f))
	if err != nil {
		return set, err
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO challenge_sets(server_id,period,starts_at,ends_at,challenges) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
		serverID, period, start, progression.PeriodEnd(period, start), raw); err != nil {
		return set, err
	}
	return scanChallengeSet(r.pool.QueryRow(ctx, `SELECT `+challengeSetCols+` FROM challenge_sets WHERE server_id=$1 AND period=$2 AND starts_at=$3`, serverID, period, start))
}

// FindSet returns the server's set for the period containing `at`, nil when none was drawn.
func (r *ChallengeRepository) FindSet(ctx context.Context, serverID int64, period string, at time.Time) (*ChallengeSet, error) {
	set, err := scanChallengeSet(r.pool.QueryRow(ctx, `SELECT `+challengeSetCols+` FROM challenge_sets WHERE server_id=$1 AND period=$2 AND starts_at=$3`,
		serverID, period, progression.PeriodStart(period, at)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &set, nil
}

// ClaimAnnouncement reserves the set's Discord card; false when it was already taken.
func (r *ChallengeRepository) ClaimAnnouncement(ctx context.Context, setID int64, now time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE challenge_sets SET announced_at=$2 WHERE id=$1 AND announced_at IS NULL`, setID, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// pvpKillsSQL counts PvP kills per killer in [$3,$4), the same victim counting at most once an hour
// (two friends trading kills do not finish each other's challenges). $5 limits it to one player
// (0 = everyone). %s adds the challenge's own condition.
const pvpKillsSQL = `SELECT k.killer_player_id, COUNT(DISTINCT (k.victim_player_id, date_trunc('hour', COALESCE(k.event_time,k.created_at))))::INT
FROM kills k WHERE k.guild_id=$1 AND k.server_id=$2 AND COALESCE(k.event_time,k.created_at)>=$3 AND COALESCE(k.event_time,k.created_at)<$4
AND k.killer_player_id IS NOT NULL AND k.victim_player_id IS NOT NULL AND k.killer_player_id<>k.victim_player_id
AND ($5::BIGINT=0 OR k.killer_player_id=$5) %s GROUP BY k.killer_player_id`

// collect runs a (player, count) query and keeps each player's highest count in into.
func (r *ChallengeRepository) collect(ctx context.Context, into map[int64]int, q string, args ...any) error {
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var player int64
		var n int
		if err := rows.Scan(&player, &n); err != nil {
			return err
		}
		if n > into[player] {
			into[player] = n
		}
	}
	return rows.Err()
}

// Progress measures one challenge over [from, to) for every player (playerID 0) or one player. now
// bounds a life still in progress (SURVIVE).
func (r *ChallengeRepository) Progress(ctx context.Context, guildID, serverID int64, c progression.Challenge, from, to time.Time, playerID int64, now time.Time) (map[int64]int, error) {
	out := map[int64]int{}
	args := []any{guildID, serverID, from, to, playerID}
	switch c.Kind {
	case progression.KindKills:
		return out, r.collect(ctx, out, fmt.Sprintf(pvpKillsSQL, ""), args...)
	case progression.KindHeadshots:
		return out, r.collect(ctx, out, fmt.Sprintf(pvpKillsSQL, "AND k.headshot"), args...)
	case progression.KindLongRange:
		return out, r.collect(ctx, out, fmt.Sprintf(pvpKillsSQL, "AND k.distance>=$6"), append(args, float64(c.Param))...)
	case progression.KindWeapon:
		match, exclude := progression.WeaponSQLPatterns(c.Class)
		if match == "" {
			return out, nil
		}
		cond := `AND LOWER(COALESCE(k.weapon_display,k.weapon_raw,'')) ~ $6 AND ($7='' OR LOWER(COALESCE(k.weapon_display,k.weapon_raw,'')) !~ $7)`
		return out, r.collect(ctx, out, fmt.Sprintf(pvpKillsSQL, cond), append(args, match, exclude)...)
	case progression.KindHotZone:
		return out, r.collect(ctx, out, fmt.Sprintf(pvpKillsSQL, `AND EXISTS (SELECT 1 FROM event_kills ek JOIN competitive_events ce ON ce.id=ek.event_id AND ce.event_type='HOT_ZONE' WHERE ek.kill_id=k.id)`), args...)
	case progression.KindTerritory:
		return out, r.collect(ctx, out, fmt.Sprintf(pvpKillsSQL, `AND EXISTS (SELECT 1 FROM territory_kills tk WHERE tk.kill_id=k.id)`), args...)
	case progression.KindPlaytime:
		return out, r.collect(ctx, out, `SELECT player_id,(SUM(observed_seconds)/60)::INT FROM player_daily_activity
WHERE guild_id=$1 AND server_id=$2 AND day>=($3::TIMESTAMPTZ AT TIME ZONE 'UTC')::DATE AND day<($4::TIMESTAMPTZ AT TIME ZONE 'UTC')::DATE
AND ($5::BIGINT=0 OR player_id=$5) GROUP BY player_id`, args...)
	case progression.KindSurvive:
		// Lives that ended in the period...
		if err := r.collect(ctx, out, `SELECT player_id,(MAX(playtime_seconds)/60)::INT FROM player_lives
WHERE guild_id=$1 AND server_id=$2 AND ended_at>=$3 AND ended_at<$4 AND playtime_seconds IS NOT NULL AND ($5::BIGINT=0 OR player_id=$5)
GROUP BY player_id`, args...); err != nil {
			return out, err
		}
		// ...and, while the period runs, lives still going for players seen in it.
		if now.Before(to) {
			q := `SELECT player_id,(playtime_seconds/60)::INT FROM (` + currentLifeSQL + ` AND a.last_seen_at>=$4::TIMESTAMPTZ AND ($5::BIGINT=0 OR a.player_id=$5)) c
WHERE playtime_seconds IS NOT NULL`
			if err := r.collect(ctx, out, q, guildID, serverID, now.UTC(), from, playerID); err != nil {
				return out, err
			}
		}
		return out, nil
	}
	return out, nil
}

// ChallengeCompletion is a completed challenge waiting for (or done with) its payment.
type ChallengeCompletion struct {
	SetID       int64     `json:"-"`
	Idx         int       `json:"index"`
	PlayerID    int64     `json:"-"`
	GuildID     int64     `json:"-"`
	ServerID    int64     `json:"-"`
	Period      string    `json:"period"`
	Points      int64     `json:"points"`
	CompletedAt time.Time `json:"completedAt"`
	Title       string    `json:"title"`
}

// Complete records the players who finished challenge idx of the set; it returns how many rows were
// new. points is what each is paid.
func (r *ChallengeRepository) Complete(ctx context.Context, guildID, setID int64, idx int, players []int64, points int64, now time.Time) (int, error) {
	if len(players) == 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `INSERT INTO challenge_completions(set_id,idx,player_id,guild_id,completed_at,points)
SELECT $1,$2,p,$3,$5,$6 FROM UNNEST($4::BIGINT[]) p ON CONFLICT DO NOTHING`, setID, idx, guildID, players, now, points)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Completed returns which players already finished challenge idx of the set.
func (r *ChallengeRepository) Completed(ctx context.Context, setID int64, idx int) (map[int64]bool, error) {
	rows, err := r.pool.Query(ctx, `SELECT player_id FROM challenge_completions WHERE set_id=$1 AND idx=$2`, setID, idx)
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

const completionCols = `c.set_id,c.idx,c.player_id,c.guild_id,s.server_id,s.period,c.points,c.completed_at,COALESCE(s.challenges->c.idx->>'title','')`

func (r *ChallengeRepository) completions(ctx context.Context, q string, args ...any) ([]ChallengeCompletion, error) {
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChallengeCompletion{}
	for rows.Next() {
		var c ChallengeCompletion
		if err := rows.Scan(&c.SetID, &c.Idx, &c.PlayerID, &c.GuildID, &c.ServerID, &c.Period, &c.Points, &c.CompletedAt, &c.Title); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Unpaid returns the guild's completions whose payment has not gone through yet.
func (r *ChallengeRepository) Unpaid(ctx context.Context, guildID int64, limit int) ([]ChallengeCompletion, error) {
	return r.completions(ctx, `SELECT `+completionCols+` FROM challenge_completions c JOIN challenge_sets s ON s.id=c.set_id
WHERE c.guild_id=$1 AND c.paid_at IS NULL ORDER BY c.completed_at LIMIT $2`, guildID, limit)
}

func (r *ChallengeRepository) MarkPaid(ctx context.Context, setID int64, idx int, playerID int64, now time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE challenge_completions SET paid_at=$4 WHERE set_id=$1 AND idx=$2 AND player_id=$3`, setID, idx, playerID, now)
	return err
}

// PlayerCompletions returns the player's completions of the given sets.
func (r *ChallengeRepository) PlayerCompletions(ctx context.Context, setIDs []int64, playerID int64) ([]ChallengeCompletion, error) {
	return r.completions(ctx, `SELECT `+completionCols+` FROM challenge_completions c JOIN challenge_sets s ON s.id=c.set_id
WHERE c.set_id=ANY($1) AND c.player_id=$2 ORDER BY c.idx`, setIDs, playerID)
}

// ChallengeStats is how many players finished each challenge of a set.
func (r *ChallengeRepository) CompletionCounts(ctx context.Context, setID int64) (map[int]int, error) {
	rows, err := r.pool.Query(ctx, `SELECT idx,COUNT(*)::INT FROM challenge_completions WHERE set_id=$1 GROUP BY idx`, setID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var idx, n int
		if err := rows.Scan(&idx, &n); err != nil {
			return nil, err
		}
		out[idx] = n
	}
	return out, rows.Err()
}
