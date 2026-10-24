package app

import (
	"context"
	"errors"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

// Trader phase 2 (docs/TRADER.md): order handover and refunds from the order cards, deals and
// sales, announcements and opening hours. Everything here is for the people who run the trader:
// the Trader role, Administrators and the Owner.

// allowed is installationID's server row and Trader when the caller runs it.
func (b *traderBackend) allowed(ctx context.Context, c discord.PanelCaller, installationID int64) (repository.PanelServer, *repository.TraderHub, error) {
	row, hub, err := b.hub(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return row, nil, err
	}
	if hub == nil {
		return row, nil, discord.ErrTraderNotSetUp
	}
	ok, err := b.isTrader(ctx, c, *hub)
	if err != nil {
		return row, nil, err
	}
	if !ok {
		return row, nil, discord.ErrTraderForbidden
	}
	return row, hub, nil
}

func (b *traderBackend) TraderAllowed(ctx context.Context, c discord.PanelCaller, installationID int64) error {
	_, _, err := b.allowed(ctx, c, installationID)
	return err
}

// staffUser is the caller's Champion account, made on first use like a buyer's: the shop records
// who delivered or refunded an order by account.
func (b *traderBackend) staffUser(ctx context.Context, c discord.TraderCaller) (int64, error) {
	if b.a.SaaSUsers == nil {
		return 0, errors.New("user repository unavailable")
	}
	user, err := b.a.SaaSUsers.EnsureDiscordUser(ctx, c.DiscordUserID, c.Username, c.GlobalName, c.Avatar)
	if err != nil {
		return 0, err
	}
	return user.ID, nil
}

func toTraderOrder(p repository.ShopPurchase) discord.TraderOrder {
	o := discord.TraderOrder{ID: p.ID, Total: p.TotalPoints, Status: p.Status, At: p.CreatedAt, Items: purchaseItemsText(p.Items)}
	if p.Delivery != nil {
		o.X, o.Z = p.Delivery.X, p.Delivery.Z
	}
	return o
}

// orderErr turns the shop's refusals of a handover or refund into the trader's.
func orderErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrShopInvalidStatus):
		return discord.ErrTraderOrderDone
	case errors.Is(err, repository.ErrShopPurchaseNotFound):
		return discord.ErrTraderOrderGone
	case errors.Is(err, shop.ErrReasonRequired):
		return discord.ErrTraderReason
	}
	return traderErr(err)
}

func (b *traderBackend) TraderDeliver(ctx context.Context, c discord.TraderCaller, installationID, orderID int64) (discord.TraderOrder, error) {
	row, _, err := b.allowed(ctx, c.PanelCaller, installationID)
	if err != nil {
		return discord.TraderOrder{}, err
	}
	scope, err := b.scope(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return discord.TraderOrder{}, traderErr(err)
	}
	userID, err := b.staffUser(ctx, c)
	if err != nil {
		return discord.TraderOrder{}, err
	}
	p, err := b.a.Shop.Fulfill(ctx, scope, userID, orderID)
	if err != nil {
		return discord.TraderOrder{}, orderErr(err)
	}
	b.audit(ctx, c.PanelCaller, row, "TRADER_ORDER_DELIVERED", map[string]any{"purchase_id": p.ID})
	return toTraderOrder(*p), nil
}

func (b *traderBackend) TraderRefund(ctx context.Context, c discord.TraderCaller, installationID, orderID int64, reason string) (discord.TraderOrder, error) {
	row, _, err := b.allowed(ctx, c.PanelCaller, installationID)
	if err != nil {
		return discord.TraderOrder{}, err
	}
	scope, err := b.scope(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return discord.TraderOrder{}, traderErr(err)
	}
	userID, err := b.staffUser(ctx, c)
	if err != nil {
		return discord.TraderOrder{}, err
	}
	res, err := b.a.Shop.RefundQuiet(ctx, scope, userID, c.DiscordUserID, orderID, reason)
	if err != nil {
		return discord.TraderOrder{}, orderErr(err)
	}
	b.audit(ctx, c.PanelCaller, row, "TRADER_ORDER_REFUNDED", map[string]any{"purchase_id": res.Purchase.ID, "total_points": res.Purchase.TotalPoints})
	return toTraderOrder(res.Purchase), nil
}

// runningOffers is installationID's running deals and sales (none without the repository).
func (b *traderBackend) runningOffers(ctx context.Context, installationID int64) ([]repository.ShopDiscount, error) {
	if b.a.ShopDiscounts == nil {
		return nil, nil
	}
	return b.a.ShopDiscounts.Running(ctx, installationID)
}

