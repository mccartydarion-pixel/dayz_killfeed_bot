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

// BaseTransferRepository backs base transfers (docs/BASE_TRANSFERS.md): a base
// owner asks to hand a registered base to an active member of their faction,
// and the server owner approves (the base changes owner) or declines.
type BaseTransferRepository struct{ pool *pgxpool.Pool }

func NewBaseTransferRepository(pool *pgxpool.Pool) *BaseTransferRepository {
	return &BaseTransferRepository{pool: pool}
}

const (
	BaseTransferPending   = "PENDING"
	BaseTransferApproved  = "APPROVED"
	BaseTransferDeclined  = "DECLINED"
	BaseTransferCancelled = "CANCELLED"
)

var (
	ErrInvalidBaseTransfer  = errors.New("invalid base transfer")
	ErrBaseTransferNotFound = errors.New("base transfer not found")
	ErrBaseTransferDecided  = errors.New("base transfer already answered")
	ErrBaseTransferNotMate  = errors.New("not your base, or not an active faction mate with a verified link")
	ErrBaseTransferLimit    = errors.New("the new owner has the most bases allowed")
	ErrBaseTransferWaiting  = errors.New("this base already has a waiting transfer")
)

type BaseTransfer struct {
	ID            int64      `json:"id"`
	BaseID        int64      `json:"baseId"`
	BaseName      string     `json:"baseName"`
	FromPlayerID  int64      `json:"fromPlayerId"`
	FromName      string     `json:"fromName,omitempty"`
	ToPlayerID    int64      `json:"toPlayerId"`
	ToName        string     `json:"toName,omitempty"`
	Status        string     `json:"status"`
	DeclineReason string     `json:"declineReason,omitempty"`
	DecidedAt     *time.Time `json:"decidedAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
}

// TransferMate is a faction mate a base can be handed to.
type TransferMate struct {
	PlayerID int64  `json:"playerId"`
	Name     string `json:"name"`
}

// TransferableBase is one of the player's bases.
type TransferableBase struct {
	BaseID   int64  `json:"baseId"`
	BaseName string `json:"baseName"`
}

// BaseTransferDecision is an answered transfer and who to tell.
type BaseTransferDecision struct {
	Transfer    BaseTransfer
	FromDiscord string
	ToDiscord   string
}

func (r *BaseTransferRepository) ready() bool { return r != nil && r.pool != nil }

// mateSQL is true when player $2 is an active, verified member of the same
// faction as player $1 (and not the same player). $3 is the guild.
const mateSQL = `($1::BIGINT<>$2::BIGINT AND EXISTS (SELECT 1 FROM faction_members me
  JOIN faction_members fo ON fo.guild_id=me.guild_id AND fo.faction_id=me.faction_id AND fo.active
  JOIN player_links l ON l.guild_id=fo.guild_id AND l.player_id=fo.player_id AND l.status='VERIFIED'
  WHERE me.guild_id=$3::BIGINT AND me.player_id=$1::BIGINT AND me.active AND fo.player_id=$2::BIGINT))`

const transferSelect = `SELECT t.id,t.base_id,b.name,t.from_player_id,COALESCE(pf.display_name,''),t.to_player_id,COALESCE(pt.display_name,''),
  t.status,t.decline_reason,t.decided_at,t.created_at
 FROM base_transfer_requests t
 JOIN case_registered_bases b ON b.id=t.base_id
 LEFT JOIN players pf ON pf.guild_id=t.guild_id AND pf.id=t.from_player_id
 LEFT JOIN players pt ON pt.guild_id=t.guild_id AND pt.id=t.to_player_id`

func scanTransfer(row pgx.Row) (BaseTransfer, error) {
	var t BaseTransfer
	err := row.Scan(&t.ID, &t.BaseID, &t.BaseName, &t.FromPlayerID, &t.FromName, &t.ToPlayerID, &t.ToName, &t.Status, &t.DeclineReason, &t.DecidedAt, &t.CreatedAt)
	return t, err
}

func scanTransfers(rows pgx.Rows) ([]BaseTransfer, error) {
	defer rows.Close()
	out := make([]BaseTransfer, 0)
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Options lists the player's bases and the faction mates they could hand one to.
func (r *BaseTransferRepository) Options(ctx context.Context, s BaseRequestScope, playerID int64) ([]TransferableBase, []TransferMate, error) {
	if !r.ready() || !s.valid() || playerID <= 0 {
		return nil, nil, ErrInvalidBaseTransfer
	}
	rows, err := r.pool.Query(ctx, `SELECT id,name FROM case_registered_bases
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND owner_player_id=$4 AND state<>'REVOKED' ORDER BY id`,
		s.InstallationID, s.GuildID, s.ServerID, playerID)
	if err != nil {
		return nil, nil, err
	}
	bases := make([]TransferableBase, 0)
	for rows.Next() {
		var b TransferableBase
		if err := rows.Scan(&b.BaseID, &b.BaseName); err != nil {
			rows.Close()
			return nil, nil, err
		}
		bases = append(bases, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = r.pool.Query(ctx, `SELECT DISTINCT fo.player_id,COALESCE(p.display_name,'')
 FROM faction_members me
 JOIN faction_members fo ON fo.guild_id=me.guild_id AND fo.faction_id=me.faction_id AND fo.active AND fo.player_id<>me.player_id
 JOIN player_links l ON l.guild_id=fo.guild_id AND l.player_id=fo.player_id AND l.status='VERIFIED'
 LEFT JOIN players p ON p.guild_id=fo.guild_id AND p.id=fo.player_id
 WHERE me.guild_id=$1 AND me.player_id=$2 AND me.active
 ORDER BY 2,1 LIMIT 50`, s.GuildID, playerID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	mates := make([]TransferMate, 0)
	for rows.Next() {
		var m TransferMate
		if err := rows.Scan(&m.PlayerID, &m.Name); err != nil {
			return nil, nil, err
		}
		mates = append(mates, m)
	}
	return bases, mates, rows.Err()
}

// heldSQL counts a player's bases plus waiting base requests on the server.
const heldSQL = `SELECT
  (SELECT COUNT(*) FROM case_registered_bases WHERE installation_id=$1 AND server_id=$2 AND owner_player_id=$3 AND state<>'REVOKED')
 +(SELECT COUNT(*) FROM case_base_requests WHERE installation_id=$1 AND server_id=$2 AND player_id=$3 AND status='PENDING')`

// Create asks to hand one of the player's bases to a faction mate.
func (r *BaseTransferRepository) Create(ctx context.Context, s BaseRequestScope, fromPlayerID, baseID, toPlayerID int64) (BaseTransfer, error) {
	if !r.ready() || !s.valid() || fromPlayerID <= 0 || baseID <= 0 || toPlayerID <= 0 || fromPlayerID == toPlayerID {
		return BaseTransfer{}, ErrInvalidBaseTransfer
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return BaseTransfer{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('base_transfer:'||$1::BIGINT::TEXT,0))`, baseID); err != nil {
		return BaseTransfer{}, err
	}
	var owns, mate bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM case_registered_bases WHERE id=$1 AND installation_id=$2 AND guild_id=$3
  AND server_id=$4 AND owner_player_id=$5 AND state<>'REVOKED')`, baseID, s.InstallationID, s.GuildID, s.ServerID, fromPlayerID).Scan(&owns); err != nil {
		return BaseTransfer{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT `+mateSQL, fromPlayerID, toPlayerID, s.GuildID).Scan(&mate); err != nil {
		return BaseTransfer{}, err
	}
	if !owns || !mate {
		return BaseTransfer{}, ErrBaseTransferNotMate
	}
	var held int
	if err := tx.QueryRow(ctx, heldSQL, s.InstallationID, s.ServerID, toPlayerID).Scan(&held); err != nil {
		return BaseTransfer{}, err
	}
	if held >= BaseRequestMaxPerPlayer {
		return BaseTransfer{}, ErrBaseTransferLimit
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO base_transfer_requests(installation_id,guild_id,server_id,base_id,from_player_id,to_player_id)
 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, s.InstallationID, s.GuildID, s.ServerID, baseID, fromPlayerID, toPlayerID).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return BaseTransfer{}, ErrBaseTransferWaiting
	}
	if err != nil {
		return BaseTransfer{}, err
	}
	t, err := scanTransfer(tx.QueryRow(ctx, transferSelect+` WHERE t.id=$1`, id))
	if err != nil {
		return BaseTransfer{}, err
	}
	return t, tx.Commit(ctx)
}

