package shop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakeStore overrides only what the service tests need; any other call would panic on the nil embedded Store.
type fakeStore struct {
	Store
	purchases    int
	lastPurchase repository.PurchaseParams
	purchaseErr  error
	duplicate    bool
	refunds      int
	lastRefund   repository.RefundParams
	listQ        repository.ProductQuery
	purchaseQ    repository.PurchaseQuery
}

func (f *fakeStore) Purchase(_ context.Context, p repository.PurchaseParams) (*repository.PurchaseResult, error) {
	f.purchases++
	f.lastPurchase = p
	if f.purchaseErr != nil {
		return nil, f.purchaseErr
	}
	return &repository.PurchaseResult{Purchase: repository.ShopPurchase{ID: 9, TotalPoints: 750}, BalanceAfter: 250, Duplicate: f.duplicate, PlayerName: "Alice", ItemName: "Crate"}, nil
}

func (f *fakeStore) Refund(_ context.Context, p repository.RefundParams) (*repository.RefundResult, error) {
	f.refunds++
	f.lastRefund = p
	return &repository.RefundResult{Purchase: repository.ShopPurchase{ID: p.PurchaseID, TotalPoints: 750}, BalanceAfter: 1000, PlayerName: "Alice", ItemName: "Crate"}, nil
}

func (f *fakeStore) ListProducts(_ context.Context, _, _ int64, q repository.ProductQuery) ([]repository.ShopProduct, bool, error) {
	f.listQ = q
	items := []repository.ShopProduct{{ID: 1, Name: "B"}, {ID: 2, Name: "A"}}
	return items, true, nil
}

func (f *fakeStore) ListPurchases(_ context.Context, _, _ int64, q repository.PurchaseQuery) ([]repository.ShopPurchase, int64, error) {
	f.purchaseQ = q
	return []repository.ShopPurchase{{ID: 30}, {ID: 20}}, 20, nil
}

type fakeIdentity struct {
	account economy.Account
	err     error
	asked   string
}

func (f *fakeIdentity) Me(_ context.Context, _ repository.EconomyScope, discord string) (economy.Account, error) {
	f.asked = discord
	return f.account, f.err
}

type recorder struct{ events []economy.Event }

func (r *recorder) Announce(e economy.Event) { r.events = append(r.events, e) }

var scope = repository.EconomyScope{OrganizationID: 1, InstallationID: 2, GuildID: 3, ServerID: 4, Status: "READY"}

func newSvc() (*Service, *fakeStore, *fakeIdentity, *recorder) {
	st, id, rec := &fakeStore{}, &fakeIdentity{account: economy.Account{AccountID: 77, Gamertag: "Alice"}}, &recorder{}
	return NewService(st, id, rec), st, id, rec
}

func TestStatusTransitionsAreExplicit(t *testing.T) {
	ok := [][2]string{
		{StatusPendingFulfillment, StatusFulfilled}, {StatusPendingFulfillment, StatusRefunded}, {StatusFulfilled, StatusRefunded},
		{StatusPaid, StatusFulfilled}, {StatusPaid, StatusRefunded}, {StatusPending, StatusCancelled}, {StatusPending, StatusPaid},
	}
	for _, p := range ok {
		if !CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s must be allowed", p[0], p[1])
		}
	}
	bad := [][2]string{
		{StatusRefunded, StatusFulfilled}, {StatusRefunded, StatusRefunded}, {StatusFulfilled, StatusPendingFulfillment}, {StatusFulfilled, StatusFulfilled},
		{StatusCancelled, StatusPaid}, {StatusFailed, StatusPaid}, {StatusPendingFulfillment, StatusPending}, {"NOPE", StatusPaid}, {StatusRefunded, StatusPendingFulfillment},
	}
	for _, p := range bad {
		if CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s must be refused", p[0], p[1])
		}
	}
	for _, s := range []string{StatusPending, StatusPaid, StatusPendingFulfillment, StatusFulfilled, StatusCancelled, StatusRefunded, StatusFailed} {
		if !IsStatus(s) {
			t.Errorf("%s is a status", s)
		}
	}
	if IsStatus("SHIPPED") {
		t.Error("unknown status")
	}
}

