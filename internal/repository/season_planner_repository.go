package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Season planner (docs/CLIENT_HUB_GROWTH.md): owners schedule a stats season rollover or a
// per-server Ranked reset ahead of time, preview what will be archived, and Champion runs and
// announces it. The actual season changes reuse SeasonRepository and RankedRepository.

const (
	SeasonActionStats  = "STATS_SEASON"
	SeasonActionRanked = "RANKED_RESET"
)

var (
	ErrSeasonActionExists   = errors.New("a season change of this kind is already scheduled")
	ErrSeasonActionNotFound = errors.New("scheduled season change not found or no longer pending")
)

type SeasonPlannerRepository struct{ pool *pgxpool.Pool }

func NewSeasonPlannerRepository(pool *pgxpool.Pool) *SeasonPlannerRepository {
	return &SeasonPlannerRepository{pool: pool}
}

type SeasonAction struct {
	ID            int64      `json:"id"`
	GuildID       int64      `json:"-"`
	ServerID      *int64     `json:"serverId"`
	ServerName    string     `json:"serverName,omitempty"`
	Kind          string     `json:"kind"`
	RunAt         time.Time  `json:"runAt"`
	NewSeasonName string     `json:"newSeasonName,omitempty"`
	Announce      bool       `json:"announce"`
	Status        string     `json:"status"`
	CreatedBy     string     `json:"createdBy,omitempty"`
	NoticeSentAt  *time.Time `json:"noticeSentAt,omitempty"`
	ExecutedAt    *time.Time `json:"executedAt,omitempty"`
	ResultDetail  string     `json:"resultDetail,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
}

const seasonActionCols = `a.id,a.guild_id,a.server_id,COALESCE(gs.display_name,''),a.kind,a.run_at,a.new_season_name,a.announce,a.status,a.created_by,a.notice_sent_at,a.executed_at,a.result_detail,a.created_at`

func scanSeasonAction(row pgx.Row) (SeasonAction, error) {
	var a SeasonAction
	err := row.Scan(&a.ID, &a.GuildID, &a.ServerID, &a.ServerName, &a.Kind, &a.RunAt, &a.NewSeasonName, &a.Announce, &a.Status, &a.CreatedBy, &a.NoticeSentAt, &a.ExecutedAt, &a.ResultDetail, &a.CreatedAt)
	return a, err
}

// Schedule stores a pending action. A RANKED_RESET server must belong to the guild.
func (r *SeasonPlannerRepository) Schedule(ctx context.Context, a SeasonAction) (SeasonAction, error) {
	row := r.pool.QueryRow(ctx, `
WITH ins AS (
  INSERT INTO scheduled_season_actions(guild_id,server_id,kind,run_at,new_season_name,announce,created_by)
  SELECT $1,$2,$3,$4,$5,$6,$7
  WHERE $2::BIGINT IS NULL OR EXISTS (SELECT 1 FROM game_servers WHERE id=$2 AND guild_id=$1)
  RETURNING *
)
SELECT `+seasonActionCols+` FROM ins a LEFT JOIN game_servers gs ON gs.id=a.server_id`,
		a.GuildID, a.ServerID, a.Kind, a.RunAt, a.NewSeasonName, a.Announce, a.CreatedBy)
	out, err := scanSeasonAction(row)
	if isUniqueViolation(err) {
		return out, ErrSeasonActionExists
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("server does not belong to this Discord")
	}
	return out, err
}

// List returns open actions first, then the most recent finished ones.
func (r *SeasonPlannerRepository) List(ctx context.Context, guildID int64, limit int) ([]SeasonAction, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+seasonActionCols+`
FROM scheduled_season_actions a LEFT JOIN game_servers gs ON gs.id=a.server_id
WHERE a.guild_id=$1
ORDER BY CASE WHEN a.status IN ('PENDING','RUNNING') THEN 0 ELSE 1 END, CASE WHEN a.status IN ('PENDING','RUNNING') THEN a.run_at END ASC, a.id DESC
LIMIT $2`, guildID, clampLimit(limit, 20, 100))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SeasonAction{}
	for rows.Next() {
		a, err := scanSeasonAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Cancel withdraws a pending action (a running or finished one cannot be cancelled).
func (r *SeasonPlannerRepository) Cancel(ctx context.Context, guildID, id int64) (SeasonAction, error) {
	row := r.pool.QueryRow(ctx, `
WITH upd AS (
  UPDATE scheduled_season_actions SET status='CANCELLED',updated_at=NOW()
  WHERE guild_id=$1 AND id=$2 AND status='PENDING' RETURNING *
)
SELECT `+seasonActionCols+` FROM upd a LEFT JOIN game_servers gs ON gs.id=a.server_id`, guildID, id)
	a, err := scanSeasonAction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrSeasonActionNotFound
	}
	return a, err
}

// PendingNotices are pending actions whose "scheduled" card has not been posted.
func (r *SeasonPlannerRepository) PendingNotices(ctx context.Context, guildID int64, limit int) ([]SeasonAction, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+seasonActionCols+`
FROM scheduled_season_actions a LEFT JOIN game_servers gs ON gs.id=a.server_id
WHERE a.guild_id=$1 AND a.status='PENDING' AND a.announce AND a.notice_sent_at IS NULL
ORDER BY a.run_at LIMIT $2`, guildID, clampLimit(limit, 10, 50))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SeasonAction{}
	for rows.Next() {
		a, err := scanSeasonAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *SeasonPlannerRepository) MarkNoticeSent(ctx context.Context, guildID, id int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE scheduled_season_actions SET notice_sent_at=$3,updated_at=NOW() WHERE guild_id=$1 AND id=$2 AND notice_sent_at IS NULL`, guildID, id, at)
	return err
}