// Cancel withdraws the player's own waiting transfer.
func (r *BaseTransferRepository) Cancel(ctx context.Context, s BaseRequestScope, fromPlayerID, transferID int64) error {
	if !r.ready() || !s.valid() || fromPlayerID <= 0 || transferID <= 0 {
		return ErrInvalidBaseTransfer
	}
	tag, err := r.pool.Exec(ctx, `UPDATE base_transfer_requests SET status='CANCELLED',decided_at=NOW()
 WHERE id=$1 AND installation_id=$2 AND guild_id=$3 AND server_id=$4 AND from_player_id=$5 AND status='PENDING'`,
		transferID, s.InstallationID, s.GuildID, s.ServerID, fromPlayerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrBaseTransferNotFound
	}
	return nil
}

// Mine lists the newest transfers the player asked for or would receive.
func (r *BaseTransferRepository) Mine(ctx context.Context, s BaseRequestScope, playerID int64, limit int) ([]BaseTransfer, error) {
	if !r.ready() || !s.valid() || playerID <= 0 {
		return nil, ErrInvalidBaseTransfer
	}
	if limit < 1 || limit > 50 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, transferSelect+` WHERE t.installation_id=$1 AND t.guild_id=$2 AND t.server_id=$3
  AND (t.from_player_id=$4 OR t.to_player_id=$4) ORDER BY t.created_at DESC,t.id DESC LIMIT $5`,
		s.InstallationID, s.GuildID, s.ServerID, playerID, limit)
	if err != nil {
		return nil, err
	}
	return scanTransfers(rows)
}

