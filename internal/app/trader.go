package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/dayzmap"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

// traderBackend serves the Discord Trader (internal/discord/trader.go, docs/TRADER.md). Buying is
// the website shop's own purchase: the same prices, stock, limits, single Champion Points debit,
// delivery record and per-person rate limit.
type traderBackend struct {
	a     *App
	panel *serverPanelBackend
	// Used only when the website API (which owns the shared limiter) is not running.
	ownPurchaseLimiter *saasRateLimiter
}

func newTraderBackend(a *App) *traderBackend {
	return &traderBackend{a: a, panel: newServerPanelBackend(a), ownPurchaseLimiter: newSaaSRateLimiter(time.Minute, 10)}
}

func (b *traderBackend) TraderServers(ctx context.Context, discordGuildID string) ([]discord.TraderServer, error) {
	rows, err := b.panel.servers(ctx, discordGuildID)
	if err != nil {
		return nil, err
	}
	out := make([]discord.TraderServer, 0, len(rows))
	for _, row := range rows {
		out = append(out, discord.TraderServer{InstallationID: row.InstallationID, Name: row.Name})
	}
	return out, nil
}

func (b *traderBackend) TraderLevel(ctx context.Context, c discord.PanelCaller, installationID int64) (permissions.Level, error) {
	return b.panel.PanelLevel(ctx, c, installationID)
}

// hub is installationID's server row and Trader, when it is an installation of discordGuildID.
func (b *traderBackend) hub(ctx context.Context, discordGuildID string, installationID int64) (repository.PanelServer, *repository.TraderHub, error) {
	row, err := b.panel.server(ctx, discordGuildID, installationID)
	if err != nil {
		return repository.PanelServer{}, nil, err
	}
	if b.a.Traders == nil {
		return row, nil, errors.New("trader repository unavailable")
	}
	hub, err := b.a.Traders.Get(ctx, installationID)
	return row, hub, err
}

func toDiscordTraderHub(h repository.TraderHub, serverName string) discord.TraderHub {
	return discord.TraderHub{InstallationID: h.InstallationID, DiscordGuildID: h.DiscordGuildID, ServerName: serverName,
		CategoryID: h.CategoryID, VoiceChannelID: h.VoiceChannelID, TraderChannelID: h.TraderChannelID, DealsChannelID: h.DealsChannelID,
		PanelChannelID: h.PanelChannelID, OrdersChannelID: h.OrdersChannelID, TraderRoleID: h.TraderRoleID,
		PostMessageID: h.PostMessageID, PanelMessageID: h.PanelMessageID, Open: h.Open, VoiceNameApplied: h.VoiceNameApplied,
		OpenChangedAt: h.OpenChangedAt, OpenChangedByDiscordID: h.OpenChangedByDiscordID}
}

func (b *traderBackend) TraderHub(ctx context.Context, discordGuildID string, installationID int64) (discord.TraderHub, error) {
	row, hub, err := b.hub(ctx, discordGuildID, installationID)
	if err != nil {
		return discord.TraderHub{}, err
	}
	if hub == nil {
		return discord.TraderHub{}, discord.ErrTraderNotSetUp
	}
	return toDiscordTraderHub(*hub, row.Name), nil
}

func (b *traderBackend) TraderHubs(ctx context.Context) ([]discord.TraderHub, error) {
	if b.a.Traders == nil {
		return nil, nil
	}
	hubs, err := b.a.Traders.List(ctx)
	if err != nil {
		return nil, err
	}
	names := map[int64]string{}
	seen := map[string]bool{}
	out := make([]discord.TraderHub, 0, len(hubs))
	for _, h := range hubs {
		if !seen[h.DiscordGuildID] {
			seen[h.DiscordGuildID] = true
			rows, err := b.panel.servers(ctx, h.DiscordGuildID)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				names[row.InstallationID] = row.Name
			}
		}
		name, ok := names[h.InstallationID]
		if !ok {
			// The installation moved to another Discord server or lost its game server.
			continue
		}
		out = append(out, toDiscordTraderHub(h, name))
	}
	return out, nil
}

