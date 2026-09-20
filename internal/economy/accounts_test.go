package economy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakeAccounts is an in-memory AccountStore over the fakeStore's ledger (service_test.go), so the
// account layer's rules run without a database: validation order, scoping, privacy, idempotency.
type fakeAccounts struct {
	ledger  *fakeStore
	scopes  map[[2]int64]repository.EconomyScope // {org, installation}
	links   map[string]int64                     // "guild/discord" -> player (VERIFIED only)
	reads   int
	lastFil repository.TransactionFilter
}

func newFakeAccounts(l *fakeStore) *fakeAccounts {
	return &fakeAccounts{
		ledger: l,
		scopes: map[[2]int64]repository.EconomyScope{
			{7, 70}: {OrganizationID: 7, InstallationID: 70, GuildID: 1, ServerID: 101, Status: "READY"},
			{8, 80}: {OrganizationID: 8, InstallationID: 80, GuildID: 2, ServerID: 200, Status: "READY"},
		},
		links: map[string]int64{"1/discord-alice": 10},
	}
}

func (f *fakeAccounts) InstallationScope(_ context.Context, org, inst int64) (repository.EconomyScope, bool, error) {
	s, ok := f.scopes[[2]int64{org, inst}]
	return s, ok, nil
}
func (f *fakeAccounts) VerifiedPlayerID(_ context.Context, guild int64, discord string) (int64, bool, error) {
	id, ok := f.links[strings.Join([]string{string(rune('0' + guild)), discord}, "/")]
	return id, ok, nil
}
func (f *fakeAccounts) Account(_ context.Context, guild, player int64) (repository.EconomyAccountRow, bool, error) {
	name, ok := f.ledger.players[[2]int64{guild, player}]
	if !ok {
		return repository.EconomyAccountRow{}, false, nil
	}
	return repository.EconomyAccountRow{PlayerID: player, PlayerName: name, Balance: f.ledger.balance[[2]int64{guild, player}]}, true, nil
}
func (f *fakeAccounts) SearchAccounts(_ context.Context, guild int64, q string, limit int) ([]repository.EconomyAccountRow, error) {
	var out []repository.EconomyAccountRow
	for k, name := range f.ledger.players {
		if k[0] == guild && strings.Contains(strings.ToLower(name), strings.ToLower(q)) {
			out = append(out, repository.EconomyAccountRow{PlayerID: k[1], PlayerName: name})
		}
	}
	return out, nil
}
func (f *fakeAccounts) Transactions(ctx context.Context, guild, player int64, limit int, before int64, fil repository.TransactionFilter) ([]repository.LedgerEntry, int64, error) {
	f.reads++
	f.lastFil = fil
	return f.ledger.History(ctx, guild, player, limit, before)
}
func (f *fakeAccounts) Reconcile(context.Context, int64, int) ([]repository.BalanceMismatch, error) {
	return nil, nil
}

func newAccountsFixture() (*Accounts, *fakeStore, *fakeAccounts) {
	l := newFakeStore()
	fa := newFakeAccounts(l)
	return NewAccounts(NewService(l, nil), fa), l, fa
}

func adj(scope repository.EconomyScope, account, amount int64) AdjustRequest {
	return AdjustRequest{Scope: scope, ActorDiscordID: "admin-1", AccountID: account, Amount: amount, Reason: "because"}
}

func TestCurrencyIsChampionPoints(t *testing.T) {
	if ChampionPoints.Code != "CHAMPION_POINTS" || ChampionPoints.Name != "Champion Points" || ChampionPoints.Symbol == "" {
		t.Fatalf("the canonical currency is the existing Champion Points: %+v", ChampionPoints)
	}
}

func TestScopeIsPerOrganizationAndInstallation(t *testing.T) {
	a, _, _ := newAccountsFixture()
	ctx := context.Background()
	if s, err := a.Scope(ctx, 7, 70); err != nil || s.GuildID != 1 {
		t.Fatalf("own scope: %+v %v", s, err)
	}
	for _, p := range [][2]int64{{8, 70}, {7, 80}, {9, 70}, {7, 999}} {
		if _, err := a.Scope(ctx, p[0], p[1]); !errors.Is(err, ErrInstallationNotFound) {
			t.Errorf("org %d installation %d: %v", p[0], p[1], err)
		}
	}
}

