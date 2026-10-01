//go:build integration

package repository

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// Migration 0083 against a real PostgreSQL: the buyer confirmation opened by the purchase trigger,
// its answers, the deadline, and the support ticket. Nothing contacts a game server or Discord.

type confirmationWorld struct {
	*attemptWorld
	conf *ShopConfirmationRepository
}

func newConfirmationWorld(t *testing.T) *confirmationWorld {
	w := newAttemptWorld(t)
	return &confirmationWorld{attemptWorld: w, conf: NewShopConfirmationRepository(w.db.Pool)}
}

// delivered returns a fulfilled order of installation A's buyer.
func (w *confirmationWorld) delivered() *order {
	w.t.Helper()
	o := w.order(w.a)
	must(w.t, w.manualFulfill(o))
	return o
}

func (w *confirmationWorld) state(o *order) string {
	w.t.Helper()
	c, err := w.conf.GetConfirmation(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, 0)
	must(w.t, err)
	return c.State
}

func (w *confirmationWorld) issue(o *order, reason string) (ShopOrderConfirmation, ShopOrderTicket, error) {
	return w.conf.ReportIssue(w.ctx, ShopIssueReport{OrganizationID: o.f.OrgID, InstallationID: o.f.InstallationID, PurchaseID: o.purchase,
		PlayerID: w.playerA, DiscordID: "buyer-discord", Via: ConfirmationViaDiscord, Reason: reason})
}

// expire moves an order's deadline into the past, keeping deadline_at > delivered_at.
func (w *confirmationWorld) expire(o *order) {
	w.t.Helper()
	_, err := w.db.Pool.Exec(w.ctx, `UPDATE shop_order_confirmations SET delivered_at = NOW() - INTERVAL '49 hours', deadline_at = NOW() - INTERVAL '1 hour' WHERE purchase_id=$1`, o.purchase)
	must(w.t, err)
}

func TestConfirmationOpensOnFulfilmentOnly(t *testing.T) {
	w := newConfirmationWorld(t)

	open := w.order(w.a)
	if _, err := w.conf.GetConfirmation(w.ctx, open.f.OrgID, open.f.InstallationID, open.purchase, 0); !errors.Is(err, ErrShopConfirmationNotFound) {
		t.Fatalf("an undelivered order: got %v, want no confirmation", err)
	}

	o := w.delivered()
	c, err := w.conf.GetConfirmation(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, w.playerA)
	must(t, err)
	if c.State != ConfirmationAwaitingBuyer || c.PlayerID != w.playerA || c.RespondedAt != nil || c.ResponseSource != nil {
		t.Fatalf("confirmation after fulfilment: %+v", c)
	}
	if window := c.DeadlineAt.Sub(c.DeliveredAt); window != ShopConfirmationWindow {
		t.Fatalf("confirmation window = %v, want %v", window, ShopConfirmationWindow)
	}

	// Another buyer, and another tenant, cannot see or answer it.
	if _, err := w.conf.GetConfirmation(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, w.playerB); !errors.Is(err, ErrShopConfirmationNotFound) {
		t.Fatalf("another player's read: got %v, want not found", err)
	}
	if _, err := w.conf.GetConfirmation(w.ctx, w.b.OrgID, w.b.InstallationID, o.purchase, 0); !errors.Is(err, ErrShopConfirmationNotFound) {
		t.Fatalf("another tenant's read: got %v, want not found", err)
	}
	if _, err := w.conf.ConfirmReceived(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, w.playerB, ConfirmationViaSite); !errors.Is(err, ErrShopConfirmationNotFound) {
		t.Fatalf("another player's answer: got %v, want not found", err)
	}
	if _, err := w.conf.ConfirmReceived(w.ctx, w.b.OrgID, w.b.InstallationID, o.purchase, w.playerA, ConfirmationViaSite); !errors.Is(err, ErrShopConfirmationNotFound) {
		t.Fatalf("another tenant's answer: got %v, want not found", err)
	}
	if got := w.state(o); got != ConfirmationAwaitingBuyer {
		t.Fatalf("state after rejected answers = %s", got)
	}
}

