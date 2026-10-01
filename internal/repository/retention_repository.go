package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RetentionRepository reads the retention dashboard (docs/RETENTION.md) from the rollups the
// presence pipeline writes: player_daily_activity (who was on, which UTC day, for how long) and
// server_hourly_activity (how many at once). A player's first day is their earliest row, so days
// recovered by the presence backfill count toward "new" and "returning" exactly like observed ones;
// seconds and sessions exist only for observed days.
type RetentionRepository struct{ pool *pgxpool.Pool }

func NewRetentionRepository(pool *pgxpool.Pool) *RetentionRepository {
	return &RetentionRepository{pool: pool}
}

// ErrInvalidTimeZone is returned when PostgreSQL does not recognise the requested time zone name.
var ErrInvalidTimeZone = errors.New("unknown time zone")

// RetentionSummary is the headline numbers as of one UTC day.
type RetentionSummary struct {
	ActiveToday      int // players seen on the day
	Active7d         int
	Active30d        int
	New7d            int // players whose first day falls in the last 7
	New30d           int
	TrackedPlayers   int   // players ever seen
	ObservedSeconds  int64 // last 30 days
	ObservedSessions int   // last 30 days
	FirstDay         *time.Time
}

func (r *RetentionRepository) Summary(ctx context.Context, serverID int64, today time.Time) (RetentionSummary, error) {
	var s RetentionSummary
	err := r.pool.QueryRow(ctx, `
WITH firsts AS (SELECT player_id, MIN(day) AS first_day FROM player_daily_activity WHERE server_id=$1 GROUP BY player_id)
SELECT
  (SELECT COUNT(*) FROM player_daily_activity WHERE server_id=$1 AND day=$2::date)::int,
  (SELECT COUNT(DISTINCT player_id) FROM player_daily_activity WHERE server_id=$1 AND day > $2::date - 7 AND day <= $2::date)::int,
  (SELECT COUNT(DISTINCT player_id) FROM player_daily_activity WHERE server_id=$1 AND day > $2::date - 30 AND day <= $2::date)::int,
  (SELECT COUNT(*) FROM firsts WHERE first_day > $2::date - 7 AND first_day <= $2::date)::int,
  (SELECT COUNT(*) FROM firsts WHERE first_day > $2::date - 30 AND first_day <= $2::date)::int,
  (SELECT COUNT(*) FROM firsts)::int,
  (SELECT COALESCE(SUM(observed_seconds), 0) FROM player_daily_activity WHERE server_id=$1 AND day > $2::date - 30 AND day <= $2::date)::bigint,
  (SELECT COALESCE(SUM(sessions), 0) FROM player_daily_activity WHERE server_id=$1 AND day > $2::date - 30 AND day <= $2::date)::int,
  (SELECT MIN(first_day)::timestamp FROM firsts)`, serverID, today.UTC()).Scan(
		&s.ActiveToday, &s.Active7d, &s.Active30d, &s.New7d, &s.New30d, &s.TrackedPlayers, &s.ObservedSeconds, &s.ObservedSessions, &s.FirstDay)
	return s, err
}

// RetentionDay is one UTC day of activity.
type RetentionDay struct {
	Day             time.Time
	Active          int
	New             int // first ever day on this server
	Returning       int // Active - New
	ObservedSeconds int64
	Sessions        int
}

// Daily returns every day in [today-days+1, today], including days nobody played.
func (r *RetentionRepository) Daily(ctx context.Context, serverID int64, today time.Time, days int) ([]RetentionDay, error) {
	rows, err := r.pool.Query(ctx, `
WITH firsts AS (SELECT player_id, MIN(day) AS first_day FROM player_daily_activity WHERE server_id=$1 GROUP BY player_id),
span AS (SELECT generate_series($2::date - ($3::int - 1), $2::date, interval '1 day')::date AS day)
SELECT span.day::timestamp, COUNT(a.player_id)::int, COUNT(a.player_id) FILTER (WHERE f.first_day = span.day)::int,
       COALESCE(SUM(a.observed_seconds), 0)::bigint, COALESCE(SUM(a.sessions), 0)::int
FROM span
LEFT JOIN player_daily_activity a ON a.server_id=$1 AND a.day = span.day
LEFT JOIN firsts f ON f.player_id = a.player_id
GROUP BY span.day ORDER BY span.day`, serverID, today.UTC(), days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RetentionDay, 0, days)
	for rows.Next() {
		var d RetentionDay
		if err := rows.Scan(&d.Day, &d.Active, &d.New, &d.ObservedSeconds, &d.Sessions); err != nil {
			return nil, err
		}
		d.Returning = d.Active - d.New
		out = append(out, d)
	}
	return out, rows.Err()
}