func TestValidateProduct(t *testing.T) {
	seven := int64(7)
	one := 1
	good := func() error {
		return ValidateProduct("Crate", "desc", 100, TypeItem, DeliveryManual, StockUnlimited, nil, nil, 0)
	}
	if err := good(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]error{
		"empty name":      ValidateProduct("", "", 100, TypeItem, DeliveryManual, StockUnlimited, nil, nil, 0),
		"long name":       ValidateProduct(strings.Repeat("n", 81), "", 100, TypeItem, DeliveryManual, StockUnlimited, nil, nil, 0),
		"long desc":       ValidateProduct("n", strings.Repeat("d", 1001), 100, TypeItem, DeliveryManual, StockUnlimited, nil, nil, 0),
		"zero price":      ValidateProduct("n", "", 0, TypeItem, DeliveryManual, StockUnlimited, nil, nil, 0),
		"negative price":  ValidateProduct("n", "", -1, TypeItem, DeliveryManual, StockUnlimited, nil, nil, 0),
		"absurd price":    ValidateProduct("n", "", MaxPricePoints+1, TypeItem, DeliveryManual, StockUnlimited, nil, nil, 0),
		"unknown type":    ValidateProduct("n", "", 5, "GUN", DeliveryManual, StockUnlimited, nil, nil, 0),
		"discord role":    ValidateProduct("n", "", 5, TypeItem, DeliveryDiscordRole, StockUnlimited, nil, nil, 0),
		"in game":         ValidateProduct("n", "", 5, TypeItem, DeliveryInGameFuture, StockUnlimited, nil, nil, 0),
		"finite no qty":   ValidateProduct("n", "", 5, TypeItem, DeliveryManual, StockFinite, nil, nil, 0),
		"finite negative": ValidateProduct("n", "", 5, TypeItem, DeliveryManual, StockFinite, ptr(-1), nil, 0),
		"unlimited + qty": ValidateProduct("n", "", 5, TypeItem, DeliveryManual, StockUnlimited, &seven, nil, 0),
		"unknown stock":   ValidateProduct("n", "", 5, TypeItem, DeliveryManual, "SOMETIMES", nil, nil, 0),
		"limit 0":         ValidateProduct("n", "", 5, TypeItem, DeliveryManual, StockUnlimited, nil, ptrInt(0), 0),
		"limit huge":      ValidateProduct("n", "", 5, TypeItem, DeliveryManual, StockUnlimited, nil, ptrInt(1001), 0),
		"sort range":      ValidateProduct("n", "", 5, TypeItem, DeliveryManual, StockUnlimited, nil, nil, 10001),
	}
	for name, err := range cases {
		var ve *ValidationError
		if !errors.As(err, &ve) || len(ve.Issues) == 0 {
			t.Errorf("%s must be rejected: %v", name, err)
		}
	}
	if err := ValidateProduct("n", "", 5, TypeService, DeliveryManual, StockFinite, ptr(0), &one, 5); err != nil {
		t.Fatalf("zero stock is a valid FINITE state (sold out): %v", err)
	}
}

func ptr(v int64) *int64 { return &v }
func ptrInt(v int) *int  { return &v }

func TestOptionalDistinguishesAbsentNullAndValue(t *testing.T) {
	var body struct {
		A Optional[int64] `json:"a"`
		B Optional[int64] `json:"b"`
		C Optional[int64] `json:"c"`
	}
	if err := json.Unmarshal([]byte(`{"a": null, "b": 5}`), &body); err != nil {
		t.Fatal(err)
	}
	if !body.A.Set || !body.A.Null || !body.B.Set || body.B.Null || body.B.Value != 5 || body.C.Set {
		t.Fatalf("%+v", body)
	}
	if err := json.Unmarshal([]byte(`{"a": "x"}`), &body); err == nil {
		t.Fatal("a wrong type is an error")
	}
}

func TestStockState(t *testing.T) {
	q := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		p    repository.ShopProduct
		want string
	}{
		{repository.ShopProduct{StockMode: StockUnlimited}, "UNLIMITED"},
		{repository.ShopProduct{StockMode: StockFinite, StockQuantity: q(0)}, "OUT_OF_STOCK"},
		{repository.ShopProduct{StockMode: StockFinite, StockQuantity: q(1)}, "LOW_STOCK"},
		{repository.ShopProduct{StockMode: StockFinite, StockQuantity: q(5)}, "LOW_STOCK"},
		{repository.ShopProduct{StockMode: StockFinite, StockQuantity: q(6)}, "IN_STOCK"},
	} {
		if got := StockState(c.p); got != c.want {
			t.Errorf("%+v: %s want %s", c.p, got, c.want)
		}
	}
}

