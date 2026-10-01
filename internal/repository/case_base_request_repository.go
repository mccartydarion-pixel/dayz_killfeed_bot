package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CaseBaseRequestRepository backs base registration requests: a verified
// player asks for a base at the position the server log last reported for
// them, and the server owner approves (which registers the base) or declines.
type CaseBaseRequestRepository struct{ pool *pgxpool.Pool }

func NewCaseBaseRequestRepository(pool *pgxpool.Pool) *CaseBaseRequestRepository {
	return &CaseBaseRequestRepository{pool: pool}
}

const (
	BaseRequestPending   = "PENDING"
	BaseRequestApproved  = "APPROVED"
	BaseRequestDeclined  = "DECLINED"
	BaseRequestCancelled = "CANCELLED"

	BaseRequestMinRadius = 10
	BaseRequestMaxRadius = 150
	// A player can't hold more than this many bases and open requests together.
	BaseRequestMaxPerPlayer = 3
)

var (
	ErrInvalidBaseRequest  = errors.New("invalid base request")
	ErrBaseRequestOpen     = errors.New("you already have a base request waiting")
	ErrBaseRequestLimit    = errors.New("you have reached the base limit")
	ErrBaseRequestNotFound = errors.New("base request not found")
	ErrBaseRequestDecided  = errors.New("base request already decided")
)

type BaseRequestScope struct{ InstallationID, GuildID, ServerID int64 }

func (s BaseRequestScope) valid() bool {
	return s.InstallationID > 0 && s.GuildID > 0 && s.ServerID > 0
}

type CaseBaseRequest struct {
	ID             int64      `json:"id"`
	PlayerID       int64      `json:"playerId"`
	PlayerName     string     `json:"playerName,omitempty"`
	Name           string     `json:"name"`
	Note           string     `json:"note,omitempty"`
	CenterX        float64    `json:"centerX"`
	CenterZ        float64    `json:"centerZ"`
	Radius         float64    `json:"radius"`
	PositionSeenAt time.Time  `json:"positionSeenAt"`
	Status         string     `json:"status"`
	BaseID         *int64     `json:"baseId,omitempty"`
	DeclineReason  string     `json:"declineReason,omitempty"`
	DecidedAt      *time.Time `json:"decidedAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	// Overlaps names other players' registered bases this circle touches (owner view only).
	Overlaps []string `json:"overlaps,omitempty"`
}

type BaseRequestInput struct {
	PlayerID       int64
	Name, Note     string
	CenterX        float64
	CenterZ        float64
	Radius         float64
	PositionSeenAt time.Time
}

func cleanRequestText(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	return s, len([]rune(s)) <= max
}

// Create records a pending request. A player has at most one pending request
// and BaseRequestMaxPerPlayer bases plus requests on a server.
func (r *CaseBaseRequestRepository) Create(ctx context.Context, s BaseRequestScope, in BaseRequestInput) (CaseBaseRequest, error) {
	name, okName := cleanRequestText(in.Name, 64)
	note, okNote := cleanRequestText(in.Note, 300)
	if r == nil || r.pool == nil || !s.valid() || in.PlayerID <= 0 || name == "" || !okName || !okNote ||
		in.Radius < BaseRequestMinRadius || in.Radius > BaseRequestMaxRadius || in.PositionSeenAt.IsZero() ||
		in.CenterX < -100000 || in.CenterX > 100000 || in.CenterZ < -100000 || in.CenterZ > 100000 {
		return CaseBaseRequest{}, ErrInvalidBaseRequest
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return CaseBaseRequest{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('base_request:'||$1::BIGINT::TEXT||':'||$2::BIGINT::TEXT,0))`,
		s.InstallationID, in.PlayerID); err != nil {
		return CaseBaseRequest{}, err
	}
	var held int
	if err := tx.QueryRow(ctx, `SELECT
  (SELECT COUNT(*) FROM case_registered_bases WHERE installation_id=$1 AND server_id=$2 AND owner_player_id=$3 AND state<>'REVOKED')
 +(SELECT COUNT(*) FROM case_base_requests WHERE installation_id=$1 AND server_id=$2 AND player_id=$3 AND status='PENDING')`,
		s.InstallationID, s.ServerID, in.PlayerID).Scan(&held); err != nil {
		return CaseBaseRequest{}, err
	}
	if held >= BaseRequestMaxPerPlayer {
		return CaseBaseRequest{}, ErrBaseRequestLimit
	}
	out := CaseBaseRequest{PlayerID: in.PlayerID, Name: name, Note: note, CenterX: in.CenterX, CenterZ: in.CenterZ, Radius: in.Radius,
		PositionSeenAt: in.PositionSeenAt, Status: BaseRequestPending}
	err = tx.QueryRow(ctx, `INSERT INTO case_base_requests
 (installation_id,guild_id,server_id,player_id,name,note,center_x,center_z,radius,position_seen_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id,created_at`,
		s.InstallationID, s.GuildID, s.ServerID, in.PlayerID, name, note, in.CenterX, in.CenterZ, in.Radius, in.PositionSeenAt).
		Scan(&out.ID, &out.CreatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return CaseBaseRequest{}, ErrBaseRequestOpen
	}
	if err != nil {
		return CaseBaseRequest{}, err
	}
	return out, tx.Commit(ctx)
}

