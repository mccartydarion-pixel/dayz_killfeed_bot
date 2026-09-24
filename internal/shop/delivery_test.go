package shop

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// deliveryStore adds the delivery reads/settings to fakeStore.
type deliveryStore struct {
	*fakeStore
	deliveryQ repository.DeliveryQuery
	getPlayer int64
	settings  repository.ShopDeliverySettings
	setKey    string
	setCalls  int
}

func (d *deliveryStore) ListDeliveries(_ context.Context, _, _ int64, q repository.DeliveryQuery) ([]repository.ShopDelivery, int64, error) {
	d.deliveryQ = q
	return []repository.ShopDelivery{{ID: 8}, {ID: 5}}, 5, nil
}

func (d *deliveryStore) GetDelivery(_ context.Context, _, _, id, playerID int64) (*repository.ShopDelivery, error) {
	d.getPlayer = playerID
	return &repository.ShopDelivery{ID: id, PlayerID: playerID}, nil
}

func (d *deliveryStore) GetDeliverySettings(context.Context, int64, int64) (repository.ShopDeliverySettings, error) {
	return d.settings, nil
}

func (d *deliveryStore) SetDeliveryMap(_ context.Context, _, _ int64, key string, _ int64) (repository.ShopDeliverySettings, error) {
	d.setCalls++
	d.setKey = key
	d.settings = repository.ShopDeliverySettings{MapKey: key}
	return d.settings, nil
}

func newDeliverySvc() (*Service, *deliveryStore, *fakeIdentity) {
	svc, st, id, rec := newSvc()
	ds := &deliveryStore{fakeStore: st}
	svc = NewService(ds, id, rec)
	return svc, ds, id
}

func f64(v float64) *float64 { return &v }