// planAllows is ErrTraderPlan when the organization's plan doesn't include the shop.
func (b *traderBackend) planAllows(ctx context.Context, organizationID int64) error {
	if !entitlements.Enforced() {
		return nil
	}
	plan, err := b.a.organizationPlan(ctx, organizationID)
	if err != nil {
		return err
	}
	if !entitlements.Has(plan, entitlements.Economy) {
		return discord.ErrTraderPlan
	}
	return nil
}

func (b *traderBackend) TraderSaveHub(ctx context.Context, c discord.PanelCaller, hub discord.TraderHub) (discord.TraderHub, error) {
	row, _, err := b.hub(ctx, c.DiscordGuildID, hub.InstallationID)
	if err != nil {
		return discord.TraderHub{}, err
	}
	level, err := b.panel.PanelLevel(ctx, c, hub.InstallationID)
	if err != nil {
		return discord.TraderHub{}, err
	}
	if level < permissions.LevelAdministrator {
		return discord.TraderHub{}, discord.ErrPanelForbidden
	}
	if err := b.planAllows(ctx, row.OrganizationID); err != nil {
		return discord.TraderHub{}, err
	}
	saved, err := b.a.Traders.Save(ctx, repository.TraderHub{InstallationID: hub.InstallationID, OrganizationID: row.OrganizationID, DiscordGuildID: c.DiscordGuildID,
		CategoryID: hub.CategoryID, VoiceChannelID: hub.VoiceChannelID, TraderChannelID: hub.TraderChannelID, DealsChannelID: hub.DealsChannelID,
		PanelChannelID: hub.PanelChannelID, OrdersChannelID: hub.OrdersChannelID, TraderRoleID: hub.TraderRoleID,
		PostMessageID: hub.PostMessageID, PanelMessageID: hub.PanelMessageID})
	if err != nil {
		return discord.TraderHub{}, err
	}
	b.audit(ctx, c, row, "TRADER_SETUP", map[string]any{"trader_role_id": saved.TraderRoleID, "category_id": saved.CategoryID})
	return toDiscordTraderHub(*saved, row.Name), nil
}

// isTrader: the caller has the Trader role, or is an Administrator or the Owner.
func (b *traderBackend) isTrader(ctx context.Context, c discord.PanelCaller, hub repository.TraderHub) (bool, error) {
	if hub.TraderRoleID != "" {
		for _, r := range c.RoleIDs {
			if r == hub.TraderRoleID {
				return true, nil
			}
		}
	}
	level, err := b.panel.PanelLevel(ctx, c, hub.InstallationID)
	if err != nil {
		return false, err
	}
	return level >= permissions.LevelAdministrator, nil
}

func (b *traderBackend) TraderSetOpen(ctx context.Context, c discord.PanelCaller, installationID int64, open bool) (discord.TraderHub, bool, error) {
	row, hub, err := b.hub(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return discord.TraderHub{}, false, err
	}
	if hub == nil {
		return discord.TraderHub{}, false, discord.ErrTraderNotSetUp
	}
	ok, err := b.isTrader(ctx, c, *hub)
	if err != nil {
		return discord.TraderHub{}, false, err
	}
	if !ok {
		return discord.TraderHub{}, false, discord.ErrTraderForbidden
	}
	updated, changed, err := b.a.Traders.SetOpen(ctx, installationID, open, c.DiscordUserID)
	if err != nil {
		return discord.TraderHub{}, false, err
	}
	if updated == nil {
		return discord.TraderHub{}, false, discord.ErrTraderNotSetUp
	}
	if changed {
		action := "TRADER_OPEN"
		if !open {
			action = "TRADER_CLOSE"
		}
		b.audit(ctx, c, row, action, nil)
	}
	return toDiscordTraderHub(*updated, row.Name), changed, nil
}

func (b *traderBackend) TraderSetMessages(ctx context.Context, installationID int64, postMessageID, panelMessageID string) error {
	return b.a.Traders.SetMessages(ctx, installationID, postMessageID, panelMessageID)
}

func (b *traderBackend) TraderVoiceApplied(ctx context.Context, installationID int64, name string) error {
	return b.a.Traders.SetVoiceApplied(ctx, installationID, name)
}

