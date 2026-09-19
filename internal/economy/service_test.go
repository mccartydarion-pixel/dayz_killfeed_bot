package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakeStore is an in-memory Store with a real balance/ledger so the service's
// behaviour (validation order, idempotency, events after commit) is exercised
// without a database. It applies the same rules as the repository: debits never
// go below zero, a reference is idempotent per (guild, player, type).
type fakeStore struct {
	players map[[2]int64]string
	servers map[int64]struct {
		guild int64
		org   *int64
	}
	balance map[[2]int64]int64
	ledger  []repository.LedgerEntry
	nextID  int64
	calls   int // Credit+Debit calls that reached the store
	failErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		players: map[[2]int64]string{{1, 10}: "Alice", {1, 11}: "Bob", {2, 20}: "OtherGuildPlayer"},
		servers: map[int64]struct {
			guild int64
			org   *int64
		}{100: {1, nil}, 101: {1, ptr(7)}, 200: {2, nil}},
		balance: map[[2]int64]int64{},
	}
}

func ptr(v int64) *int64 { return &v }

func (f *fakeStore) find(p repository.LedgerParams) (repository.LedgerEntry, bool) {
	if p.ReferenceID == "" {
		return repository.LedgerEntry{}, false
	}
	for _, e := range f.ledger {
		if e.GuildID == p.GuildID && e.PlayerID == p.PlayerID && e.Type == p.Type && e.ReferenceID == p.ReferenceID {
			return e, true
		}
	}
	return repository.LedgerEntry{}, false
}

func (f *fakeStore) apply(p repository.LedgerParams, debit bool) (repository.LedgerEntry, error) {
	f.calls++
	if f.failErr != nil {
		return repository.LedgerEntry{}, f.failErr
	}
	if e, ok := f.find(p); ok {
		e.Duplicate = true
		return e, nil
	}
	key := [2]int64{p.GuildID, p.PlayerID}
	amount := p.Amount
	if debit {
		if f.balance[key] < amount {
			return repository.LedgerEntry{}, repository.ErrInsufficientFunds
		}
		amount = -amount
	}
	f.balance[key] += amount
	f.nextID++
	e := repository.LedgerEntry{ID: f.nextID, GuildID: p.GuildID, PlayerID: p.PlayerID, ServerID: p.ServerID, Type: p.Type, Amount: amount, BalanceAfter: f.balance[key], ReferenceID: p.ReferenceID, Description: p.Description, CreatedBy: p.CreatedBy, CreatedAt: time.Now()}
	f.ledger = append(f.ledger, e)
	return e, nil
}

