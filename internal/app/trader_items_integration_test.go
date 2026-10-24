//go:build integration

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The Trader role adds and edits the shop's items from Discord: the website shop's own catalog and
// validation, scoped to its installation, audited.
func TestTraderItemsAreTheShopsCatalog(t *testing.T) {
	w := newFactionWorld(t)
	w.a.ServerPanels = repository.NewServerPanelRepository(w.a.DB.Pool)
	w.a.Traders = repository.NewTraderRepository(w.a.DB.Pool)
	w.a.ShopDiscounts = repository.NewShopDiscountRepository(w.a.DB.Pool)
	w.a.AdminAudit = repository.NewAuditRepository(w.a.DB.Pool)
	b := newTraderBackend(w.a)
	ctx := context.Background()
	guild, inst := w.a1.DiscordGuildID, w.a1.InstallationID
	owner := discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: w.a1.OwnerDiscordID}
	if _, err := b.TraderSaveHub(ctx, owner, discord.TraderHub{InstallationID: inst, TraderChannelID: "trader", TraderRoleID: "role-trader"}); err != nil {
		t.Fatal(err)
	}
	trader := discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: w.players[2], RoleIDs: []string{"role-trader"}}
	player := discord.PanelCaller{DiscordGuildID: guild, DiscordUserID: w.players[0]}

	stock := int64(10)
	if _, err := b.TraderAddItem(ctx, player, inst, discord.TraderItemInput{Name: "Free stuff", Price: 1}); !errors.Is(err, discord.ErrTraderForbidden) {
		t.Fatalf("a player added an item: %v", err)
	}
	var invalid *discord.TraderItemInvalid
	if _, err := b.TraderAddItem(ctx, trader, inst, discord.TraderItemInput{Name: "Too cheap", Price: 0}); !errors.As(err, &invalid) || invalid.Reason != "The price must be from 1 to 1,000,000,000 points." {
		t.Fatalf("a price of 0: %v", err)
	}
	kit, err := b.TraderAddItem(ctx, trader, inst, discord.TraderItemInput{Name: "M4-A1 Kit", Price: 1800, Stock: &stock, Category: "Weapons", Description: "Rifle and mags"})
	if err != nil || kit.ID == 0 || !kit.Active || kit.Price != 1800 || kit.Stock == nil || *kit.Stock != 10 || kit.CategoryName != "Weapons" || kit.DropOff {
		t.Fatalf("add: %+v %v", kit, err)
	}
	// The same category whatever its capitals; no stock is unlimited.
	olga, err := b.TraderAddItem(ctx, trader, inst, discord.TraderItemInput{Name: "Olga 24", Price: 25000, Category: "weapons"})
	if err != nil || olga.CategoryID != kit.CategoryID || olga.Stock != nil {
		t.Fatalf("second item: %+v %v", olga, err)
	}
	cats, err := b.TraderItemCategories(ctx, trader, inst)
	if err != nil || len(cats) != 1 || cats[0].Name != "Weapons" {
		t.Fatalf("categories: %+v %v", cats, err)
	}
	// Players see it in the trader straight away.
	if p, err := b.TraderProduct(ctx, guild, inst, kit.ID); err != nil || p.Price != 1800 || p.Stock != discord.TraderStockIn {
		t.Fatalf("on sale: %+v %v", p, err)
	}

	// Edit: details, stock, limit, featured, drop-off, off and back on.
	name, desc, price, none := "M4-A1 Kit (full)", "Rifle, mags, optic", int64(2000), ""
	limit := 2
	kit, err = b.TraderEditItem(ctx, trader, inst, kit.ID, discord.TraderItemEdit{Name: &name, Description: &desc, Price: &price, Category: &none, Limit: &limit})
	if err != nil || kit.Name != name || kit.Price != 2000 || kit.CategoryID != 0 || kit.Limit == nil || *kit.Limit != 2 || kit.Stock == nil || *kit.Stock != 10 {
		t.Fatalf("edit details: %+v %v", kit, err)
	}
	if kit, err = b.TraderEditItem(ctx, trader, inst, kit.ID, discord.TraderItemEdit{Unlimited: true, ClearLimit: true}); err != nil || kit.Stock != nil || kit.Limit != nil {
		t.Fatalf("unlimited, no limit: %+v %v", kit, err)
	}
	zero := int64(0)
	if kit, err = b.TraderEditItem(ctx, trader, inst, kit.ID, discord.TraderItemEdit{Stock: &zero}); err != nil || kit.Stock == nil || *kit.Stock != 0 {
		t.Fatalf("sold out: %+v %v", kit, err)
	}
	if p, _ := b.TraderProduct(ctx, guild, inst, kit.ID); p.Stock != discord.TraderStockOut {
		t.Fatalf("players see it sold out: %+v", p)
	}
	on, off := true, false
	if kit, err = b.TraderEditItem(ctx, trader, inst, kit.ID, discord.TraderItemEdit{Featured: &on, DropOff: &on}); err != nil || !kit.Featured || !kit.DropOff || !kit.NeedsMap {
		t.Fatalf("featured drop-off without a map: %+v %v", kit, err)
	}
	if kit, err = b.TraderEditItem(ctx, trader, inst, kit.ID, discord.TraderItemEdit{Active: &off}); err != nil || kit.Active {
		t.Fatalf("off: %+v %v", kit, err)
	}
	if _, err := b.TraderProduct(ctx, guild, inst, kit.ID); !errors.Is(err, discord.ErrTraderUnavailable) {
		t.Fatalf("players still see an item that's off: %v", err)
	}
	items, err := b.TraderItems(ctx, trader, inst, 0)
	if err != nil || len(items) != 2 {
		t.Fatalf("staff still see items that are off: %+v %v", items, err)
	}
	if inCat, err := b.TraderItems(ctx, trader, inst, olga.CategoryID); err != nil || len(inCat) != 1 || inCat[0].ID != olga.ID {
		t.Fatalf("items of a category: %+v %v", inCat, err)
	}
	bad := int64(-1)
	if _, err := b.TraderEditItem(ctx, trader, inst, kit.ID, discord.TraderItemEdit{Stock: &bad}); !errors.As(err, &invalid) {
		t.Fatalf("negative stock: %v", err)
	}
	if _, err := b.TraderEditItem(ctx, trader, inst, 99999999, discord.TraderItemEdit{Active: &on}); !errors.Is(err, discord.ErrTraderItemGone) {
		t.Fatalf("an item that doesn't exist: %v", err)
	}
	// Another server's item is not this trader's.
	other := w.product(w.b1, "Other server's item", 100, nil)
	if _, err := b.TraderEditItem(ctx, trader, inst, other, discord.TraderItemEdit{Active: &off}); !errors.Is(err, discord.ErrTraderItemGone) {
		t.Fatalf("edited another server's item: %v", err)
	}
	// A category turned off on the website is not turned back on from Discord.
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE shop_categories SET is_active=FALSE WHERE id=$1`, olga.CategoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.TraderAddItem(ctx, trader, inst, discord.TraderItemInput{Name: "Mosin", Price: 900, Category: "WEAPONS"}); !errors.As(err, &invalid) {
		t.Fatalf("an item in a category that's off: %v", err)
	}

	var adds, edits int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE action='TRADER_ITEM_ADD'), COUNT(*) FILTER (WHERE action='TRADER_ITEM_EDIT')
FROM admin_audit_log WHERE installation_id=$1 AND after_state->>'source'='discord_trader'`, inst).Scan(&adds, &edits); err != nil || adds != 2 || edits != 5 {
		t.Fatalf("audit: %d adds %d edits %v", adds, edits, err)
	}
}