// scope is installationID's economy scope, when it is an installation of discordGuildID whose plan
// includes the shop.
func (b *traderBackend) scope(ctx context.Context, discordGuildID string, installationID int64) (repository.EconomyScope, error) {
	if b.a.Shop == nil || b.a.EconomyAccounts == nil {
		return repository.EconomyScope{}, errors.New("shop unavailable")
	}
	row, err := b.panel.server(ctx, discordGuildID, installationID)
	if err != nil {
		return repository.EconomyScope{}, err
	}
	if err := b.planAllows(ctx, row.OrganizationID); err != nil {
		return repository.EconomyScope{}, err
	}
	return b.a.EconomyAccounts.Scope(ctx, row.OrganizationID, installationID)
}

func (b *traderBackend) toProduct(ctx context.Context, scope repository.EconomyScope, p repository.ShopProduct) discord.TraderProduct {
	out := discord.TraderProduct{ID: p.ID, Name: p.Name, Description: p.Description, Price: p.PricePoints, Limit: p.PurchaseLimit,
		Featured: p.IsFeatured, CategoryName: p.CategoryName, Coordinates: p.DeliveryPolicy == repository.DeliveryPolicyManualCoordinate}
	if p.CategoryID != nil {
		out.CategoryID = *p.CategoryID
	}
	switch shop.StockState(p) {
	case "OUT_OF_STOCK":
		out.Stock = discord.TraderStockOut
	case "LOW_STOCK":
		out.Stock = discord.TraderStockLow
	case "IN_STOCK":
		out.Stock = discord.TraderStockIn
	default:
		out.Stock = discord.TraderStockUnlimited
	}
	if automatic, err := b.a.shopAutomatic(ctx, scope, p.ID); err == nil {
		out.Automatic = automatic
	}
	return out
}

func (b *traderBackend) mapName(ctx context.Context, scope repository.EconomyScope) string {
	st, err := b.a.Shop.DeliverySettings(ctx, scope)
	if err != nil {
		return ""
	}
	if m, ok := dayzmap.Lookup(st.MapKey); ok {
		return m.DisplayName
	}
	return ""
}

func (b *traderBackend) TraderCatalog(ctx context.Context, discordGuildID string, installationID int64) (discord.TraderCatalog, error) {
	scope, err := b.scope(ctx, discordGuildID, installationID)
	if err != nil {
		return discord.TraderCatalog{}, traderErr(err)
	}
	cats, err := b.a.Shop.Categories(ctx, scope, false)
	if err != nil {
		return discord.TraderCatalog{}, err
	}
	var out discord.TraderCatalog
	for _, c := range cats {
		if c.IsActive && c.ProductCount > 0 {
			out.Categories = append(out.Categories, discord.TraderCategory{ID: c.ID, Name: c.Name, Count: c.ProductCount})
		}
	}
	page, err := b.a.Shop.Products(ctx, scope, shop.ProductList{Limit: shop.MaxLimit}, false)
	if err != nil {
		return discord.TraderCatalog{}, err
	}
	for _, p := range page.Items {
		tp := b.toProduct(ctx, scope, p)
		if tp.Featured {
			out.Featured = append(out.Featured, tp)
		}
		if tp.Stock == discord.TraderStockLow || tp.Stock == discord.TraderStockOut {
			out.StockAlerts = append(out.StockAlerts, tp)
		}
	}
	if n, err := b.a.Traders.OrdersWaiting(ctx, installationID); err == nil {
		out.OrdersWaiting = n
	}
	out.MapName = b.mapName(ctx, scope)
	return out, nil
}

func (b *traderBackend) TraderProducts(ctx context.Context, discordGuildID string, installationID, categoryID int64) ([]discord.TraderProduct, error) {
	scope, err := b.scope(ctx, discordGuildID, installationID)
	if err != nil {
		return nil, traderErr(err)
	}
	q := shop.ProductList{Limit: 25}
	if categoryID != 0 {
		cats, err := b.a.Shop.Categories(ctx, scope, false)
		if err != nil {
			return nil, err
		}
		found := false
		for _, c := range cats {
			if c.ID == categoryID && c.IsActive {
				q.CategorySlug, found = c.Slug, true
			}
		}
		if !found {
			return nil, nil
		}
	}
	page, err := b.a.Shop.Products(ctx, scope, q, false)
	if err != nil {
		return nil, err
	}
	out := make([]discord.TraderProduct, 0, len(page.Items))
	for _, p := range page.Items {
		out = append(out, b.toProduct(ctx, scope, p))
	}
	return out, nil
}

