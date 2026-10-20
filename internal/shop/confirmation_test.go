package shop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeConfirmationStore struct {
	calls       []string
	player      int64
	via         string
	report      repository.ShopIssueReport
	resolution  string
	note        string
	actor       int64
	status      string
	sweepNow    []time.Time
	sweepBatch  []int
	err         error
	ticketScope int64
}

func (f *fakeConfirmationStore) GetConfirmation(_ context.Context, _, _, purchaseID, playerID int64) (repository.ShopOrderConfirmation, error) {
	f.calls, f.player = append(f.calls, "get"), playerID
	return repository.ShopOrderConfirmation{PurchaseID: purchaseID, PlayerID: playerID}, f.err
}

func (f *fakeConfirmationStore) ConfirmReceived(_ context.Context, _, _, purchaseID, playerID int64, via string) (repository.ShopOrderConfirmation, error) {
	f.calls, f.player, f.via = append(f.calls, "received"), playerID, via
	return repository.ShopOrderConfirmation{PurchaseID: purchaseID, State: repository.ConfirmationReceived}, f.err
}

func (f *fakeConfirmationStore) ReportIssue(_ context.Context, in repository.ShopIssueReport) (repository.ShopOrderConfirmation, repository.ShopOrderTicket, error) {
	f.calls, f.report = append(f.calls, "issue"), in
	return repository.ShopOrderConfirmation{State: repository.ConfirmationIssueReported}, repository.ShopOrderTicket{ID: 9, Reason: in.Reason}, f.err
}

func (f *fakeConfirmationStore) AutoCompleteDue(_ context.Context, now time.Time, limit int) ([]repository.ShopOrderConfirmation, error) {
	f.sweepNow = append(f.sweepNow, now)
	if len(f.sweepBatch) == 0 {
		return nil, f.err
	}
	n := f.sweepBatch[0]
	f.sweepBatch = f.sweepBatch[1:]
	if n > limit {
		n = limit
	}
	return make([]repository.ShopOrderConfirmation, n), f.err
}

func (f *fakeConfirmationStore) ListTickets(_ context.Context, _, _ int64, status string, _ int64, _ int) ([]repository.ShopOrderTicket, error) {
	f.calls, f.status = append(f.calls, "list"), status
	return nil, f.err
}

func (f *fakeConfirmationStore) GetTicket(_ context.Context, _, _, id, playerID int64) (repository.ShopOrderTicket, error) {
	f.calls, f.ticketScope = append(f.calls, "ticket"), playerID
	return repository.ShopOrderTicket{ID: id}, f.err
}

func (f *fakeConfirmationStore) ResolveTicket(_ context.Context, _, _, id, actor int64, resolution, note string) (repository.ShopOrderTicket, error) {
	f.calls, f.actor, f.resolution, f.note = append(f.calls, "resolve"), actor, resolution, note
	return repository.ShopOrderTicket{ID: id, Status: repository.TicketResolved}, f.err
}

func newConfirmations() (*Confirmations, *fakeConfirmationStore, *fakeIdentity) {
	st, id := &fakeConfirmationStore{}, &fakeIdentity{account: economy.Account{AccountID: 77}}
	return NewConfirmations(st, id), st, id
}