func TestPurchaseValidatesBeforeTouchingAnything(t *testing.T) {
	s, st, id, rec := newSvc()
	ctx := context.Background()
	req := PurchaseRequest{ProductID: 5, Quantity: 1, IdempotencyKey: "purchase-key-01"}
	for _, q := range []int{0, -1, 101, 1 << 30} {
		r := req
		r.Quantity = q
		if _, err := s.Purchase(ctx, scope, 1, "d1", r); !errors.Is(err, ErrInvalidQuantity) {
			t.Errorf("quantity %d: %v", q, err)
		}
	}
	for _, key := range []string{"", "short", strings.Repeat("k", 65), "has spaces in key"} {
		r := req
		r.IdempotencyKey = key
		if _, err := s.Purchase(ctx, scope, 1, "d1", r); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("key %q: %v", key, err)
		}
	}
	if _, err := s.Purchase(ctx, scope, 1, "d1", PurchaseRequest{ProductID: 0, Quantity: 1, IdempotencyKey: "purchase-key-01"}); !errors.Is(err, repository.ErrShopProductNotFound) {
		t.Errorf("product 0: %v", err)
	}
	suspended := scope
	suspended.Status = "SUSPENDED"
	if _, err := s.Purchase(ctx, suspended, 1, "d1", req); !errors.Is(err, economy.ErrSuspended) {
		t.Errorf("suspended: %v", err)
	}
	noServer := scope
	noServer.ServerID = 0
	if _, err := s.Purchase(ctx, noServer, 1, "d1", req); !errors.Is(err, economy.ErrNoServer) {
		t.Errorf("no server: %v", err)
	}
	id.err = economy.ErrIdentityRequired
	if _, err := s.Purchase(ctx, scope, 1, "d1", req); !errors.Is(err, economy.ErrIdentityRequired) {
		t.Errorf("no verified identity: %v", err)
	}
	if st.purchases != 0 || len(rec.events) != 0 {
		t.Fatalf("no rejected request may reach the store or the notifier: %d %d", st.purchases, len(rec.events))
	}
}

func TestPurchaseUsesTheVerifiedIdentityAndAnnouncesOnce(t *testing.T) {
	s, st, id, rec := newSvc()
	ctx := context.Background()
	res, err := s.Purchase(ctx, scope, 11, "discord-11", PurchaseRequest{ProductID: 5, Quantity: 3, IdempotencyKey: " purchase-key-01 "})
	if err != nil || res.Purchase.ID != 9 {
		t.Fatalf("%+v %v", res, err)
	}
	p := st.lastPurchase
	if id.asked != "discord-11" || p.PlayerID != 77 || p.UserID != 11 || p.GuildID != 3 || p.ServerID != 4 || p.OrganizationID != 1 || p.InstallationID != 2 || p.ProductID != 5 || p.Quantity != 3 || p.IdempotencyKey != "purchase-key-01" || p.ActorDiscordID != "discord-11" {
		t.Fatalf("the buyer is the verified player and the scope comes from the installation: %+v", p)
	}
	if len(rec.events) != 1 || rec.events[0].Type != economy.TypeShopPurchase || rec.events[0].Credit || rec.events[0].Amount != 750 || rec.events[0].BalanceAfter != 250 || rec.events[0].Item != "Crate" {
		t.Fatalf("event: %+v", rec.events)
	}
	// A replay charges nothing and announces nothing; a store failure announces nothing.
	st.duplicate = true
	if _, err := s.Purchase(ctx, scope, 11, "discord-11", PurchaseRequest{ProductID: 5, Quantity: 3, IdempotencyKey: "purchase-key-01"}); err != nil || len(rec.events) != 1 {
		t.Fatalf("replay: %v %d", err, len(rec.events))
	}
	st.duplicate, st.purchaseErr = false, repository.ErrInsufficientFunds
	if _, err := s.Purchase(ctx, scope, 11, "discord-11", PurchaseRequest{ProductID: 5, Quantity: 1, IdempotencyKey: "purchase-key-02"}); !errors.Is(err, repository.ErrInsufficientFunds) || len(rec.events) != 1 {
		t.Fatalf("failure: %v %d", err, len(rec.events))
	}
	// A nil announcer is fine.
	s2 := NewService(&fakeStore{}, id, nil)
	if _, err := s2.Purchase(ctx, scope, 1, "d", PurchaseRequest{ProductID: 1, Quantity: 1, IdempotencyKey: "purchase-key-03"}); err != nil {
		t.Fatal(err)
	}
}