func TestConfirmReceivedIsFinal(t *testing.T) {
	w := newConfirmationWorld(t)
	o := w.delivered()

	c, err := w.conf.ConfirmReceived(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, w.playerA, ConfirmationViaSite)
	must(t, err)
	if c.State != ConfirmationReceived || c.RespondedAt == nil || c.ResponseSource == nil || *c.ResponseSource != ConfirmationViaSite {
		t.Fatalf("after received: %+v", c)
	}
	_, err = w.conf.ConfirmReceived(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, w.playerA, ConfirmationViaSite)
	wantIs(t, "a second received", err, ErrShopConfirmationState)
	_, _, err = w.issue(o, "changed my mind")
	wantIs(t, "an issue after received", err, ErrShopConfirmationState)
	var tickets int
	must(t, w.db.Pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM shop_order_tickets WHERE purchase_id=$1`, o.purchase).Scan(&tickets))
	if tickets != 0 {
		t.Fatalf("a rejected issue left %d ticket(s)", tickets)
	}
	// The purchase and delivery themselves are untouched by the answer.
	if p, d := w.statuses(o); p != "FULFILLED" || d != "FULFILLED" {
		t.Fatalf("purchase/delivery = %s/%s, want FULFILLED/FULFILLED", p, d)
	}
}

func TestReportIssueOpensOneTicketAndStaffResolveIt(t *testing.T) {
	w := newConfirmationWorld(t)
	o := w.delivered()

	c, ticket, err := w.issue(o, "the bandage never appeared")
	must(t, err)
	if c.State != ConfirmationIssueReported || ticket.Status != TicketOpen || ticket.PurchaseID != o.purchase || ticket.PlayerID != w.playerA ||
		ticket.OpenedVia != ConfirmationViaDiscord || ticket.OpenedByDiscordID != "buyer-discord" || ticket.Reason != "the bandage never appeared" || ticket.Resolution != nil {
		t.Fatalf("after issue: confirmation=%+v ticket=%+v", c, ticket)
	}
	_, _, err = w.issue(o, "again")
	wantIs(t, "a second issue", err, ErrShopConfirmationState)
	_, err = w.conf.ConfirmReceived(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, w.playerA, ConfirmationViaSite)
	wantIs(t, "received after an issue", err, ErrShopConfirmationState)

	// Listing and reading are tenant-scoped; the buyer scope hides another player's ticket.
	list, err := w.conf.ListTickets(w.ctx, o.f.OrgID, o.f.InstallationID, TicketOpen, 0, 25)
	must(t, err)
	if len(list) != 1 || list[0].ID != ticket.ID {
		t.Fatalf("open tickets = %+v", list)
	}
	other, err := w.conf.ListTickets(w.ctx, w.b.OrgID, w.b.InstallationID, "", 0, 25)
	must(t, err)
	if len(other) != 0 {
		t.Fatalf("another tenant sees %d ticket(s)", len(other))
	}
	_, err = w.conf.GetTicket(w.ctx, w.b.OrgID, w.b.InstallationID, ticket.ID, 0)
	wantIs(t, "another tenant's ticket read", err, ErrShopTicketNotFound)
	_, err = w.conf.GetTicket(w.ctx, o.f.OrgID, o.f.InstallationID, ticket.ID, w.playerB)
	wantIs(t, "another player's ticket read", err, ErrShopTicketNotFound)

	// The Discord channel is recorded once.
	withChannel, err := w.conf.SetTicketChannel(w.ctx, o.f.OrgID, o.f.InstallationID, ticket.ID, "123456789012345678")
	must(t, err)
	if withChannel.DiscordChannelID == nil || *withChannel.DiscordChannelID != "123456789012345678" {
		t.Fatalf("channel = %v", withChannel.DiscordChannelID)
	}
	_, err = w.conf.SetTicketChannel(w.ctx, o.f.OrgID, o.f.InstallationID, ticket.ID, "999")
	wantIs(t, "a second channel", err, ErrShopTicketState)

	_, err = w.conf.ResolveTicket(w.ctx, w.b.OrgID, w.b.InstallationID, ticket.ID, w.b.OwnerUserID, TicketResolutionCompleted, "")
	wantIs(t, "another tenant's resolve", err, ErrShopTicketNotFound)
	done, err := w.conf.ResolveTicket(w.ctx, o.f.OrgID, o.f.InstallationID, ticket.ID, o.f.OwnerUserID, TicketResolutionCompleted, "re-delivered by hand")
	must(t, err)
	if done.Status != TicketResolved || done.Resolution == nil || *done.Resolution != TicketResolutionCompleted || done.ResolvedAt == nil ||
		done.ResolvedByUserID == nil || *done.ResolvedByUserID != o.f.OwnerUserID || done.ResolutionNote == nil || *done.ResolutionNote != "re-delivered by hand" {
		t.Fatalf("after resolve: %+v", done)
	}
	_, err = w.conf.ResolveTicket(w.ctx, o.f.OrgID, o.f.InstallationID, ticket.ID, o.f.OwnerUserID, TicketResolutionOther, "x")
	wantIs(t, "a second resolve", err, ErrShopTicketState)
	// Resolving a ticket moves no points and changes no order.
	if p, d := w.statuses(o); p != "FULFILLED" || d != "FULFILLED" {
		t.Fatalf("purchase/delivery after resolve = %s/%s", p, d)
	}
	if got := w.state(o); got != ConfirmationIssueReported {
		t.Fatalf("confirmation after resolve = %s", got)
	}
}

func TestAutoCompleteAfterTheDeadlineStillAllowsAnIssue(t *testing.T) {
	w := newConfirmationWorld(t)
	due, fresh, answered := w.delivered(), w.delivered(), w.delivered()
	w.expire(due)
	w.expire(answered)
	_, err := w.conf.ConfirmReceived(w.ctx, answered.f.OrgID, answered.f.InstallationID, answered.purchase, w.playerA, ConfirmationViaSite)
	must(t, err)

	now := time.Now()
	closed, err := w.conf.AutoCompleteDue(w.ctx, now, 100)
	must(t, err)
	found := false
	for _, c := range closed {
		switch c.PurchaseID {
		case due.purchase:
			found = true
			if c.State != ConfirmationAutoCompleted || c.ResponseSource == nil || *c.ResponseSource != "AUTO" || c.RespondedAt == nil {
				t.Fatalf("auto-completed row: %+v", c)
			}
		case fresh.purchase, answered.purchase:
			t.Fatalf("the sweep closed purchase %d, which was not due and unanswered", c.PurchaseID)
		}
	}
	if !found {
		t.Fatal("the overdue confirmation was not auto-completed")
	}
	if got := w.state(fresh); got != ConfirmationAwaitingBuyer {
		t.Fatalf("a confirmation inside its window = %s", got)
	}
	if got := w.state(answered); got != ConfirmationReceived {
		t.Fatalf("an answered confirmation = %s", got)
	}
	again, err := w.conf.AutoCompleteDue(w.ctx, now, 100)
	must(t, err)
	for _, c := range again {
		if c.PurchaseID == due.purchase {
			t.Fatal("the same confirmation was auto-completed twice")
		}
	}

	// Silence is not consent: "received" is no longer offered, but an issue still opens a ticket.
	_, err = w.conf.ConfirmReceived(w.ctx, due.f.OrgID, due.f.InstallationID, due.purchase, w.playerA, ConfirmationViaSite)
	wantIs(t, "received after auto-complete", err, ErrShopConfirmationState)
	c, ticket, err := w.issue(due, "found out late that it never arrived")
	must(t, err)
	if c.State != ConfirmationIssueReported || ticket.Status != TicketOpen {
		t.Fatalf("issue after auto-complete: confirmation=%+v ticket=%+v", c, ticket)
	}
}

func TestRefundVoidsOnlyAnUnansweredConfirmation(t *testing.T) {
	w := newConfirmationWorld(t)

	waiting := w.delivered()
	must(t, w.refund(waiting))
	c, err := w.conf.GetConfirmation(w.ctx, waiting.f.OrgID, waiting.f.InstallationID, waiting.purchase, 0)
	must(t, err)
	if c.State != ConfirmationVoid || c.ResponseSource == nil || *c.ResponseSource != "SYSTEM" || c.RespondedAt == nil {
		t.Fatalf("after refund while awaiting: %+v", c)
	}
	_, err = w.conf.ConfirmReceived(w.ctx, waiting.f.OrgID, waiting.f.InstallationID, waiting.purchase, w.playerA, ConfirmationViaSite)
	wantIs(t, "received on a refunded order", err, ErrShopConfirmationState)
	_, _, err = w.issue(waiting, "refunded already")
	wantIs(t, "issue on a refunded order", err, ErrShopConfirmationState)

	// A refund that follows an issue keeps the buyer's answer on record.
	reported := w.delivered()
	_, _, err = w.issue(reported, "never arrived")
	must(t, err)
	must(t, w.refund(reported))
	if got := w.state(reported); got != ConfirmationIssueReported {
		t.Fatalf("after refund of a reported order = %s, want the answer kept", got)
	}

	// An order refunded before delivery never gets a confirmation.
	undelivered := w.order(w.a)
	must(t, w.refund(undelivered))
	_, err = w.conf.GetConfirmation(w.ctx, undelivered.f.OrgID, undelivered.f.InstallationID, undelivered.purchase, 0)
	wantIs(t, "an order refunded before delivery", err, ErrShopConfirmationNotFound)
}

func TestConcurrentAnswersHaveExactlyOneWinner(t *testing.T) {
	w := newConfirmationWorld(t)
	for round := 0; round < 5; round++ {
		o := w.delivered()
		w.expire(o) // the deadline sweep races the buyer too
		errs := make([]error, 6)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				switch i % 3 {
				case 0:
					_, errs[i] = w.conf.ConfirmReceived(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, w.playerA, ConfirmationViaSite)
				case 1:
					_, _, errs[i] = w.issue(o, "racing")
				default:
					_, errs[i] = w.conf.AutoCompleteDue(w.ctx, time.Now(), 500)
				}
			}(i)
		}
		wg.Wait()

		received := 0
		for i, err := range errs {
			switch {
			case err == nil && i%3 == 0:
				received++
			case err != nil && !errors.Is(err, ErrShopConfirmationState):
				t.Fatalf("round %d call %d: unexpected error %v", round, i, err)
			}
		}
		var tickets int
		must(t, w.db.Pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM shop_order_tickets WHERE purchase_id=$1`, o.purchase).Scan(&tickets))
		state := w.state(o)
		switch {
		case received > 1, tickets > 1:
			t.Fatalf("round %d: %d received answers and %d tickets", round, received, tickets)
		case received == 1 && (tickets != 0 || state != ConfirmationReceived):
			t.Fatalf("round %d: received won but state=%s tickets=%d", round, state, tickets)
		case received == 0 && (tickets != 1 || state != ConfirmationIssueReported):
			// With "received" lost, an issue always lands: directly, or after the sweep.
			t.Fatalf("round %d: received lost but state=%s tickets=%d", round, state, tickets)
		}
	}
}