// Cancel withdraws the player's own pending request.
func (r *CaseBaseRequestRepository) Cancel(ctx context.Context, s BaseRequestScope, playerID, requestID int64) error {
	if r == nil || r.pool == nil || !s.valid() || playerID <= 0 || requestID <= 0 {
		return ErrInvalidBaseRequest
	}
	tag, err := r.pool.Exec(ctx, `UPDATE case_base_requests SET status='CANCELLED',decided_at=NOW()
 WHERE id=$1 AND installation_id=$2 AND guild_id=$3 AND server_id=$4 AND player_id=$5 AND status='PENDING'`,
		requestID, s.InstallationID, s.GuildID, s.ServerID, playerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrBaseRequestNotFound
	}
	return nil
}

const baseRequestCols = `q.id,q.player_id,COALESCE(p.display_name,''),q.name,q.note,q.center_x,q.center_z,q.radius,q.position_seen_at,
 q.status,q.base_id,q.decline_reason,q.decided_at,q.created_at`

func scanBaseRequest(row pgx.Row, withOverlaps bool) (CaseBaseRequest, error) {
	var q CaseBaseRequest
	dest := []any{&q.ID, &q.PlayerID, &q.PlayerName, &q.Name, &q.Note, &q.CenterX, &q.CenterZ, &q.Radius, &q.PositionSeenAt,
		&q.Status, &q.BaseID, &q.DeclineReason, &q.DecidedAt, &q.CreatedAt}
	if withOverlaps {
		dest = append(dest, &q.Overlaps)
	}
	err := row.Scan(dest...)
	return q, err
}

