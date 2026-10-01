package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

// Buyer confirmation of delivered Shop orders and their support tickets
// (docs/SHOP_ORDER_CONFIRMATION.md).
//
//	Buyer (their own purchases only; another player's is "not found"):
//	  GET  .../shop/me/purchases/{purchaseID}/confirmation
//	  POST .../shop/me/purchases/{purchaseID}/confirm-received
//	  POST .../shop/me/purchases/{purchaseID}/report-issue      {"reason": "..."}
//	Staff (organization OWNER/ADMIN):
//	  GET  .../shop/admin/purchases/{purchaseID}/confirmation
//	  GET  .../shop/admin/tickets?status=OPEN|RESOLVED&before=<id>&limit=
//	  GET  .../shop/admin/tickets/{ticketID}
//	  POST .../shop/admin/tickets/{ticketID}/resolve            {"resolution": "COMPLETED|REFUNDED|OTHER", "note": "..."}
//
// Every route runs the Shop's own chain (service auth, acting user, tenant scope, plan feature).
const (
	codeConfirmationNotFound = "ORDER_CONFIRMATION_NOT_FOUND"
	codeConfirmationState    = "INVALID_CONFIRMATION_STATE"
	codeShopTicketNotFound   = "SHOP_TICKET_NOT_FOUND"
	codeShopTicketResolved   = "SHOP_TICKET_ALREADY_RESOLVED"
)

func init() {
	httpStatusForCode[codeConfirmationNotFound] = http.StatusNotFound
	httpStatusForCode[codeConfirmationState] = http.StatusConflict
	httpStatusForCode[codeShopTicketNotFound] = http.StatusNotFound
	httpStatusForCode[codeShopTicketResolved] = http.StatusConflict
}

// shopConfirmationSweepInterval is how often overdue confirmations are auto-completed.
const shopConfirmationSweepInterval = 5 * time.Minute

func (a *App) registerShopConfirmationRoutes(base string) {
	h := a.HTTPServer.Handle
	h("GET "+base+"/me/purchases/{purchaseID}/confirmation", a.handleShopMyConfirmation)
	h("POST "+base+"/me/purchases/{purchaseID}/confirm-received", a.handleShopConfirmReceived)
	h("POST "+base+"/me/purchases/{purchaseID}/report-issue", a.handleShopReportIssue)
	h("GET "+base+"/admin/purchases/{purchaseID}/confirmation", a.handleShopAdminConfirmation)
	h("GET "+base+"/admin/tickets", a.handleShopAdminTickets)
	h("GET "+base+"/admin/tickets/{ticketID}", a.handleShopAdminTicket)
	h("POST "+base+"/admin/tickets/{ticketID}/resolve", a.handleShopResolveTicket)
}

type shopConfirmationDTO struct {
	PurchaseID     int64      `json:"purchaseId"`
	State          string     `json:"state"`
	DeliveredAt    time.Time  `json:"deliveredAt"`
	DeadlineAt     time.Time  `json:"deadlineAt"`
	RespondedAt    *time.Time `json:"respondedAt"`
	ResponseSource *string    `json:"responseSource"`
	// What the buyer may still do (the site and the Discord buttons render from these).
	CanConfirmReceived bool `json:"canConfirmReceived"`
	CanReportIssue     bool `json:"canReportIssue"`
}

func confirmationDTO(c repository.ShopOrderConfirmation) shopConfirmationDTO {
	return shopConfirmationDTO{
		PurchaseID: c.PurchaseID, State: c.State, DeliveredAt: c.DeliveredAt, DeadlineAt: c.DeadlineAt,
		RespondedAt: c.RespondedAt, ResponseSource: c.ResponseSource,
		CanConfirmReceived: c.State == repository.ConfirmationAwaitingBuyer,
		CanReportIssue:     c.State == repository.ConfirmationAwaitingBuyer || c.State == repository.ConfirmationAutoCompleted,
	}
}

// shopTicketDTO is the buyer-safe ticket; adminShopTicketDTO adds what only staff see.
type shopTicketDTO struct {
	ID         int64      `json:"id"`
	PurchaseID int64      `json:"purchaseId"`
	Status     string     `json:"status"`
	Reason     string     `json:"reason"`
	Resolution *string    `json:"resolution"`
	OpenedAt   time.Time  `json:"openedAt"`
	ResolvedAt *time.Time `json:"resolvedAt"`
}

