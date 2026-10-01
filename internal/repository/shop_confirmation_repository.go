package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Buyer confirmation of a delivered Shop order and its support tickets (migration 0087,
// docs/SHOP_ORDER_CONFIRMATION.md). The confirmation row is opened by a database trigger when the
// purchase becomes FULFILLED; this file only moves it forward. Every transition is a compare-and-set
// on the current state, so of two concurrent answers (or an answer racing the deadline) exactly one
// wins and the other gets ErrShopConfirmationState.

// Confirmation states.
const (
	ConfirmationAwaitingBuyer = "AWAITING_BUYER"
	ConfirmationReceived      = "RECEIVED"
	ConfirmationIssueReported = "ISSUE_REPORTED"
	ConfirmationAutoCompleted = "AUTO_COMPLETED"
	ConfirmationVoid          = "VOID"
)

// Where an answer came from.
const (
	ConfirmationViaSite    = "SITE"
	ConfirmationViaDiscord = "DISCORD"
)

// Ticket statuses and resolutions.
const (
	TicketOpen     = "OPEN"
	TicketResolved = "RESOLVED"

	TicketResolutionCompleted = "COMPLETED"
	TicketResolutionRefunded  = "REFUNDED"
	TicketResolutionOther     = "OTHER"
)

// ShopConfirmationWindow mirrors the deadline_at default of migration 0087.
const ShopConfirmationWindow = 48 * time.Hour

var (
	// ErrShopConfirmationNotFound: the purchase has no confirmation (not delivered yet, or it was
	// delivered before the feature existed), or it is not the caller's purchase.
	ErrShopConfirmationNotFound = errors.New("shop order confirmation not found")
	// ErrShopConfirmationState: the confirmation is not in a state that allows this answer.
	ErrShopConfirmationState = errors.New("shop order confirmation is not awaiting this answer")
	ErrShopTicketNotFound    = errors.New("shop order ticket not found")
	// ErrShopTicketState: the ticket is already resolved.
	ErrShopTicketState = errors.New("shop order ticket is already resolved")
)

// ShopOrderConfirmation is one purchase's confirmation.
type ShopOrderConfirmation struct {
	PurchaseID, OrganizationID, InstallationID, PlayerID int64
	State                                                string
	DeliveredAt, DeadlineAt                              time.Time
	RespondedAt                                          *time.Time
	ResponseSource                                       *string
}

// ShopOrderTicket is one support ticket about an order.
type ShopOrderTicket struct {
	ID, OrganizationID, InstallationID, PurchaseID, PlayerID int64
	OpenedByDiscordID, OpenedVia, Reason, Status             string
	Resolution, ResolutionNote, DiscordChannelID             *string
	OpenedAt                                                 time.Time
	ResolvedAt                                               *time.Time
	ResolvedByUserID                                         *int64
}

// ShopConfirmationRepository reads and advances confirmations and tickets.
type ShopConfirmationRepository struct{ pool pgxBeginner }

func NewShopConfirmationRepository(pool pgxBeginner) *ShopConfirmationRepository {
	return &ShopConfirmationRepository{pool: pool}
}

const confirmationCols = `purchase_id, organization_id, installation_id, player_id, state, delivered_at, deadline_at, responded_at, response_source`

func scanConfirmation(row pgx.Row) (ShopOrderConfirmation, error) {
	var c ShopOrderConfirmation
	err := row.Scan(&c.PurchaseID, &c.OrganizationID, &c.InstallationID, &c.PlayerID, &c.State, &c.DeliveredAt, &c.DeadlineAt, &c.RespondedAt, &c.ResponseSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrShopConfirmationNotFound
	}
	return c, err
}

const ticketCols = `id, organization_id, installation_id, purchase_id, player_id, opened_by_discord_id, opened_via, reason, status,
 resolution, resolution_note, discord_channel_id, opened_at, resolved_at, resolved_by_user_id`

func scanTicket(row pgx.Row) (ShopOrderTicket, error) {
	var t ShopOrderTicket
	err := row.Scan(&t.ID, &t.OrganizationID, &t.InstallationID, &t.PurchaseID, &t.PlayerID, &t.OpenedByDiscordID, &t.OpenedVia, &t.Reason, &t.Status,
		&t.Resolution, &t.ResolutionNote, &t.DiscordChannelID, &t.OpenedAt, &t.ResolvedAt, &t.ResolvedByUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrShopTicketNotFound
	}
	return t, err
}

// GetConfirmation reads a purchase's confirmation. playerID > 0 scopes it to that buyer (another
// player's purchase is "not found"); 0 is the admin read.
func (r *ShopConfirmationRepository) GetConfirmation(ctx context.Context, org, inst, purchaseID, playerID int64) (ShopOrderConfirmation, error) {
	return getConfirmation(ctx, r.pool, org, inst, purchaseID, playerID, false)
}

func getConfirmation(ctx context.Context, q shopQuerier, org, inst, purchaseID, playerID int64, lock bool) (ShopOrderConfirmation, error) {
	sql := `SELECT ` + confirmationCols + ` FROM shop_order_confirmations
 WHERE purchase_id=$3 AND organization_id=$1 AND installation_id=$2 AND ($4 = 0 OR player_id = $4)`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanConfirmation(q.QueryRow(ctx, sql, org, inst, purchaseID, playerID))
}

// ConfirmReceived records the buyer's "received" answer: AWAITING_BUYER -> RECEIVED.
func (r *ShopConfirmationRepository) ConfirmReceived(ctx context.Context, org, inst, purchaseID, playerID int64, via string) (ShopOrderConfirmation, error) {
	c, err := scanConfirmation(r.pool.QueryRow(ctx, `UPDATE shop_order_confirmations
   SET state='RECEIVED', responded_at=NOW(), response_source=$5, updated_at=NOW()
 WHERE purchase_id=$3 AND organization_id=$1 AND installation_id=$2 AND player_id=$4 AND state='AWAITING_BUYER'
 RETURNING `+confirmationCols, org, inst, purchaseID, playerID, via))
	if errors.Is(err, ErrShopConfirmationNotFound) {
		return c, r.absentOrWrongState(ctx, org, inst, purchaseID, playerID)
	}
	return c, err
}

// absentOrWrongState tells "no such confirmation for this buyer" from "it exists but not in that state".
func (r *ShopConfirmationRepository) absentOrWrongState(ctx context.Context, org, inst, purchaseID, playerID int64) error {
	if _, err := r.GetConfirmation(ctx, org, inst, purchaseID, playerID); err != nil {
		return err
	}
	return ErrShopConfirmationState
}

// ShopIssueReport is the buyer's "issue with order".
type ShopIssueReport struct {
	OrganizationID, InstallationID, PurchaseID, PlayerID int64
	DiscordID, Via, Reason                               string
}

// ReportIssue records the buyer's issue and opens its ticket in one transaction:
// AWAITING_BUYER or AUTO_COMPLETED -> ISSUE_REPORTED. A buyer who already answered RECEIVED, or whose
// order was refunded, gets ErrShopConfirmationState; so does a second report (one OPEN ticket per order).
func (r *ShopConfirmationRepository) ReportIssue(ctx context.Context, in ShopIssueReport) (ShopOrderConfirmation, ShopOrderTicket, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ShopOrderConfirmation{}, ShopOrderTicket{}, err
	}
	defer tx.Rollback(ctx)
	c, err := scanConfirmation(tx.QueryRow(ctx, `UPDATE shop_order_confirmations
   SET state='ISSUE_REPORTED', responded_at=NOW(), response_source=$5, updated_at=NOW()
 WHERE purchase_id=$3 AND organization_id=$1 AND installation_id=$2 AND player_id=$4 AND state IN ('AWAITING_BUYER','AUTO_COMPLETED')
 RETURNING `+confirmationCols, in.OrganizationID, in.InstallationID, in.PurchaseID, in.PlayerID, in.Via))
	if errors.Is(err, ErrShopConfirmationNotFound) {
		if _, gerr := getConfirmation(ctx, tx, in.OrganizationID, in.InstallationID, in.PurchaseID, in.PlayerID, false); gerr != nil {
			return c, ShopOrderTicket{}, gerr
		}
		return c, ShopOrderTicket{}, ErrShopConfirmationState
	}
	if err != nil {
		return c, ShopOrderTicket{}, err
	}
	t, err := scanTicket(tx.QueryRow(ctx, `INSERT INTO shop_order_tickets(organization_id, installation_id, purchase_id, player_id, opened_by_discord_id, opened_via, reason)
 VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+ticketCols, in.OrganizationID, in.InstallationID, in.PurchaseID, in.PlayerID, in.DiscordID, in.Via, in.Reason))
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" { // uq_shop_order_tickets_open
			return c, t, ErrShopConfirmationState
		}
		return c, t, err
	}
	if err := tx.Commit(ctx); err != nil {
		return c, t, err
	}
	return c, t, nil
}

// AutoCompleteDue closes confirmations whose deadline has passed: AWAITING_BUYER -> AUTO_COMPLETED.
// It returns what it closed (at most limit per call), oldest deadline first.
func (r *ShopConfirmationRepository) AutoCompleteDue(ctx context.Context, now time.Time, limit int) ([]ShopOrderConfirmation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `UPDATE shop_order_confirmations c
   SET state='AUTO_COMPLETED', responded_at=$1, response_source='AUTO', updated_at=NOW()
 WHERE c.purchase_id IN (SELECT purchase_id FROM shop_order_confirmations
                          WHERE state='AWAITING_BUYER' AND deadline_at <= $1
                          ORDER BY deadline_at LIMIT $2 FOR UPDATE SKIP LOCKED)
   AND c.state='AWAITING_BUYER'
 RETURNING `+confirmationCols, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShopOrderConfirmation
	for rows.Next() {
		c, err := scanConfirmation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListTickets lists an installation's tickets, newest first. status "" lists all.
func (r *ShopConfirmationRepository) ListTickets(ctx context.Context, org, inst int64, status string, beforeID int64, limit int) ([]ShopOrderTicket, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := r.pool.Query(ctx, `SELECT `+ticketCols+` FROM shop_order_tickets
 WHERE organization_id=$1 AND installation_id=$2 AND ($3 = '' OR status = $3) AND ($4 = 0 OR id < $4)
 ORDER BY id DESC LIMIT $5`, org, inst, status, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShopOrderTicket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTicket reads one ticket. playerID > 0 scopes it to the buyer.
func (r *ShopConfirmationRepository) GetTicket(ctx context.Context, org, inst, id, playerID int64) (ShopOrderTicket, error) {
	return scanTicket(r.pool.QueryRow(ctx, `SELECT `+ticketCols+` FROM shop_order_tickets
 WHERE id=$3 AND organization_id=$1 AND installation_id=$2 AND ($4 = 0 OR player_id = $4)`, org, inst, id, playerID))
}

// ResolveTicket closes an OPEN ticket (staff). A second resolve is ErrShopTicketState. It records the
// decision only: a refund is still performed through the Shop's own refund flow.
func (r *ShopConfirmationRepository) ResolveTicket(ctx context.Context, org, inst, id, actorUserID int64, resolution, note string) (ShopOrderTicket, error) {
	t, err := scanTicket(r.pool.QueryRow(ctx, `UPDATE shop_order_tickets
   SET status='RESOLVED', resolution=$5, resolution_note=NULLIF($6,''), resolved_at=NOW(), resolved_by_user_id=$4, updated_at=NOW()
 WHERE id=$3 AND organization_id=$1 AND installation_id=$2 AND status='OPEN'
 RETURNING `+ticketCols, org, inst, id, actorUserID, resolution, note))
	if errors.Is(err, ErrShopTicketNotFound) {
		if _, gerr := r.GetTicket(ctx, org, inst, id, 0); gerr != nil {
			return t, gerr
		}
		return t, ErrShopTicketState
	}
	return t, err
}

// SetTicketChannel records the Discord channel created for an OPEN ticket (set once).
func (r *ShopConfirmationRepository) SetTicketChannel(ctx context.Context, org, inst, id int64, channelID string) (ShopOrderTicket, error) {
	t, err := scanTicket(r.pool.QueryRow(ctx, `UPDATE shop_order_tickets
   SET discord_channel_id=$4, updated_at=NOW()
 WHERE id=$3 AND organization_id=$1 AND installation_id=$2 AND discord_channel_id IS NULL
 RETURNING `+ticketCols, org, inst, id, channelID))
	if errors.Is(err, ErrShopTicketNotFound) {
		if _, gerr := r.GetTicket(ctx, org, inst, id, 0); gerr != nil {
			return t, gerr
		}
		return t, ErrShopTicketState
	}
	return t, err
}