// RetentionCohort is the players whose first day fell in one week (Monday-based, UTC) and how many
// of them were seen again in each of the following weeks. Retained[i] is week i+1 after the cohort
// week; a week that has not finished yet (or not started) is nil rather than a misleading zero.
type RetentionCohort struct {
	WeekStart time.Time
	Size      int
	Retained  []*int
}

// CohortWeeks is how many following weeks each cohort is tracked for.
const CohortWeeks = 4

// Cohorts returns the last `weeks` weekly cohorts, oldest first.
func (r *RetentionRepository) Cohorts(ctx context.Context, serverID int64, today time.Time, weeks int) ([]RetentionCohort, error) {
	rows, err := r.pool.Query(ctx, `
WITH firsts AS (
    SELECT player_id, date_trunc('week', MIN(day)::timestamp)::date AS cohort FROM player_daily_activity WHERE server_id=$1 GROUP BY player_id
), cohorts AS (
    SELECT generate_series(date_trunc('week', $2::date::timestamp)::date - 7 * ($3::int - 1), date_trunc('week', $2::date::timestamp)::date, interval '7 days')::date AS cohort
), seen AS (
    SELECT DISTINCT f.cohort, a.player_id, ((date_trunc('week', a.day::timestamp)::date - f.cohort) / 7) AS week_no
    FROM firsts f JOIN player_daily_activity a ON a.server_id=$1 AND a.player_id=f.player_id
)
SELECT c.cohort::timestamp,
       (SELECT COUNT(*) FROM firsts f WHERE f.cohort = c.cohort)::int,
       (SELECT COUNT(*) FROM seen s WHERE s.cohort = c.cohort AND s.week_no = 1)::int,
       (SELECT COUNT(*) FROM seen s WHERE s.cohort = c.cohort AND s.week_no = 2)::int,
       (SELECT COUNT(*) FROM seen s WHERE s.cohort = c.cohort AND s.week_no = 3)::int,
       (SELECT COUNT(*) FROM seen s WHERE s.cohort = c.cohort AND s.week_no = 4)::int
FROM cohorts c ORDER BY c.cohort`, serverID, today.UTC(), weeks)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	day := today.UTC().Truncate(24 * time.Hour)
	var out []RetentionCohort
	for rows.Next() {
		var c RetentionCohort
		counts := make([]int, CohortWeeks)
		if err := rows.Scan(&c.WeekStart, &c.Size, &counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
			return nil, err
		}
		c.Retained = make([]*int, CohortWeeks)
		for i := range counts {
			// Week i+1 after the cohort week ends 7*(i+2) days after the cohort's Monday.
			if !day.Before(c.WeekStart.AddDate(0, 0, 7*(i+2))) {
				n := counts[i]
				c.Retained[i] = &n
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PeakHour is one hour of the week in the requested time zone: how many players were on at once.
type PeakHour struct {
	Weekday     int // 0 = Sunday
	Hour        int
	AveragePeak float64 // mean of the hourly peaks observed for this slot
	MaxPeak     int
	Samples     int // how many such hours were observed
}

// PeakHours aggregates the last `days` days of hourly concurrency by weekday and hour in tz (an
// IANA name). Only observed hours contribute, so a slot with no rows was never sampled.
func (r *RetentionRepository) PeakHours(ctx context.Context, serverID int64, now time.Time, days int, tz string) ([]PeakHour, error) {
	rows, err := r.pool.Query(ctx, `
SELECT EXTRACT(DOW FROM hour AT TIME ZONE $4)::int, EXTRACT(HOUR FROM hour AT TIME ZONE $4)::int,
       AVG(peak_players)::float8, MAX(peak_players)::int, COUNT(*)::int
FROM server_hourly_activity
WHERE server_id=$1 AND hour > $2::timestamptz - make_interval(days => $3::int) AND hour <= $2::timestamptz
GROUP BY 1, 2 ORDER BY 1, 2`, serverID, now.UTC(), days, tz)
	if err != nil {
		return nil, mapTimeZoneError(err)
	}
	defer rows.Close()
	out := []PeakHour{}
	for rows.Next() {
		var p PeakHour
		if err := rows.Scan(&p.Weekday, &p.Hour, &p.AveragePeak, &p.MaxPeak, &p.Samples); err != nil {
			return nil, mapTimeZoneError(err)
		}
		out = append(out, p)
	}
	return out, mapTimeZoneError(rows.Err())
}

// mapTimeZoneError turns PostgreSQL's "time zone not recognized" into ErrInvalidTimeZone.
func mapTimeZoneError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22023" {
		return ErrInvalidTimeZone
	}
	return err
}

// LapsedPlayer is a player who stopped showing up.
type LapsedPlayer struct {
	PlayerID        int64
	PlayerName      string
	LastSeenDay     time.Time
	DaysSinceSeen   int
	ActiveDays      int
	ObservedSeconds int64
	Kills           int
	Linked          bool // holds a VERIFIED Discord link, so staff can reach them
}

// Lapsed returns players last seen between minDays and maxDays ago, most invested first (active
// days, then playtime) - the players a server most wants back.
func (r *RetentionRepository) Lapsed(ctx context.Context, guildID, serverID int64, today time.Time, minDays, maxDays, limit int) ([]LapsedPlayer, error) {
	rows, err := r.pool.Query(ctx, `
SELECT p.id, p.display_name, t.last_day::timestamp, ($3::date - t.last_day)::int, t.active_days, t.seconds,
       (SELECT COUNT(*) FROM kills k WHERE k.guild_id=$1 AND k.server_id=$2 AND k.killer_player_id=p.id)::int,
       EXISTS(SELECT 1 FROM player_links pl WHERE pl.guild_id=$1 AND pl.player_id=p.id AND pl.status='VERIFIED')
FROM (
    SELECT player_id, MAX(day) AS last_day, COUNT(*)::int AS active_days, COALESCE(SUM(observed_seconds), 0)::bigint AS seconds
    FROM player_daily_activity WHERE server_id=$2 GROUP BY player_id
) t JOIN players p ON p.id = t.player_id AND p.guild_id=$1
WHERE t.last_day <= $3::date - $4::int AND t.last_day >= $3::date - $5::int
ORDER BY t.active_days DESC, t.seconds DESC, p.id LIMIT $6`, guildID, serverID, today.UTC(), minDays, maxDays, clampLimit(limit, 50, 200))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LapsedPlayer{}
	for rows.Next() {
		var l LapsedPlayer
		if err := rows.Scan(&l.PlayerID, &l.PlayerName, &l.LastSeenDay, &l.DaysSinceSeen, &l.ActiveDays, &l.ObservedSeconds, &l.Kills, &l.Linked); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ShopProductRevenue is what one product earned in Champion Points over a window. Refunded,
// cancelled and failed purchases are excluded.
type ShopProductRevenue struct {
	ProductID   *int64 // nil: the product has since been deleted
	ProductName string
	Purchases   int
	Units       int
	Points      int64
	Buyers      int
}

func (r *RetentionRepository) ShopRevenue(ctx context.Context, installationID int64, from, to time.Time, limit int) ([]ShopProductRevenue, error) {
	rows, err := r.pool.Query(ctx, `
SELECT i.product_id, MAX(i.product_name), COUNT(DISTINCT sp.id)::int, COALESCE(SUM(i.quantity), 0)::int, COALESCE(SUM(i.line_total_points), 0)::bigint,
       COUNT(DISTINCT sp.player_id)::int
FROM shop_purchases sp JOIN shop_purchase_items i ON i.purchase_id = sp.id
WHERE sp.installation_id=$1 AND sp.status IN ('PAID','PENDING_FULFILLMENT','FULFILLED') AND sp.created_at >= $2 AND sp.created_at < $3
GROUP BY i.product_id, CASE WHEN i.product_id IS NULL THEN i.product_name END
ORDER BY 5 DESC, 2 LIMIT $4`, installationID, from, to, clampLimit(limit, 20, 100))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShopProductRevenue{}
	for rows.Next() {
		var s ShopProductRevenue
		if err := rows.Scan(&s.ProductID, &s.ProductName, &s.Purchases, &s.Units, &s.Points, &s.Buyers); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