func (b *traderBackend) TraderProduct(ctx context.Context, discordGuildID string, installationID, productID int64) (discord.TraderProduct, error) {
	scope, err := b.scope(ctx, discordGuildID, installationID)
	if err != nil {
		return discord.TraderProduct{}, traderErr(err)
	}
	p, err := b.a.Shop.Product(ctx, scope, productID, false)
	if err != nil {
		return discord.TraderProduct{}, traderErr(err)
	}
	if p == nil || !p.IsActive {
		return discord.TraderProduct{}, discord.ErrTraderUnavailable
	}
	return b.toProduct(ctx, scope, *p), nil
}

func (b *traderBackend) TraderWallet(ctx context.Context, c discord.TraderCaller, installationID int64) (discord.TraderWallet, error) {
	scope, err := b.scope(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return discord.TraderWallet{}, traderErr(err)
	}
	acct, err := b.a.EconomyAccounts.Me(ctx, scope, c.DiscordUserID)
	if err != nil {
		return discord.TraderWallet{}, traderErr(err)
	}
	w := discord.TraderWallet{Gamertag: acct.Gamertag, Balance: acct.Balance}
	if page, err := b.a.EconomyAccounts.Transactions(ctx, scope, acct.AccountID, 5, "", repository.TransactionFilter{}, false); err == nil {
		for _, t := range page.Items {
			w.Recent = append(w.Recent, discord.TraderEntry{At: t.CreatedAt, Amount: t.Amount, Credit: t.Direction == economy.DirectionCredit, Description: t.Description})
		}
	}
	return w, nil
}

func (b *traderBackend) TraderOrders(ctx context.Context, c discord.TraderCaller, installationID int64) ([]discord.TraderOrder, error) {
	scope, err := b.scope(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return nil, traderErr(err)
	}
	page, err := b.a.Shop.MyPurchases(ctx, scope, c.DiscordUserID, shop.PurchaseList{Limit: 5})
	if err != nil {
		return nil, traderErr(err)
	}
	out := make([]discord.TraderOrder, 0, len(page.Items))
	for _, p := range page.Items {
		o := discord.TraderOrder{ID: p.ID, Total: p.TotalPoints, Status: p.Status, At: p.CreatedAt, Items: purchaseItemsText(p.Items)}
		if p.Delivery != nil {
			o.X, o.Z = p.Delivery.X, p.Delivery.Z
		}
		out = append(out, o)
	}
	return out, nil
}

func purchaseItemsText(items []repository.ShopPurchaseItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%d × %s", it.Quantity, it.ProductName))
	}
	return strings.Join(parts, ", ")
}

func (b *traderBackend) purchaseLimiter() *saasRateLimiter {
	if b.a.saasShopPurchaseLimiter != nil {
		return b.a.saasShopPurchaseLimiter
	}
	return b.ownPurchaseLimiter
}