func TestConfirmationConstraints(t *testing.T) {
	w := newConfirmationWorld(t)
	o := w.delivered()
	bad := []struct{ what, sql string }{
		{"an unknown state", `UPDATE shop_order_confirmations SET state='DONE', responded_at=NOW() WHERE purchase_id=$1`},
		{"an answer without a time", `UPDATE shop_order_confirmations SET state='RECEIVED' WHERE purchase_id=$1`},
		{"a time without an answer", `UPDATE shop_order_confirmations SET responded_at=NOW() WHERE purchase_id=$1`},
		{"an unknown source", `UPDATE shop_order_confirmations SET state='RECEIVED', responded_at=NOW(), response_source='EMAIL' WHERE purchase_id=$1`},
		{"a deadline before delivery", `UPDATE shop_order_confirmations SET deadline_at = delivered_at WHERE purchase_id=$1`},
		{"an empty ticket reason", `INSERT INTO shop_order_tickets(organization_id, installation_id, purchase_id, player_id, opened_by_discord_id, opened_via, reason)
 SELECT organization_id, installation_id, id, player_id, 'd', 'SITE', '' FROM shop_purchases WHERE id=$1`},
		{"a resolved ticket without a resolution", `INSERT INTO shop_order_tickets(organization_id, installation_id, purchase_id, player_id, opened_by_discord_id, opened_via, reason, status, resolved_at)
 SELECT organization_id, installation_id, id, player_id, 'd', 'SITE', 'x', 'RESOLVED', NOW() FROM shop_purchases WHERE id=$1`},
	}
	for _, b := range bad {
		if _, err := w.db.Pool.Exec(w.ctx, b.sql, o.purchase); err == nil {
			t.Fatalf("%s was accepted by the database", b.what)
		}
	}
}