func (f *fakeStore) Credit(_ context.Context, p repository.LedgerParams) (repository.LedgerEntry, error) {
	return f.apply(p, false)
}
func (f *fakeStore) Debit(_ context.Context, p repository.LedgerParams) (repository.LedgerEntry, error) {
	return f.apply(p, true)
}
func (f *fakeStore) Balance(_ context.Context, g, p int64) (int64, error) {
	return f.balance[[2]int64{g, p}], nil
}
func (f *fakeStore) History(_ context.Context, g, p int64, limit int, before int64) ([]repository.LedgerEntry, int64, error) {
	if limit <= 0 {
		limit = repository.DefaultHistoryLimit
	}
	if limit > repository.MaxHistoryLimit {
		limit = repository.MaxHistoryLimit
	}
	var out []repository.LedgerEntry
	for i := len(f.ledger) - 1; i >= 0; i-- {
		e := f.ledger[i]
		if e.GuildID == g && e.PlayerID == p && (before == 0 || e.ID < before) {
			out = append(out, e)
		}
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}
func (f *fakeStore) PlayerName(_ context.Context, g, p int64) (string, bool, error) {
	n, ok := f.players[[2]int64{g, p}]
	return n, ok, nil
}
func (f *fakeStore) ServerScope(_ context.Context, id int64) (int64, *int64, bool, error) {
	s, ok := f.servers[id]
	return s.guild, s.org, ok, nil
}

type recordingNotifier struct{ events []Event }

func (r *recordingNotifier) Notify(e Event) { r.events = append(r.events, e) }

type panickingNotifier struct{}

func (panickingNotifier) Notify(Event) { panic("discord notifier bug") }

func newService() (*Service, *fakeStore, *recordingNotifier) {
	store, n := newFakeStore(), &recordingNotifier{}
	return NewService(store, n), store, n
}

var ctx = context.Background()

// --- A/B/C/D: balance, credit, debit, insufficient ------------------------------------

func TestBalanceOfValidPlayerWithNoTransactionsIsZero(t *testing.T) {
	svc, _, _ := newService()
	if b, err := svc.Balance(ctx, 1, 10); err != nil || b != 0 {
		t.Fatalf("expected 0, got %d err=%v", b, err)
	}
	if _, err := svc.Balance(ctx, 1, 999); !errors.Is(err, ErrPlayerNotFound) {
		t.Fatalf("an unknown player must be ErrPlayerNotFound, got %v", err)
	}
	if _, err := svc.Balance(ctx, 1, 20); !errors.Is(err, ErrPlayerNotFound) {
		t.Fatalf("another guild's player must not resolve, got %v", err)
	}
}

func TestAdminCreditAndDebitMoveTheBalanceAndReportCommittedState(t *testing.T) {
	svc, store, n := newService()
	res, err := svc.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 50000, Actor: "admin-1", Description: "prize"})
	if err != nil || res.Balance != 50000 || res.TransactionID == 0 || res.Duplicate {
		t.Fatalf("credit: %+v %v", res, err)
	}
	res, err = svc.AdminDebit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 20000, Actor: "admin-1"})
	if err != nil || res.Balance != 30000 {
		t.Fatalf("debit: %+v %v", res, err)
	}
	if b, _ := svc.Balance(ctx, 1, 10); b != 30000 {
		t.Fatalf("balance %d", b)
	}
	if len(n.events) != 2 {
		t.Fatalf("expected 2 events, got %+v", n.events)
	}
	if e := n.events[0]; e.Type != TypeAdminCredit || !e.Credit || e.Amount != 50000 || e.PlayerName != "Alice" || e.BalanceAfter != 50000 {
		t.Fatalf("unexpected credit event: %+v", e)
	}
	if e := n.events[1]; e.Type != TypeAdminDebit || e.Credit || e.Amount != 20000 || e.BalanceAfter != 30000 {
		t.Fatalf("unexpected debit event: %+v", e)
	}
	// The audit trail is on the ledger row, not in the public event.
	if store.ledger[0].CreatedBy != "admin-1" || store.ledger[0].Description != "prize" {
		t.Fatalf("the actor and reason must be recorded: %+v", store.ledger[0])
	}
}

func TestDebitNeverGoesNegative(t *testing.T) {
	svc, store, n := newService()
	if _, err := svc.AdminDebit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 1, Actor: "a"}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("expected insufficient funds on an empty balance, got %v", err)
	}
	svc.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 100, Actor: "a"})
	if _, err := svc.AdminDebit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 101, Actor: "a"}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("expected insufficient funds, got %v", err)
	}
	if b, _ := svc.Balance(ctx, 1, 10); b != 100 || len(store.ledger) != 1 || len(n.events) != 1 {
		t.Fatalf("a rejected debit must change and report nothing: balance=%d ledger=%d events=%d", b, len(store.ledger), len(n.events))
	}
	// Spending the exact balance is allowed.
	if res, err := svc.AdminDebit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 100, Actor: "a"}); err != nil || res.Balance != 0 {
		t.Fatalf("spending the exact balance must work: %+v %v", res, err)
	}
}

// --- validation ------------------------------------------------------------------------------

func TestAmountBounds(t *testing.T) {
	svc, store, n := newService()
	for _, amount := range []int64{0, -5, MaxAmount + 1} {
		if _, err := svc.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: amount, Actor: "a"}); !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("credit %d: expected ErrInvalidAmount, got %v", amount, err)
		}
		if _, err := svc.AdminDebit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: amount, Actor: "a"}); !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("debit %d: expected ErrInvalidAmount, got %v", amount, err)
		}
	}
	if store.calls != 0 || len(n.events) != 0 {
		t.Fatal("an invalid amount must never reach the store")
	}
	// The maximum itself is fine (64-bit ledger: far above the bounty 32-bit cap).
	if res, err := svc.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: MaxAmount, Actor: "a"}); err != nil || res.Balance != MaxAmount {
		t.Fatalf("the maximum must be accepted: %+v %v", res, err)
	}
	if MaxAmount <= 1<<31 {
		t.Fatal("economy amounts must exceed the 32-bit range")
	}
}

// N: admin adjustments must record who did them.
func TestAdminAdjustmentsRequireAnActor(t *testing.T) {
	svc, store, _ := newService()
	for _, actor := range []string{"", "   "} {
		if _, err := svc.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 5, Actor: actor}); !errors.Is(err, ErrActorRequired) {
			t.Fatalf("credit without an actor: %v", err)
		}
		if _, err := svc.AdminDebit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 5, Actor: actor}); !errors.Is(err, ErrActorRequired) {
			t.Fatalf("debit without an actor: %v", err)
		}
	}
	if store.calls != 0 {
		t.Fatal("an unaudited adjustment must never reach the store")
	}
}