func TestRefundNeedsAReasonAndAnnouncesACredit(t *testing.T) {
	s, st, _, rec := newSvc()
	ctx := context.Background()
	for _, reason := range []string{"", "   ", "\x00\n\t"} {
		if _, err := s.Refund(ctx, scope, 1, "admin", 5, reason); !errors.Is(err, ErrReasonRequired) {
			t.Errorf("reason %q: %v", reason, err)
		}
	}
	suspended := scope
	suspended.Status = "SUSPENDED"
	if _, err := s.Refund(ctx, suspended, 1, "admin", 5, "x"); !errors.Is(err, economy.ErrSuspended) {
		t.Errorf("suspended: %v", err)
	}
	if st.refunds != 0 {
		t.Fatal("rejected refunds never reach the store")
	}
	long := strings.Repeat("r", 500)
	if _, err := s.Refund(ctx, scope, 1, "admin", 5, "  broken \n item  "+long); err != nil {
		t.Fatal(err)
	}
	if st.lastRefund.Reason[:12] != "broken item " || len([]rune(st.lastRefund.Reason)) != MaxReasonLen || st.lastRefund.GuildID != 3 || st.lastRefund.PurchaseID != 5 || st.lastRefund.ActorDiscordID != "admin" {
		t.Fatalf("refund params: %+v", st.lastRefund)
	}
	if len(rec.events) != 1 || rec.events[0].Type != economy.TypeShopRefund || !rec.events[0].Credit || rec.events[0].Amount != 750 {
		t.Fatalf("event: %+v", rec.events)
	}
}

func TestCatalogQueriesAreClampedAndCursorsRoundTrip(t *testing.T) {
	s, st, _, _ := newSvc()
	ctx := context.Background()
	page, err := s.Products(ctx, scope, ProductList{Limit: 100000, CategorySlug: "  Vehicles ", Search: "  m4   rifle "}, false)
	if err != nil || page.Limit != MaxLimit || st.listQ.Limit != MaxLimit || st.listQ.CategorySlug != "vehicles" || st.listQ.Search != "m4 rifle" || st.listQ.IncludeInactive {
		t.Fatalf("%+v %+v %v", page, st.listQ, err)
	}
	if page.NextCursor == "" {
		t.Fatal("more pages: a cursor")
	}
	c, ok := repository.DecodeProductCursor(page.NextCursor)
	if !ok || c.ID != 2 || c.NameLower != "a" {
		t.Fatalf("the cursor continues after the last item: %+v %v", c, ok)
	}
	if _, err := s.Products(ctx, scope, ProductList{Cursor: page.NextCursor}, true); err != nil || !st.listQ.IncludeInactive || st.listQ.Cursor == nil {
		t.Fatalf("admin list with cursor: %v %+v", err, st.listQ)
	}
	for _, bad := range []string{"bogus", "!!!"} {
		if _, err := s.Products(ctx, scope, ProductList{Cursor: bad}, false); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("cursor %q: %v", bad, err)
		}
	}
	if _, err := s.Products(ctx, scope, ProductList{Search: strings.Repeat("x", 51)}, false); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("long search: %v", err)
	}
	if def, _ := s.Products(ctx, scope, ProductList{}, false); def.Limit != DefaultLimit {
		t.Errorf("default limit %d", def.Limit)
	}
}

func TestPurchaseListsUseKeysetCursorsAndStatusFilter(t *testing.T) {
	s, st, id, _ := newSvc()
	ctx := context.Background()
	page, err := s.MyPurchases(ctx, scope, "discord-1", PurchaseList{Status: " refunded ", Limit: 500})
	if err != nil || page.Limit != MaxLimit || st.purchaseQ.PlayerID != 77 || st.purchaseQ.Status != "REFUNDED" || id.asked != "discord-1" {
		t.Fatalf("a player list is confined to the verified player: %+v %v %v", st.purchaseQ, page, err)
	}
	if page.NextCursor == "" {
		t.Fatal("cursor expected")
	}
	if _, err := s.AdminPurchases(ctx, scope, PurchaseList{PlayerID: 5, ProductID: 6, Cursor: page.NextCursor}); err != nil || st.purchaseQ.BeforeID != 20 || st.purchaseQ.PlayerID != 5 || st.purchaseQ.ProductID != 6 {
		t.Fatalf("admin list: %+v %v", st.purchaseQ, err)
	}
	if _, err := s.AdminPurchases(ctx, scope, PurchaseList{Status: "SHIPPED"}); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("status: %v", err)
	}
	if _, err := s.AdminPurchases(ctx, scope, PurchaseList{Cursor: "nope"}); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("cursor: %v", err)
	}
	id.err = economy.ErrIdentityRequired
	if _, err := s.MyPurchases(ctx, scope, "d", PurchaseList{}); !errors.Is(err, economy.ErrIdentityRequired) {
		t.Errorf("no identity: %v", err)
	}
}

func TestCleanTextHelpers(t *testing.T) {
	if got := cleanLine("  a \x00\n b\t c  "); got != "a b c" {
		t.Errorf("cleanLine: %q", got)
	}
	if got := cleanText("  line1\nline2\x00  "); got != "line1\nline2" {
		t.Errorf("cleanText: %q", got)
	}
}