// ClaimDue moves due pending actions to RUNNING and returns them. Each action is claimed once:
// a second scheduler (or a restart) never runs it again. An action left RUNNING for more than
// ten minutes was interrupted mid-change; it is failed, never re-run, so a reset cannot apply twice.
func (r *SeasonPlannerRepository) ClaimDue(ctx context.Context, guildID int64, now time.Time, limit int) ([]SeasonAction, error) {
	if _, err := r.pool.Exec(ctx, `UPDATE scheduled_season_actions SET status='FAILED',executed_at=$2,updated_at=NOW(),
result_detail='Interrupted while running. Check the current season before scheduling again.'
WHERE guild_id=$1 AND status='RUNNING' AND started_at < $2::timestamptz - INTERVAL '10 minutes'`, guildID, now); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
WITH due AS (
  SELECT id FROM scheduled_season_actions WHERE guild_id=$1 AND status='PENDING' AND run_at<=$2::timestamptz
  ORDER BY run_at LIMIT $3 FOR UPDATE SKIP LOCKED
), upd AS (
  UPDATE scheduled_season_actions s SET status='RUNNING',started_at=$2,updated_at=NOW() FROM due WHERE s.id=due.id RETURNING s.*
)
SELECT `+seasonActionCols+` FROM upd a LEFT JOIN game_servers gs ON gs.id=a.server_id ORDER BY a.run_at`, guildID, now, clampLimit(limit, 5, 20))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SeasonAction{}
	for rows.Next() {
		a, err := scanSeasonAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Finish records the outcome of a claimed action (status DONE or FAILED).
func (r *SeasonPlannerRepository) Finish(ctx context.Context, guildID, id int64, status, detail string, at time.Time) error {
	if status != "DONE" && status != "FAILED" {
		return fmt.Errorf("invalid finish status %q", status)
	}
	if len(detail) > 500 {
		detail = detail[:500]
	}
	_, err := r.pool.Exec(ctx, `UPDATE scheduled_season_actions SET status=$3,result_detail=$4,executed_at=$5,updated_at=NOW() WHERE guild_id=$1 AND id=$2 AND status='RUNNING'`, guildID, id, status, detail, at)
	return err
}

// --- previews: what the change would archive -------------------------------------------------------

type PreviewLeader struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

type StatsSeasonPreview struct {
	SeasonID    int64           `json:"seasonId"`
	Name        string          `json:"name"`
	StartsAt    time.Time       `json:"startsAt"`
	Kills       int64           `json:"kills"`
	Players     int64           `json:"players"`
	TopKillers  []PreviewLeader `json:"topKillers"`
	LongestKill *PreviewLeader  `json:"longestKill"`
}

// StatsSeasonPreview summarises the active stats season (nil when none is active). It reads the
// same kills rows FinalizeSeason archives and writes nothing.
func (r *SeasonPlannerRepository) StatsSeasonPreview(ctx context.Context, guildID int64) (*StatsSeasonPreview, error) {
	var p StatsSeasonPreview
	err := r.pool.QueryRow(ctx, `SELECT id,name,starts_at FROM seasons WHERE guild_id=$1 AND status='ACTIVE'`, guildID).Scan(&p.SeasonID, &p.Name, &p.StartsAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*),COUNT(DISTINCT killer_player_id) FROM kills WHERE guild_id=$1 AND season_id=$2`, guildID, p.SeasonID).Scan(&p.Kills, &p.Players); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT p.display_name,COUNT(*) FROM kills k JOIN players p ON p.id=k.killer_player_id