type adminShopTicketDTO struct {
	shopTicketDTO
	PlayerID          int64   `json:"playerId"`
	OpenedByDiscordID string  `json:"openedByDiscordId"`
	OpenedVia         string  `json:"openedVia"`
	ResolutionNote    *string `json:"resolutionNote"`
	DiscordChannelID  *string `json:"discordChannelId"`
	ResolvedByUserID  *int64  `json:"resolvedByUserId"`
}

func ticketDTO(t repository.ShopOrderTicket) shopTicketDTO {
	return shopTicketDTO{ID: t.ID, PurchaseID: t.PurchaseID, Status: t.Status, Reason: t.Reason, Resolution: t.Resolution, OpenedAt: t.OpenedAt, ResolvedAt: t.ResolvedAt}
}

func adminTicketDTO(t repository.ShopOrderTicket) adminShopTicketDTO {
	return adminShopTicketDTO{shopTicketDTO: ticketDTO(t), PlayerID: t.PlayerID, OpenedByDiscordID: t.OpenedByDiscordID, OpenedVia: t.OpenedVia,
		ResolutionNote: t.ResolutionNote, DiscordChannelID: t.DiscordChannelID, ResolvedByUserID: t.ResolvedByUserID}
}

// confirmationContext is shopContext plus the confirmation service.
func (a *App) confirmationContext(w http.ResponseWriter, r *http.Request, admin bool) (economyRequest, bool) {
	if a.ShopConfirmations == nil {
		writeSaaSError(w, codeInternalError, "shop order confirmation unavailable")
		return economyRequest{}, false
	}
	return a.shopContext(w, r, admin)
}

func confirmationFailed(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, repository.ErrShopConfirmationNotFound):
		writeSaaSError(w, codeConfirmationNotFound, "this order has no delivery to confirm")
	case errors.Is(err, repository.ErrShopConfirmationState):
		writeSaaSError(w, codeConfirmationState, "this order was already answered or is no longer awaiting an answer")
	case errors.Is(err, repository.ErrShopTicketNotFound):
		writeSaaSError(w, codeShopTicketNotFound, "ticket not found")
	case errors.Is(err, repository.ErrShopTicketState):
		writeSaaSError(w, codeShopTicketResolved, "this ticket is already resolved")
	case errors.Is(err, shop.ErrInvalidResolution):
		writeSaaSError(w, codeInvalidRequest, "resolution must be COMPLETED, REFUNDED or OTHER")
	case errors.Is(err, shop.ErrInvalidTicketStatus):
		writeSaaSError(w, codeInvalidRequest, "status must be OPEN or RESOLVED")
	default:
		shopFailed(w, what, err)
	}
}

func (a *App) handleShopMyConfirmation(w http.ResponseWriter, r *http.Request) {
	a.confirmationDetail(w, r, false)
}

func (a *App) handleShopAdminConfirmation(w http.ResponseWriter, r *http.Request) {
	a.confirmationDetail(w, r, true)
}

func (a *App) confirmationDetail(w http.ResponseWriter, r *http.Request, admin bool) {
	er, ok := a.confirmationContext(w, r, admin)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	var c repository.ShopOrderConfirmation
	var err error
	if admin {
		c, err = a.ShopConfirmations.AdminConfirmation(ctx, er.scope, id)
	} else {
		c, err = a.ShopConfirmations.Mine(ctx, er.scope, er.user.DiscordUserID, id)
	}
	if err != nil {
		confirmationFailed(w, "load shop order confirmation", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Confirmation shopConfirmationDTO `json:"confirmation"`
	}{confirmationDTO(c)})
}