func toTraderOffer(d repository.ShopDiscount) discord.TraderOffer {
	o := discord.TraderOffer{ID: d.ID, Kind: d.Kind, Name: d.ProductName, PercentOff: d.PercentOff, Title: d.Title, EndsAt: d.EndsAt,
		Ended: d.EndedAt != nil || !d.EndsAt.After(time.Now()), PostChannelID: d.PostChannelID, PostMessageID: d.PostMessageID}
	if d.ProductID != nil {
		o.ProductID = *d.ProductID
	}
	if d.CategoryID != nil {
		o.CategoryID = *d.CategoryID
		o.Name = d.CategoryName
	}
	return o
}

func (b *traderBackend) discounts() (*repository.ShopDiscountRepository, error) {
	if b.a.ShopDiscounts == nil {
		return nil, errors.New("deals unavailable")
	}
	return b.a.ShopDiscounts, nil
}

func (b *traderBackend) TraderStartOffer(ctx context.Context, c discord.PanelCaller, installationID int64, in discord.TraderOfferInput) (discord.TraderOffer, error) {
	row, _, err := b.allowed(ctx, c, installationID)
	if err != nil {
		return discord.TraderOffer{}, err
	}
	if _, err := b.scope(ctx, c.DiscordGuildID, installationID); err != nil {
		return discord.TraderOffer{}, traderErr(err)
	}
	if in.PercentOff < 1 || in.PercentOff > discord.TraderMaxPercentOff || in.Hours < 1 || in.Hours > discord.TraderMaxOfferHours {
		return discord.TraderOffer{}, discord.ErrTraderOfferBad
	}
	repo, err := b.discounts()
	if err != nil {
		return discord.TraderOffer{}, err
	}
	input := repository.ShopDiscountInput{OrganizationID: row.OrganizationID, InstallationID: installationID, PercentOff: in.PercentOff,
		Title: in.Title, Duration: time.Duration(in.Hours) * time.Hour, CreatedByDiscordID: c.DiscordUserID}
	switch in.Kind {
	case discord.TraderOfferDeal:
		if in.ProductID <= 0 {
			return discord.TraderOffer{}, discord.ErrTraderOfferBad
		}
		product := in.ProductID
		input.Kind, input.ProductID = repository.DiscountDeal, &product
	case discord.TraderOfferSale:
		input.Kind = repository.DiscountSale
		if in.CategoryID > 0 {
			category := in.CategoryID
			input.CategoryID = &category
		}
	default:
		return discord.TraderOffer{}, discord.ErrTraderOfferBad
	}
	d, err := repo.Create(ctx, input)
	switch {
	case errors.Is(err, repository.ErrDiscountNotFound):
		return discord.TraderOffer{}, discord.ErrTraderUnavailable
	case errors.Is(err, repository.ErrDiscountInvalid):
		return discord.TraderOffer{}, discord.ErrTraderOfferBad
	case err != nil:
		return discord.TraderOffer{}, err
	}
	action := "TRADER_DEAL_START"
	if d.Kind == repository.DiscountSale {
		action = "TRADER_SALE_START"
	}
	b.audit(ctx, c, row, action, map[string]any{"discount_id": d.ID, "percent_off": d.PercentOff, "hours": in.Hours, "product_id": d.ProductID, "category_id": d.CategoryID})
	return toTraderOffer(*d), nil
}

func (b *traderBackend) TraderEndOffer(ctx context.Context, c discord.PanelCaller, installationID, offerID int64) (discord.TraderOffer, error) {
	row, _, err := b.allowed(ctx, c, installationID)
	if err != nil {
		return discord.TraderOffer{}, err
	}
	repo, err := b.discounts()
	if err != nil {
		return discord.TraderOffer{}, err
	}
	d, changed, err := repo.End(ctx, installationID, offerID, c.DiscordUserID)
	if errors.Is(err, repository.ErrDiscountNotFound) {
		return discord.TraderOffer{}, discord.ErrTraderOfferBad
	}
	if err != nil {
		return discord.TraderOffer{}, err
	}
	if changed {
		b.audit(ctx, c, row, "TRADER_OFFER_END", map[string]any{"discount_id": d.ID})
	}
	return toTraderOffer(*d), nil
}

func (b *traderBackend) TraderEndAllOffers(ctx context.Context, c discord.PanelCaller, installationID int64) ([]discord.TraderOffer, error) {
	row, _, err := b.allowed(ctx, c, installationID)
	if err != nil {
		return nil, err
	}
	repo, err := b.discounts()
	if err != nil {
		return nil, err
	}
	ended, err := repo.EndAll(ctx, installationID, c.DiscordUserID)
	if err != nil {
		return nil, err
	}
	out := make([]discord.TraderOffer, 0, len(ended))
	ids := make([]int64, 0, len(ended))
	for _, d := range ended {
		out = append(out, toTraderOffer(d))
		ids = append(ids, d.ID)
	}
	if len(ids) > 0 {
		b.audit(ctx, c, row, "TRADER_OFFER_END", map[string]any{"discount_ids": ids})
	}
	return out, nil
}

