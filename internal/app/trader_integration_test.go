//go:build integration

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The Discord Trader buys through the website shop: the same debit, stock, limit, idempotency and
// delivery record. Its open/closed switch is the Trader role's, the Owner's and Administrators',
// and it is audited.
func TestTraderBuysThroughTheShop(t *testing.T) {
	w := newFactionWorld(t)
	w.a.ServerPanels = repository.NewServerPanelRepository(w.a.DB.Pool)
	w.a.Traders = repository.NewTraderRepository(w.a.DB.Pool)
	w.a.AdminAudit = repository.NewAuditRepository(w.a.DB.Pool)
	b := newTraderBackend(w.a)
	ctx := context.Background()
	guild, inst := w.a1.DiscordGuildID, w.a1.InstallationID
	owner := discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: w.a1.OwnerDiscordID}

	if _, err := b.TraderHub(ctx, guild, inst); !errors.Is(err, discord.ErrTraderNotSetUp) {
		t.Fatalf("before setup: %v", err)
	}
	player := w.players[0]
	setup := discord.TraderHub{InstallationID: inst, CategoryID: "cat", VoiceChannelID: "voice", TraderChannelID: "trader", DealsChannelID: "deals",
		PanelChannelID: "panel", OrdersChannelID: "orders", TraderRoleID: "role-trader"}
	if _, err := b.TraderSaveHub(ctx, discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: player}, setup); !errors.Is(err, discord.ErrPanelForbidden) {
		t.Fatalf("a player set up the trader: %v", err)
	}
	hub, err := b.TraderSaveHub(ctx, owner, setup)
	if err != nil || !hub.Open || hub.TraderChannelID != "trader" || hub.TraderRoleID != "role-trader" || hub.ServerName == "" {
		t.Fatalf("setup: %+v %v", hub, err)
	}
	if _, err := b.TraderHub(ctx, "another-discord-server", inst); !errors.Is(err, discord.ErrPanelServerNotFound) {
		t.Fatalf("another Discord server's trader: %v", err)
	}
	hubs, err := b.TraderHubs(ctx)
	listed := false
	for _, h := range hubs {
		listed = listed || h.InstallationID == inst && h.ServerName == hub.ServerName
	}
	if err != nil || !listed {
		t.Fatalf("hubs: %+v %v", hubs, err)
	}

	pid := w.linkPlayer(w.a1, player, "Rusty Axe")
	item := w.product(w.a1, "M4-A1 Kit", 600, map[string]any{"stockMode": "FINITE", "stockQuantity": 3})
	drop := w.product(w.a1, "Supply Drop", 100, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE"})
	w.grant(w.a1, pid, 1000)
	rec := &ecoRecorder{}
	w.a.EconomyService.SetNotifier(rec)

	buyer := discord.TraderCaller{PanelCaller: discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: player}, Username: "rusty"}
	if wallet, err := b.TraderWallet(ctx, buyer, inst); err != nil || wallet.Balance != 1000 || wallet.Gamertag != "Rusty Axe" || len(wallet.Recent) == 0 {
		t.Fatalf("wallet: %+v %v", wallet, err)
	}
	stranger := discord.TraderCaller{PanelCaller: discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: w.players[1]}}
	if _, err := b.TraderWallet(ctx, stranger, inst); !errors.Is(err, discord.ErrTraderNotLinked) {
		t.Fatalf("an unlinked player's wallet: %v", err)
	}
	products, err := b.TraderProducts(ctx, guild, inst, 0)
	if err != nil || len(products) != 2 {
		t.Fatalf("products: %+v %v", products, err)
	}
	if p, err := b.TraderProduct(ctx, guild, inst, drop); err != nil || !p.Coordinates || p.Stock != discord.TraderStockUnlimited {
		t.Fatalf("coordinate product: %+v %v", p, err)
	}

	key := "discord-trader.0123456789abcdef"
	r, err := b.TraderBuy(ctx, buyer, inst, discord.TraderBuy{ProductID: item, Quantity: 1, Key: key})
	if err != nil || r.Total != 600 || r.Balance != 400 || r.OrderID == 0 || r.Duplicate || r.Item != "M4-A1 Kit" || r.Gamertag != "Rusty Axe" {
		t.Fatalf("buy: %+v %v", r, err)
	}
	again, err := b.TraderBuy(ctx, buyer, inst, discord.TraderBuy{ProductID: item, Quantity: 1, Key: key})
	if err != nil || !again.Duplicate || again.OrderID != r.OrderID || again.Balance != 400 {
		t.Fatalf("a second click on the same confirm: %+v %v", again, err)
	}
	if _, err := b.TraderBuy(ctx, buyer, inst, discord.TraderBuy{ProductID: item, Quantity: 1, Key: "discord-trader.1111111111111111"}); !errors.Is(err, discord.ErrTraderFunds) {
		t.Fatalf("buying without the points: %v", err)
	}
	x, z := 4621.0, 10318.0
	if _, err := b.TraderBuy(ctx, buyer, inst, discord.TraderBuy{ProductID: drop, Quantity: 1, Key: "discord-trader.2222222222222222", X: &x, Z: &z}); !errors.Is(err, discord.ErrTraderNoMap) {
		t.Fatalf("a drop-off with no delivery map: %v", err)
	}
	rec.mu.Lock()
	announced := len(rec.events)
	rec.mu.Unlock()
	if announced != 0 {
		t.Fatalf("a trader order was announced in the public economy feed: %+v", rec.events)
	}
	orders, err := b.TraderOrders(ctx, buyer, inst)
	if err != nil || len(orders) != 1 || orders[0].ID != r.OrderID || orders[0].Status != discord.TraderOrderWaiting || orders[0].Items != "1 × M4-A1 Kit" {
		t.Fatalf("orders: %+v %v", orders, err)
	}
	cat, err := b.TraderCatalog(ctx, guild, inst)
	if err != nil || cat.OrdersWaiting != 1 || len(cat.StockAlerts) != 1 || cat.StockAlerts[0].Stock != discord.TraderStockLow {
		t.Fatalf("catalog: %+v %v", cat, err)
	}

	// The Trader role, the Owner and Administrators (a mapped staff role or Discord's Administrator)
	// open and close it; a Moderator without the Trader role can't. Closed takes no orders.
	if _, _, err := b.TraderSetOpen(ctx, buyer.PanelCaller, inst, false); !errors.Is(err, discord.ErrTraderForbidden) {
		t.Fatalf("a player closed the trader: %v", err)
	}
	for role, level := range map[string]string{"role-admins": "ADMINISTRATOR", "role-mods": "MODERATOR"} {
		if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO installation_role_permissions(installation_id,discord_role_id,permission_level) VALUES($1,$2,$3)`, inst, role, level); err != nil {
			t.Fatal(err)
		}
	}
	w.a.Permissions = repository.NewPermissionsRepository(w.a.DB.Pool)
	mod := discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: w.players[3], RoleIDs: []string{"role-mods"}}
	if _, _, err := b.TraderSetOpen(ctx, mod, inst, false); !errors.Is(err, discord.ErrTraderForbidden) {
		t.Fatalf("a Moderator closed the trader: %v", err)
	}
	if staff, err := b.TraderStaff(ctx, guild, inst); err != nil || len(staff.RoleIDs) != 1 || staff.RoleIDs[0] != "role-admins" || staff.OwnerDiscordID != w.a1.OwnerDiscordID {
		t.Fatalf("staff: %+v %v", staff, err)
	}
	withRole := discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: w.players[3], RoleIDs: []string{"role-mods", "role-trader"}}
	if _, changed, err := b.TraderSetOpen(ctx, withRole, inst, true); err != nil || changed {
		t.Fatalf("the Trader role opened an open trader: %v %v", changed, err)
	}
	trader := discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: w.players[2], RoleIDs: []string{"role-admins"}}
	closed, changed, err := b.TraderSetOpen(ctx, trader, inst, false)
	if err != nil || !changed || closed.Open || closed.OpenChangedByDiscordID != w.players[2] {
		t.Fatalf("close: %+v %v %v", closed, changed, err)
	}
	if _, changed, err := b.TraderSetOpen(ctx, trader, inst, false); err != nil || changed {
		t.Fatalf("closing a closed trader changed it: %v %v", changed, err)
	}
	w.grant(w.a1, pid, 1000)
	if _, err := b.TraderBuy(ctx, buyer, inst, discord.TraderBuy{ProductID: item, Quantity: 1, Key: "discord-trader.3333333333333333"}); !errors.Is(err, discord.ErrTraderClosed) {
		t.Fatalf("bought from a closed trader: %v", err)
	}
	// Setting up again keeps it closed.
	if again, err := b.TraderSaveHub(ctx, owner, setup); err != nil || again.Open {
		t.Fatalf("re-running setup reopened the trader: %+v %v", again, err)
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
	if len(actions) != 3 || actions[0] != "TRADER_SETUP" || actions[1] != "TRADER_CLOSE" || actions[2] != "TRADER_SETUP" {
		t.Fatalf("audit: %v", actions)
	}
	if err := w.a.Traders.SetVoiceApplied(ctx, inst, discord.TraderVoiceClosed); err != nil {
		t.Fatal(err)
	}
	if h, err := b.TraderHub(ctx, guild, inst); err != nil || h.VoiceNameApplied != discord.TraderVoiceClosed {
		t.Fatalf("voice name: %+v %v", h, err)
	}
}