func TestMeRequiresAVerifiedLink(t *testing.T) {
	a, l, _ := newAccountsFixture()
	ctx := context.Background()
	scope, _ := a.Scope(ctx, 7, 70)
	l.balance[[2]int64{1, 10}] = 42
	acc, err := a.Me(ctx, scope, "discord-alice")
	if err != nil || acc.AccountID != 10 || acc.Balance != 42 || acc.Gamertag != "Alice" {
		t.Fatalf("linked user: %+v %v", acc, err)
	}
	// No link (or a name that merely matches): never an account.
	for _, who := range []string{"discord-nobody", "", "Alice"} {
		if _, err := a.Me(ctx, scope, who); !errors.Is(err, ErrIdentityRequired) {
			t.Errorf("%q: %v", who, err)
		}
	}
}

func TestAccountLookupIsGuildScoped(t *testing.T) {
	a, _, _ := newAccountsFixture()
	ctx := context.Background()
	scope, _ := a.Scope(ctx, 7, 70)
	if _, err := a.Account(ctx, scope, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Account(ctx, scope, 20); !errors.Is(err, ErrAccountNotFound) { // a player of guild 2
		t.Fatalf("another guild's account: %v", err)
	}
}

func TestSearchValidation(t *testing.T) {
	a, _, _ := newAccountsFixture()
	ctx := context.Background()
	scope, _ := a.Scope(ctx, 7, 70)
	for _, q := range []string{"", " ", "a", strings.Repeat("x", 51)} {
		if _, err := a.Search(ctx, scope, q, 10); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("q=%q: %v", q, err)
		}
	}
	got, err := a.Search(ctx, scope, "  ali   ", 10)
	if err != nil || len(got) != 1 || got[0].Gamertag != "Alice" {
		t.Fatalf("search: %+v %v", got, err)
	}
	if other, _ := a.Search(ctx, scope, "OtherGuild", 10); len(other) != 0 {
		t.Fatal("search is confined to the installation's guild")
	}
}

func TestCursorRoundTripAndRejection(t *testing.T) {
	if id, ok := DecodeCursor(encodeCursor(12345)); !ok || id != 12345 {
		t.Fatalf("round trip: %d %v", id, ok)
	}
	for _, bad := range []string{"", "bogus", "!!!", encodeCursor(0), encodeCursor(-5)} {
		if _, ok := DecodeCursor(bad); ok {
			t.Errorf("%q must be rejected", bad)
		}
	}
	// A cursor of another family (the faction leaderboard's) is not accepted.
	if _, ok := DecodeCursor("bGIxOktJTExTOjE6MjozOjQ6NQ"); ok {
		t.Error("a foreign cursor must be rejected")
	}
}

func TestParseTypeFilter(t *testing.T) {
	for raw, want := range map[string]repository.TransactionFilter{
		"":               {},
		" admin_credit ": {Types: []string{TypeAdminCredit}},
		"BOUNTY_CLAIM":   {Types: []string{TypeBountyClaim}},
		"system_reward":  {Types: []string{TypeSystemReward}},
		"EVENT_PRIZE":    {TypePrefix: "EVENT_"},
		"ADMIN_DEBIT":    {Types: []string{TypeAdminDebit}},
	} {
		got, err := ParseTypeFilter(raw)
		if err != nil || strings.Join(got.Types, ",") != strings.Join(want.Types, ",") || got.TypePrefix != want.TypePrefix {
			t.Errorf("%q: %+v %v", raw, got, err)
		}
	}
	for _, bad := range []string{"CASINO_BET", "SHOP_PURCHASE", "EVENT_FIRST_PLACE", "ADMIN_%", "a;b"} {
		if _, err := ParseTypeFilter(bad); !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("%q must be rejected: %v", bad, err)
		}
	}
}