// Only the types the service allows can be moved through the generic API.
func TestTypeRestrictions(t *testing.T) {
	svc, store, _ := newService()
	for _, ty := range []string{TypeBountyClaim, "SHOP_PURCHASE", "CASINO_WIN", "", "EVENT_FIRST_PLACE"} {
		if _, err := svc.Credit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 5, Type: ty, Actor: "a"}); !errors.Is(err, ErrTypeNotAllowed) {
			t.Fatalf("credit type %q must be rejected, got %v", ty, err)
		}
	}
	for _, ty := range []string{TypeAdminCredit, TypeBountyClaim, "SHOP_PURCHASE", "CASINO_LOSS", ""} {
		if _, err := svc.Debit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 5, Type: ty, Actor: "a"}); !errors.Is(err, ErrTypeNotAllowed) {
			t.Fatalf("debit type %q must be rejected, got %v", ty, err)
		}
	}
	if store.calls != 0 {
		t.Fatal("a rejected type must never reach the store")
	}
	// SYSTEM_REWARD is creditable without an admin actor (recorded as SYSTEM).
	if _, err := svc.Credit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 5, Type: TypeSystemReward}); err != nil {
		t.Fatalf("a system reward must be creditable: %v", err)
	}
	if store.ledger[0].CreatedBy != "SYSTEM" {
		t.Fatalf("expected the SYSTEM actor, got %q", store.ledger[0].CreatedBy)
	}
}

// O: cross-tenant isolation.
func TestTenantIsolation(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
		want error
	}{
		{"player of another guild", Request{GuildID: 1, PlayerID: 20, Amount: 5, Actor: "a"}, ErrPlayerNotFound},
		{"unknown player", Request{GuildID: 1, PlayerID: 999, Amount: 5, Actor: "a"}, ErrPlayerNotFound},
		{"server of another guild", Request{GuildID: 1, ServerID: 200, PlayerID: 10, Amount: 5, Actor: "a"}, ErrServerNotInGuild},
		{"unknown server", Request{GuildID: 1, ServerID: 999, PlayerID: 10, Amount: 5, Actor: "a"}, ErrServerNotInGuild},
		{"server claimed by a different organization", Request{GuildID: 1, ServerID: 101, OrganizationID: 8, PlayerID: 10, Amount: 5, Actor: "a"}, ErrForbiddenTenant},
		{"organization caller without a server", Request{GuildID: 1, OrganizationID: 7, PlayerID: 10, Amount: 5, Actor: "a"}, ErrServerRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, n := newService()
			if _, err := svc.AdminCredit(ctx, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if store.calls != 0 || len(n.events) != 0 {
				t.Fatal("a rejected operation must store and report nothing")
			}
		})
	}
	svc, _, _ := newService()
	if _, err := svc.AdminCredit(ctx, Request{GuildID: 1, ServerID: 101, OrganizationID: 7, PlayerID: 10, Amount: 5, Actor: "a"}); err != nil {
		t.Fatalf("the owning organization must be allowed: %v", err)
	}
	if _, err := svc.Balance(ctx, 2, 10); !errors.Is(err, ErrPlayerNotFound) {
		t.Fatalf("guild 2 must not see guild 1's player: %v", err)
	}
	if _, err := svc.History(ctx, 2, 10, 10, 0); !errors.Is(err, ErrPlayerNotFound) {
		t.Fatalf("guild 2 must not read guild 1's history: %v", err)
	}
}

// --- H: idempotency ------------------------------------------------------------------------------

func TestIdempotentReferenceAppliesOnce(t *testing.T) {
	svc, store, n := newService()
	req := Request{GuildID: 1, PlayerID: 10, Amount: 500, Actor: "a", ReferenceID: "admin:grant-42"}
	first, err := svc.AdminCredit(ctx, req)
	if err != nil || first.Duplicate {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := svc.AdminCredit(ctx, req)
	if err != nil || !second.Duplicate || second.TransactionID != first.TransactionID || second.Balance != first.Balance {
		t.Fatalf("a replay must return the original entry: %+v vs %+v (%v)", second, first, err)
	}
	if b, _ := svc.Balance(ctx, 1, 10); b != 500 || len(store.ledger) != 1 {
		t.Fatalf("a replay must not credit twice: balance=%d ledger=%d", b, len(store.ledger))
	}
	if len(n.events) != 1 {
		t.Fatalf("a replay must not be announced again, got %d events", len(n.events))
	}
	// Different type or different player is a different transaction.
	if _, err := svc.AdminDebit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 100, Actor: "a", ReferenceID: "admin:grant-42"}); err != nil {
		t.Fatalf("the same reference under another type is a different transaction: %v", err)
	}
	if res, _ := svc.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 11, Amount: 500, Actor: "a", ReferenceID: "admin:grant-42"}); res.Duplicate {
		t.Fatal("the same reference for another player is a different transaction")
	}
}