func TestPurchaseDeliveryCoordinateShape(t *testing.T) {
	svc, st, _, _ := newSvc()
	ctx := context.Background()
	base := PurchaseRequest{ProductID: 1, Quantity: 1, IdempotencyKey: "abcdefgh12"}

	// No delivery object: pickup-shaped request, nothing passed on.
	if _, err := svc.Purchase(ctx, scope, 5, "d1", base); err != nil || st.lastPurchase.Delivery != nil {
		t.Fatalf("%v %+v", err, st.lastPurchase.Delivery)
	}
	// Valid coordinates are passed on exactly; there is no y.
	req := base
	req.Delivery = &DeliveryInput{X: f64(7500.25), Z: f64(8300.5)}
	if _, err := svc.Purchase(ctx, scope, 5, "d1", req); err != nil || st.lastPurchase.Delivery == nil ||
		st.lastPurchase.Delivery.X != 7500.25 || st.lastPurchase.Delivery.Z != 8300.5 {
		t.Fatalf("%v %+v", err, st.lastPurchase.Delivery)
	}
	// Shape errors never reach the store.
	calls := st.purchases
	for name, d := range map[string]*DeliveryInput{
		"missing x": {Z: f64(1)},
		"missing z": {X: f64(1)},
		"both nil":  {},
		"nan":       {X: f64(math.NaN()), Z: f64(1)},
		"inf":       {X: f64(1), Z: f64(math.Inf(1))},
		"minus inf": {X: f64(math.Inf(-1)), Z: f64(1)},
	} {
		r := base
		r.Delivery = d
		if _, err := svc.Purchase(ctx, scope, 5, "d1", r); !errors.Is(err, ErrInvalidCoordinates) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if st.purchases != calls {
		t.Fatal("an invalid delivery object must not reach the purchase transaction")
	}
	// Map/bounds/policy errors come from the transaction and pass through unchanged.
	st.purchaseErr = repository.ErrShopDeliveryMapUnresolved
	if _, err := svc.Purchase(ctx, scope, 5, "d1", req); !errors.Is(err, repository.ErrShopDeliveryMapUnresolved) {
		t.Fatal(err)
	}
}

func TestProductDeliveryPolicyValidation(t *testing.T) {
	for _, policy := range []string{PolicyManualPickup, PolicyManualCoordinate} {
		if err := ValidateProduct("n", "", 5, TypeItem, DeliveryManual, policy, StockUnlimited, nil, nil, 0); err != nil {
			t.Errorf("%s: %v", policy, err)
		}
	}
	for _, policy := range []string{"", "IN_GAME_FUTURE", "AUTO_SPAWN", "manual_pickup"} {
		var ve *ValidationError
		if err := ValidateProduct("n", "", 5, TypeItem, DeliveryManual, policy, StockUnlimited, nil, nil, 0); !errors.As(err, &ve) || ve.Issues["deliveryPolicy"] == "" {
			t.Errorf("%q must be rejected: %v", policy, err)
		}
	}
	// The reserved delivery type stays inactive whatever the policy.
	if err := ValidateProduct("n", "", 5, TypeItem, DeliveryInGameFuture, PolicyManualCoordinate, StockUnlimited, nil, nil, 0); err == nil {
		t.Fatal("IN_GAME_FUTURE must stay reserved")
	}
}

func TestAdminDeliveriesQueryValidation(t *testing.T) {
	svc, st, _ := newDeliverySvc()
	ctx := context.Background()
	page, err := svc.AdminDeliveries(ctx, scope, DeliveryList{Status: "manual_ready", Policy: "manual_coordinate", PlayerID: 7, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if st.deliveryQ.Status != DeliveryReady || st.deliveryQ.Policy != PolicyManualCoordinate || st.deliveryQ.PlayerID != 7 || st.deliveryQ.Limit != MaxLimit {
		t.Fatalf("%+v", st.deliveryQ)
	}
	if page.NextCursor == "" {
		t.Fatal("a next page must have a cursor")
	}
	if _, err := svc.AdminDeliveries(ctx, scope, DeliveryList{Cursor: page.NextCursor}); err != nil || st.deliveryQ.BeforeID != 5 || st.deliveryQ.Limit != DefaultLimit {
		t.Fatalf("%v %+v", err, st.deliveryQ)
	}
	for name, q := range map[string]DeliveryList{
		"reserved status": {Status: "FILE_STAGED"},
		"unknown status":  {Status: "SHIPPED"},
		"unknown policy":  {Policy: "DRONE"},
		"bad cursor":      {Cursor: "not-a-cursor"},
		"purchase cursor": {Cursor: encodePurchaseCursor(5)},
	} {
		if _, err := svc.AdminDeliveries(ctx, scope, q); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

func TestMyDeliveryIsScopedToTheVerifiedPlayer(t *testing.T) {
	svc, st, id := newDeliverySvc()
	d, err := svc.MyDelivery(context.Background(), scope, "discord-1", 42)
	if err != nil || st.getPlayer != 77 || id.asked != "discord-1" || d.PlayerID != 77 {
		t.Fatalf("%v player %d asked %q", err, st.getPlayer, id.asked)
	}
	if _, err := svc.AdminDelivery(context.Background(), scope, 42); err != nil || st.getPlayer != 0 {
		t.Fatal("admin reads are installation-wide")
	}
	id.err = errors.New("no link")
	if _, err := svc.MyDelivery(context.Background(), scope, "discord-2", 42); err == nil {
		t.Fatal("no verified identity, no delivery")
	}
}

func TestSetDeliveryMapAcceptsOnlyVerifiedMaps(t *testing.T) {
	svc, st, _ := newDeliverySvc()
	ctx := context.Background()
	s, err := svc.SetDeliveryMap(ctx, scope, 1, " ChernarusPlus ")
	if err != nil || st.setKey != "chernarusplus" || s.Map == nil || s.Map.SizeMetres != 15360 {
		t.Fatalf("%v %q %+v", err, st.setKey, s)
	}
	for _, key := range []string{"sakhal", "deerisle", "chernarus", "../etc"} {
		if _, err := svc.SetDeliveryMap(ctx, scope, 1, key); !errors.Is(err, ErrUnsupportedMap) {
			t.Errorf("%q: %v", key, err)
		}
	}
	if st.setCalls != 1 {
		t.Fatal("an unsupported map must not be stored")
	}
	if s, err := svc.SetDeliveryMap(ctx, scope, 1, ""); err != nil || st.setKey != "" || s.Map != nil {
		t.Fatalf("clearing: %v %+v", err, s)
	}
	// A stored key that is no longer supported resolves to no map (coordinate delivery unavailable).
	st.settings = repository.ShopDeliverySettings{MapKey: "sakhal"}
	if s, _ := svc.DeliverySettings(ctx, scope); s.Map != nil || s.MapKey != "sakhal" {
		t.Fatalf("%+v", s)
	}
}

func TestAutomaticDeliveryIsDisabled(t *testing.T) {
	var a DeliveryAdapter = DisabledAdapter{}
	if a.Enabled() || a.Name() != "disabled" {
		t.Fatal("the only adapter must be disabled")
	}
	plan := DeliveryPlan{DeliveryID: 1, PurchaseID: 2, MapKey: "chernarusplus", X: 1, Z: 2}
	if err := a.Deliver(context.Background(), plan); !errors.Is(err, ErrAutomaticDeliveryDisabled) {
		t.Fatalf("Deliver must refuse: %v", err)
	}
	r := a.DryRun(context.Background(), plan)
	if r.Enabled || len(r.Blockers) == 0 || len(r.Steps) == 0 {
		t.Fatalf("%+v", r)
	}
	for _, s := range r.Steps {
		if len(s) < 13 || s[:13] != "NOT EXECUTED:" {
			t.Fatalf("a dry run never claims to have executed anything: %q", s)
		}
	}
}