func TestTransactionsLimitsCursorAndPrivacy(t *testing.T) {
	a, l, fa := newAccountsFixture()
	ctx := context.Background()
	scope, _ := a.Scope(ctx, 7, 70)
	for i := 0; i < 30; i++ {
		if _, err := a.Grant(ctx, adj(scope, 10, 100)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Debit(ctx, adj(scope, 10, 30)); err != nil {
		t.Fatal(err)
	}
	page, err := a.Transactions(ctx, scope, 10, 0, "", repository.TransactionFilter{}, false)
	if err != nil || len(page.Items) != 25 || page.Limit != 25 || page.NextCursor == "" {
		t.Fatalf("default page: %d %q %v", len(page.Items), page.NextCursor, err)
	}
	top := page.Items[0]
	if top.Direction != DirectionDebit || top.Amount != 30 || top.BalanceAfter != 2970 || top.Description != "Admin debit" {
		t.Fatalf("newest first, positive magnitude, generated description: %+v", top)
	}
	if top.Reason != "" || top.ActorID != "" || top.ReferenceID != "" || top.ServerID != 0 {
		t.Fatalf("a player view carries no admin detail: %+v", top)
	}
	adm, _ := a.Transactions(ctx, scope, 10, 1, "", repository.TransactionFilter{}, true)
	if adm.Items[0].Reason != "because" || adm.Items[0].ActorID != "admin-1" || adm.Items[0].ServerID != 101 {
		t.Fatalf("an admin view carries the reason, the actor and the server: %+v", adm.Items[0])
	}
	if big, _ := a.Transactions(ctx, scope, 10, 100000, "", repository.TransactionFilter{}, false); big.Limit != MaxTransactionLimit {
		t.Fatalf("limit is clamped: %d", big.Limit)
	}
	// The second page continues where the first stopped, without overlap.
	p2, err := a.Transactions(ctx, scope, 10, 25, page.NextCursor, repository.TransactionFilter{}, false)
	if err != nil || len(p2.Items) != 6 || p2.NextCursor != "" || p2.Items[0].ID >= page.Items[len(page.Items)-1].ID {
		t.Fatalf("page 2: %d %q %v", len(p2.Items), p2.NextCursor, err)
	}
	if _, err := a.Transactions(ctx, scope, 10, 25, "junk", repository.TransactionFilter{}, false); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("junk cursor: %v", err)
	}
	// The ledger of an account outside the installation's guild is never read.
	before := fa.reads
	if _, err := a.Transactions(ctx, scope, 20, 25, "", repository.TransactionFilter{}, false); !errors.Is(err, ErrAccountNotFound) || fa.reads != before {
		t.Fatalf("a foreign account: %v (store reads %d -> %d)", err, before, fa.reads)
	}
	_ = l
}

func TestAdjustValidationOrderAndScope(t *testing.T) {
	a, l, _ := newAccountsFixture()
	ctx := context.Background()
	scope, _ := a.Scope(ctx, 7, 70)

	for _, amount := range []int64{0, -1, MaxAdminAmount + 1, 1 << 62} {
		if _, err := a.Grant(ctx, adj(scope, 10, amount)); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("amount %d: %v", amount, err)
		}
	}
	r := adj(scope, 10, 5)
	r.Reason = "  \x00\n "
	if _, err := a.Grant(ctx, r); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("blank reason: %v", err)
	}
	for _, key := range []string{"short", "has space in it....", strings.Repeat("k", 65), "bad/char/key1234"} {
		r := adj(scope, 10, 5)
		r.IdempotencyKey = key
		if _, err := a.Grant(ctx, r); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("key %q: %v", key, err)
		}
	}
	suspended := scope
	suspended.Status = "SUSPENDED"
	if _, err := a.Grant(ctx, adj(suspended, 10, 5)); !errors.Is(err, ErrSuspended) {
		t.Fatalf("suspended: %v", err)
	}
	noServer := scope
	noServer.ServerID = 0
	if _, err := a.Grant(ctx, adj(noServer, 10, 5)); !errors.Is(err, ErrNoServer) {
		t.Fatalf("no server: %v", err)
	}
	if _, err := a.Grant(ctx, adj(scope, 20, 5)); !errors.Is(err, ErrAccountNotFound) { // guild 2's player
		t.Fatalf("foreign account: %v", err)
	}
	if l.calls != 0 || len(l.ledger) != 0 {
		t.Fatalf("no rejected request may reach the ledger: %d calls", l.calls)
	}
	// An organization that does not own the server of the scope is refused by the service (defence in depth).
	wrongOrg := scope
	wrongOrg.OrganizationID = 99
	if _, err := a.Grant(ctx, adj(wrongOrg, 10, 5)); !errors.Is(err, ErrForbiddenTenant) {
		t.Fatalf("wrong organization for the server: %v", err)
	}
	// A valid grant and an overdraft.
	res, err := a.Grant(ctx, adj(scope, 10, 100))
	if err != nil || res.Transaction.Direction != DirectionCredit || res.Transaction.Amount != 100 || res.Transaction.Type != TypeAdminCredit || res.Duplicate {
		t.Fatalf("grant: %+v %v", res, err)
	}
	if _, err := a.Debit(ctx, adj(scope, 10, 101)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("overdraft: %v", err)
	}
	if l.balance[[2]int64{1, 10}] != 100 {
		t.Fatal("a refused debit changes nothing")
	}
}

