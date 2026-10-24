//go:build integration

package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Trader phase 2: deals and sales are the shop's price everywhere (Discord and the website) and
// are recorded on the order; staff hand orders over and refund them from the order card; opening
// hours open and close the trader once per opening or closing time.
func TestTraderDealsHandoverAndHours(t *testing.T) {
	w := newFactionWorld(t)
	w.a.ServerPanels = repository.NewServerPanelRepository(w.a.DB.Pool)
	w.a.Traders = repository.NewTraderRepository(w.a.DB.Pool)
	w.a.ShopDiscounts = repository.NewShopDiscountRepository(w.a.DB.Pool)
	w.a.AdminAudit = repository.NewAuditRepository(w.a.DB.Pool)
	b := newTraderBackend(w.a)
	ctx := context.Background()
	guild, inst := w.a1.DiscordGuildID, w.a1.InstallationID
	ownerPanel := discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: w.a1.OwnerDiscordID}
	owner := discord.TraderCaller{PanelCaller: ownerPanel, Username: "owner"}
	if _, err := b.TraderSaveHub(ctx, ownerPanel, discord.TraderHub{InstallationID: inst, TraderChannelID: "trader", DealsChannelID: "deals",
		PanelChannelID: "panel", OrdersChannelID: "orders", TraderRoleID: "role-trader", DealsRoleID: "role-deals"}); err != nil {
		t.Fatal(err)
	}
	if hub, err := b.TraderHub(ctx, guild, inst); err != nil || hub.DealsRoleID != "role-deals" {
		t.Fatalf("deals role: %+v %v", hub, err)
	}

	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Rusty Axe")
	w.grant(w.a1, pid, 5000)
	kit := w.product(w.a1, "M4-A1 Kit", 1000, nil)
	buyer := discord.TraderCaller{PanelCaller: discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: player}, Username: "rusty"}

	// Only the people who run the trader start deals, within the cap.
	if _, err := b.TraderStartOffer(ctx, buyer.PanelCaller, inst, discord.TraderOfferInput{Kind: discord.TraderOfferDeal, ProductID: kit, PercentOff: 25, Hours: 24}); !errors.Is(err, discord.ErrTraderForbidden) {
		t.Fatalf("a player started a deal: %v", err)
	}
	if _, err := b.TraderStartOffer(ctx, ownerPanel, inst, discord.TraderOfferInput{Kind: discord.TraderOfferDeal, ProductID: kit, PercentOff: 90, Hours: 24}); !errors.Is(err, discord.ErrTraderOfferBad) {
		t.Fatalf("a 90%% deal: %v", err)
	}
	if _, err := b.TraderStartOffer(ctx, ownerPanel, inst, discord.TraderOfferInput{Kind: discord.TraderOfferDeal, ProductID: 99999999, PercentOff: 25, Hours: 24}); !errors.Is(err, discord.ErrTraderUnavailable) {
		t.Fatalf("a deal on another server's item: %v", err)
	}
	withRole := discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: w.players[2], RoleIDs: []string{"role-trader"}}
	deal, err := b.TraderStartOffer(ctx, withRole, inst, discord.TraderOfferInput{Kind: discord.TraderOfferDeal, ProductID: kit, PercentOff: 25, Hours: 24, Title: "Weekend raid"})
	if err != nil || deal.ProductID != kit || deal.Name != "M4-A1 Kit" || deal.PercentOff != 25 || deal.Ended || deal.EndsAt.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("deal: %+v %v", deal, err)
	}
	if p, err := b.TraderProduct(ctx, guild, inst, kit); err != nil || p.Price != 750 || p.ListPrice != 1000 || p.PercentOff != 25 || p.OfferEnds == nil {
		t.Fatalf("product on a deal: %+v %v", p, err)
	}
	if cat, err := b.TraderCatalog(ctx, guild, inst); err != nil || len(cat.Offers) != 1 || cat.Offers[0].ID != deal.ID {
		t.Fatalf("catalog offers: %+v %v", cat.Offers, err)
	}

	// A bigger sale on everything wins while it runs.
	sale, err := b.TraderStartOffer(ctx, ownerPanel, inst, discord.TraderOfferInput{Kind: discord.TraderOfferSale, PercentOff: 40, Hours: 2})
	if err != nil || sale.CategoryID != 0 || sale.Kind != discord.TraderOfferSale {
		t.Fatalf("sale: %+v %v", sale, err)
	}
	if p, _ := b.TraderProduct(ctx, guild, inst, kit); p.Price != 600 || p.PercentOff != 40 {
		t.Fatalf("the bigger sale: %+v", p)
	}
	// The website shows it and charges it.
	products := w.getJSON(w.shopPath(w.a1, "/products"), player)["items"].([]any)
	var listed map[string]any
	for _, raw := range products {
		if p := raw.(map[string]any); int64(p["id"].(float64)) == kit {
			listed = p
		}
	}
	if s, ok := listed["sale"].(map[string]any); !ok || s["pricePoints"].(float64) != 600 || s["percentOff"].(float64) != 40 || s["kind"] != "SALE" || listed["pricePoints"].(float64) != 1000 {
		t.Fatalf("website listing: %+v", listed)
	}
	web := w.buyOK(w.a1, player, kit, 1)["purchase"].(map[string]any)
	if web["totalPoints"].(float64) != 600 {
		t.Fatalf("website purchase on sale: %+v", web)
	}
	item := web["items"].([]any)[0].(map[string]any)
	if item["discountPercent"].(float64) != 40 || item["listUnitPricePoints"].(float64) != 1000 || item["unitPricePoints"].(float64) != 600 {
		t.Fatalf("website purchase item: %+v", item)
	}
	ended, err := b.TraderEndOffer(ctx, withRole, inst, sale.ID)
	if err != nil || !ended.Ended {
		t.Fatalf("end sale: %+v %v", ended, err)
	}

	// Discord buys at the deal price and the receipt shows the saving.
	r, err := b.TraderBuy(ctx, buyer, inst, discord.TraderBuy{ProductID: kit, Quantity: 2, Key: "discord-trader.aaaaaaaaaaaaaaaa"})
	if err != nil || r.Total != 1500 || r.Saved != 500 || r.Balance != 5000-600-1500 {
		t.Fatalf("buy on a deal: %+v %v", r, err)
	}
	var percent int
	var list int64
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT discount_percent, list_unit_price_points FROM shop_purchase_items WHERE purchase_id=$1`, r.OrderID).Scan(&percent, &list); err != nil || percent != 25 || list != 1000 {
		t.Fatalf("order records the deal: %d %d %v", percent, list, err)
	}

	// The deal's post is ended once the deal is over.
	if err := b.TraderOfferPosted(ctx, inst, deal.ID, "deals", "m-deal"); err != nil {
		t.Fatal(err)
	}
	if over, err := b.TraderOfferPostsToEnd(ctx, inst); err != nil || len(over) != 0 {
		t.Fatalf("a running deal's post: %+v %v", over, err)
	}
	all, err := b.TraderEndAllOffers(ctx, ownerPanel, inst)
	if err != nil || len(all) != 1 || all[0].ID != deal.ID {
		t.Fatalf("end all: %+v %v", all, err)
	}
	over, err := b.TraderOfferPostsToEnd(ctx, inst)
	if err != nil || len(over) != 1 || over[0].PostMessageID != "m-deal" {
		t.Fatalf("posts to end: %+v %v", over, err)
	}
	if err := b.TraderOfferPostEnded(ctx, inst, deal.ID); err != nil {
		t.Fatal(err)
	}
	if over, _ := b.TraderOfferPostsToEnd(ctx, inst); len(over) != 0 {
		t.Fatalf("an ended post again: %+v", over)
	}
	if p, _ := b.TraderProduct(ctx, guild, inst, kit); p.Price != 1000 || p.ListPrice != 0 {
		t.Fatalf("price after the deal: %+v", p)
	}

	// Handover and refund from the order card: the Trader role, never a player, once each.
	rec := &ecoRecorder{}
	w.a.EconomyService.SetNotifier(rec)
	second, err := b.TraderBuy(ctx, buyer, inst, discord.TraderBuy{ProductID: kit, Quantity: 1, Key: "discord-trader.bbbbbbbbbbbbbbbb"})
	if err != nil || second.Total != 1000 {
		t.Fatalf("full price: %+v %v", second, err)
	}
	if _, err := b.TraderDeliver(ctx, buyer, inst, r.OrderID); !errors.Is(err, discord.ErrTraderForbidden) {
		t.Fatalf("a player delivered: %v", err)
	}
	trader := discord.TraderCaller{PanelCaller: withRole, Username: "deelo"}
	done, err := b.TraderDeliver(ctx, trader, inst, r.OrderID)
	if err != nil || done.Status != discord.TraderOrderDelivered || done.ID != r.OrderID {
		t.Fatalf("deliver: %+v %v", done, err)
	}
	if _, err := b.TraderDeliver(ctx, trader, inst, r.OrderID); !errors.Is(err, discord.ErrTraderOrderDone) {
		t.Fatalf("delivered twice: %v", err)
	}
	if _, err := b.TraderRefund(ctx, owner, inst, second.OrderID, "  "); !errors.Is(err, discord.ErrTraderReason) {
		t.Fatalf("a refund without a reason: %v", err)
	}
	refunded, err := b.TraderRefund(ctx, owner, inst, second.OrderID, "Couldn't deliver")
	if err != nil || refunded.Status != discord.TraderOrderRefunded {
		t.Fatalf("refund: %+v %v", refunded, err)
	}
	if _, err := b.TraderRefund(ctx, owner, inst, second.OrderID, "again"); !errors.Is(err, discord.ErrTraderOrderDone) {
		t.Fatalf("refunded twice: %v", err)
	}
	if wallet, _ := b.TraderWallet(ctx, buyer, inst); wallet.Balance != 5000-600-1500 {
		t.Fatalf("balance after the refund: %d", wallet.Balance)
	}
	rec.mu.Lock()
	announced := len(rec.events)
	rec.mu.Unlock()
	if announced != 0 {
		t.Fatalf("a trader order or refund reached the public economy feed: %+v", rec.events)
	}

	// Closing with a note; opening clears it.
	hub, _, err := b.TraderSetOpen(ctx, ownerPanel, inst, false, "Restocking, back at 8")
	if err != nil || hub.Open || hub.ClosedNote != "Restocking, back at 8" {
		t.Fatalf("close with a note: %+v %v", hub, err)
	}
	if hub, _, err = b.TraderSetOpen(ctx, ownerPanel, inst, true, ""); err != nil || !hub.Open || hub.ClosedNote != "" {
		t.Fatalf("open: %+v %v", hub, err)
	}

	// Opening hours that don't include now close the trader at once, and only once.
	if _, _, err := b.TraderSetHours(ctx, ownerPanel, inst, "25:00", "02:00", "UTC"); !errors.Is(err, discord.ErrTraderHoursBad) {
		t.Fatalf("bad hours: %v", err)
	}
	now := time.Now().UTC()
	from, to := now.Add(2*time.Hour).Format("15:04"), now.Add(4*time.Hour).Format("15:04")
	hub, changed, err := b.TraderSetHours(ctx, withRole, inst, from, to, "UTC")
	if err != nil || !changed || hub.Open || hub.HoursOpen != from || hub.HoursTZ != "UTC" {
		t.Fatalf("set hours: %+v %v %v", hub, changed, err)
	}
	if _, changed, err := b.TraderApplyHours(ctx, inst); err != nil || changed {
		t.Fatalf("the same closing time applied twice: %v %v", changed, err)
	}
	// Opened by hand: that lasts until the next opening or closing time.
	if _, _, err := b.TraderSetOpen(ctx, ownerPanel, inst, true, ""); err != nil {
		t.Fatal(err)
	}
	if hub, changed, err := b.TraderApplyHours(ctx, inst); err != nil || changed || !hub.Open {
		t.Fatalf("hours undid a manual open: %+v %v %v", hub, changed, err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE trader_hubs SET hours_applied_at = NOW() - INTERVAL '2 days' WHERE installation_id=$1`, inst); err != nil {
		t.Fatal(err)
	}
	if hub, changed, err := b.TraderApplyHours(ctx, inst); err != nil || !changed || hub.Open {
		t.Fatalf("a closing time passed: %+v %v %v", hub, changed, err)
	}
	if hub, _, err := b.TraderSetHours(ctx, ownerPanel, inst, "", "", ""); err != nil || hub.HoursOpen != "" || hub.Open {
		t.Fatalf("hours off keeps the state: %+v %v", hub, err)
	}

	var actions []string
	rows, err := w.a.DB.Pool.Query(ctx, `SELECT action FROM admin_audit_log WHERE installation_id=$1 AND after_state->>'source'='discord_trader' ORDER BY id`, inst)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, a)
	}
	rows.Close()
	want := []string{"TRADER_SETUP", "TRADER_DEAL_START", "TRADER_SALE_START", "TRADER_OFFER_END", "TRADER_OFFER_END", "TRADER_ORDER_DELIVERED",
		"TRADER_ORDER_REFUNDED", "TRADER_CLOSE", "TRADER_OPEN", "TRADER_HOURS", "TRADER_OPEN", "TRADER_HOURS"}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("audit:\n got %v\nwant %v", actions, want)
	}
}