func (b *traderBackend) TraderBuy(ctx context.Context, c discord.TraderCaller, installationID int64, req discord.TraderBuy) (discord.TraderReceipt, error) {
	_, hub, err := b.hub(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return discord.TraderReceipt{}, err
	}
	if hub == nil {
		return discord.TraderReceipt{}, discord.ErrTraderNotSetUp
	}
	if !hub.Open {
		return discord.TraderReceipt{}, discord.ErrTraderClosed
	}
	scope, err := b.scope(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return discord.TraderReceipt{}, traderErr(err)
	}
	if !b.purchaseLimiter().Allow(c.DiscordUserID) {
		return discord.TraderReceipt{}, discord.ErrPanelRateLimited
	}
	// Automatic delivery drops the item at the buyer's logged position, which the website checks
	// at checkout; Discord can't, so those items stay on the website.
	if automatic, err := b.a.shopAutomatic(ctx, scope, req.ProductID); err != nil {
		return discord.TraderReceipt{}, err
	} else if automatic {
		return discord.TraderReceipt{}, discord.ErrTraderAutomatic
	}
	if b.a.SaaSUsers == nil {
		return discord.TraderReceipt{}, errors.New("user repository unavailable")
	}
	user, err := b.a.SaaSUsers.EnsureDiscordUser(ctx, c.DiscordUserID, c.Username, c.GlobalName, c.Avatar)
	if err != nil {
		return discord.TraderReceipt{}, err
	}
	if user.BannedAt != nil {
		return discord.TraderReceipt{}, discord.ErrTraderBanned
	}
	pr := shop.PurchaseRequest{ProductID: req.ProductID, Quantity: req.Quantity, IdempotencyKey: req.Key, Quiet: true}
	if req.X != nil && req.Z != nil {
		pr.Delivery = &shop.DeliveryInput{X: req.X, Z: req.Z}
	}
	res, err := b.a.Shop.Purchase(ctx, scope, user.ID, c.DiscordUserID, pr)
	if err != nil {
		return discord.TraderReceipt{}, traderErr(err)
	}
	slog.Info("component=discord", "event", "trader_order", "installation_id", installationID, "purchase_id", res.Purchase.ID,
		"product_id", req.ProductID, "quantity", req.Quantity, "total_points", res.Purchase.TotalPoints, "duplicate", res.Duplicate)
	r := discord.TraderReceipt{OrderID: res.Purchase.ID, Item: res.ItemName, Quantity: req.Quantity, Total: res.Purchase.TotalPoints,
		Balance: res.BalanceAfter, Gamertag: res.PlayerName, Duplicate: res.Duplicate}
	if len(res.Purchase.Items) > 0 {
		r.Item, r.Quantity = res.Purchase.Items[0].ProductName, res.Purchase.Items[0].Quantity
	}
	if d := res.Purchase.Delivery; d != nil {
		r.X, r.Z = d.X, d.Z
		if m, ok := dayzmap.Lookup(d.MapKey); ok {
			r.MapName = m.DisplayName
		}
	}
	return r, nil
}

// traderErr turns shop and economy refusals into the trader's.
func traderErr(err error) error {
	switch {
	case errors.Is(err, economy.ErrIdentityRequired):
		return discord.ErrTraderNotLinked
	case errors.Is(err, repository.ErrInsufficientFunds):
		return discord.ErrTraderFunds
	case errors.Is(err, repository.ErrShopOutOfStock):
		return discord.ErrTraderSoldOut
	case errors.Is(err, repository.ErrShopLimitReached):
		return discord.ErrTraderLimit
	case errors.Is(err, repository.ErrShopProductNotFound), errors.Is(err, repository.ErrShopProductDisabled):
		return discord.ErrTraderUnavailable
	case errors.Is(err, repository.ErrShopDeliveryMapUnresolved):
		return discord.ErrTraderNoMap
	case errors.Is(err, repository.ErrShopDeliveryOutOfBounds), errors.Is(err, shop.ErrInvalidCoordinates):
		return discord.ErrTraderOffMap
	case errors.Is(err, economy.ErrSuspended):
		return discord.ErrTraderSuspended
	case errors.Is(err, economy.ErrNoServer):
		return discord.ErrTraderNoServer
	}
	return err
}

// audit records a trader change in the admin audit log, best-effort.
func (b *traderBackend) audit(ctx context.Context, c discord.PanelCaller, row repository.PanelServer, action string, extra map[string]any) {
	if b.a.AdminAudit == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	installationID := row.InstallationID
	after := map[string]any{"source": "discord_trader"}
	for k, v := range extra {
		after[k] = v
	}
	entry := repository.AuditEntry{OrganizationID: row.OrganizationID, InstallationID: &installationID, ActorUserID: b.panel.account(writeCtx, c.DiscordUserID),
		ActorDiscordID: c.DiscordUserID, Action: action, Target: "trader", Result: "success"}
	if raw, err := json.Marshal(after); err == nil {
		entry.AfterState = raw
	}
	if err := b.a.AdminAudit.Record(writeCtx, entry); err != nil {
		slog.Warn("component=discord", "msg", "trader audit write failed", "action", action, "err", err.Error())
	}
}