func TestAdjustIdempotencyKey(t *testing.T) {
	a, l, _ := newAccountsFixture()
	ctx := context.Background()
	scope, _ := a.Scope(ctx, 7, 70)
	r := adj(scope, 10, 500)
	r.IdempotencyKey = "reward-key-0001"
	first, err := a.Grant(ctx, r)
	if err != nil || first.Duplicate {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := a.Grant(ctx, r)
	if err != nil || !second.Duplicate || second.Transaction.ID != first.Transaction.ID {
		t.Fatalf("replay returns the original: %+v %v", second, err)
	}
	if l.balance[[2]int64{1, 10}] != 500 || len(l.ledger) != 1 {
		t.Fatal("applied exactly once")
	}
	r.Amount = 501
	if _, err := a.Grant(ctx, r); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("same key, another amount: %v", err)
	}
	// The key is per operation: a debit with the same key is its own transaction.
	d := adj(scope, 10, 500)
	d.IdempotencyKey = "reward-key-0001"
	if res, err := a.Debit(ctx, d); err != nil || res.Duplicate {
		t.Fatalf("debit with the same key: %+v %v", res, err)
	}
	if l.balance[[2]int64{1, 10}] != 0 {
		t.Fatalf("balance %d", l.balance[[2]int64{1, 10}])
	}
}

func TestAdjustNotifiesOnlyCommittedTransactions(t *testing.T) {
	l := newFakeStore()
	rec := &recordingNotifier{}
	a := NewAccounts(NewService(l, rec), newFakeAccounts(l))
	ctx := context.Background()
	scope, _ := a.Scope(ctx, 7, 70)
	r := adj(scope, 10, 100)
	r.IdempotencyKey = "notify-key-0001"
	_, _ = a.Grant(ctx, r)
	_, _ = a.Grant(ctx, r)                    // replay
	_, _ = a.Debit(ctx, adj(scope, 10, 5000)) // refused
	_, _ = a.Debit(ctx, adj(scope, 10, 40))
	if len(rec.events) != 2 || !rec.events[0].Credit || rec.events[1].Credit || rec.events[1].BalanceAfter != 60 {
		t.Fatalf("events: %+v", rec.events)
	}
}

func TestReferenceTypes(t *testing.T) {
	for typ, want := range map[string]string{TypeBountyClaim: "BOUNTY", "EVENT_FIRST_PLACE": "EVENT", TypeAdminCredit: "", TypeSystemReward: ""} {
		if got := referenceType(typ); got != want {
			t.Errorf("%s: %q want %q", typ, got, want)
		}
	}
}