func (b *traderBackend) TraderOffers(ctx context.Context, discordGuildID string, installationID int64) ([]discord.TraderOffer, error) {
	if _, err := b.panel.server(ctx, discordGuildID, installationID); err != nil {
		return nil, err
	}
	running, err := b.runningOffers(ctx, installationID)
	if err != nil {
		return nil, err
	}
	out := make([]discord.TraderOffer, 0, len(running))
	for _, d := range running {
		out = append(out, toTraderOffer(d))
	}
	return out, nil
}

func (b *traderBackend) TraderOfferPosted(ctx context.Context, installationID, offerID int64, channelID, messageID string) error {
	repo, err := b.discounts()
	if err != nil {
		return err
	}
	return repo.SetPost(ctx, installationID, offerID, channelID, messageID)
}

func (b *traderBackend) TraderOfferPostsToEnd(ctx context.Context, installationID int64) ([]discord.TraderOffer, error) {
	if b.a.ShopDiscounts == nil {
		return nil, nil
	}
	over, err := b.a.ShopDiscounts.PostsToEnd(ctx, installationID)
	if err != nil {
		return nil, err
	}
	out := make([]discord.TraderOffer, 0, len(over))
	for _, d := range over {
		out = append(out, toTraderOffer(d))
	}
	return out, nil
}

func (b *traderBackend) TraderOfferPostEnded(ctx context.Context, installationID, offerID int64) error {
	repo, err := b.discounts()
	if err != nil {
		return err
	}
	return repo.MarkPostEnded(ctx, installationID, offerID)
}

func (b *traderBackend) TraderAnnounced(ctx context.Context, c discord.PanelCaller, installationID int64, title string) error {
	row, _, err := b.allowed(ctx, c, installationID)
	if err != nil {
		return err
	}
	b.audit(ctx, c, row, "TRADER_ANNOUNCE", map[string]any{"title": title})
	return nil
}

func (b *traderBackend) TraderSetHours(ctx context.Context, c discord.PanelCaller, installationID int64, open, close, tz string) (discord.TraderHub, bool, error) {
	row, hub, err := b.allowed(ctx, c, installationID)
	if err != nil {
		return discord.TraderHub{}, false, err
	}
	open, close, tz, err = discord.ParseTraderHours(open, close, tz)
	if err != nil {
		return discord.TraderHub{}, false, err
	}
	var at *time.Time
	shouldOpen := hub.Open
	if open != "" {
		state, boundary, ok := discord.TraderHoursState(open, close, tz, time.Now())
		if !ok {
			return discord.TraderHub{}, false, discord.ErrTraderHoursBad
		}
		shouldOpen, at = state, &boundary
	}
	updated, err := b.a.Traders.SetHours(ctx, installationID, open, close, tz, at)
	if err != nil {
		return discord.TraderHub{}, false, err
	}
	if updated == nil {
		return discord.TraderHub{}, false, discord.ErrTraderNotSetUp
	}
	changed := false
	if updated.Open != shouldOpen {
		if updated, changed, err = b.a.Traders.SetOpen(ctx, installationID, shouldOpen, c.DiscordUserID, ""); err != nil {
			return discord.TraderHub{}, false, err
		}
		if updated == nil {
			return discord.TraderHub{}, false, discord.ErrTraderNotSetUp
		}
	}
	b.audit(ctx, c, row, "TRADER_HOURS", map[string]any{"open": open, "close": close, "tz": tz})
	return toDiscordTraderHub(*updated, row.Name), changed, nil
}

func (b *traderBackend) TraderApplyHours(ctx context.Context, installationID int64) (discord.TraderHub, bool, error) {
	hub, err := b.a.Traders.Get(ctx, installationID)
	if err != nil || hub == nil {
		return discord.TraderHub{}, false, err
	}
	if hub.HoursOpen == "" {
		return toDiscordTraderHub(*hub, ""), false, nil
	}
	shouldOpen, at, ok := discord.TraderHoursState(hub.HoursOpen, hub.HoursClose, hub.HoursTZ, time.Now())
	if !ok {
		return toDiscordTraderHub(*hub, ""), false, nil
	}
	updated, applied, changed, err := b.a.Traders.ApplyHours(ctx, installationID, shouldOpen, at)
	if err != nil {
		return discord.TraderHub{}, false, err
	}
	if !applied || updated == nil {
		return toDiscordTraderHub(*hub, ""), false, nil
	}
	return toDiscordTraderHub(*updated, ""), changed, nil
}
