//go:build integration

package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// End-to-end Champion Shop tests over the real routes and a real PostgreSQL (docs/SHOP.md): catalog and
// admin management, the atomic purchase with the Champion Points debit, insufficient funds, idempotency,
// stock and purchase limits under concurrency, history, fulfillment, refunds, tenant isolation, the shared
// guild balance, reconciliation, audit, rate limits, the ECONOMY notifier and catalog volume.

func (w *factionWorld) shopPath(f installationFixture, suffix string) string {
	return fmt.Sprintf("/api/saas/organizations/%d/installations/%d/shop%s", f.OrgID, f.InstallationID, suffix)
}

// product creates an active MANUAL product as the org admin and returns its id.
func (w *factionWorld) product(f installationFixture, name string, price int64, extra map[string]any) int64 {
	w.t.Helper()
	body := map[string]any{"name": name, "description": "About " + name, "pricePoints": price, "productType": "ITEM"}
	for k, v := range extra {
		body[k] = v
	}
	actor := w.admin
	if f.OrgID == w.b1.OrgID {
		actor = w.b1.OwnerDiscordID
	}
	r := w.expect(w.do(http.MethodPost, w.shopPath(f, "/products"), actor, body), http.StatusCreated, "create product "+name)
	return int64(r.JSON(w.t)["product"].(map[string]any)["id"].(float64))
}

var buySeq atomic.Int64