func TestConfirmationAnswersAreScopedToTheBuyersOwnPlayer(t *testing.T) {
	c, st, id := newConfirmations()
	ctx := context.Background()

	if _, err := c.Mine(ctx, scope, "discord-1", 5); err != nil || st.player != 77 {
		t.Fatalf("Mine: err=%v player=%d, want the verified player 77", err, st.player)
	}
	got, err := c.Received(ctx, scope, "discord-1", 5, repository.ConfirmationViaDiscord)
	if err != nil || got.State != repository.ConfirmationReceived || st.player != 77 || st.via != repository.ConfirmationViaDiscord || id.asked != "discord-1" {
		t.Fatalf("Received: err=%v state=%s player=%d via=%s asked=%s", err, got.State, st.player, st.via, id.asked)
	}
	_, ticket, err := c.ReportIssue(ctx, scope, "discord-1", 5, repository.ConfirmationViaSite, "  item was\tnot there  ")
	if err != nil || ticket.ID != 9 {
		t.Fatalf("ReportIssue: err=%v ticket=%+v", err, ticket)
	}
	want := repository.ShopIssueReport{OrganizationID: 1, InstallationID: 2, PurchaseID: 5, PlayerID: 77, DiscordID: "discord-1", Via: repository.ConfirmationViaSite, Reason: st.report.Reason}
	if st.report != want || strings.TrimSpace(st.report.Reason) != st.report.Reason || st.report.Reason == "" {
		t.Fatalf("ReportIssue passed %+v", st.report)
	}
}

func TestConfirmationRejectsBeforeTouchingTheStore(t *testing.T) {
	ctx := context.Background()
	suspended := scope
	suspended.Status = "SUSPENDED"
	notLinked := errors.New("not linked")

	cases := []struct {
		name string
		run  func(c *Confirmations, id *fakeIdentity) error
		is   error
	}{
		{"received from an unknown surface", func(c *Confirmations, _ *fakeIdentity) error {
			_, err := c.Received(ctx, scope, "d", 5, "EMAIL")
			return err
		}, ErrInvalidVia},
		{"received on a suspended installation", func(c *Confirmations, _ *fakeIdentity) error {
			_, err := c.Received(ctx, suspended, "d", 5, repository.ConfirmationViaSite)
			return err
		}, economy.ErrSuspended},
		{"received without a verified player", func(c *Confirmations, id *fakeIdentity) error {
			id.err = notLinked
			_, err := c.Received(ctx, scope, "d", 5, repository.ConfirmationViaSite)
			return err
		}, notLinked},
		{"issue from an unknown surface", func(c *Confirmations, _ *fakeIdentity) error {
			_, _, err := c.ReportIssue(ctx, scope, "d", 5, "", "broken")
			return err
		}, ErrInvalidVia},
		{"issue without a reason", func(c *Confirmations, _ *fakeIdentity) error {
			_, _, err := c.ReportIssue(ctx, scope, "d", 5, repository.ConfirmationViaSite, " \n\t ")
			return err
		}, ErrReasonRequired},
		{"issue on a suspended installation", func(c *Confirmations, _ *fakeIdentity) error {
			_, _, err := c.ReportIssue(ctx, suspended, "d", 5, repository.ConfirmationViaSite, "broken")
			return err
		}, economy.ErrSuspended},
		{"issue without a verified player", func(c *Confirmations, id *fakeIdentity) error {
			id.err = notLinked
			_, _, err := c.ReportIssue(ctx, scope, "d", 5, repository.ConfirmationViaSite, "broken")
			return err
		}, notLinked},
		{"unknown resolution", func(c *Confirmations, _ *fakeIdentity) error {
			_, err := c.ResolveTicket(ctx, scope, 1, 9, "CLOSED", "")
			return err
		}, ErrInvalidResolution},
		{"OTHER without a note", func(c *Confirmations, _ *fakeIdentity) error {
			_, err := c.ResolveTicket(ctx, scope, 1, 9, repository.TicketResolutionOther, "  ")
			return err
		}, ErrReasonRequired},
		{"resolve on a suspended installation", func(c *Confirmations, _ *fakeIdentity) error {
			_, err := c.ResolveTicket(ctx, suspended, 1, 9, repository.TicketResolutionCompleted, "")
			return err
		}, economy.ErrSuspended},
		{"unknown ticket filter", func(c *Confirmations, _ *fakeIdentity) error {
			_, err := c.Tickets(ctx, scope, "CLOSED", 0, 25)
			return err
		}, ErrInvalidTicketStatus},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st, id := newConfirmations()
			if err := tc.run(c, id); !errors.Is(err, tc.is) {
				t.Fatalf("got %v, want %v", err, tc.is)
			}
			if len(st.calls) != 0 {
				t.Fatalf("the store was called: %v", st.calls)
			}
		})
	}
}

