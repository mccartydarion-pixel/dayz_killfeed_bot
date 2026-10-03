package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Double RP (docs/RANKED_DOUBLE_RP.md): a window on one server during which every ranked kill
// earns its season's RP times the window's multiplier. Whether a kill is doubled depends on when
// the kill happened, never on when it was processed, so a kill that reaches the bot late (Nitrado
// shows logs in steps of minutes) is still doubled if it happened inside the window.

const (
	RPBoostScheduled = "SCHEDULED"
	RPBoostLive      = "LIVE"
	RPBoostEnded     = "ENDED"

	RPBoostMultiplier = 2
	RPBoostMaxHours   = 72
	RPBoostMaxAhead   = 14 * 24 * time.Hour
	// RPBoostResultsDelay must match the interval in DueRPBoostAnnouncements.
	RPBoostResultsDelay = 10 * time.Minute
)

var (
	ErrRPBoostOverlap  = errors.New("another double RP window overlaps this time")
	ErrRPBoostNotFound = errors.New("double RP window not found or already over")
	ErrRPBoostInvalid  = errors.New("double RP needs a start and an end, 1 to 72 hours apart, within the next 14 days")
)

// RPBoost is one double RP window. EndsAt is when it actually ends (earlier than planned when
// staff stopped it).
type RPBoost struct {
	ID         int64     `json:"id"`
	ServerID   int64     `json:"serverId"`
	Multiplier int       `json:"multiplier"`
	StartsAt   time.Time `json:"startsAt"`
	EndsAt     time.Time `json:"endsAt"`
	PlannedEnd time.Time `json:"plannedEnd"`
	Stopped    bool      `json:"stopped"`
	Status     string    `json:"status"`
	CreatedBy  string    `json:"-"`
}

// rpBoostEnd is a window's real end: its planned end, or when staff stopped it if that was earlier.
const rpBoostEnd = `LEAST(b.ends_at, COALESCE(b.stopped_at, b.ends_at))`

const rpBoostCols = `b.id,b.server_id,b.multiplier,b.starts_at,` + rpBoostEnd + `,b.ends_at,b.stopped_at IS NOT NULL,b.created_by`

func scanRPBoost(row pgx.Row, now time.Time) (RPBoost, error) {
	var b RPBoost
	err := row.Scan(&b.ID, &b.ServerID, &b.Multiplier, &b.StartsAt, &b.EndsAt, &b.PlannedEnd, &b.Stopped, &b.CreatedBy)
	b.Status = rpBoostStatus(b, now)
	return b, err
}

func rpBoostStatus(b RPBoost, now time.Time) string {
	switch {
	case !b.EndsAt.After(now) || !b.EndsAt.After(b.StartsAt):
		return RPBoostEnded
	case b.StartsAt.After(now):
		return RPBoostScheduled
	}
	return RPBoostLive
}

