//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

// Champion Shop Delivery Engine 2.0 over the real routes and a real PostgreSQL (docs/SHOP_DELIVERY.md):
// delivery policies, coordinate and map validation, the purchase + delivery transaction and its
// rollback, idempotency, concurrency, fulfillment, refund cancellation, the delivery queue, tenant
// isolation, player privacy, the backfill/heal path and restart safety. Nothing contacts a game server.

func (w *factionWorld) buyAt(f installationFixture, actor string, product int64, qty int, key string, delivery any) *apiResult {
	w.t.Helper()
	body := map[string]any{"productId": product, "quantity": qty, "idempotencyKey": key}
	if delivery != nil {
		body["delivery"] = delivery
	}
	return w.do(http.MethodPost, w.shopPath(f, "/purchases"), actor, body)
}

func (w *factionWorld) setMap(f installationFixture, key any) *apiResult {
	w.t.Helper()
	return w.do(http.MethodPut, w.shopPath(f, "/delivery-settings"), w.adminFor(f), map[string]any{"mapKey": key})
}

func deliveryOf(t *testing.T, purchase map[string]any) map[string]any {
	t.Helper()
	d, ok := purchase["delivery"].(map[string]any)
	if !ok {
		t.Fatalf("purchase without a delivery: %v", purchase)
	}
	return d
}

// snapshotCounts is everything a refused purchase must leave untouched.
type snapshotCounts struct{ purchases, deliveries, ledger, balance int64 }

func (w *factionWorld) snapshot(f installationFixture, actor string) snapshotCounts {
	w.t.Helper()
	return snapshotCounts{
		purchases:  w.count(`SELECT COUNT(*) FROM shop_purchases WHERE installation_id=$1`, f.InstallationID),
		deliveries: w.count(`SELECT COUNT(*) FROM shop_deliveries WHERE installation_id=$1`, f.InstallationID),
		ledger:     w.count(`SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1`, w.guildOf(f)),
		balance:    w.balanceOf(f, actor),
	}
}

func TestShopDeliveryPickupIsBackwardCompatible(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Pickup Pete")
	w.grant(w.a1, pid, 1000)
	item := w.product(w.a1, "Pickup Crate", 100, map[string]any{"stockMode": "FINITE", "stockQuantity": 5}) // no deliveryPolicy: the default

	prod := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/products/%d", item)), player)["product"].(map[string]any)
	if prod["deliveryPolicy"] != "MANUAL_PICKUP" || prod["deliveryType"] != "MANUAL" {
		t.Fatalf("existing/new products default to MANUAL_PICKUP: %v", prod)
	}
	// The Phase 1 request shape still works and now carries its delivery record.
	resp := w.expect(w.buy(w.a1, player, item, 1, idemKeyFor("pickup")), http.StatusCreated, "pickup purchase").JSON(t)
	d := deliveryOf(t, resp["purchase"].(map[string]any))
	if d["policy"] != "MANUAL_PICKUP" || d["status"] != "MANUAL_READY" || d["deliveryType"] != "MANUAL" || d["map"] != nil || d["coordinates"] != nil ||
		int64(d["purchaseId"].(float64)) != purchaseID(resp) || d["fulfilledAt"] != nil {
		t.Fatalf("pickup delivery: %v", d)
	}
	// Coordinates are refused for a pickup product - and nothing is written.
	before := w.snapshot(w.a1, player)
	r := w.buyAt(w.a1, player, item, 1, idemKeyFor("pickup-xz"), map[string]any{"x": 100, "z": 100})
	if r.Status != http.StatusBadRequest || r.errCode(t) != "DELIVERY_COORDINATES_NOT_ACCEPTED" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if after := w.snapshot(w.a1, player); after != before {
		t.Fatalf("a refused purchase must leave nothing behind: %+v -> %+v", before, after)
	}
	if q, _ := w.stockOf(w.a1, item); q != 4 {
		t.Fatalf("stock: %v", q)
	}
	w.allIntegrity(w.a1)
}