func TestConfirmationTextLimits(t *testing.T) {
	c, st, _ := newConfirmations()
	ctx := context.Background()
	var ve *ValidationError

	if _, _, err := c.ReportIssue(ctx, scope, "d", 5, repository.ConfirmationViaSite, strings.Repeat("é", MaxIssueReasonRunes+1)); !errors.As(err, &ve) {
		t.Fatalf("an over-long reason: got %v, want a validation error", err)
	}
	if _, err := c.ResolveTicket(ctx, scope, 1, 9, repository.TicketResolutionCompleted, strings.Repeat("x", MaxIssueReasonRunes+1)); !errors.As(err, &ve) {
		t.Fatalf("an over-long note: got %v, want a validation error", err)
	}
	if len(st.calls) != 0 {
		t.Fatalf("the store was called: %v", st.calls)
	}
	// Exactly the limit, counted in characters rather than bytes, is accepted.
	if _, _, err := c.ReportIssue(ctx, scope, "d", 5, repository.ConfirmationViaSite, strings.Repeat("é", MaxIssueReasonRunes)); err != nil {
		t.Fatalf("a reason at the limit: %v", err)
	}
}

func TestStaffReadsAndResolutionAreNotBuyerScoped(t *testing.T) {
	c, st, id := newConfirmations()
	ctx := context.Background()

	if _, err := c.AdminConfirmation(ctx, scope, 5); err != nil || st.player != 0 {
		t.Fatalf("AdminConfirmation: err=%v player=%d, want the unscoped read", err, st.player)
	}
	if _, err := c.Ticket(ctx, scope, 9); err != nil || st.ticketScope != 0 {
		t.Fatalf("Ticket: err=%v player=%d, want the unscoped read", err, st.ticketScope)
	}
	for _, status := range []string{"", repository.TicketOpen, repository.TicketResolved} {
		if _, err := c.Tickets(ctx, scope, status, 0, 25); err != nil || st.status != status {
			t.Fatalf("Tickets(%q): err=%v passed %q", status, err, st.status)
		}
	}
	got, err := c.ResolveTicket(ctx, scope, 42, 9, repository.TicketResolutionRefunded, " refunded by hand ")
	if err != nil || got.Status != repository.TicketResolved || st.actor != 42 || st.resolution != repository.TicketResolutionRefunded || st.note != "refunded by hand" {
		t.Fatalf("ResolveTicket: err=%v ticket=%+v actor=%d resolution=%s note=%q", err, got, st.actor, st.resolution, st.note)
	}
	if id.asked != "" {
		t.Fatalf("a staff action resolved a player identity (%q)", id.asked)
	}
}

func TestSweepDueDrainsInBatchesWithTheServiceClock(t *testing.T) {
	c, st, _ := newConfirmations()
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	c.SetClock(func() time.Time { return at })

	st.sweepBatch = []int{100, 100, 7}
	n, err := c.SweepDue(context.Background())
	if err != nil || n != 207 || len(st.sweepNow) != 3 {
		t.Fatalf("SweepDue: n=%d err=%v calls=%d, want 207 over 3 calls", n, err, len(st.sweepNow))
	}
	for _, now := range st.sweepNow {
		if !now.Equal(at) {
			t.Fatalf("swept at %v, want the service clock %v", now, at)
		}
	}

	st.sweepNow, st.sweepBatch, st.err = nil, []int{100}, errors.New("db down")
	if n, err := c.SweepDue(context.Background()); err == nil || n != 0 || len(st.sweepNow) != 1 {
		t.Fatalf("SweepDue on failure: n=%d err=%v calls=%d, want it to stop at the first error", n, err, len(st.sweepNow))
	}
}