func (r *RankedRepository) collectRPBoosts(rows pgx.Rows, err error, now time.Time) ([]RPBoost, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RPBoost{}
	for rows.Next() {
		b, err := scanRPBoost(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RPBoostMultiplierAt is the multiplier for a kill on serverID at eventTime (1 when no window covers it).
func rpBoostMultiplierAt(ctx context.Context, q querier, serverID int64, eventTime time.Time) (int64, error) {
	var m int64
	err := q.QueryRow(ctx, `SELECT COALESCE(MAX(b.multiplier),1) FROM ranked_rp_boosts b
WHERE b.server_id=$1 AND b.starts_at<=$2 AND $2<`+rpBoostEnd, serverID, eventTime).Scan(&m)
	return m, err
}

// CreateRPBoost plans a window. It must not overlap another window that is not over yet.
func (r *RankedRepository) CreateRPBoost(ctx context.Context, serverID int64, startsAt, endsAt, now time.Time, by string) (RPBoost, error) {
	if serverID <= 0 || !endsAt.After(startsAt) || endsAt.Sub(startsAt) > RPBoostMaxHours*time.Hour ||
		startsAt.Before(now.Add(-5*time.Minute)) || startsAt.After(now.Add(RPBoostMaxAhead)) {
		return RPBoost{}, ErrRPBoostInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RPBoost{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rp_boost:'||$1::BIGINT::TEXT,0))`, serverID); err != nil {
		return RPBoost{}, err
	}
	var overlap bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ranked_rp_boosts b WHERE b.server_id=$1 AND b.starts_at<$3 AND `+rpBoostEnd+`>$2 AND `+rpBoostEnd+`>b.starts_at)`,
		serverID, startsAt, endsAt).Scan(&overlap); err != nil {
		return RPBoost{}, err
	}
	if overlap {
		return RPBoost{}, ErrRPBoostOverlap
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO ranked_rp_boosts(server_id,multiplier,starts_at,ends_at,created_by) VALUES($1,$2,$3,$4,$5) RETURNING id`,
		serverID, RPBoostMultiplier, startsAt, endsAt, by).Scan(&id); err != nil {
		return RPBoost{}, err
	}
	b, err := scanRPBoost(tx.QueryRow(ctx, `SELECT `+rpBoostCols+` FROM ranked_rp_boosts b WHERE b.id=$1`, id), now)
	if err != nil {
		return RPBoost{}, err
	}
	return b, tx.Commit(ctx)
}

// StopRPBoost ends a live window now, or cancels a scheduled one.
func (r *RankedRepository) StopRPBoost(ctx context.Context, serverID, boostID int64, now time.Time, by string) (RPBoost, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE ranked_rp_boosts b SET stopped_at=$3,stopped_by=$4 WHERE b.server_id=$1 AND b.id=$2 AND `+rpBoostEnd+`>$3`, serverID, boostID, now, by)
	if err != nil {
		return RPBoost{}, err
	}
	if tag.RowsAffected() == 0 {
		return RPBoost{}, ErrRPBoostNotFound
	}
	return scanRPBoost(r.pool.QueryRow(ctx, `SELECT `+rpBoostCols+` FROM ranked_rp_boosts b WHERE b.id=$1`, boostID), now)
}

// ListRPBoosts returns the server's windows that are not over, then the most recent ones that are.
func (r *RankedRepository) ListRPBoosts(ctx context.Context, serverID int64, now time.Time, limit int) ([]RPBoost, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+rpBoostCols+` FROM ranked_rp_boosts b WHERE b.server_id=$1
ORDER BY (`+rpBoostEnd+`>$2) DESC, CASE WHEN `+rpBoostEnd+`>$2 THEN b.starts_at END ASC, b.starts_at DESC LIMIT $3`, serverID, now, clampLimit(limit, 20, 100))
	return r.collectRPBoosts(rows, err, now)
}

// CurrentRPBoost is the live window, else the next scheduled one, else nil.
func (r *RankedRepository) CurrentRPBoost(ctx context.Context, serverID int64, now time.Time) (*RPBoost, error) {
	b, err := scanRPBoost(r.pool.QueryRow(ctx, `SELECT `+rpBoostCols+` FROM ranked_rp_boosts b
WHERE b.server_id=$1 AND `+rpBoostEnd+`>$2 AND `+rpBoostEnd+`>b.starts_at ORDER BY b.starts_at LIMIT 1`, serverID, now), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// RPBoostAnnouncement is a window whose Discord card is due. Kind is SCHEDULED, STARTED or ENDED.
type RPBoostAnnouncement struct {
	Boost RPBoost
	Kind  string
}

// DueRPBoostAnnouncements returns the cards the guild's servers owe: a scheduled window more than a
// minute away, a window that has started, and a started window that ended at least
// RPBoostResultsDelay ago (kills reach the bot minutes after they happen, so the results wait for
// the last ones). A window stopped before it started gets nothing more.
func (r *RankedRepository) DueRPBoostAnnouncements(ctx context.Context, guildID int64, now time.Time) ([]RPBoostAnnouncement, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+rpBoostCols+`,
CASE WHEN b.start_announced_at IS NOT NULL THEN 'ENDED'
     WHEN b.starts_at<=$2 THEN 'STARTED' ELSE 'SCHEDULED' END
FROM ranked_rp_boosts b JOIN game_servers gs ON gs.id=b.server_id
WHERE gs.guild_id=$1 AND `+rpBoostEnd+`>b.starts_at AND (
  (b.start_announced_at IS NOT NULL AND b.end_announced_at IS NULL AND `+rpBoostEnd+`<=$2-INTERVAL '10 minutes')
  OR (b.start_announced_at IS NULL AND b.starts_at<=$2 AND `+rpBoostEnd+`>$2)
  OR (b.start_announced_at IS NULL AND b.scheduled_announced_at IS NULL AND b.starts_at>$2+INTERVAL '1 minute'))
ORDER BY b.starts_at LIMIT 20`, guildID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RPBoostAnnouncement
	for rows.Next() {
		var a RPBoostAnnouncement
		b := &a.Boost
		if err := rows.Scan(&b.ID, &b.ServerID, &b.Multiplier, &b.StartsAt, &b.EndsAt, &b.PlannedEnd, &b.Stopped, &b.CreatedBy, &a.Kind); err != nil {
			return nil, err
		}
		b.Status = rpBoostStatus(*b, now)
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkRPBoostAnnounced records that a card was posted (or deliberately skipped).
func (r *RankedRepository) MarkRPBoostAnnounced(ctx context.Context, boostID int64, kind string, now time.Time) error {
	column := map[string]string{"SCHEDULED": "scheduled_announced_at", "STARTED": "start_announced_at", "ENDED": "end_announced_at"}[kind]
	if column == "" {
		return fmt.Errorf("unknown double RP announcement %q", kind)
	}
	_, err := r.pool.Exec(ctx, `UPDATE ranked_rp_boosts SET `+column+`=COALESCE(`+column+`,$2) WHERE id=$1`, boostID, now)
	return err
}

// RPBoostLeader is one player's RP earned inside a window.
type RPBoostLeader struct {
	PlayerName string `json:"playerName"`
	RP         int64  `json:"rp"`
	Kills      int    `json:"kills"`
}

// RPBoostLeaders ranks the players who earned the most RP inside a window.
func (r *RankedRepository) RPBoostLeaders(ctx context.Context, b RPBoost, limit int) ([]RPBoostLeader, error) {
	rows, err := r.pool.Query(ctx, `SELECT COALESCE(p.display_name,''),SUM(a.amount)::BIGINT,COUNT(*)::INT
FROM ranked_awards a JOIN ranked_seasons s ON s.id=a.season_id AND s.server_id=$1
LEFT JOIN players p ON p.id=a.attacker_key::BIGINT
WHERE a.outcome='AWARDED' AND a.event_time>=$2 AND a.event_time<$3
GROUP BY a.attacker_key,p.display_name ORDER BY SUM(a.amount) DESC, MIN(a.event_time) LIMIT $4`, b.ServerID, b.StartsAt, b.EndsAt, clampLimit(limit, 3, 10))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RPBoostLeader{}
	for rows.Next() {
		var l RPBoostLeader
		if err := rows.Scan(&l.PlayerName, &l.RP, &l.Kills); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
