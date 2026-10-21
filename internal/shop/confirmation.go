package shop

import (
	"context"
	"errors"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Buyer confirmation of a delivered order (docs/SHOP_ORDER_CONFIRMATION.md). After an order is
// delivered the buyer answers "received" or "issue with order"; an issue opens a support ticket for
// staff. No answer by the deadline completes the order automatically, and the buyer may still report
// an issue afterwards. Only the buyer's own VERIFIED player can answer for a purchase.

// ConfirmationStore is the persistence the confirmation flow needs;
// *repository.ShopConfirmationRepository implements it.
type ConfirmationStore interface {
	GetConfirmation(ctx context.Context, org, inst, purchaseID, playerID int64) (repository.ShopOrderConfirmation, error)
	ConfirmReceived(ctx context.Context, org, inst, purchaseID, playerID int64, via string) (repository.ShopOrderConfirmation, error)
	ReportIssue(ctx context.Context, in repository.ShopIssueReport) (repository.ShopOrderConfirmation, repository.ShopOrderTicket, error)
	AutoCompleteDue(ctx context.Context, now time.Time, limit int) ([]repository.ShopOrderConfirmation, error)
	ListTickets(ctx context.Context, org, inst int64, status string, beforeID int64, limit int) ([]repository.ShopOrderTicket, error)
	GetTicket(ctx context.Context, org, inst, id, playerID int64) (repository.ShopOrderTicket, error)
	ResolveTicket(ctx context.Context, org, inst, id, actorUserID int64, resolution, note string) (repository.ShopOrderTicket, error)
}

// MaxIssueReasonRunes bounds the buyer's description and the staff resolution note.
const MaxIssueReasonRunes = 1000

var (
	// ErrInvalidVia: the answer did not come from a known surface.
	ErrInvalidVia = errors.New("shop: unknown confirmation source")
	// ErrInvalidResolution: the ticket resolution is not COMPLETED, REFUNDED or OTHER.
	ErrInvalidResolution = errors.New("shop: unknown ticket resolution")
	// ErrInvalidTicketStatus: the ticket list filter is not OPEN, RESOLVED or empty.
	ErrInvalidTicketStatus = errors.New("shop: unknown ticket status")
)

// Confirmations is the buyer-confirmation and ticket service.
type Confirmations struct {
	store    ConfirmationStore
	identity Identity
	now      func() time.Time
}

func NewConfirmations(store ConfirmationStore, identity Identity) *Confirmations {
	return &Confirmations{store: store, identity: identity, now: time.Now}
}

// SetClock replaces the clock (tests).
func (c *Confirmations) SetClock(now func() time.Time) { c.now = now }

func validVia(via string) bool {
	return via == repository.ConfirmationViaSite || via == repository.ConfirmationViaDiscord
}

// buyer resolves the acting user's verified player.
func (c *Confirmations) buyer(ctx context.Context, scope repository.EconomyScope, discordUserID string) (int64, error) {
	acct, err := c.identity.Me(ctx, scope, discordUserID)
	if err != nil {
		return 0, err
	}
	return acct.AccountID, nil
}

// Mine reads the acting buyer's confirmation for one of their own purchases.
func (c *Confirmations) Mine(ctx context.Context, scope repository.EconomyScope, discordUserID string, purchaseID int64) (repository.ShopOrderConfirmation, error) {
	player, err := c.buyer(ctx, scope, discordUserID)
	if err != nil {
		return repository.ShopOrderConfirmation{}, err
	}
	return c.store.GetConfirmation(ctx, scope.OrganizationID, scope.InstallationID, purchaseID, player)
}

// Received records "received order" for the acting buyer's own purchase.
func (c *Confirmations) Received(ctx context.Context, scope repository.EconomyScope, discordUserID string, purchaseID int64, via string) (repository.ShopOrderConfirmation, error) {
	if !validVia(via) {
		return repository.ShopOrderConfirmation{}, ErrInvalidVia
	}
	if scope.Status == "SUSPENDED" {
		return repository.ShopOrderConfirmation{}, economy.ErrSuspended
	}
	player, err := c.buyer(ctx, scope, discordUserID)
	if err != nil {
		return repository.ShopOrderConfirmation{}, err
	}
	return c.store.ConfirmReceived(ctx, scope.OrganizationID, scope.InstallationID, purchaseID, player, via)
}

// ReportIssue records "issue with order" for the acting buyer's own purchase and opens its ticket.
// The reason is required (1-1000 characters after cleaning) and is shown to staff.
func (c *Confirmations) ReportIssue(ctx context.Context, scope repository.EconomyScope, discordUserID string, purchaseID int64, via, reason string) (repository.ShopOrderConfirmation, repository.ShopOrderTicket, error) {
	var none repository.ShopOrderConfirmation
	if !validVia(via) {
		return none, repository.ShopOrderTicket{}, ErrInvalidVia
	}
	reason = cleanText(reason)
	if reason == "" {
		return none, repository.ShopOrderTicket{}, ErrReasonRequired
	}
	if runes(reason) > MaxIssueReasonRunes {
		ve := &ValidationError{}
		ve.add("reason", "reason must be at most 1000 characters")
		return none, repository.ShopOrderTicket{}, ve.err()
	}
	if scope.Status == "SUSPENDED" {
		return none, repository.ShopOrderTicket{}, economy.ErrSuspended
	}
	player, err := c.buyer(ctx, scope, discordUserID)
	if err != nil {
		return none, repository.ShopOrderTicket{}, err
	}
	return c.store.ReportIssue(ctx, repository.ShopIssueReport{
		OrganizationID: scope.OrganizationID, InstallationID: scope.InstallationID, PurchaseID: purchaseID, PlayerID: player,
		DiscordID: discordUserID, Via: via, Reason: reason,
	})
}

// AdminConfirmation reads any purchase's confirmation (organization admins only).
func (c *Confirmations) AdminConfirmation(ctx context.Context, scope repository.EconomyScope, purchaseID int64) (repository.ShopOrderConfirmation, error) {
	return c.store.GetConfirmation(ctx, scope.OrganizationID, scope.InstallationID, purchaseID, 0)
}

// Tickets lists the installation's tickets, newest first (organization admins only).
func (c *Confirmations) Tickets(ctx context.Context, scope repository.EconomyScope, status string, beforeID int64, limit int) ([]repository.ShopOrderTicket, error) {
	if status != "" && status != repository.TicketOpen && status != repository.TicketResolved {
		return nil, ErrInvalidTicketStatus
	}
	return c.store.ListTickets(ctx, scope.OrganizationID, scope.InstallationID, status, beforeID, limit)
}

// Ticket reads one ticket (organization admins only).
func (c *Confirmations) Ticket(ctx context.Context, scope repository.EconomyScope, id int64) (repository.ShopOrderTicket, error) {
	return c.store.GetTicket(ctx, scope.OrganizationID, scope.InstallationID, id, 0)
}

// ResolveTicket closes an open ticket with the staff decision. It records the decision only: a
// REFUNDED resolution does not move points - staff refund through the Shop's refund action, which
// keeps its own guards (ledger attempts, once per purchase). A note is required for OTHER.
func (c *Confirmations) ResolveTicket(ctx context.Context, scope repository.EconomyScope, actorUserID, id int64, resolution, note string) (repository.ShopOrderTicket, error) {
	switch resolution {
	case repository.TicketResolutionCompleted, repository.TicketResolutionRefunded, repository.TicketResolutionOther:
	default:
		return repository.ShopOrderTicket{}, ErrInvalidResolution
	}
	note = cleanText(note)
	if resolution == repository.TicketResolutionOther && note == "" {
		return repository.ShopOrderTicket{}, ErrReasonRequired
	}
	if runes(note) > MaxIssueReasonRunes {
		ve := &ValidationError{}
		ve.add("note", "note must be at most 1000 characters")
		return repository.ShopOrderTicket{}, ve.err()
	}
	if scope.Status == "SUSPENDED" {
		return repository.ShopOrderTicket{}, economy.ErrSuspended
	}
	return c.store.ResolveTicket(ctx, scope.OrganizationID, scope.InstallationID, id, actorUserID, resolution, note)
}

// SweepDue auto-completes every confirmation whose deadline has passed and returns how many it
// closed. It is safe to call from several instances: each row is closed once.
func (c *Confirmations) SweepDue(ctx context.Context) (int, error) {
	total := 0
	for {
		done, err := c.store.AutoCompleteDue(ctx, c.now(), 100)
		if err != nil {
			return total, err
		}
		total += len(done)
		if len(done) < 100 {
			return total, nil
		}
	}
}