func idemKeyFor(prefix string) string {
	n := buySeq.Add(1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

func (w *factionWorld) buy(f installationFixture, actor string, product int64, qty any, key string) *apiResult {
	w.t.Helper()
	return w.do(http.MethodPost, w.shopPath(f, "/purchases"), actor, map[string]any{"productId": product, "quantity": qty, "idempotencyKey": key})
}

func (w *factionWorld) buyOK(f installationFixture, actor string, product int64, qty int) map[string]any {
	w.t.Helper()
	r := w.expect(w.buy(f, actor, product, qty, idemKeyFor("buy")), http.StatusCreated, "purchase")
	return r.JSON(w.t)
}

func purchaseID(resp map[string]any) int64 {
	return int64(resp["purchase"].(map[string]any)["id"].(float64))
}

func (w *factionWorld) stockOf(f installationFixture, product int64) (float64, bool) {
	w.t.Helper()
	p := w.getJSON(w.shopPath(f, fmt.Sprintf("/admin/products/%d", product)), w.adminFor(f))["product"].(map[string]any)
	q, ok := p["stockQuantity"].(float64)
	return q, ok
}

func (w *factionWorld) adminFor(f installationFixture) string {
	if f.OrgID == w.b1.OrgID {
		return w.b1.OwnerDiscordID
	}
	return w.admin
}

func (w *factionWorld) shopReconcile(f installationFixture) []repository.ShopMismatch {
	w.t.Helper()
	scope, err := w.a.EconomyAccounts.Scope(context.Background(), f.OrgID, f.InstallationID)
	if err != nil {
		w.t.Fatal(err)
	}
	m, err := w.a.Shop.Reconcile(context.Background(), scope, 50)
	if err != nil {
		w.t.Fatal(err)
	}
	return m
}

func (w *factionWorld) allIntegrity(f installationFixture) {
	w.t.Helper()
	w.ledgerIntegrity(f)
	if m := w.shopReconcile(f); len(m) != 0 {
		w.t.Fatalf("shop reconciliation: %+v", m)
	}
}

func TestShopCatalogAndAdminManagement(t *testing.T) {
	w := newFactionWorld(t)
	reader, leader := w.players[0], w.players[1]
	w.linkPlayer(w.a1, leader, "Faction Boss")
	w.createFaction(w.a1, leader, "Shop Faction", "SHP", "OPEN") // a faction role grants no shop rights

	// Categories: create (slug from the name), update, disable.
	cr := w.expect(w.do(http.MethodPost, w.shopPath(w.a1, "/categories"), w.admin, map[string]any{"name": "Weapons & Gear", "description": "Guns", "sortOrder": 1}), http.StatusCreated, "category").JSON(t)
	cat := cr["category"].(map[string]any)
	if cat["slug"] != "weapons-gear" || cat["isActive"] != true {
		t.Fatalf("category: %v", cat)
	}
	catID := int64(cat["id"].(float64))
	cat2 := w.expect(w.do(http.MethodPost, w.shopPath(w.a1, "/categories"), w.admin, map[string]any{"name": "Weapons & Gear"}), http.StatusCreated, "same name").JSON(t)["category"].(map[string]any)
	if cat2["slug"] != "weapons-gear-2" {
		t.Fatalf("slugs are unique per installation: %v", cat2["slug"])
	}
	vehicles := w.expect(w.do(http.MethodPost, w.shopPath(w.a1, "/categories"), w.admin, map[string]any{"name": "Vehicles", "sortOrder": 2}), http.StatusCreated, "vehicles").JSON(t)["category"].(map[string]any)
	vehID := int64(vehicles["id"].(float64))

	// Products.
	m4 := w.product(w.a1, "M4-A1 Rifle", 500, map[string]any{"categoryId": catID, "isFeatured": true, "stockMode": "FINITE", "stockQuantity": 30, "purchaseLimit": 2, "description": "Reliable ASSAULT rifle"})
	ak := w.product(w.a1, "AKM", 400, map[string]any{"categoryId": catID, "sortOrder": 5})
	dup := w.product(w.a1, "AKM", 450, map[string]any{"categoryId": catID, "sortOrder": 6})
	truck := w.product(w.a1, "Olga Truck", 9000, map[string]any{"categoryId": vehID, "productType": "VEHICLE"})
	hidden := w.product(w.a1, "Hidden Gem", 100, map[string]any{"isActive": false})
	uncategorised := w.product(w.a1, "Bandage Pack", 20, nil)

	slugs := map[int64]string{}
	for _, id := range []int64{ak, dup} {
		slugs[id] = w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/admin/products/%d", id)), w.admin)["product"].(map[string]any)["slug"].(string)
	}
	if slugs[ak] != "akm" || slugs[dup] != "akm-2" {
		t.Fatalf("product slugs: %v", slugs)
	}

	// Player catalog: active products only, featured first, then sortOrder, name, id.
	list := w.getJSON(w.shopPath(w.a1, "/products"), reader)
	names := []string{}
	for _, it := range list["items"].([]any) {
		names = append(names, it.(map[string]any)["name"].(string))
	}
	if fmt.Sprint(names) != "[M4-A1 Rifle Bandage Pack Olga Truck AKM AKM]" {
		t.Fatalf("catalog order: %v", names)
	}
	if list["currency"].(map[string]any)["code"] != "CHAMPION_POINTS" || list["limit"].(float64) != 25 || list["nextCursor"] != nil {
		t.Fatalf("list envelope: %v", list)
	}
	first := list["items"].([]any)[0].(map[string]any)
	if first["pricePoints"].(float64) != 500 || first["isFeatured"] != true || first["productType"] != "ITEM" || first["deliveryType"] != "MANUAL" ||
		first["stockState"] != "IN_STOCK" || first["purchaseLimit"].(float64) != 2 || first["category"].(map[string]any)["slug"] != "weapons-gear" {
		t.Fatalf("public product: %v", first)
	}
	if u := list["items"].([]any)[1].(map[string]any); u["category"] != nil || u["stockState"] != "UNLIMITED" || u["purchaseLimit"] != nil {
		t.Fatalf("uncategorised unlimited product: %v", u)
	}
	// Public DTOs never carry admin-only data.
	raw := w.do(http.MethodGet, w.shopPath(w.a1, "/products"), reader, nil).Body
	for _, banned := range []string{"stockQuantity", "isActive", "stockMode", "createdAt", "updatedAt", "organizationId", "installationId", "sortOrder", "imageKey", "Hidden Gem"} {
		if bytes.Contains(raw, []byte(banned)) {
			t.Errorf("the public catalog must not contain %q", banned)
		}
	}
	// Filters: category slug, search (name and description, case-insensitive, wildcards literal).
	if got := w.getJSON(w.shopPath(w.a1, "/products?category=vehicles"), reader)["items"].([]any); len(got) != 1 || got[0].(map[string]any)["name"] != "Olga Truck" {
		t.Fatalf("category filter: %v", got)
	}
	if got := w.getJSON(w.shopPath(w.a1, "/products?q=assault"), reader)["items"].([]any); len(got) != 1 {
		t.Fatalf("description search: %v", got)
	}
	if got := w.getJSON(w.shopPath(w.a1, "/products?q="+url.QueryEscape("akm")), reader)["items"].([]any); len(got) != 2 {
		t.Fatalf("name search: %v", got)
	}
	for _, q := range []string{"%", "_", "a%m"} {
		if got := w.getJSON(w.shopPath(w.a1, "/products?q="+url.QueryEscape(q)), reader)["items"].([]any); len(got) != 0 {
			t.Errorf("q=%q must be literal: %v", q, got)
		}
	}
	if cats := w.getJSON(w.shopPath(w.a1, "/categories"), reader)["items"].([]any); len(cats) != 3 || cats[1].(map[string]any)["productCount"].(float64) != 3 {
		t.Fatalf("categories with product counts: %v", cats)
	}
	// Detail: public fields for an active product, 404 for a disabled one.
	if d := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/products/%d", m4)), reader)["product"].(map[string]any); d["name"] != "M4-A1 Rifle" {
		t.Fatalf("detail: %v", d)
	}
	if r := w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/products/%d", hidden)), reader, nil); r.Status != http.StatusNotFound || r.errCode(t) != "SHOP_PRODUCT_NOT_FOUND" {
		t.Fatalf("a disabled product is not public: %d %s", r.Status, r.Body)
	}
	// Admin sees everything, including stock and inactive products.
	al := w.getJSON(w.shopPath(w.a1, "/admin/products"), w.admin)["items"].([]any)
	if len(al) != 6 {
		t.Fatalf("admin list: %d", len(al))
	}
	if q, ok := w.stockOf(w.a1, m4); !ok || q != 30 {
		t.Fatalf("admin stock: %v", q)
	}

	// Update: price, description, clear the category and the limit, disable.
	upd := w.expect(w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", m4)), w.admin, map[string]any{"pricePoints": 600, "categoryId": nil, "purchaseLimit": nil, "name": "M4-A1 Carbine"}), http.StatusOK, "update").JSON(t)["product"].(map[string]any)
	if upd["pricePoints"].(float64) != 600 || upd["category"] != nil || upd["purchaseLimit"] != nil || upd["name"] != "M4-A1 Carbine" || upd["slug"] != "m4-a1-rifle" {
		t.Fatalf("update (the slug is stable): %v", upd)
	}
	w.expect(w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", truck)), w.admin, map[string]any{"isActive": false}), http.StatusOK, "disable")
	if got := w.getJSON(w.shopPath(w.a1, "/products?category=vehicles"), reader)["items"].([]any); len(got) != 0 {
		t.Fatal("a disabled product leaves the player catalog")
	}
	// A disabled category hides its products from players.
	w.expect(w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/categories/%d", catID)), w.admin, map[string]any{"isActive": false}), http.StatusOK, "disable category")
	if got := w.getJSON(w.shopPath(w.a1, "/products?q=akm"), reader)["items"].([]any); len(got) != 0 {
		t.Fatal("products of a disabled category are hidden")
	}
	if r := w.buy(w.a1, reader, ak, 1, idemKeyFor("cat")); r.errCode(t) == "" {
		t.Fatal("a product of a disabled category cannot be bought")
	}
	_ = uncategorised

	// Validation.
	bad := []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"name": "x", "pricePoints": 0, "productType": "ITEM"}, "price"},
		{map[string]any{"name": "x", "pricePoints": -5, "productType": "ITEM"}, "price"},
		{map[string]any{"name": "x", "pricePoints": 1_000_000_001, "productType": "ITEM"}, "price"},
		{map[string]any{"name": "", "pricePoints": 5, "productType": "ITEM"}, "name"},
		{map[string]any{"name": "x", "pricePoints": 5, "productType": "GUN"}, "type"},
		{map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM", "deliveryType": "DISCORD_ROLE"}, "reserved"},
		{map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM", "deliveryType": "IN_GAME_FUTURE"}, "reserved"},
		{map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM", "stockMode": "FINITE"}, "stock"},
		{map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM", "stockMode": "FINITE", "stockQuantity": -1}, "stock"},
		{map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM", "stockMode": "UNLIMITED", "stockQuantity": 4}, "stock"},
		{map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM", "purchaseLimit": 0}, "limit"},
		{map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM", "imageUrl": "https://evil.example/x.png"}, ""},
		{map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM", "organizationId": 1}, ""},
		{map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM", "categoryId": 999999999}, ""},
	}
	for i, c := range bad {
		r := w.do(http.MethodPost, w.shopPath(w.a1, "/products"), w.admin, c.body)
		if r.Status != http.StatusBadRequest && r.Status != http.StatusNotFound {
			t.Errorf("case %d %v: %d %s", i, c.body, r.Status, r.Body)
		}
	}
	if r := w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", ak)), w.admin, map[string]any{"installationId": w.b1.InstallationID}); r.Status != http.StatusBadRequest {
		t.Fatalf("tenant reassignment must be rejected: %d %s", r.Status, r.Body)
	}
	if r := w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", ak)), w.admin, map[string]any{"stockMode": "FINITE"}); r.Status != http.StatusBadRequest {
		t.Fatalf("switching to FINITE stock needs a quantity: %d %s", r.Status, r.Body)
	}
	if r := w.do(http.MethodDelete, w.shopPath(w.a1, fmt.Sprintf("/products/%d", ak)), w.admin, nil); r.Status == http.StatusOK || r.Status == http.StatusNoContent {
		t.Fatalf("there is no product deletion (soft disable only): %d", r.Status)
	}
	// Authorization: a faction leader, a player and an org MEMBER cannot manage the shop.
	for _, actor := range []string{leader, reader, w.member} {
		for _, req := range []struct{ method, path string }{
			{http.MethodPost, "/products"}, {http.MethodPost, "/categories"}, {http.MethodGet, "/admin/products"}, {http.MethodGet, "/admin/categories"}, {http.MethodGet, "/admin/purchases"},
			{http.MethodPut, fmt.Sprintf("/products/%d", ak)}, {http.MethodPost, "/purchases/1/fulfill"}, {http.MethodPost, "/purchases/1/refund"},
		} {
			r := w.do(req.method, w.shopPath(w.a1, req.path), actor, map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM", "reason": "x"})
			if r.Status != http.StatusForbidden || r.errCode(t) != "SHOP_FORBIDDEN" {
				t.Errorf("%s %s as a non-admin: %d %s", req.method, req.path, r.Status, r.Body)
			}
		}
	}
	w.expect(w.do(http.MethodGet, w.shopPath(w.a1, "/products"), "", nil), http.StatusUnauthorized, "no acting user")
	// Parameter contract.
	for _, bad := range []string{"limit=0", "limit=-1", "limit=abc", "cursor=bogus", "q=" + strings.Repeat("x", 51), "q=a;b"} {
		w.expect(w.do(http.MethodGet, w.shopPath(w.a1, "/products?"+bad), reader, nil), http.StatusBadRequest, bad)
	}
	if got := w.getJSON(w.shopPath(w.a1, "/products?limit=100000"), reader)["limit"].(float64); got != 100 {
		t.Fatalf("limit clamps to 100: %v", got)
	}
}

func TestShopPurchaseIsAtomicAndUsesTheLedger(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Big Spender")
	rec := &ecoRecorder{}
	w.a.EconomyService.SetNotifier(rec)
	item := w.product(w.a1, "Care Package", 750, map[string]any{"stockMode": "FINITE", "stockQuantity": 10})
	w.grant(w.a1, pid, 1000)
	rec.mu.Lock()
	rec.events = nil // the grant's own event
	rec.mu.Unlock()

	resp := w.buyOK(w.a1, player, item, 1)
	pur := resp["purchase"].(map[string]any)
	if pur["status"] != "PENDING_FULFILLMENT" || pur["totalPoints"].(float64) != 750 || pur["deliveryType"] != "MANUAL" || pur["paidAt"] == nil || pur["fulfilledAt"] != nil {
		t.Fatalf("purchase: %v", pur)
	}
	if resp["remainingBalance"].(float64) != 250 || resp["duplicate"] != false || resp["currency"].(map[string]any)["code"] != "CHAMPION_POINTS" {
		t.Fatalf("response: %v", resp)
	}
	items := pur["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["productName"] != "Care Package" || items[0].(map[string]any)["unitPricePoints"].(float64) != 750 ||
		items[0].(map[string]any)["quantity"].(float64) != 1 || items[0].(map[string]any)["lineTotalPoints"].(float64) != 750 {
		t.Fatalf("item snapshot: %v", items)
	}
	if got := w.balanceOf(w.a1, player); got != 250 {
		t.Fatalf("the balance is the committed value: %d", got)
	}
	// One SHOP_PURCHASE debit, referencing the purchase, through the economy ledger.
	id := purchaseID(resp)
	var amount, after int64
	var ref, typ, by string
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT amount, balance_after, source_key, reason_type, COALESCE(created_by,'') FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND reason_type='SHOP_PURCHASE'`, w.guildOf(w.a1), pid).Scan(&amount, &after, &ref, &typ, &by); err != nil {
		t.Fatal(err)
	}
	if amount != -750 || after != 250 || ref != fmt.Sprintf("purchase:%d", id) || by != player {
		t.Fatalf("ledger debit: %d %d %s %s", amount, after, ref, by)
	}
	if q, _ := w.stockOf(w.a1, item); q != 9 {
		t.Fatalf("stock: %v", q)
	}
	// Economy history shows it as a Shop purchase (generated text, reference type SHOP, filterable).
	hist := w.getJSON(w.eco(w.a1, "/me/transactions?type=SHOP_PURCHASE"), player)["items"].([]any)
	if len(hist) != 1 || hist[0].(map[string]any)["description"] != "Shop purchase" || hist[0].(map[string]any)["referenceType"] != "SHOP" || hist[0].(map[string]any)["direction"] != "DEBIT" {
		t.Fatalf("economy history: %v", hist)
	}
	// One ECONOMY notification, after the commit, naming the public product only.
	evs := rec.list()
	if len(evs) != 1 || evs[0].Type != "SHOP_PURCHASE" || evs[0].Credit || evs[0].Amount != 750 || evs[0].BalanceAfter != 250 || evs[0].Item != "Care Package" || evs[0].PlayerName != "Big Spender" {
		t.Fatalf("economy event: %+v", evs)
	}
	w.allIntegrity(w.a1)

	// Insufficient funds: 409, and NOTHING remains - no purchase, no debit, no stock change.
	before := w.count(`SELECT COUNT(*) FROM shop_purchases WHERE installation_id=$1`, w.a1.InstallationID)
	rows := w.count(`SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1`, w.guildOf(w.a1))
	r := w.buy(w.a1, player, item, 1, idemKeyFor("poor"))
	if r.Status != http.StatusConflict || r.errCode(t) != "INSUFFICIENT_FUNDS" {
		t.Fatalf("insufficient funds: %d %s", r.Status, r.Body)
	}
	if w.count(`SELECT COUNT(*) FROM shop_purchases WHERE installation_id=$1`, w.a1.InstallationID) != before ||
		w.count(`SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1`, w.guildOf(w.a1)) != rows || w.balanceOf(w.a1, player) != 250 {
		t.Fatal("a refused purchase must leave no purchase, no ledger row and no balance change")
	}
	if q, _ := w.stockOf(w.a1, item); q != 9 {
		t.Fatalf("no stock decrement on failure: %v", q)
	}
	if len(rec.list()) != 1 {
		t.Fatal("a refused purchase is not announced")
	}
	// The snapshot survives editing and disabling the product.
	w.expect(w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", item)), w.admin, map[string]any{"name": "Renamed", "pricePoints": 1, "isActive": false}), http.StatusOK, "edit")
	got := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/me/purchases/%d", id)), player)["purchase"].(map[string]any)
	it := got["items"].([]any)[0].(map[string]any)
	if it["productName"] != "Care Package" || it["unitPricePoints"].(float64) != 750 || got["totalPoints"].(float64) != 750 {
		t.Fatalf("history must not change when the product does: %v", got)
	}
	// A disabled product cannot be bought.
	if r := w.buy(w.a1, player, item, 1, idemKeyFor("off")); r.Status != http.StatusConflict || r.errCode(t) != "SHOP_PRODUCT_DISABLED" {
		t.Fatalf("disabled: %d %s", r.Status, r.Body)
	}
}

func TestShopPurchaseValidationAndIdentity(t *testing.T) {
	w := newFactionWorld(t)
	player, unlinked := w.players[0], w.players[1]
	pid := w.linkPlayer(w.a1, player, "Validator")
	w.grant(w.a1, pid, 1_000_000)
	item := w.product(w.a1, "Widget", 10, nil)

	for _, q := range []any{0, -1, 101, 1000000, 1.5, "abc"} {
		r := w.buy(w.a1, player, item, q, idemKeyFor("q"))
		if r.Status != http.StatusBadRequest || r.errCode(t) != "INVALID_QUANTITY" && r.errCode(t) != "INVALID_REQUEST" {
			t.Errorf("quantity %v: %d %s", q, r.Status, r.Body)
		}
	}
	if r := w.buy(w.a1, player, item, 0, idemKeyFor("q")); r.errCode(t) != "INVALID_QUANTITY" {
		t.Fatalf("quantity 0 must be INVALID_QUANTITY: %s", r.Body)
	}
	for _, key := range []string{"", "short", strings.Repeat("k", 65), "bad key with spaces!"} {
		if r := w.buy(w.a1, player, item, 1, key); r.Status != http.StatusBadRequest {
			t.Errorf("key %q: %d %s", key, r.Status, r.Body)
		}
	}
	for _, body := range []any{`{"quantity":1,"idempotencyKey":"abcdefgh12"}`, `{"productId":"x","quantity":1,"idempotencyKey":"abcdefgh12"}`, `{"productId":1,"quantity":1,"idempotencyKey":"abcdefgh12","price":1}`, `{"productId":1,"quantity":1,"idempotencyKey":"abcdefgh12","userId":9}`} {
		if r := w.do(http.MethodPost, w.shopPath(w.a1, "/purchases"), player, body); r.Status != http.StatusBadRequest {
			t.Errorf("body %v: %d %s", body, r.Status, r.Body)
		}
	}
	if r := w.buy(w.a1, player, 999999999, 1, idemKeyFor("nf")); r.Status != http.StatusNotFound || r.errCode(t) != "SHOP_PRODUCT_NOT_FOUND" {
		t.Fatalf("unknown product: %d %s", r.Status, r.Body)
	}
	// A price the client sends is never used: the total comes from the product (extra fields are rejected above).
	// Overflow safety at the extremes: the most expensive product times the largest quantity.
	big := w.product(w.a1, "Bank", 1_000_000_000, nil)
	for i := 0; i < 10; i++ {
		w.grant(w.a1, pid, 1_000_000_000) // 10^10 + 10^6 in total
	}
	resp := w.expect(w.buy(w.a1, player, big, 100, idemKeyFor("max")), http.StatusConflict, "1e11 total exceeds the balance") // 100 x 1e9 = 1e11 > 1.0001e10
	if resp.errCode(t) != "INSUFFICIENT_FUNDS" {
		t.Fatalf("%s", resp.Body)
	}
	if got := w.buy(w.a1, player, big, 10, idemKeyFor("max")); got.Status != http.StatusCreated {
		t.Fatalf("10 x 1e9 = 1e10 is affordable: %d %s", got.Status, got.Body)
	}
	// Identity: an unlinked user cannot buy, cannot read history, and nothing is created.
	for _, r := range []*apiResult{w.buy(w.a1, unlinked, item, 1, idemKeyFor("u")), w.do(http.MethodGet, w.shopPath(w.a1, "/me/purchases"), unlinked, nil), w.do(http.MethodGet, w.shopPath(w.a1, "/me/purchases/1"), unlinked, nil)} {
		if r.Status != http.StatusConflict || r.errCode(t) != "PLAYER_IDENTITY_REQUIRED" {
			t.Errorf("unlinked user: %d %s", r.Status, r.Body)
		}
	}
	// The catalog itself is readable without a link.
	w.expect(w.do(http.MethodGet, w.shopPath(w.a1, "/products"), unlinked, nil), http.StatusOK, "browse without a link")
	w.allIntegrity(w.a1)
}

func TestShopIdempotency(t *testing.T) {
	w := newFactionWorld(t)
	a, b := w.players[0], w.players[1]
	pa := w.linkPlayer(w.a1, a, "Retry A")
	pb := w.linkPlayer(w.a1, b, "Retry B")
	w.grant(w.a1, pa, 5000)
	w.grant(w.a1, pb, 5000)
	one := w.product(w.a1, "Last One", 100, map[string]any{"stockMode": "FINITE", "stockQuantity": 1})
	two := w.product(w.a1, "Other", 100, nil)

	key := "retry-key-000000001"
	first := w.expect(w.buy(w.a1, a, one, 1, key), http.StatusCreated, "first").JSON(t)
	second := w.expect(w.buy(w.a1, a, one, 1, key), http.StatusOK, "retry").JSON(t)
	if second["duplicate"] != true || purchaseID(first) != purchaseID(second) || second["remainingBalance"].(float64) != 4900 {
		t.Fatalf("the retry returns the original: %v", second)
	}
	if n := w.count(`SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND reason_type='SHOP_PURCHASE'`, w.guildOf(w.a1), pa); n != 1 {
		t.Fatalf("charged once: %d", n)
	}
	// The retry works even though the product has sold out since (the original is returned, not OUT_OF_STOCK).
	if q, _ := w.stockOf(w.a1, one); q != 0 {
		t.Fatalf("stock %v", q)
	}
	w.expect(w.buy(w.a1, a, one, 1, key), http.StatusOK, "retry after sell-out")
	// The same key for different data is refused.
	for name, r := range map[string]*apiResult{"other product": w.buy(w.a1, a, two, 1, key), "other quantity": w.buy(w.a1, a, one, 2, key)} {
		if r.Status != http.StatusConflict || r.errCode(t) != "DUPLICATE_PURCHASE" {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
	}
	if w.balanceOf(w.a1, a) != 4900 {
		t.Fatal("no conflicting request may charge")
	}
	// Keys are per user: another user with the same key buys independently.
	w.expect(w.buy(w.a1, b, two, 1, key), http.StatusCreated, "same key, another user")
	// 20 concurrent identical requests: one purchase, one debit.
	burst := "burst-key-0000000009"
	var wg sync.WaitGroup
	var mu sync.Mutex
	created, replayed := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := w.buy(w.a1, b, two, 1, burst)
			mu.Lock()
			defer mu.Unlock()
			switch r.Status {
			case http.StatusCreated:
				created++
			case http.StatusOK:
				replayed++
			default:
				t.Errorf("burst: %d %s", r.Status, r.Body)
			}
		}()
	}
	wg.Wait()
	if created != 1 || replayed != 19 || w.balanceOf(w.a1, b) != 5000-100-100 {
		t.Fatalf("burst: created %d replayed %d balance %d", created, replayed, w.balanceOf(w.a1, b))
	}
	w.allIntegrity(w.a1)
}

func TestShopConcurrentPurchasesNeverOverspendOrOversell(t *testing.T) {
	w := newFactionWorld(t)
	a := w.players[0]
	pa := w.linkPlayer(w.a1, a, "Racer One")
	w.grant(w.a1, pa, 1000)
	// Balance 1000, product 750, two concurrent attempts with different keys: exactly one succeeds.
	item := w.product(w.a1, "Pricey", 750, nil)
	statuses := make([]*apiResult, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = w.buy(w.a1, a, item, 1, idemKeyFor("race"))
		}(i)
	}
	wg.Wait()
	ok, poor := 0, 0
	for _, r := range statuses {
		switch {
		case r.Status == http.StatusCreated:
			ok++
		case r.Status == http.StatusConflict && r.errCode(t) == "INSUFFICIENT_FUNDS":
			poor++
		default:
			t.Fatalf("unexpected: %d %s", r.Status, r.Body)
		}
	}
	if ok != 1 || poor != 1 || w.balanceOf(w.a1, a) != 250 {
		t.Fatalf("one purchase, one refusal, balance 250: %d/%d/%d", ok, poor, w.balanceOf(w.a1, a))
	}

	// The last item: stock 1, two funded buyers, exactly one success and one OUT_OF_STOCK, one debit.
	b := w.players[1]
	pb := w.linkPlayer(w.a1, b, "Racer Two")
	w.grant(w.a1, pa, 10_000)
	w.grant(w.a1, pb, 10_000)
	last := w.product(w.a1, "The Last One", 100, map[string]any{"stockMode": "FINITE", "stockQuantity": 1})
	results := make([]*apiResult, 2)
	for i, actor := range []string{a, b} {
		wg.Add(1)
		go func(i int, actor string) {
			defer wg.Done()
			results[i] = w.buy(w.a1, actor, last, 1, idemKeyFor("last"))
		}(i, actor)
	}
	wg.Wait()
	ok, out := 0, 0
	for _, r := range results {
		switch {
		case r.Status == http.StatusCreated:
			ok++
		case r.Status == http.StatusConflict && r.errCode(t) == "OUT_OF_STOCK":
			out++
		default:
			t.Fatalf("last item: %d %s", r.Status, r.Body)
		}
	}
	if q, _ := w.stockOf(w.a1, last); ok != 1 || out != 1 || q != 0 {
		t.Fatalf("last item: %d ok, %d out of stock, stock %v", ok, out, q)
	}
	var lastDebits int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM shop_purchase_items pi JOIN shop_purchases sp ON sp.id=pi.purchase_id WHERE pi.product_id=$1`, last).Scan(&lastDebits); err != nil || lastDebits != 1 {
		t.Fatalf("exactly one purchase of the last item: %d %v", lastDebits, err)
	}

	// Ten buyers compete for 3 units: exactly 3 succeed and stock never goes negative.
	buyers := []string{}
	for i := 2; i < 6; i++ {
		buyers = append(buyers, w.players[i])
		w.grant(w.a1, w.linkPlayer(w.a1, w.players[i], fmt.Sprintf("Buyer %d", i)), 1000)
	}
	buyers = append(buyers, a, b)
	three := w.product(w.a1, "Three Left", 10, map[string]any{"stockMode": "FINITE", "stockQuantity": 3})
	var mu sync.Mutex
	won := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := w.buy(w.a1, buyers[i%len(buyers)], three, 1, idemKeyFor("ten"))
			mu.Lock()
			defer mu.Unlock()
			if r.Status == http.StatusCreated {
				won++
			} else if r.errCode(t) != "OUT_OF_STOCK" {
				t.Errorf("unexpected: %d %s", r.Status, r.Body)
			}
		}(i)
	}
	wg.Wait()
	if q, _ := w.stockOf(w.a1, three); won != 3 || q != 0 {
		t.Fatalf("3 units, %d sold, stock %v", won, q)
	}
	// Mixed storm: purchases racing an admin debit on the same account never make the balance negative.
	c, pc := a, pa
	w.grant(w.a1, pc, 500)
	cheap := w.product(w.a1, "Cheap", 60, nil)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%3 == 0 {
				_ = w.adjust(w.a1, "debit", pc, w.admin, map[string]any{"amount": 40, "reason": "race"})
			} else {
				_ = w.buy(w.a1, c, cheap, 1, idemKeyFor("storm"))
			}
		}(i)
	}
	wg.Wait()
	if w.balanceOf(w.a1, c) < 0 {
		t.Fatal("negative balance")
	}
	w.allIntegrity(w.a1)
}

func TestShopPurchaseLimits(t *testing.T) {
	w := newFactionWorld(t)
	a, b := w.players[0], w.players[1]
	pa := w.linkPlayer(w.a1, a, "Limited")
	pb := w.linkPlayer(w.a1, b, "Other")
	w.grant(w.a1, pa, 10_000)
	w.grant(w.a1, pb, 10_000)
	one := w.product(w.a1, "Once Only", 100, map[string]any{"purchaseLimit": 1})
	three := w.product(w.a1, "Up To Three", 100, map[string]any{"purchaseLimit": 3})

	first := w.buyOK(w.a1, a, one, 1)
	if r := w.buy(w.a1, a, one, 1, idemKeyFor("lim")); r.Status != http.StatusConflict || r.errCode(t) != "PURCHASE_LIMIT_REACHED" {
		t.Fatalf("limit 1: %d %s", r.Status, r.Body)
	}
	w.buyOK(w.a1, b, one, 1) // the limit is per player
	// The limit counts units, not purchases.
	w.buyOK(w.a1, a, three, 2)
	if r := w.buy(w.a1, a, three, 2, idemKeyFor("lim")); r.errCode(t) != "PURCHASE_LIMIT_REACHED" {
		t.Fatalf("2 + 2 > 3: %d %s", r.Status, r.Body)
	}
	w.buyOK(w.a1, a, three, 1)
	// A refunded purchase frees the limit again.
	w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", purchaseID(first))), w.admin, map[string]any{"reason": "changed mind"}), http.StatusOK, "refund")
	w.buyOK(w.a1, a, one, 1)
	// Concurrent attempts by one player at a limit of 1: exactly one wins.
	w2 := w.product(w.a1, "Race Limit", 10, map[string]any{"purchaseLimit": 1})
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := w.buy(w.a1, b, w2, 1, idemKeyFor("lr"))
			mu.Lock()
			defer mu.Unlock()
			if r.Status == http.StatusCreated {
				won++
			} else if r.errCode(t) != "PURCHASE_LIMIT_REACHED" {
				t.Errorf("unexpected %d %s", r.Status, r.Body)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("the limit must hold under concurrency: %d purchases", won)
	}
	w.allIntegrity(w.a1)
}

func TestShopHistoryFulfillmentAndAdminList(t *testing.T) {
	w := newFactionWorld(t)
	a, b := w.players[0], w.players[1]
	pa := w.linkPlayer(w.a1, a, "History A")
	pb := w.linkPlayer(w.a1, b, "History B")
	w.grant(w.a1, pa, 100_000)
	w.grant(w.a1, pb, 100_000)
	x := w.product(w.a1, "Item X", 50, nil)
	y := w.product(w.a1, "Item Y", 70, nil)
	var ids []int64
	for i := 0; i < 12; i++ {
		p := x
		if i%3 == 0 {
			p = y
		}
		ids = append(ids, purchaseID(w.buyOK(w.a1, a, p, 1+i%2)))
	}
	bid := purchaseID(w.buyOK(w.a1, b, x, 1))

	// Player history: newest first, cursor pagination, status filter; only their own.
	seen := map[float64]bool{}
	cursor, prev := "", 1e18
	for pages := 0; pages < 10; pages++ {
		path := "/me/purchases?limit=5"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		pg := w.getJSON(w.shopPath(w.a1, path), a)
		for _, it := range pg["items"].([]any) {
			m := it.(map[string]any)
			id := m["id"].(float64)
			if seen[id] || id >= prev || int64(id) == bid {
				t.Fatalf("history must be unique, newest first and only the player's own: %v", id)
			}
			seen[id], prev = true, id
			if _, leaked := m["player"]; leaked || m["refundReason"] != nil && false {
				t.Fatal("the player DTO must not carry admin fields")
			}
		}
		next, _ := pg["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 12 {
		t.Fatalf("walked %d of 12 purchases", len(seen))
	}
	if got := w.getJSON(w.shopPath(w.a1, "/me/purchases?status=FULFILLED"), a)["items"].([]any); len(got) != 0 {
		t.Fatal("nothing fulfilled yet")
	}
	// Detail: own only; another player's purchase and an unknown id are the same 404.
	w.expect(w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/me/purchases/%d", ids[0])), a, nil), http.StatusOK, "own detail")
	for _, id := range []int64{bid, 999999999} {
		if r := w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/me/purchases/%d", id)), a, nil); r.Status != http.StatusNotFound || r.errCode(t) != "PURCHASE_NOT_FOUND" {
			t.Fatalf("another player's purchase: %d %s", r.Status, r.Body)
		}
	}

	// Admin list with filters and pagination; admin detail shows the player.
	all := w.getJSON(w.shopPath(w.a1, "/admin/purchases?limit=100"), w.admin)["items"].([]any)
	if len(all) != 13 {
		t.Fatalf("admin list: %d", len(all))
	}
	if first := all[0].(map[string]any); first["player"].(map[string]any)["gamertag"] != "History B" || int64(first["id"].(float64)) != bid {
		t.Fatalf("newest first with the player: %v", first)
	}
	if got := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/admin/purchases?playerId=%d&limit=100", pb)), w.admin)["items"].([]any); len(got) != 1 {
		t.Fatalf("player filter: %d", len(got))
	}
	if got := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/admin/purchases?productId=%d&limit=100", y)), w.admin)["items"].([]any); len(got) != 4 {
		t.Fatalf("product filter: %d", len(got))
	}
	for _, bad := range []string{"status=NOPE", "playerId=abc", "productId=-1", "cursor=zzz"} {
		w.expect(w.do(http.MethodGet, w.shopPath(w.a1, "/admin/purchases?"+bad), w.admin, nil), http.StatusBadRequest, bad)
	}

	// Fulfillment: PENDING_FULFILLMENT -> FULFILLED once, by an admin, with an audit line.
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	fid := ids[1]
	f1 := w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", fid)), w.admin, nil), http.StatusOK, "fulfill").JSON(t)["purchase"].(map[string]any)
	if f1["status"] != "FULFILLED" || f1["fulfilledAt"] == nil || f1["fulfilledByDiscordUserId"] != w.admin {
		t.Fatalf("fulfilled: %v", f1)
	}
	if r := w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", fid)), w.admin, nil); r.Status != http.StatusConflict || r.errCode(t) != "INVALID_PURCHASE_STATUS" {
		t.Fatalf("a second fulfill: %d %s", r.Status, r.Body)
	}
	if got := w.getJSON(w.shopPath(w.a1, "/admin/purchases?status=fulfilled"), w.admin)["items"].([]any); len(got) != 1 {
		t.Fatalf("status filter: %d", len(got))
	}
	if got := w.getJSON(w.shopPath(w.a1, "/me/purchases?status=FULFILLED"), a)["items"].([]any); len(got) != 1 {
		t.Fatal("the player sees the fulfilled status")
	}
	if r := w.do(http.MethodPost, w.shopPath(w.a1, "/purchases/999999999/fulfill"), w.admin, nil); r.Status != http.StatusNotFound || r.errCode(t) != "PURCHASE_NOT_FOUND" {
		t.Fatalf("unknown purchase: %d %s", r.Status, r.Body)
	}
	// Concurrent fulfills: exactly one succeeds.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", ids[2])), w.admin, nil)
			mu.Lock()
			defer mu.Unlock()
			if r.Status == http.StatusOK {
				ok++
			} else if r.Status != http.StatusConflict {
				t.Errorf("fulfill race: %d %s", r.Status, r.Body)
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("exactly one concurrent fulfill succeeds: %d", ok)
	}
	out := logs.String()
	if !strings.Contains(out, "shop_purchase_fulfilled") || !strings.Contains(out, "purchase_id="+strconv.FormatInt(fid, 10)) {
		t.Errorf("fulfillment must be audited: %s", out)
	}
	for _, banned := range []string{"History A", "Item X", "test-secret"} {
		if strings.Contains(out, banned) {
			t.Errorf("the audit log must not contain %q", banned)
		}
	}
	w.allIntegrity(w.a1)
}

func TestShopRefunds(t *testing.T) {
	w := newFactionWorld(t)
	a := w.players[0]
	pa := w.linkPlayer(w.a1, a, "Refundee")
	rec := &ecoRecorder{}
	w.grant(w.a1, pa, 2000)
	w.a.EconomyService.SetNotifier(rec)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	item := w.product(w.a1, "Refundable", 300, map[string]any{"stockMode": "FINITE", "stockQuantity": 5})

	p1 := purchaseID(w.buyOK(w.a1, a, item, 2)) // 600, stock 3
	if q, _ := w.stockOf(w.a1, item); q != 3 || w.balanceOf(w.a1, a) != 1400 {
		t.Fatalf("after purchase: stock %v balance %d", q, w.balanceOf(w.a1, a))
	}
	// Validation and authorization.
	refund := func(id int64, actor string, body any) *apiResult {
		return w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", id)), actor, body)
	}
	for _, body := range []any{map[string]any{}, map[string]any{"reason": "   "}, `{"reason":"x","amount":5}`} {
		if r := refund(p1, w.admin, body); r.Status != http.StatusBadRequest {
			t.Errorf("body %v: %d %s", body, r.Status, r.Body)
		}
	}
	if r := refund(p1, a, map[string]any{"reason": "please"}); r.Status != http.StatusForbidden || r.errCode(t) != "SHOP_FORBIDDEN" {
		t.Fatalf("a player cannot refund: %d %s", r.Status, r.Body)
	}
	if r := refund(999999999, w.admin, map[string]any{"reason": "x"}); r.Status != http.StatusNotFound {
		t.Fatalf("unknown: %d", r.Status)
	}
	// Refund: compensating credit, status, restock (not delivered yet), reason hidden from the player.
	rr := w.expect(refund(p1, w.admin, map[string]any{"reason": "Out of stock in game"}), http.StatusOK, "refund").JSON(t)
	rp := rr["purchase"].(map[string]any)
	if rp["status"] != "REFUNDED" || rp["refundedAt"] == nil || rp["refundReason"] != "Out of stock in game" || rp["refundedByDiscordUserId"] != w.admin || rr["remainingBalance"].(float64) != 2000 {
		t.Fatalf("refund response: %v", rr)
	}
	if got := w.balanceOf(w.a1, a); got != 2000 {
		t.Fatalf("the points are back: %d", got)
	}
	if q, _ := w.stockOf(w.a1, item); q != 5 {
		t.Fatalf("an undelivered purchase restocks: %v", q)
	}
	var debit, credit int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT
  (SELECT amount FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND reason_type='SHOP_PURCHASE' AND source_key=$3),
  (SELECT amount FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND reason_type='SHOP_REFUND' AND source_key=$3)`, w.guildOf(w.a1), pa, fmt.Sprintf("purchase:%d", p1)).Scan(&debit, &credit); err != nil {
		t.Fatal(err)
	}
	if debit != -600 || credit != 600 {
		t.Fatalf("the original debit is untouched and a compensating credit exists: %d %d", debit, credit)
	}
	player := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/me/purchases/%d", p1)), a)["purchase"].(map[string]any)
	if player["status"] != "REFUNDED" || bytes.Contains(w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/me/purchases/%d", p1)), a, nil).Body, []byte("Out of stock in game")) {
		t.Fatalf("the player sees REFUNDED but never the admin's reason: %v", player)
	}
	if hist := w.getJSON(w.eco(w.a1, "/me/transactions?type=SHOP_REFUND"), a)["items"].([]any); len(hist) != 1 || hist[0].(map[string]any)["direction"] != "CREDIT" || hist[0].(map[string]any)["description"] != "Shop refund" {
		t.Fatalf("economy history: %v", hist)
	}
	if evs := rec.list(); len(evs) != 2 || evs[1].Type != "SHOP_REFUND" || !evs[1].Credit || evs[1].Amount != 600 {
		t.Fatalf("refund event: %+v", evs)
	}
	// A purchase can only be refunded once - also under concurrency - and a refunded one cannot be fulfilled.
	if r := refund(p1, w.admin, map[string]any{"reason": "again"}); r.Status != http.StatusConflict || r.errCode(t) != "INVALID_PURCHASE_STATUS" {
		t.Fatalf("second refund: %d %s", r.Status, r.Body)
	}
	if r := w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", p1)), w.admin, nil); r.Status != http.StatusConflict {
		t.Fatalf("fulfilling a refunded purchase: %d", r.Status)
	}
	p2 := purchaseID(w.buyOK(w.a1, a, item, 1)) // 300
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := refund(p2, w.admin, map[string]any{"reason": "race"})
			mu.Lock()
			defer mu.Unlock()
			if r.Status == http.StatusOK {
				ok++
			} else if r.Status != http.StatusConflict {
				t.Errorf("refund race: %d %s", r.Status, r.Body)
			}
		}()
	}
	wg.Wait()
	if n := w.count(`SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1 AND reason_type='SHOP_REFUND' AND source_key=$2`, w.guildOf(w.a1), fmt.Sprintf("purchase:%d", p2)); ok != 1 || n != 1 || w.balanceOf(w.a1, a) != 2000 {
		t.Fatalf("exactly one concurrent refund: %d succeeded, %d ledger rows, balance %d", ok, n, w.balanceOf(w.a1, a))
	}
	if q, _ := w.stockOf(w.a1, item); q != 5 {
		t.Fatalf("restocked exactly once: %v", q)
	}
	// A delivered (fulfilled) purchase can still be refunded but is NOT restocked.
	p3 := purchaseID(w.buyOK(w.a1, a, item, 1))
	w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", p3)), w.admin, nil), http.StatusOK, "fulfill")
	w.expect(refund(p3, w.admin, map[string]any{"reason": "bad item"}), http.StatusOK, "refund fulfilled")
	if q, _ := w.stockOf(w.a1, item); q != 4 || w.balanceOf(w.a1, a) != 2000 {
		t.Fatalf("a delivered product is not restocked: stock %v balance %d", q, w.balanceOf(w.a1, a))
	}
	out := logs.String()
	if !strings.Contains(out, "shop_purchase_refunded") {
		t.Error("refunds must be audited")
	}
	for _, banned := range []string{"Out of stock in game", "bad item", "Refundee"} {
		if strings.Contains(out, banned) {
			t.Errorf("the audit log must not contain %q", banned)
		}
	}
	w.allIntegrity(w.a1)
}

func TestShopTenantIsolationAndSharedGuildBalance(t *testing.T) {
	w := newFactionWorld(t)
	a, b := w.players[0], w.players[1]
	pa := w.linkPlayer(w.a1, a, "Tenant A")
	pb := w.linkPlayer(w.b1, b, "Tenant B")
	a1b := w.secondInstallation(w.a1) // same organization AND Discord guild, another server
	w.grant(w.a1, pa, 5000)
	w.expect(w.adjust(w.b1, "grant", pb, w.b1.OwnerDiscordID, map[string]any{"amount": 5000, "reason": "fund"}), http.StatusOK, "fund B")

	prodA := w.product(w.a1, "A Only", 100, nil)
	prodB := w.product(w.b1, "B Only", 100, nil)
	prodA1b := w.product(a1b, "Second Server Item", 200, nil)
	buyA := purchaseID(w.buyOK(w.a1, a, prodA, 1))
	buyB := purchaseID(w.buyOK(w.b1, b, prodB, 1))

	// Catalogs are per installation (also for the same guild's other server).
	for _, tc := range []struct {
		f    installationFixture
		want string
	}{{w.a1, "[A Only]"}, {w.b1, "[B Only]"}, {a1b, "[Second Server Item]"}} {
		names := []string{}
		for _, it := range w.getJSON(w.shopPath(tc.f, "/products"), a)["items"].([]any) {
			names = append(names, it.(map[string]any)["name"].(string))
		}
		if fmt.Sprint(names) != tc.want {
			t.Errorf("catalog of %+v: %v", tc.f, names)
		}
	}
	// Another tenant's product id is a safe 404 for reading, buying and editing.
	if r := w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/products/%d", prodB)), a, nil); r.Status != http.StatusNotFound || r.errCode(t) != "SHOP_PRODUCT_NOT_FOUND" {
		t.Fatalf("read: %d %s", r.Status, r.Body)
	}
	if r := w.buy(w.a1, a, prodB, 1, idemKeyFor("x")); r.Status != http.StatusNotFound || r.errCode(t) != "SHOP_PRODUCT_NOT_FOUND" {
		t.Fatalf("buy: %d %s", r.Status, r.Body)
	}
	if r := w.buy(w.a1, a, prodA1b, 1, idemKeyFor("x")); r.Status != http.StatusNotFound {
		t.Fatalf("a product of the same guild's other installation: %d %s", r.Status, r.Body)
	}
	if r := w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", prodB)), w.admin, map[string]any{"pricePoints": 1}); r.Status != http.StatusNotFound {
		t.Fatalf("edit: %d %s", r.Status, r.Body)
	}
	// Another tenant's purchase id: 404 for detail, fulfill and refund; lists never mix.
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, fmt.Sprintf("/me/purchases/%d", buyB)}, {http.MethodGet, fmt.Sprintf("/admin/purchases/%d", buyB)},
		{http.MethodPost, fmt.Sprintf("/purchases/%d/fulfill", buyB)}, {http.MethodPost, fmt.Sprintf("/purchases/%d/refund", buyB)},
	} {
		if r := w.do(req.method, w.shopPath(w.a1, req.path), w.admin, map[string]any{"reason": "x"}); r.Status != http.StatusNotFound && !(strings.HasPrefix(req.path, "/me/") && r.Status == http.StatusConflict) {
			t.Errorf("%s %s: %d %s", req.method, req.path, r.Status, r.Body)
		}
	}
	if got := w.getJSON(w.shopPath(w.a1, "/admin/purchases"), w.admin)["items"].([]any); len(got) != 1 || int64(got[0].(map[string]any)["id"].(float64)) != buyA {
		t.Fatalf("admin list is scoped: %v", got)
	}
	// Org A's admin cannot manage org B; mismatched org/installation pairs are 404.
	if r := w.do(http.MethodPost, w.shopPath(w.b1, "/products"), w.admin, map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM"}); r.Status != http.StatusForbidden {
		t.Fatalf("org A's admin on org B: %d", r.Status)
	}
	crossOrg := installationFixture{OrgID: w.b1.OrgID, InstallationID: w.a1.InstallationID}
	crossInst := installationFixture{OrgID: w.a1.OrgID, InstallationID: w.b1.InstallationID}
	for _, scope := range []installationFixture{crossOrg, crossInst} {
		w.expect(w.do(http.MethodGet, w.shopPath(scope, "/products"), a, nil), http.StatusNotFound, "mismatched scope")
		w.expect(w.buy(scope, a, prodA, 1, idemKeyFor("m")), http.StatusNotFound, "mismatched scope purchase")
	}

	// The documented caveat: two installations of the SAME Discord guild share one Champion Points balance,
	// so a purchase on the second server spends the same underlying balance; another guild's is separate.
	before := w.balanceOf(w.a1, a)
	resp := w.buyOK(a1b, a, prodA1b, 1)
	if resp["remainingBalance"].(float64) != float64(before-200) || w.balanceOf(w.a1, a) != before-200 || w.balanceOf(a1b, a) != before-200 {
		t.Fatalf("shared guild balance: before %d, remaining %v", before, resp["remainingBalance"])
	}
	if w.balanceOf(w.b1, b) != 4900 {
		t.Fatalf("another organization's balance is separate: %d", w.balanceOf(w.b1, b))
	}
	// The purchase is attributed to the installation it was made on.
	var srv1, srv2 int64
	_, srv1 = w.gameContext(w.a1)
	_, srv2 = w.gameContext(a1b)
	last := w.getJSON(w.shopPath(a1b, "/admin/purchases"), w.admin)["items"].([]any)[0].(map[string]any)
	if int64(last["gameServerId"].(float64)) != srv2 || srv1 == srv2 {
		t.Fatalf("attribution: %v vs %d", last["gameServerId"], srv2)
	}
	w.allIntegrity(w.a1)
	w.allIntegrity(w.b1)
}

func TestShopIsIndependentOfFactions(t *testing.T) {
	w := newFactionWorld(t)
	leader, joiner := w.players[0], w.players[1]
	w.linkPlayer(w.a1, leader, "Boss")
	jp := w.linkPlayer(w.a1, joiner, "Shopper")
	w.grant(w.a1, jp, 1000)
	item := w.product(w.a1, "Faction Neutral", 100, nil)
	id := purchaseID(w.buyOK(w.a1, joiner, item, 1))
	f := w.createFaction(w.a1, leader, "Shop Independence", "SIN", "OPEN")
	w.joined(leader, idOf(f), joiner)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("/%d/leave", idOf(f))), joiner, nil), http.StatusOK, "leave")
	if w.balanceOf(w.a1, joiner) != 900 {
		t.Fatalf("balance: %d", w.balanceOf(w.a1, joiner))
	}
	got := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/me/purchases/%d", id)), joiner)["purchase"].(map[string]any)
	if got["status"] != "PENDING_FULFILLMENT" {
		t.Fatalf("faction changes never alter purchases: %v", got)
	}
}

func TestShopRateLimits(t *testing.T) {
	w := newFactionWorld(t)
	a := w.players[0]
	pa := w.linkPlayer(w.a1, a, "Throttled")
	w.grant(w.a1, pa, 10_000)
	item := w.product(w.a1, "Thing", 10, nil)
	w.a.saasShopPurchaseLimiter = newSaaSRateLimiter(time.Hour, 3)
	for i := 0; i < 3; i++ {
		w.expect(w.buy(w.a1, a, item, 1, idemKeyFor("rl")), http.StatusCreated, "within the limit")
	}
	if r := w.buy(w.a1, a, item, 1, idemKeyFor("rl")); r.Status != http.StatusTooManyRequests || r.errCode(t) != "RATE_LIMITED" {
		t.Fatalf("purchases are rate limited: %d %s", r.Status, r.Body)
	}
	if got := w.balanceOf(w.a1, a); got != 10_000-30 {
		t.Fatalf("a limited request must not charge: %d", got)
	}
	w.a.saasShopAdminLimiter = newSaaSRateLimiter(time.Hour, 2)
	pid := w.getJSON(w.shopPath(w.a1, "/admin/purchases"), w.admin)["items"].([]any)[0].(map[string]any)["id"].(float64)
	w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", int64(pid))), w.admin, nil), http.StatusOK, "1")
	w.expect(w.do(http.MethodPost, w.shopPath(w.a1, "/categories"), w.admin, map[string]any{"name": "C"}), http.StatusCreated, "2")
	for _, r := range []*apiResult{
		w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", int64(pid))), w.admin, map[string]any{"reason": "x"}),
		w.do(http.MethodPost, w.shopPath(w.a1, "/products"), w.admin, map[string]any{"name": "x", "pricePoints": 5, "productType": "ITEM"}),
	} {
		if r.Status != http.StatusTooManyRequests {
			t.Fatalf("admin mutations are rate limited: %d %s", r.Status, r.Body)
		}
	}
	// Catalog and history reads are not limited by these limiters.
	for i := 0; i < 40; i++ {
		w.expect(w.do(http.MethodGet, w.shopPath(w.a1, "/products"), a, nil), http.StatusOK, "browse")
	}
}

func TestShopAuditEventsForProducts(t *testing.T) {
	w := newFactionWorld(t)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	id := w.product(w.a1, "Audited Secret Name", 100, nil)
	w.expect(w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", id)), w.admin, map[string]any{"pricePoints": 120}), http.StatusOK, "update")
	w.expect(w.do(http.MethodPut, w.shopPath(w.a1, fmt.Sprintf("/products/%d", id)), w.admin, map[string]any{"isActive": false}), http.StatusOK, "disable")
	out := logs.String()
	for _, want := range []string{"shop_product_created", "shop_product_updated", "shop_product_disabled", "product_id=" + strconv.FormatInt(id, 10), "organization_id=", "installation_id=", "acting_user_id="} {
		if !strings.Contains(out, want) {
			t.Errorf("audit log is missing %q", want)
		}
	}
	if strings.Contains(out, "Audited Secret Name") || strings.Contains(out, "About ") {
		t.Error("audit lines carry ids only, never names or descriptions")
	}
}

func TestShopReconciliationDetectsMissingLedgerRows(t *testing.T) {
	w := newFactionWorld(t)
	a := w.players[0]
	pa := w.linkPlayer(w.a1, a, "Audited")
	w.grant(w.a1, pa, 1000)
	item := w.product(w.a1, "Reconciled", 100, nil)
	p1 := purchaseID(w.buyOK(w.a1, a, item, 1))
	p2 := purchaseID(w.buyOK(w.a1, a, item, 1))
	w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", p2)), w.admin, map[string]any{"reason": "x"}), http.StatusOK, "refund")
	if m := w.shopReconcile(w.a1); len(m) != 0 {
		t.Fatalf("a healthy shop reconciles: %+v", m)
	}
	// Corruption outside the API (the delete guard is disabled for the simulation): the debit of p1 disappears.
	ctx := context.Background()
	tx, err := w.a.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE point_transactions DISABLE TRIGGER trg_point_transactions_no_delete`,
		fmt.Sprintf(`DELETE FROM point_transactions WHERE reason_type='SHOP_PURCHASE' AND source_key='purchase:%d'`, p1),
		`ALTER TABLE point_transactions ENABLE TRIGGER trg_point_transactions_no_delete`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	m := w.shopReconcile(w.a1)
	if len(m) != 1 || m[0].Kind != repository.ShopMismatchMissingDebit || m[0].PurchaseID != p1 {
		t.Fatalf("a purchase without its debit: %+v", m)
	}
	if again := w.shopReconcile(w.a1); len(again) != 1 {
		t.Fatal("reads never repair")
	}
	// A refunded purchase whose refund credit vanished, and a refund credit for a purchase that is not refunded.
	scopeFix := func(stmt string) {
		tx, _ := w.a.DB.Pool.Begin(ctx)
		for _, s := range []string{`ALTER TABLE point_transactions DISABLE TRIGGER trg_point_transactions_no_delete`, stmt, `ALTER TABLE point_transactions ENABLE TRIGGER trg_point_transactions_no_delete`} {
			if _, err := tx.Exec(ctx, s); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
		}
		_ = tx.Commit(ctx)
	}
	scopeFix(fmt.Sprintf(`DELETE FROM point_transactions WHERE reason_type='SHOP_REFUND' AND source_key='purchase:%d'`, p2))
	kinds := map[string]bool{}
	for _, x := range w.shopReconcile(w.a1) {
		kinds[x.Kind] = true
	}
	if !kinds[repository.ShopMismatchMissingRefund] || !kinds[repository.ShopMismatchMissingDebit] {
		t.Fatalf("kinds: %v", kinds)
	}
}

// TestShopCatalogAndHistoryScaleWithVolume loads PERF_PRODUCTS products (default 3000) in 20 categories and
// PERF_PURCHASES purchases (default 60000) and checks every read stays a few queries and fast.
func TestShopCatalogAndHistoryScaleWithVolume(t *testing.T) {
	products, purchases := 3000, 60000
	if v, err := strconv.Atoi(os.Getenv("PERF_PRODUCTS")); err == nil && v > 0 {
		products = v
	}
	if v, err := strconv.Atoi(os.Getenv("PERF_PURCHASES")); err == nil && v > 0 {
		purchases = v
	}
	w := newFactionWorld(t)
	a := w.players[0]
	pa := w.linkPlayer(w.a1, a, "Whale Shopper")
	ctx := context.Background()
	inst, org, guild := w.a1.InstallationID, w.a1.OrgID, w.guildOf(w.a1)
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO shop_categories(organization_id, installation_id, name, slug, sort_order) SELECT $1, $2, 'Category ' || g, 'category-' || g, g FROM generate_series(1,20) g`, org, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO shop_products(organization_id, installation_id, category_id, name, slug, description, price_points, product_type, is_featured, sort_order, is_active)
SELECT $1, $2, (SELECT id FROM shop_categories WHERE installation_id=$2 AND slug='category-' || (1 + g % 20)), 'Product ' || lpad(g::text, 5, '0'), 'product-' || g, 'A generated product number ' || g,
       10 + g % 500, 'ITEM', g % 50 = 0, g % 7, g % 10 <> 0
FROM generate_series(1, $3::int) g`, org, inst, products); err != nil {
		t.Fatal(err)
	}
	// Purchases with one item each, spread over 200 generated players plus the whale; ledger rows are not needed for read volume.
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO players(guild_id, dayz_player_id, display_name) SELECT $1::bigint, 'shopperf-' || $2::bigint::text || '-' || g, 'Shopper ' || g FROM generate_series(1,200) g`, guild, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `WITH pl AS (SELECT array_agg(id) AS ids FROM players WHERE guild_id=$1 AND display_name LIKE 'Shopper %'),
 pr AS (SELECT array_agg(id ORDER BY id) AS ids FROM shop_products WHERE installation_id=$3)
INSERT INTO shop_purchases(organization_id, installation_id, player_id, status, total_points, delivery_type, idempotency_key, paid_at)
SELECT $2, $3, CASE WHEN g % 10 = 0 THEN $5 ELSE (SELECT ids[1 + g % 200] FROM pl) END, CASE WHEN g % 4 = 0 THEN 'FULFILLED' ELSE 'PENDING_FULFILLMENT' END, 50, 'MANUAL', 'perf-key-' || g, NOW()
FROM generate_series(1, $4::int) g`, guild, org, inst, purchases, pa); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO shop_purchase_items(purchase_id, product_id, product_name, unit_price_points, quantity, line_total_points)
SELECT sp.id, (SELECT id FROM shop_products WHERE installation_id=$1 ORDER BY id OFFSET (sp.id % 1000) LIMIT 1), 'Product', 50, 1, 50 FROM shop_purchases sp WHERE sp.installation_id=$1`, inst); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"shop_products", "shop_purchases", "shop_purchase_items", "shop_categories"} {
		if _, err := w.a.DB.Pool.Exec(ctx, `ANALYZE `+tbl); err != nil {
			t.Fatal(err)
		}
	}
	timeIt := func(name string, fn func()) time.Duration {
		fn()
		s := time.Now()
		fn()
		d := time.Since(s)
		t.Logf("%-50s %v", name, d.Round(time.Microsecond))
		return d
	}
	d1 := timeIt("catalog first page (25 of ~"+strconv.Itoa(products*9/10)+")", func() { w.getJSON(w.shopPath(w.a1, "/products"), a) })
	d2 := timeIt("catalog page 100", func() { w.getJSON(w.shopPath(w.a1, "/products?limit=100"), a) })
	cursor := w.getJSON(w.shopPath(w.a1, "/products?limit=100"), a)["nextCursor"].(string)
	d3 := timeIt("catalog next page via cursor", func() { w.getJSON(w.shopPath(w.a1, "/products?limit=100&cursor="+url.QueryEscape(cursor)), a) })
	d4 := timeIt("catalog by category", func() { w.getJSON(w.shopPath(w.a1, "/products?category=category-3"), a) })
	d5 := timeIt("catalog search (name/description)", func() { w.getJSON(w.shopPath(w.a1, "/products?q=number+12"), a) })
	d6 := timeIt("categories with counts", func() { w.getJSON(w.shopPath(w.a1, "/categories"), a) })
	d7 := timeIt("my purchases page (whale, 25)", func() { w.getJSON(w.shopPath(w.a1, "/me/purchases"), a) })
	c2 := w.getJSON(w.shopPath(w.a1, "/me/purchases?limit=100"), a)["nextCursor"].(string)
	d8 := timeIt("my purchases deep page", func() { w.getJSON(w.shopPath(w.a1, "/me/purchases?limit=100&cursor="+url.QueryEscape(c2)), a) })
	d9 := timeIt("admin purchases filtered by status", func() { w.getJSON(w.shopPath(w.a1, "/admin/purchases?status=FULFILLED&limit=100"), w.admin) })
	if n := len(w.getJSON(w.shopPath(w.a1, "/products?limit=100"), a)["items"].([]any)); n != 100 {
		t.Fatalf("page size %d", n)
	}
	for name, d := range map[string]time.Duration{"first": d1, "100": d2, "cursor": d3, "category": d4, "search": d5, "categories": d6, "mine": d7, "deep": d8, "admin": d9} {
		if d > 500*time.Millisecond {
			t.Errorf("%s took %v", name, d)
		}
	}
	_ = economy.ChampionPoints
}