// handleShopConfirmReceived is the buyer's "received order".
func (a *App) handleShopConfirmReceived(w http.ResponseWriter, r *http.Request) {
	er, ok := a.confirmationContext(w, r, false)
	if !ok || !enforceRateLimit(w, a.saasShopPurchaseLimiter, rateLimitKey(r)) {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	c, err := a.ShopConfirmations.Received(ctx, er.scope, er.user.DiscordUserID, id, repository.ConfirmationViaSite)
	if err != nil {
		confirmationFailed(w, "confirm shop order received", err)
		return
	}
	shopAudit("shop_order_confirmed_received", er, "purchase_id", id, "via", repository.ConfirmationViaSite)
	writeSaaSJSON(w, http.StatusOK, struct {
		Confirmation shopConfirmationDTO `json:"confirmation"`
	}{confirmationDTO(c)})
}

type shopReportIssueBody struct {
	Reason string `json:"reason"`
}

// handleShopReportIssue is the buyer's "issue with order": it opens a support ticket.
func (a *App) handleShopReportIssue(w http.ResponseWriter, r *http.Request) {
	er, ok := a.confirmationContext(w, r, false)
	if !ok || !enforceRateLimit(w, a.saasShopPurchaseLimiter, rateLimitKey(r)) {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	var body shopReportIssueBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	c, t, err := a.ShopConfirmations.ReportIssue(ctx, er.scope, er.user.DiscordUserID, id, repository.ConfirmationViaSite, body.Reason)
	if err != nil {
		confirmationFailed(w, "report shop order issue", err)
		return
	}
	shopAudit("shop_order_issue_reported", er, "purchase_id", id, "ticket_id", t.ID, "via", repository.ConfirmationViaSite)
	writeSaaSJSON(w, http.StatusCreated, struct {
		Confirmation shopConfirmationDTO `json:"confirmation"`
		Ticket       shopTicketDTO       `json:"ticket"`
	}{confirmationDTO(c), ticketDTO(t)})
}

func (a *App) handleShopAdminTickets(w http.ResponseWriter, r *http.Request) {
	er, ok := a.confirmationContext(w, r, true)
	if !ok {
		return
	}
	q, ok := shopQuery(w, r)
	if !ok {
		return
	}
	limit, ok := factionLimit(w, r, 25, 100)
	if !ok {
		return
	}
	before, ok := optionalID(w, q, "before")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	list, err := a.ShopConfirmations.Tickets(ctx, er.scope, strings.ToUpper(strings.TrimSpace(q.Get("status"))), before, limit)
	if err != nil {
		confirmationFailed(w, "list shop order tickets", err)
		return
	}
	items := make([]adminShopTicketDTO, 0, len(list))
	for _, t := range list {
		items = append(items, adminTicketDTO(t))
	}
	var next *int64
	if len(list) == limit && limit > 0 {
		id := list[len(list)-1].ID
		next = &id
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Items      []adminShopTicketDTO `json:"items"`
		NextBefore *int64               `json:"nextBefore"`
	}{items, next})
}

func (a *App) handleShopAdminTicket(w http.ResponseWriter, r *http.Request) {
	er, ok := a.confirmationContext(w, r, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "ticketID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	t, err := a.ShopConfirmations.Ticket(ctx, er.scope, id)
	if err != nil {
		confirmationFailed(w, "load shop order ticket", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Ticket adminShopTicketDTO `json:"ticket"`
	}{adminTicketDTO(t)})
}

type shopResolveTicketBody struct {
	Resolution string `json:"resolution"`
	Note       string `json:"note"`
}

// handleShopResolveTicket records the staff decision on a ticket. A REFUNDED resolution moves no
// points: the refund itself is the Shop's refund action, with its own guards.
func (a *App) handleShopResolveTicket(w http.ResponseWriter, r *http.Request) {
	er, ok := a.confirmationContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	id, ok := pathInt64(w, r, "ticketID")
	if !ok {
		return
	}
	var body shopResolveTicketBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	t, err := a.ShopConfirmations.ResolveTicket(ctx, er.scope, er.user.ID, id, strings.ToUpper(strings.TrimSpace(body.Resolution)), body.Note)
	if err != nil {
		confirmationFailed(w, "resolve shop order ticket", err)
		return
	}
	shopAudit("shop_order_ticket_resolved", er, "ticket_id", t.ID, "purchase_id", t.PurchaseID, "resolution", body.Resolution)
	writeSaaSJSON(w, http.StatusOK, struct {
		Ticket adminShopTicketDTO `json:"ticket"`
	}{adminTicketDTO(t)})
}

// runShopConfirmationSweeper auto-completes confirmations whose deadline has passed, once at start
// and then every shopConfirmationSweepInterval. Safe with several instances (each row closes once).
func (a *App) runShopConfirmationSweeper(ctx context.Context) {
	sweep := func() {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n, err := a.ShopConfirmations.SweepDue(sctx)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Warn("component=shop_confirmation", "event", "sweep_failed", "err", err.Error())
		case n > 0:
			slog.Info("component=shop_confirmation", "event", "auto_completed", "count", n)
		}
	}
	sweep()
	t := time.NewTicker(shopConfirmationSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