func TestShopCoordinateDeliveryValidatesMapAndBounds(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Coord Carl")
	w.grant(w.a1, pid, 10_000)
	item := w.product(w.a1, "Supply Drop", 100, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE", "stockMode": "FINITE", "stockQuantity": 10})
	good := map[string]any{"x": 7500.25, "z": 8300.5}

	// No map configured: coordinate delivery is refused with an actionable error, nothing written.
	s := w.getJSON(w.shopPath(w.a1, "/delivery-settings"), player)["settings"].(map[string]any)
	if s["map"] != nil || s["coordinateDeliveryAvailable"] != false || s["automaticDelivery"] != false {
		t.Fatalf("settings before configuration: %v", s)
	}
	before := w.snapshot(w.a1, player)
	r := w.buyAt(w.a1, player, item, 1, idemKeyFor("nomap"), good)
	if r.Status != http.StatusConflict || r.errCode(t) != "DELIVERY_MAP_UNRESOLVED" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if after := w.snapshot(w.a1, player); after != before {
		t.Fatalf("no map: nothing may be written: %+v -> %+v", before, after)
	}
	// Only verified maps can be configured, and only by OWNER/ADMIN.
	for _, key := range []string{"sakhal", "deerisle", "chernarus"} {
		if r := w.setMap(w.a1, key); r.Status != http.StatusBadRequest || r.errCode(t) != "UNSUPPORTED_MAP" {
			t.Errorf("%s: %d %s", key, r.Status, r.Body)
		}
	}
	if r := w.do(http.MethodPut, w.shopPath(w.a1, "/delivery-settings"), w.member, map[string]any{"mapKey": "chernarusplus"}); r.Status != http.StatusForbidden {
		t.Fatalf("member: %d %s", r.Status, r.Body)
	}
	adm := w.expect(w.setMap(w.a1, "ChernarusPlus"), http.StatusOK, "set map").JSON(t)["settings"].(map[string]any)
	if adm["mapKey"] != "chernarusplus" || len(adm["supportedMaps"].([]any)) != 2 || adm["updatedByDiscordUserId"] != w.admin {
		t.Fatalf("admin settings: %v", adm)
	}
	s = w.getJSON(w.shopPath(w.a1, "/delivery-settings"), player)["settings"].(map[string]any)
	m := s["map"].(map[string]any)
	if s["coordinateDeliveryAvailable"] != true || m["key"] != "chernarusplus" || m["name"] != "Chernarus" || m["maxX"].(float64) != 15360 || m["maxZ"].(float64) != 15360 || m["minX"].(float64) != 0 {
		t.Fatalf("player settings: %v", s)
	}
	if _, leaked := s["updatedByDiscordUserId"]; leaked {
		t.Fatal("players do not see who configured the map")
	}

	// Shape and bounds.
	before = w.snapshot(w.a1, player)
	for name, c := range map[string]struct {
		delivery any
		code     string
	}{
		"missing":        {nil, "DELIVERY_COORDINATES_REQUIRED"},
		"only x":         {map[string]any{"x": 10}, "DELIVERY_COORDINATES_INVALID"},
		"only z":         {map[string]any{"z": 10}, "DELIVERY_COORDINATES_INVALID"},
		"null x":         {map[string]any{"x": nil, "z": 10}, "DELIVERY_COORDINATES_INVALID"},
		"string x":       {map[string]any{"x": "10", "z": 10}, "INVALID_REQUEST"},
		"a y is refused": {map[string]any{"x": 10, "y": 5, "z": 10}, "INVALID_REQUEST"},
		"negative":       {map[string]any{"x": -1, "z": 10}, "DELIVERY_COORDINATES_OUT_OF_BOUNDS"},
		"beyond x":       {map[string]any{"x": 15360.5, "z": 10}, "DELIVERY_COORDINATES_OUT_OF_BOUNDS"},
		"beyond z":       {map[string]any{"x": 10, "z": 99999}, "DELIVERY_COORDINATES_OUT_OF_BOUNDS"},
	} {
		r := w.buyAt(w.a1, player, item, 1, idemKeyFor("shape"), c.delivery)
		if r.Status != http.StatusBadRequest || r.errCode(t) != c.code {
			t.Errorf("%s: want %s, got %d %s", name, c.code, r.Status, r.Body)
		}
	}
	for _, raw := range []string{
		`{"productId":%d,"quantity":1,"idempotencyKey":"nonfinite-1","delivery":{"x":1e999,"z":1}}`,
		`{"productId":%d,"quantity":1,"idempotencyKey":"nonfinite-2","delivery":{"x":NaN,"z":1}}`,
	} {
		if r := w.do(http.MethodPost, w.shopPath(w.a1, "/purchases"), player, fmt.Sprintf(raw, item)); r.Status != http.StatusBadRequest {
			t.Errorf("non-finite JSON: %d %s", r.Status, r.Body)
		}
	}
	if after := w.snapshot(w.a1, player); after != before {
		t.Fatalf("invalid coordinates: nothing may be written: %+v -> %+v", before, after)
	}

	// A valid coordinate purchase: the policy, map and coordinates are snapshotted on the order.
	resp := w.expect(w.buyAt(w.a1, player, item, 2, idemKeyFor("coord"), good), http.StatusCreated, "coordinate purchase").JSON(t)
	id := purchaseID(resp)
	d := deliveryOf(t, resp["purchase"].(map[string]any))
	c := d["coordinates"].(map[string]any)
	if d["policy"] != "MANUAL_COORDINATE" || d["status"] != "MANUAL_READY" || d["map"].(map[string]any)["key"] != "chernarusplus" || c["x"].(float64) != 7500.25 || c["z"].(float64) != 8300.5 {
		t.Fatalf("coordinate delivery: %v", d)
	}
	if _, hasY := c["y"]; hasY {
		t.Fatal("no altitude is ever invented")
	}
	// The edges are on the map.
	w.expect(w.buyAt(w.a1, player, item, 1, idemKeyFor("edge"), map[string]any{"x": 0, "z": 15360}), http.StatusCreated, "edge")

	// Editing the product and the map never rewrites an existing order.
	w.expect(w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", item)), w.admin, map[string]any{"deliveryPolicy": "MANUAL_PICKUP", "name": "Renamed Drop"}), http.StatusOK, "edit product")
	w.expect(w.setMap(w.a1, "enoch"), http.StatusOK, "change map")
	got := deliveryOf(t, w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/me/purchases/%d", id)), player)["purchase"].(map[string]any))
	if got["policy"] != "MANUAL_COORDINATE" || got["map"].(map[string]any)["key"] != "chernarusplus" || got["coordinates"].(map[string]any)["x"].(float64) != 7500.25 {
		t.Fatalf("the order keeps its snapshot: %v", got)
	}
	// The same position is now off Livonia (12800 m) for a new coordinate product.
	item2 := w.product(w.a1, "Livonia Drop", 100, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE"})
	if r := w.buyAt(w.a1, player, item2, 1, idemKeyFor("liv"), map[string]any{"x": 13000, "z": 100}); r.errCode(t) != "DELIVERY_COORDINATES_OUT_OF_BOUNDS" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	// Clearing the map makes coordinate delivery unavailable again; pickup keeps working.
	w.expect(w.setMap(w.a1, nil), http.StatusOK, "clear map")
	if r := w.buyAt(w.a1, player, item2, 1, idemKeyFor("cleared"), good); r.errCode(t) != "DELIVERY_MAP_UNRESOLVED" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	w.expect(w.buy(w.a1, player, item, 1, idemKeyFor("now-pickup")), http.StatusCreated, "the edited product is a pickup now")
	w.allIntegrity(w.a1)
}

func TestShopProductDeliveryPolicyAdministration(t *testing.T) {
	w := newFactionWorld(t)
	for _, bad := range []string{"IN_GAME_FUTURE", "AUTO", "COORDINATES"} {
		body := map[string]any{"name": "Bad " + bad, "pricePoints": 5, "productType": "ITEM", "deliveryPolicy": bad}
		if r := w.do(http.MethodPost, w.shopPath(w.a1, "/products"), w.admin, body); r.Status != http.StatusBadRequest {
			t.Errorf("%q: %d %s", bad, r.Status, r.Body)
		}
	}
	blank := w.product(w.a1, "Blank Policy", 5, map[string]any{"deliveryPolicy": "  "})
	if p := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/admin/products/%d", blank)), w.admin)["product"].(map[string]any); p["deliveryPolicy"] != "MANUAL_PICKUP" {
		t.Fatalf("a blank policy defaults to MANUAL_PICKUP: %v", p)
	}
	if r := w.do(http.MethodPost, w.shopPath(w.a1, "/products"), w.admin, map[string]any{"name": "X", "pricePoints": 5, "productType": "ITEM", "deliveryType": "IN_GAME_FUTURE"}); r.Status != http.StatusBadRequest {
		t.Fatalf("IN_GAME_FUTURE stays reserved: %d %s", r.Status, r.Body)
	}
	id := w.product(w.a1, "Policy Product", 5, map[string]any{"deliveryPolicy": "manual_coordinate"})
	p := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/admin/products/%d", id)), w.admin)["product"].(map[string]any)
	if p["deliveryPolicy"] != "MANUAL_COORDINATE" {
		t.Fatalf("%v", p)
	}
	if r := w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", id)), w.member, map[string]any{"deliveryPolicy": "MANUAL_PICKUP"}); r.Status != http.StatusForbidden || r.errCode(t) != "SHOP_FORBIDDEN" {
		t.Fatalf("member: %d %s", r.Status, r.Body)
	}
	if r := w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", id)), w.admin, map[string]any{"deliveryPolicy": "DRONE"}); r.Status != http.StatusBadRequest {
		t.Fatalf("unknown policy: %d %s", r.Status, r.Body)
	}
	up := w.expect(w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", id)), w.admin, map[string]any{"deliveryPolicy": "MANUAL_PICKUP"}), http.StatusOK, "switch").JSON(t)
	if up["product"].(map[string]any)["deliveryPolicy"] != "MANUAL_PICKUP" {
		t.Fatalf("%v", up)
	}
}

func TestShopDeliveryPersistenceFailureRollsBackThePurchase(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Rollback Rita")
	w.grant(w.a1, pid, 1000)
	item := w.product(w.a1, "Fragile", 100, map[string]any{"stockMode": "FINITE", "stockQuantity": 3})
	ctx := context.Background()
	// Make the delivery insert fail for this installation only (a stand-in for any persistence error).
	fn := fmt.Sprintf("test_fail_delivery_%d", w.a1.InstallationID)
	if _, err := w.a.DB.Pool.Exec(ctx, fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
BEGIN IF NEW.installation_id = %d THEN RAISE EXCEPTION 'simulated delivery persistence failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql`, fn, w.a1.InstallationID)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON shop_deliveries FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn)); err != nil {
		t.Fatal(err)
	}
	dropped := false
	drop := func() {
		if !dropped {
			dropped = true
			_, _ = w.a.DB.Pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON shop_deliveries; DROP FUNCTION IF EXISTS %s()`, fn, fn))
		}
	}
	t.Cleanup(drop)

	before := w.snapshot(w.a1, player)
	key := idemKeyFor("fail")
	if r := w.buy(w.a1, player, item, 1, key); r.Status != http.StatusInternalServerError {
		t.Fatalf("expected the purchase to fail: %d %s", r.Status, r.Body)
	}
	if after := w.snapshot(w.a1, player); after != before {
		t.Fatalf("no purchase, no debit, no delivery may survive: %+v -> %+v", before, after)
	}
	if q, _ := w.stockOf(w.a1, item); q != 3 {
		t.Fatalf("no stock deduction: %v", q)
	}
	drop()
	// The key was never consumed: the same request succeeds once the failure is gone.
	r := w.expect(w.buy(w.a1, player, item, 1, key), http.StatusCreated, "retry after the failure").JSON(t)
	if deliveryOf(t, r["purchase"].(map[string]any))["status"] != "MANUAL_READY" || w.balanceOf(w.a1, player) != 900 {
		t.Fatalf("%v", r)
	}
	w.allIntegrity(w.a1)
}

func TestShopDeliveryIdempotency(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Retry Rex")
	w.grant(w.a1, pid, 5000)
	w.expect(w.setMap(w.a1, "chernarusplus"), http.StatusOK, "map")
	item := w.product(w.a1, "Idem Drop", 100, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE"})
	key := idemKeyFor("idem")
	at := map[string]any{"x": 1000.5, "z": 2000.25}

	first := w.expect(w.buyAt(w.a1, player, item, 1, key, at), http.StatusCreated, "first").JSON(t)
	again := w.expect(w.buyAt(w.a1, player, item, 1, key, at), http.StatusOK, "identical replay").JSON(t)
	if again["duplicate"] != true || purchaseID(again) != purchaseID(first) ||
		deliveryOf(t, again["purchase"].(map[string]any))["id"] != deliveryOf(t, first["purchase"].(map[string]any))["id"] {
		t.Fatalf("replay returns the existing purchase and delivery: %v", again)
	}
	// A reused key with different delivery instructions never alters the order.
	for name, d := range map[string]any{
		"moved x":        map[string]any{"x": 1000.75, "z": 2000.25},
		"moved z":        map[string]any{"x": 1000.5, "z": 1},
		"no coordinates": nil,
	} {
		if r := w.buyAt(w.a1, player, item, 1, key, d); r.Status != http.StatusConflict || r.errCode(t) != "DUPLICATE_PURCHASE" {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
	}
	if r := w.buyAt(w.a1, player, item, 2, key, at); r.errCode(t) != "DUPLICATE_PURCHASE" {
		t.Fatalf("other quantity: %s", r.Body)
	}
	stored := deliveryOf(t, w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/me/purchases/%d", purchaseID(first))), player)["purchase"].(map[string]any))
	if c := stored["coordinates"].(map[string]any); c["x"].(float64) != 1000.5 || c["z"].(float64) != 2000.25 {
		t.Fatalf("the stored instructions are unchanged: %v", stored)
	}
	// The replay still works after the product's policy changes: the original is returned as-is.
	w.expect(w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", item)), w.admin, map[string]any{"deliveryPolicy": "MANUAL_PICKUP"}), http.StatusOK, "policy change")
	w.expect(w.buyAt(w.a1, player, item, 1, key, at), http.StatusOK, "replay after the policy change")
	if n := w.count(`SELECT COUNT(*) FROM shop_deliveries WHERE purchase_id=$1`, purchaseID(first)); n != 1 {
		t.Fatalf("one delivery per purchase: %d", n)
	}
	if w.balanceOf(w.a1, player) != 4900 {
		t.Fatal("charged exactly once")
	}
	// 20 concurrent identical coordinate requests: one purchase, one delivery, one debit.
	burst := idemKeyFor("burst")
	item2 := w.product(w.a1, "Burst Drop", 50, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE"})
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := w.buyAt(w.a1, player, item2, 1, burst, at)
			mu.Lock()
			codes[r.Status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[http.StatusCreated] != 1 || codes[http.StatusOK] != 19 {
		t.Fatalf("burst: %v", codes)
	}
	if n := w.count(`SELECT COUNT(*) FROM shop_deliveries d JOIN shop_purchase_items i ON i.purchase_id = d.purchase_id WHERE i.product_id=$1`, item2); n != 1 {
		t.Fatalf("deliveries for the burst: %d", n)
	}
	w.allIntegrity(w.a1)
}

func TestShopDeliveryConcurrentPurchasesNeverOversell(t *testing.T) {
	w := newFactionWorld(t)
	w.expect(w.setMap(w.a1, "enoch"), http.StatusOK, "map")
	item := w.product(w.a1, "Scarce Drop", 100, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE", "stockMode": "FINITE", "stockQuantity": 3})
	buyers := w.players[:6]
	for i, b := range buyers {
		w.grant(w.a1, w.linkPlayer(w.a1, b, fmt.Sprintf("Racer %d", i)), 1000)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	created, sold := 0, 0
	for i, b := range buyers {
		wg.Add(1)
		go func(i int, b string) {
			defer wg.Done()
			r := w.buyAt(w.a1, b, item, 1, idemKeyFor("race"), map[string]any{"x": float64(100 * (i + 1)), "z": 50.5})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case r.Status == http.StatusCreated:
				created++
			case r.Status == http.StatusConflict && r.errCode(t) == "OUT_OF_STOCK":
				sold++
			default:
				t.Errorf("race: %d %s", r.Status, r.Body)
			}
		}(i, b)
	}
	wg.Wait()
	if created != 3 || sold != 3 {
		t.Fatalf("created %d, sold out %d", created, sold)
	}
	if q, _ := w.stockOf(w.a1, item); q != 0 {
		t.Fatalf("stock: %v", q)
	}
	if n := w.count(`SELECT COUNT(*) FROM shop_deliveries d JOIN shop_purchase_items i ON i.purchase_id = d.purchase_id WHERE i.product_id=$1 AND d.map_key='enoch'`, item); n != 3 {
		t.Fatalf("one delivery per sold unit: %d", n)
	}
	w.allIntegrity(w.a1)
}

func TestShopDeliveryFulfillmentAndRefunds(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Lifecycle Lou")
	w.grant(w.a1, pid, 100_000)
	w.expect(w.setMap(w.a1, "chernarusplus"), http.StatusOK, "map")
	item := w.product(w.a1, "Lifecycle Drop", 100, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE", "stockMode": "FINITE", "stockQuantity": 100})
	at := map[string]any{"x": 5000, "z": 6000}
	buy := func() int64 {
		return purchaseID(w.expect(w.buyAt(w.a1, player, item, 1, idemKeyFor("life"), at), http.StatusCreated, "buy").JSON(t))
	}
	fulfill := func(id int64) *apiResult {
		return w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", id)), w.admin, nil)
	}
	refund := func(id int64) *apiResult {
		return w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", id)), w.admin, map[string]any{"reason": "test"})
	}
	adminDelivery := func(purchase int64) map[string]any {
		return deliveryOf(t, w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/admin/purchases/%d", purchase)), w.admin)["purchase"].(map[string]any))
	}

	// Saving coordinates never fulfills anything.
	p1 := buy()
	if d := adminDelivery(p1); d["status"] != "MANUAL_READY" || d["fulfilledAt"] != nil {
		t.Fatalf("%v", d)
	}
	// 10 concurrent fulfills: exactly one succeeds; purchase and delivery move together.
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := fulfill(p1)
			mu.Lock()
			codes[r.Status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[http.StatusOK] != 1 || codes[http.StatusConflict] != 9 {
		t.Fatalf("concurrent fulfill: %v", codes)
	}
	d := adminDelivery(p1)
	if d["status"] != "FULFILLED" || d["fulfilledAt"] == nil || d["fulfilledByDiscordUserId"] != w.admin {
		t.Fatalf("fulfilled delivery: %v", d)
	}
	// Refunding a delivered order keeps its historical fulfillment.
	w.expect(refund(p1), http.StatusOK, "refund after delivery")
	d = adminDelivery(p1)
	if d["status"] != "FULFILLED" || d["fulfilledAt"] == nil || d["cancelledAt"] != nil {
		t.Fatalf("delivered-then-refunded keeps its fulfillment: %v", d)
	}
	stockAfterP1, _ := w.stockOf(w.a1, item)

	// Refunding an undelivered order cancels its delivery atomically; it can never be fulfilled after.
	p2 := buy()
	w.expect(refund(p2), http.StatusOK, "refund before delivery")
	d = adminDelivery(p2)
	if d["status"] != "CANCELLED" || d["cancelReason"] != "REFUNDED" || d["cancelledByDiscordUserId"] != w.admin || d["fulfilledAt"] != nil {
		t.Fatalf("cancelled delivery: %v", d)
	}
	if r := fulfill(p2); r.Status != http.StatusConflict || r.errCode(t) != "INVALID_PURCHASE_STATUS" {
		t.Fatalf("a refunded order must not be fulfillable: %d %s", r.Status, r.Body)
	}
	if q, _ := w.stockOf(w.a1, item); q != stockAfterP1 {
		t.Fatalf("an undelivered refund restocks (bought 1, returned 1): %v vs %v", q, stockAfterP1)
	}
	// Concurrent duplicate refunds: exactly one.
	p3 := buy()
	codes = map[int]int{}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := refund(p3)
			mu.Lock()
			codes[r.Status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[http.StatusOK] != 1 || codes[http.StatusConflict] != 7 {
		t.Fatalf("concurrent refunds: %v", codes)
	}
	// Fulfill racing refund: whatever wins, purchase and delivery always agree (reconciliation below).
	for i := 0; i < 6; i++ {
		p := buy()
		var fr, rr *apiResult
		wg.Add(2)
		go func() { defer wg.Done(); fr = fulfill(p) }()
		go func() { defer wg.Done(); rr = refund(p) }()
		wg.Wait()
		if rr.Status != http.StatusOK {
			t.Fatalf("refund must succeed (from either state): %d %s", rr.Status, rr.Body)
		}
		d := adminDelivery(p)
		switch fr.Status {
		case http.StatusOK: // fulfilled first, then refunded: history kept
			if d["status"] != "FULFILLED" {
				t.Fatalf("%v", d)
			}
		case http.StatusConflict: // refunded first: cancelled, never fulfilled
			if d["status"] != "CANCELLED" || d["fulfilledAt"] != nil {
				t.Fatalf("%v", d)
			}
		default:
			t.Fatalf("fulfill: %d %s", fr.Status, fr.Body)
		}
	}
	w.allIntegrity(w.a1)
}

func TestShopDeliveryQueuePrivacyAndTenantIsolation(t *testing.T) {
	w := newFactionWorld(t)
	alice, bob := w.players[0], w.players[1]
	pa := w.linkPlayer(w.a1, alice, "Alice Q")
	pb := w.linkPlayer(w.a1, bob, "Bob Q")
	w.grant(w.a1, pa, 10_000)
	w.grant(w.a1, pb, 10_000)
	w.expect(w.setMap(w.a1, "chernarusplus"), http.StatusOK, "map")
	coord := w.product(w.a1, "Queue Drop", 10, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE"})
	pickup := w.product(w.a1, "Queue Pickup", 10, nil)
	var alicePurchases []int64
	for i := 0; i < 3; i++ {
		alicePurchases = append(alicePurchases, purchaseID(w.expect(w.buyAt(w.a1, alice, coord, 1, idemKeyFor("qa"), map[string]any{"x": 10 + i, "z": 20}), http.StatusCreated, "alice").JSON(t)))
	}
	bobResp := w.expect(w.buy(w.a1, bob, pickup, 2, idemKeyFor("qb")), http.StatusCreated, "bob").JSON(t)
	bobDelivery := int64(deliveryOf(t, bobResp["purchase"].(map[string]any))["id"].(float64))
	w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", alicePurchases[0])), w.admin, nil), http.StatusOK, "fulfill one")

	// Another tenant: installation b1 with its own order.
	carol := w.players[2]
	pc := w.linkPlayer(w.b1, carol, "Carol B")
	w.grantAs(w.b1, pc, 1000)
	other := w.product(w.b1, "Other Tenant Item", 10, nil)
	otherResp := w.expect(w.buy(w.b1, carol, other, 1, idemKeyFor("qc")), http.StatusCreated, "b1 purchase").JSON(t)
	otherDelivery := int64(deliveryOf(t, otherResp["purchase"].(map[string]any))["id"].(float64))

	// The queue: installation-scoped, newest first, filterable, paginated without gaps or repeats.
	q := w.getJSON(w.shopPath(w.a1, "/admin/deliveries"), w.admin)
	items := q["items"].([]any)
	if len(items) != 4 || q["currency"] == nil {
		t.Fatalf("queue: %v", q)
	}
	first := items[0].(map[string]any)
	if int64(first["id"].(float64)) != bobDelivery || first["player"].(map[string]any)["gamertag"] != "Bob Q" || first["policy"] != "MANUAL_PICKUP" ||
		first["purchaseStatus"] != "PENDING_FULFILLMENT" || first["items"].([]any)[0].(map[string]any)["quantity"].(float64) != 2 ||
		first["items"].([]any)[0].(map[string]any)["productName"] != "Queue Pickup" {
		t.Fatalf("queue entry: %v", first)
	}
	for _, it := range items {
		if int64(it.(map[string]any)["id"].(float64)) == otherDelivery {
			t.Fatal("another installation's delivery leaked into the queue")
		}
	}
	ready := w.getJSON(w.shopPath(w.a1, "/admin/deliveries?status=MANUAL_READY&policy=MANUAL_COORDINATE"), w.admin)["items"].([]any)
	if len(ready) != 2 {
		t.Fatalf("filtered queue: %d", len(ready))
	}
	for _, it := range ready {
		m := it.(map[string]any)
		if m["status"] != "MANUAL_READY" || m["coordinates"] == nil || m["map"].(map[string]any)["key"] != "chernarusplus" {
			t.Fatalf("%v", m)
		}
	}
	if n := len(w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/admin/deliveries?playerId=%d", pb)), w.admin)["items"].([]any)); n != 1 {
		t.Fatalf("player filter: %d", n)
	}
	seen := map[int64]bool{}
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		path := "/admin/deliveries?limit=1"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		page := w.getJSON(w.shopPath(w.a1, path), w.admin)
		for _, it := range page["items"].([]any) {
			id := int64(it.(map[string]any)["id"].(float64))
			if seen[id] {
				t.Fatalf("repeated %d", id)
			}
			seen[id] = true
		}
		next, _ := page["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 4 {
		t.Fatalf("pagination saw %d", len(seen))
	}
	for _, bad := range []string{"?status=FILE_STAGED", "?status=SHIPPED", "?policy=DRONE", "?cursor=zzz", "?limit=0", "?playerId=x"} {
		if r := w.do(http.MethodGet, w.shopPath(w.a1, "/admin/deliveries"+bad), w.admin, nil); r.Status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, r.Status, r.Body)
		}
	}

	// Authorization and tenant isolation.
	for _, actor := range []string{w.member, alice, w.b1.OwnerDiscordID} {
		if r := w.do(http.MethodGet, w.shopPath(w.a1, "/admin/deliveries"), actor, nil); r.Status != http.StatusForbidden {
			t.Errorf("%s reading a1's queue: %d", actor, r.Status)
		}
	}
	if r := w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/admin/deliveries/%d", otherDelivery)), w.admin, nil); r.Status != http.StatusNotFound || r.errCode(t) != "DELIVERY_NOT_FOUND" {
		t.Fatalf("another installation's delivery by id: %d %s", r.Status, r.Body)
	}
	det := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/admin/deliveries/%d", bobDelivery)), w.admin)["delivery"].(map[string]any)
	if det["player"].(map[string]any)["gamertag"] != "Bob Q" || det["purchaseId"] == nil {
		t.Fatalf("%v", det)
	}

	// Player privacy: own delivery only, and no admin-only fields.
	own := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/me/deliveries/%d", bobDelivery)), bob)["delivery"].(map[string]any)
	for _, k := range []string{"player", "fulfilledByDiscordUserId", "cancelledByDiscordUserId", "cancelReason", "items", "gameServerId"} {
		if _, leaked := own[k]; leaked {
			t.Errorf("player view must not include %q: %v", k, own)
		}
	}
	if r := w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/me/deliveries/%d", bobDelivery)), alice, nil); r.Status != http.StatusNotFound || r.errCode(t) != "DELIVERY_NOT_FOUND" {
		t.Fatalf("another player's delivery: %d %s", r.Status, r.Body)
	}
	if r := w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/me/deliveries/%d", otherDelivery)), carol, nil); r.Status != http.StatusConflict && r.Status != http.StatusNotFound && r.Status != http.StatusForbidden {
		t.Fatalf("a b1 player reading through a1: %d %s", r.Status, r.Body)
	}
	fulfilled := deliveryOf(t, w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/me/purchases/%d", alicePurchases[0])), alice)["purchase"].(map[string]any))
	if fulfilled["status"] != "FULFILLED" || fulfilled["fulfilledAt"] == nil {
		t.Fatalf("%v", fulfilled)
	}
	if _, leaked := fulfilled["fulfilledByDiscordUserId"]; leaked {
		t.Fatal("the purchase view hides who fulfilled it")
	}
	if r := w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/me/deliveries/%d", bobDelivery)), w.players[3], nil); r.Status != http.StatusConflict || r.errCode(t) != "PLAYER_IDENTITY_REQUIRED" {
		t.Fatalf("an unlinked user: %d %s", r.Status, r.Body)
	}
	w.allIntegrity(w.a1)
}

// grantAs grants on an installation whose admin is its owner (b1).
func (w *factionWorld) grantAs(f installationFixture, account int64, amount int64) {
	w.t.Helper()
	w.expect(w.adjust(f, "grant", account, w.adminFor(f), map[string]any{"amount": amount, "reason": "test funds"}), http.StatusOK, "grant")
}

func TestShopDeliveryBackfillHealAndRestartSafety(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Legacy Lee")
	w.grant(w.a1, pid, 10_000)
	item := w.product(w.a1, "Legacy Item", 100, map[string]any{"stockMode": "FINITE", "stockQuantity": 50})
	ctx := context.Background()
	open := purchaseID(w.buyOK(w.a1, player, item, 1))
	delivered := purchaseID(w.buyOK(w.a1, player, item, 1))
	deliveredRefunded := purchaseID(w.buyOK(w.a1, player, item, 1))
	refunded := purchaseID(w.buyOK(w.a1, player, item, 1))
	for _, id := range []int64{delivered, deliveredRefunded} {
		w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", id)), w.admin, nil), http.StatusOK, "fulfill")
	}
	for _, id := range []int64{deliveredRefunded, refunded} {
		w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", id)), w.admin, map[string]any{"reason": "x"}), http.StatusOK, "refund")
	}
	statusBefore := map[int64]string{}
	for _, id := range []int64{open, delivered, deliveredRefunded, refunded} {
		var s string
		if err := w.a.DB.Pool.QueryRow(ctx, `SELECT status FROM shop_purchases WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		statusBefore[id] = s
	}
	// Simulate pre-0049 history: these purchases have no delivery record.
	if _, err := w.a.DB.Pool.Exec(ctx, `DELETE FROM shop_deliveries WHERE purchase_id = ANY($1)`, []int64{open, delivered, deliveredRefunded, refunded}); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, m := range w.shopReconcile(w.a1) {
		kinds[m.Kind]++
	}
	if kinds[repository.ShopMismatchMissingDelivery] != 4 {
		t.Fatalf("reconciliation reports missing deliveries: %v", kinds)
	}
	// The migration's backfill reconstructs them without touching the purchases.
	if _, err := w.a.DB.Pool.Exec(ctx, database.ShopDeliveryBackfillSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, database.ShopDeliveryBackfillSQL); err != nil {
		t.Fatal("the backfill is idempotent: ", err)
	}
	want := map[int64]string{open: "MANUAL_READY", delivered: "FULFILLED", deliveredRefunded: "FULFILLED", refunded: "CANCELLED"}
	for id, st := range want {
		var got, policy string
		var n int64
		if err := w.a.DB.Pool.QueryRow(ctx, `SELECT status, delivery_policy, (SELECT COUNT(*) FROM shop_deliveries WHERE purchase_id=$1) FROM shop_deliveries WHERE purchase_id=$1`, id).Scan(&got, &policy, &n); err != nil {
			t.Fatal(err)
		}
		if got != st || policy != "MANUAL_PICKUP" || n != 1 {
			t.Errorf("purchase %d: delivery %s (%s, %d rows), want %s", id, got, policy, n, st)
		}
		var s string
		_ = w.a.DB.Pool.QueryRow(ctx, `SELECT status FROM shop_purchases WHERE id=$1`, id).Scan(&s)
		if s != statusBefore[id] {
			t.Errorf("the backfill must not change purchase %d: %s -> %s", id, statusBefore[id], s)
		}
	}
	w.allIntegrity(w.a1)

	// Heal: a purchase written by an older instance (no delivery row) is still fulfillable.
	late := purchaseID(w.buyOK(w.a1, player, item, 1))
	if _, err := w.a.DB.Pool.Exec(ctx, `DELETE FROM shop_deliveries WHERE purchase_id=$1`, late); err != nil {
		t.Fatal(err)
	}
	w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", late)), w.admin, nil), http.StatusOK, "fulfill a healed purchase")
	late2 := purchaseID(w.buyOK(w.a1, player, item, 1))
	if _, err := w.a.DB.Pool.Exec(ctx, `DELETE FROM shop_deliveries WHERE purchase_id=$1`, late2); err != nil {
		t.Fatal(err)
	}
	w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", late2)), w.admin, map[string]any{"reason": "late"}), http.StatusOK, "refund a healed purchase")
	w.allIntegrity(w.a1)

	// Restart safety: the queue is database state, so a freshly constructed service (a new process)
	// sees exactly the same open delivery and can fulfill it.
	pending := purchaseID(w.buyOK(w.a1, player, item, 1))
	scope, err := w.a.EconomyAccounts.Scope(ctx, w.a1.OrgID, w.a1.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	restarted := shop.NewService(repository.NewShopRepository(w.a.DB.Pool), w.a.EconomyAccounts, nil)
	page, err := restarted.AdminDeliveries(ctx, scope, shop.DeliveryList{Status: shop.DeliveryReady})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range page.Items {
		found = found || d.PurchaseID == pending
	}
	if !found {
		t.Fatal("an open delivery must survive a restart")
	}
	w.a.Shop = restarted
	w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", pending)), w.admin, nil), http.StatusOK, "fulfill after restart")

	// Automatic delivery is disabled: the database refuses every reserved state.
	for _, st := range []string{"QUEUED", "FILE_STAGED", "AWAITING_RESTART", "RESTART_OBSERVED", "VERIFICATION_REQUIRED", "FAILED_REVIEW"} {
		if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE shop_deliveries SET status=$2 WHERE purchase_id=$1`, open, st); err == nil {
			t.Fatalf("%s must be refused until Phase 2B", st)
		}
	}
	// A coordinate delivery without coordinates (or with an altitude-free NaN) is refused by the database too.
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE shop_deliveries SET delivery_policy='MANUAL_COORDINATE' WHERE purchase_id=$1`, open); err == nil {
		t.Fatal("MANUAL_COORDINATE needs a map and coordinates")
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE shop_deliveries SET delivery_policy='MANUAL_COORDINATE', map_key='chernarusplus', coord_x='NaN', coord_z=1 WHERE purchase_id=$1`, open); err == nil {
		t.Fatal("NaN coordinates must be refused")
	}
	w.allIntegrity(w.a1)
}