// ForOwner lists waiting transfers first, then the newest answered ones.
func (r *BaseTransferRepository) ForOwner(ctx context.Context, s BaseRequestScope, limit int) ([]BaseTransfer, error) {
	if !r.ready() || !s.valid() {
		return nil, ErrInvalidBaseTransfer
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, transferSelect+` WHERE t.installation_id=$1 AND t.guild_id=$2 AND t.server_id=$3
 ORDER BY (t.status='PENDING') DESC,t.created_at DESC,t.id DESC LIMIT $4`, s.InstallationID, s.GuildID, s.ServerID, limit)
	if err != nil {
		return nil, err
	}
	return scanTransfers(rows)
}

func (r *BaseTransferRepository) decide(ctx context.Context, s BaseRequestScope, transferID int64, approve bool, reason string, actorUserID *int64) (BaseTransferDecision, error) {
	var out BaseTransferDecision
	reason = strings.TrimSpace(reason)
	if !r.ready() || !s.valid() || transferID <= 0 || len([]rune(reason)) > 300 {
		return out, ErrInvalidBaseTransfer
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var baseID int64
	err = tx.QueryRow(ctx, `SELECT base_id FROM base_transfer_requests WHERE id=$1 AND installation_id=$2 AND guild_id=$3 AND server_id=$4`,
		transferID, s.InstallationID, s.GuildID, s.ServerID).Scan(&baseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrBaseTransferNotFound
	}
	if err != nil {
		return out, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('base_transfer:'||$1::BIGINT::TEXT,0))`, baseID); err != nil {
		return out, err
	}
	t, err := scanTransfer(tx.QueryRow(ctx, transferSelect+` WHERE t.id=$1 FOR UPDATE OF t`, transferID))
	if err != nil {
		return out, err
	}
	if t.Status != BaseTransferPending {
		return out, ErrBaseTransferDecided
	}
	status := BaseTransferDeclined
	if approve {
		// Still their base, still faction mates, and room for one more base.
		var owns, mate bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM case_registered_bases WHERE id=$1 AND owner_player_id=$2 AND state<>'REVOKED')`,
			t.BaseID, t.FromPlayerID).Scan(&owns); err != nil {
			return out, err
		}
		if err := tx.QueryRow(ctx, `SELECT `+mateSQL, t.FromPlayerID, t.ToPlayerID, s.GuildID).Scan(&mate); err != nil {
			return out, err
		}
		if !owns || !mate {
			return out, ErrBaseTransferNotMate
		}
		var held int
		if err := tx.QueryRow(ctx, heldSQL, s.InstallationID, s.ServerID, t.ToPlayerID).Scan(&held); err != nil {
			return out, err
		}
		if held >= BaseRequestMaxPerPlayer {
			return out, ErrBaseTransferLimit
		}
		if _, err := tx.Exec(ctx, `UPDATE case_registered_bases SET owner_player_id=$2,updated_at=NOW() WHERE id=$1`, t.BaseID, t.ToPlayerID); err != nil {
			return out, err
		}
		status, reason = BaseTransferApproved, ""
	}
	if err := tx.QueryRow(ctx, `UPDATE base_transfer_requests SET status=$2,decline_reason=$3,decided_by_user_id=$4,decided_at=NOW()
 WHERE id=$1 RETURNING decided_at`, transferID, status, reason, actorUserID).Scan(&t.DecidedAt); err != nil {
		return out, err
	}
	t.Status, t.DeclineReason = status, reason
	if err := tx.QueryRow(ctx, `SELECT
  COALESCE((SELECT discord_user_id FROM player_links WHERE guild_id=$1 AND player_id=$2 AND status='VERIFIED'),''),
  COALESCE((SELECT discord_user_id FROM player_links WHERE guild_id=$1 AND player_id=$3 AND status='VERIFIED'),'')`,
		s.GuildID, t.FromPlayerID, t.ToPlayerID).Scan(&out.FromDiscord, &out.ToDiscord); err != nil {
		return out, err
	}
	out.Transfer = t
	return out, tx.Commit(ctx)
}

// Approve hands the base to the faction mate, if it's still allowed.
func (r *BaseTransferRepository) Approve(ctx context.Context, s BaseRequestScope, transferID int64, actorUserID *int64) (BaseTransferDecision, error) {
	return r.decide(ctx, s, transferID, true, "", actorUserID)
}

// Decline answers no, with an optional reason for the players.
func (r *BaseTransferRepository) Decline(ctx context.Context, s BaseRequestScope, transferID int64, reason string, actorUserID *int64) (BaseTransferDecision, error) {
	return r.decide(ctx, s, transferID, false, reason, actorUserID)
}