WHERE k.guild_id=$1 AND k.season_id=$2 GROUP BY p.id,p.display_name ORDER BY COUNT(*) DESC,p.id LIMIT 3`, guildID, p.SeasonID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p.TopKillers = []PreviewLeader{}
	for rows.Next() {
		var l PreviewLeader
		var n int64
		if err := rows.Scan(&l.Name, &n); err != nil {
			return nil, err
		}
		l.Value = float64(n)
		p.TopKillers = append(p.TopKillers, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var longest PreviewLeader
	err = r.pool.QueryRow(ctx, `SELECT p.display_name,k.distance FROM kills k JOIN players p ON p.id=k.killer_player_id
WHERE k.guild_id=$1 AND k.season_id=$2 AND k.distance IS NOT NULL ORDER BY k.distance DESC LIMIT 1`, guildID, p.SeasonID).Scan(&longest.Name, &longest.Value)
	if err == nil {
		p.LongestKill = &longest
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return &p, nil
}

type RankedSeasonPreview struct {
	SeasonID  int64     `json:"seasonId"`
	ServerID  int64     `json:"serverId"`
	StartsAt  time.Time `json:"startsAt"`
	RPPerKill int64     `json:"rpPerKill"`
	// SameVictimCooldownMinutes: minutes before the same victim earns the killer RP again (0 = no wait).
	SameVictimCooldownMinutes int             `json:"sameVictimCooldownMinutes"`
	AwardedKills              int64           `json:"awardedKills"`
	Players                   int64           `json:"players"`
	Top                       []PreviewLeader `json:"top"`
}

// RankedSeasonPreview summarises a server's active Ranked season (nil when none is active).
func (r *SeasonPlannerRepository) RankedSeasonPreview(ctx context.Context, guildID, serverID int64) (*RankedSeasonPreview, error) {
	p := RankedSeasonPreview{ServerID: serverID, Top: []PreviewLeader{}}
	err := r.pool.QueryRow(ctx, `SELECT s.id,s.starts_at,s.rp_per_kill,s.same_victim_cooldown_minutes FROM ranked_seasons s JOIN game_servers gs ON gs.id=s.server_id AND gs.guild_id=$1
WHERE s.scope='SERVER' AND s.status='ACTIVE' AND s.server_id=$2`, guildID, serverID).Scan(&p.SeasonID, &p.StartsAt, &p.RPPerKill, &p.SameVictimCooldownMinutes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*),COUNT(DISTINCT attacker_key) FROM ranked_awards WHERE season_id=$1 AND outcome='AWARDED'`, p.SeasonID).Scan(&p.AwardedKills, &p.Players); err != nil {
		return nil, err
	}
	return &p, nil
}