// Mine lists the player's newest requests on this server.
func (r *CaseBaseRequestRepository) Mine(ctx context.Context, s BaseRequestScope, playerID int64, limit int) ([]CaseBaseRequest, error) {
	if r == nil || r.pool == nil || !s.valid() || playerID <= 0 {
		return nil, ErrInvalidBaseRequest
	}
	if limit < 1 || limit > 50 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, `SELECT `+baseRequestCols+` FROM case_base_requests q
 LEFT JOIN players p ON p.guild_id=q.guild_id AND p.id=q.player_id
 WHERE q.installation_id=$1 AND q.guild_id=$2 AND q.server_id=$3 AND q.player_id=$4
 ORDER BY q.created_at DESC,q.id DESC LIMIT $5`, s.InstallationID, s.GuildID, s.ServerID, playerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CaseBaseRequest, 0)
	for rows.Next() {
		q, err := scanBaseRequest(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ForOwner lists pending requests (oldest first) and then the newest decided
// ones, each with the names of other players' bases its circle overlaps.
func (r *CaseBaseRequestRepository) ForOwner(ctx context.Context, s BaseRequestScope, limit int) ([]CaseBaseRequest, error) {
	if r == nil || r.pool == nil || !s.valid() {
		return nil, ErrInvalidBaseRequest
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `SELECT `+baseRequestCols+`,
 COALESCE((SELECT array_agg(b.name ORDER BY b.id) FROM case_registered_bases b
  WHERE b.installation_id=q.installation_id AND b.server_id=q.server_id AND b.state<>'REVOKED' AND b.owner_player_id<>q.player_id
   AND (b.center_x-q.center_x)*(b.center_x-q.center_x)+(b.center_z-q.center_z)*(b.center_z-q.center_z) < (b.radius+q.radius)*(b.radius+q.radius)),
  '{}'::TEXT[])
 FROM case_base_requests q
 LEFT JOIN players p ON p.guild_id=q.guild_id AND p.id=q.player_id
 WHERE q.installation_id=$1 AND q.guild_id=$2 AND q.server_id=$3 AND q.status IN ('PENDING','APPROVED','DECLINED')
 ORDER BY (q.status='PENDING') DESC,
  CASE WHEN q.status='PENDING' THEN EXTRACT(EPOCH FROM q.created_at) ELSE -EXTRACT(EPOCH FROM q.decided_at) END
 LIMIT $4`, s.InstallationID, s.GuildID, s.ServerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CaseBaseRequest, 0)
	for rows.Next() {
		q, err := scanBaseRequest(rows, true)
		if err != nil {
			return nil, err
		}
		if q.Status != BaseRequestPending {
			q.Overlaps = nil
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// BaseRequestDecision is what the owner chose; it tells the caller whom to notify.
type BaseRequestDecision struct {
	Request        CaseBaseRequest
	DiscordUserID  string // the requester's verified link, "" if none
	InstallationID int64
	GuildID        int64
}

// Approve registers the base (as a draft, like an owner-created one) and marks
// the request approved, in one transaction. Name and radius may be adjusted.
func (r *CaseBaseRequestRepository) Approve(ctx context.Context, s BaseRequestScope, requestID int64, mapKey, name string, radius float64, actorUserID *int64) (BaseRequestDecision, error) {
	mapKey = strings.TrimSpace(mapKey)
	name, okName := cleanRequestText(name, 64)
	if r == nil || r.pool == nil || !s.valid() || requestID <= 0 || mapKey == "" || len(mapKey) > 80 || !okName ||
		(radius != 0 && (radius < BaseRequestMinRadius || radius > BaseRequestMaxRadius)) {
		return BaseRequestDecision{}, ErrInvalidBaseRequest
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return BaseRequestDecision{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q, err := r.lockPending(ctx, tx, s, requestID)
	if err != nil {
		return BaseRequestDecision{}, err
	}
	if name != "" {
		q.Name = name
	}
	if radius != 0 {
		q.Radius = radius
	}
	var baseID int64
	if err := tx.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		s.InstallationID, s.GuildID, s.ServerID, q.PlayerID, mapKey, q.Name, q.CenterX, q.CenterZ, q.Radius).Scan(&baseID); err != nil {
		return BaseRequestDecision{}, err
	}
	if err := tx.QueryRow(ctx, `UPDATE case_base_requests SET status='APPROVED',base_id=$2,name=$3,radius=$4,
  decided_by_user_id=$5,decided_at=NOW() WHERE id=$1 RETURNING decided_at`,
		requestID, baseID, q.Name, q.Radius, actorUserID).Scan(&q.DecidedAt); err != nil {
		return BaseRequestDecision{}, err
	}
	q.Status, q.BaseID = BaseRequestApproved, &baseID
	out := BaseRequestDecision{Request: q, InstallationID: s.InstallationID, GuildID: s.GuildID}
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT discord_user_id FROM player_links WHERE guild_id=$1 AND player_id=$2 AND status='VERIFIED'),'')`,
		s.GuildID, q.PlayerID).Scan(&out.DiscordUserID); err != nil {
		return BaseRequestDecision{}, err
	}
	return out, tx.Commit(ctx)
}

// Decline closes the request with an optional reason.
func (r *CaseBaseRequestRepository) Decline(ctx context.Context, s BaseRequestScope, requestID int64, reason string, actorUserID *int64) (BaseRequestDecision, error) {
	reason, okReason := cleanRequestText(reason, 300)
	if r == nil || r.pool == nil || !s.valid() || requestID <= 0 || !okReason {
		return BaseRequestDecision{}, ErrInvalidBaseRequest
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return BaseRequestDecision{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	q, err := r.lockPending(ctx, tx, s, requestID)
	if err != nil {
		return BaseRequestDecision{}, err
	}
	if err := tx.QueryRow(ctx, `UPDATE case_base_requests SET status='DECLINED',decline_reason=$2,decided_by_user_id=$3,decided_at=NOW()
 WHERE id=$1 RETURNING decided_at`, requestID, reason, actorUserID).Scan(&q.DecidedAt); err != nil {
		return BaseRequestDecision{}, err
	}
	q.Status, q.DeclineReason = BaseRequestDeclined, reason
	out := BaseRequestDecision{Request: q, InstallationID: s.InstallationID, GuildID: s.GuildID}
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT discord_user_id FROM player_links WHERE guild_id=$1 AND player_id=$2 AND status='VERIFIED'),'')`,
		s.GuildID, q.PlayerID).Scan(&out.DiscordUserID); err != nil {
		return BaseRequestDecision{}, err
	}
	return out, tx.Commit(ctx)
}

func (r *CaseBaseRequestRepository) lockPending(ctx context.Context, tx pgx.Tx, s BaseRequestScope, requestID int64) (CaseBaseRequest, error) {
	q, err := scanBaseRequest(tx.QueryRow(ctx, `SELECT `+baseRequestCols+` FROM case_base_requests q
 LEFT JOIN players p ON p.guild_id=q.guild_id AND p.id=q.player_id
 WHERE q.id=$1 AND q.installation_id=$2 AND q.guild_id=$3 AND q.server_id=$4 FOR UPDATE OF q`,
		requestID, s.InstallationID, s.GuildID, s.ServerID), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return q, ErrBaseRequestNotFound
	}
	if err != nil {
		return q, err
	}
	if q.Status != BaseRequestPending {
		return q, ErrBaseRequestDecided
	}
	return q, nil
}

// MapKeyHint suggests the map for an approval: the one most used by this
// server's registered bases, or "" when it has none.
func (r *CaseBaseRequestRepository) MapKeyHint(ctx context.Context, s BaseRequestScope) (string, error) {
	if r == nil || r.pool == nil || !s.valid() {
		return "", ErrInvalidBaseRequest
	}
	var key string
	err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT map_key FROM case_registered_bases
 WHERE installation_id=$1 AND server_id=$2 AND state<>'REVOKED' GROUP BY map_key ORDER BY COUNT(*) DESC,map_key LIMIT 1),'')`,
		s.InstallationID, s.ServerID).Scan(&key)
	return key, err
}

// PlayerBases lists the registered (non-withdrawn) bases a player owns on this server.
func (r *CaseBaseRequestRepository) PlayerBases(ctx context.Context, s BaseRequestScope, playerID int64) ([]BlackBoxBase, error) {
	if r == nil || r.pool == nil || !s.valid() || playerID <= 0 {
		return nil, ErrInvalidBaseRequest
	}
	rows, err := r.pool.Query(ctx, `SELECT id,name FROM case_registered_bases
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND owner_player_id=$4 AND state<>'REVOKED' ORDER BY id`,
		s.InstallationID, s.GuildID, s.ServerID, playerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BlackBoxBase, 0)
	for rows.Next() {
		var b BlackBoxBase
		if err := rows.Scan(&b.ID, &b.Name); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RequestNotice returns who to tell about a new request: the organization
// owner's Discord account and the requesting player's name.
func (r *CaseBaseRequestRepository) RequestNotice(ctx context.Context, s BaseRequestScope, playerID int64) (ownerDiscordID, playerName string, err error) {
	if r == nil || r.pool == nil || !s.valid() || playerID <= 0 {
		return "", "", ErrInvalidBaseRequest
	}
	err = r.pool.QueryRow(ctx, `SELECT
  COALESCE((SELECT u.discord_user_id FROM installations i JOIN organizations o ON o.id=i.organization_id
    JOIN app_users u ON u.id=o.owner_user_id WHERE i.id=$1),''),
  COALESCE((SELECT display_name FROM players WHERE guild_id=$2 AND id=$3),'')`,
		s.InstallationID, s.GuildID, playerID).Scan(&ownerDiscordID, &playerName)
	return ownerDiscordID, playerName, err
}

// InstallationForServer finds the installation a guild's game server belongs
// to (for Discord commands, which only know the guild and server).
func (r *CaseBaseRequestRepository) InstallationForServer(ctx context.Context, guildID, serverID int64) (int64, error) {
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 {
		return 0, ErrInvalidBaseRequest
	}
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT i.id FROM installations i
 JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id
 WHERE i.game_server_id=$2 AND c.guild_id=$1 ORDER BY i.id LIMIT 1`, guildID, serverID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrBaseRequestNotFound
	}
	return id, err
}