// --- G: history ---------------------------------------------------------------------------------

func TestHistoryIsNewestFirstBoundedAndCursorPaged(t *testing.T) {
	svc, _, _ := newService()
	for i := 1; i <= 60; i++ {
		svc.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: int64(i), Actor: "secret-admin-id", Description: "reason"})
	}
	page, err := svc.History(ctx, 1, 10, 0, 0)
	if err != nil || len(page.Items) != repository.DefaultHistoryLimit || page.NextCursor == 0 {
		t.Fatalf("default page: %d items next=%d err=%v", len(page.Items), page.NextCursor, err)
	}
	if page.Items[0].Amount != 60 || page.Items[1].Amount != 59 {
		t.Fatalf("history must be newest first, got %+v", page.Items[:2])
	}
	big, _ := svc.History(ctx, 1, 10, 100000, 0)
	if len(big.Items) != repository.MaxHistoryLimit {
		t.Fatalf("history must be hard-capped at %d, got %d", repository.MaxHistoryLimit, len(big.Items))
	}
	next, _ := svc.History(ctx, 1, 10, 10, page.NextCursor)
	if next.Items[0].Amount != 50 {
		t.Fatalf("the cursor must continue after the first page, got %+v", next.Items[0])
	}
	for _, it := range page.Items {
		if it.Label != "Admin credit" || it.Description != "reason" || it.BalanceAfter == 0 {
			t.Fatalf("unexpected item: %+v", it)
		}
	}
	if svcHistoryLeaksActor(page) {
		t.Fatal("history must never expose the acting admin")
	}
}

func svcHistoryLeaksActor(p Page) bool {
	for _, it := range p.Items {
		if strings.Contains(it.Description, "secret-admin-id") || strings.Contains(it.Label, "secret-admin-id") {
			return true
		}
	}
	return false
}

func TestTypeLabels(t *testing.T) {
	for in, want := range map[string]string{
		TypeBountyClaim: "Bounty reward", TypeAdminCredit: "Admin credit", TypeAdminDebit: "Admin debit",
		TypeSystemReward: "Reward", "EVENT_FIRST_PLACE": "Event prize", "EVENT_PLACEMENT": "Event prize", "SOMETHING": "Adjustment",
	} {
		if got := TypeLabel(in); got != want {
			t.Fatalf("TypeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- M: Discord failure -------------------------------------------------------------------------

func TestNotifierFailureNeverAffectsACommittedTransaction(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, panickingNotifier{})
	res, err := svc.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 700, Actor: "a"})
	if err != nil || res.Balance != 700 {
		t.Fatalf("a panicking notifier must not affect the result: %+v %v", res, err)
	}
	if b, _ := svc.Balance(ctx, 1, 10); b != 700 || len(store.ledger) != 1 {
		t.Fatal("the transaction must stay committed")
	}
	// No notifier at all (no route / no Discord) is equally fine.
	svc2 := NewService(newFakeStore(), nil)
	if _, err := svc2.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 1, Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	var nilSvc *Service
	nilSvc.SetNotifier(nil)
}

// A store failure reports nothing (nothing committed).
func TestStoreFailureReportsNothing(t *testing.T) {
	svc, store, n := newService()
	store.failErr = errors.New("db down")
	if _, err := svc.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 5, Actor: "a"}); err == nil {
		t.Fatal("expected the store error")
	}
	if len(n.events) != 0 {
		t.Fatal("nothing committed, so nothing may be announced")
	}
}

// The reason text is sanitised and bounded; it never reaches the public event.
func TestReasonIsSanitisedAndNotPublished(t *testing.T) {
	svc, store, n := newService()
	long := strings.Repeat("x", 500)
	svc.AdminCredit(ctx, Request{GuildID: 1, PlayerID: 10, Amount: 5, Actor: "a", Description: "  line1\nline2\x00\x07 " + long})
	got := store.ledger[0].Description
	if strings.ContainsAny(got, "\n\x00\x07") || len([]rune(got)) > MaxDescriptionLen {
		t.Fatalf("the reason must be sanitised and bounded, got %q", got)
	}
	if n.events[0].PlayerName != "Alice" || n.events[0].Type != TypeAdminCredit {
		t.Fatalf("unexpected event: %+v", n.events[0])
	}
}
