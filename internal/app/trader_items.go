package app

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

// Trader items (docs/TRADER.md): the Trader role, Administrators and the Owner add and edit the shop's
// items from #trader-panel, through the same shop service, validation and catalog as the website.

func (b *traderBackend) itemScope(ctx context.Context, c discord.PanelCaller, installationID int64) (repository.PanelServer, repository.EconomyScope, error) {
	row, _, err := b.allowed(ctx, c, installationID)
	if err != nil {
		return row, repository.EconomyScope{}, err
	}
	scope, err := b.scope(ctx, c.DiscordGuildID, installationID)
	if err != nil {
		return row, repository.EconomyScope{}, traderErr(err)
	}
	return row, scope, nil
}

func (b *traderBackend) toTraderItem(ctx context.Context, scope repository.EconomyScope, p repository.ShopProduct, mapSet bool) discord.TraderItem {
	it := discord.TraderItem{ID: p.ID, Name: p.Name, Description: p.Description, Price: p.PricePoints, CategoryName: p.CategoryName,
		Active: p.IsActive, Featured: p.IsFeatured, Limit: p.PurchaseLimit, DropOff: p.DeliveryPolicy == repository.DeliveryPolicyManualCoordinate}
	if p.CategoryID != nil {
		it.CategoryID = *p.CategoryID
	}
	if p.StockMode == shop.StockFinite && p.StockQuantity != nil {
		stock := *p.StockQuantity
		it.Stock = &stock
	}
	it.NeedsMap = it.DropOff && !mapSet
	if automatic, err := b.a.shopAutomatic(ctx, scope, p.ID); err == nil {
		it.Automatic = automatic
	}
	return it
}

func (b *traderBackend) TraderItemCategories(ctx context.Context, c discord.PanelCaller, installationID int64) ([]discord.TraderCategory, error) {
	_, scope, err := b.itemScope(ctx, c, installationID)
	if err != nil {
		return nil, err
	}
	cats, err := b.a.Shop.Categories(ctx, scope, true)
	if err != nil {
		return nil, err
	}
	out := make([]discord.TraderCategory, 0, len(cats))
	for _, cat := range cats {
		if cat.IsActive {
			out = append(out, discord.TraderCategory{ID: cat.ID, Name: cat.Name, Count: cat.ProductCount})
		}
	}
	return out, nil
}

func (b *traderBackend) TraderItems(ctx context.Context, c discord.PanelCaller, installationID, categoryID int64) ([]discord.TraderItem, error) {
	_, scope, err := b.itemScope(ctx, c, installationID)
	if err != nil {
		return nil, err
	}
	q := shop.ProductList{Limit: 25}
	if categoryID != 0 {
		cats, err := b.a.Shop.Categories(ctx, scope, true)
		if err != nil {
			return nil, err
		}
		for _, cat := range cats {
			if cat.ID == categoryID {
				q.CategorySlug = cat.Slug
			}
		}
		if q.CategorySlug == "" {
			return nil, nil
		}
	}
	page, err := b.a.Shop.Products(ctx, scope, q, true)
	if err != nil {
		return nil, err
	}
	mapSet := b.mapName(ctx, scope) != ""
	out := make([]discord.TraderItem, 0, len(page.Items))
	for _, p := range page.Items {
		out = append(out, b.toTraderItem(ctx, scope, p, mapSet))
	}
	return out, nil
}

func (b *traderBackend) TraderItem(ctx context.Context, c discord.PanelCaller, installationID, productID int64) (discord.TraderItem, error) {
	_, scope, err := b.itemScope(ctx, c, installationID)
	if err != nil {
		return discord.TraderItem{}, err
	}
	p, err := b.a.Shop.Product(ctx, scope, productID, true)
	if err != nil {
		return discord.TraderItem{}, itemErr(err)
	}
	return b.toTraderItem(ctx, scope, *p, b.mapName(ctx, scope) != ""), nil
}

// categoryFor is the id of the category named name (any capitals), made when there is none; nil for
// "". A category staff turned off on the website is refused rather than turned back on.
func (b *traderBackend) categoryFor(ctx context.Context, scope repository.EconomyScope, name string) (*int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	cats, err := b.a.Shop.Categories(ctx, scope, true)
	if err != nil {
		return nil, err
	}
	for _, cat := range cats {
		if strings.EqualFold(strings.TrimSpace(cat.Name), name) {
			if !cat.IsActive {
				return nil, &discord.TraderItemInvalid{Reason: "The category \"" + cat.Name + "\" is turned off on the Champion website. Turn it on there, or use another category."}
			}
			id := cat.ID
			return &id, nil
		}
	}
	made, err := b.a.Shop.CreateCategory(ctx, scope, shop.CategoryCreate{Name: name})
	if err != nil {
		return nil, itemErr(err)
	}
	return &made.ID, nil
}

// itemErr turns the shop's refusals of an item into words staff can act on.
func itemErr(err error) error {
	var v *shop.ValidationError
	switch {
	case errors.As(err, &v):
		reasons := map[string]string{
			"name":          "The name must be 1 to 80 characters.",
			"description":   "The description can be at most 1,000 characters.",
			"pricePoints":   "The price must be from 1 to 1,000,000,000 points.",
			"stockQuantity": "Stock must be from 0 to 1,000,000,000.",
			"purchaseLimit": "The limit per player must be from 1 to 1,000.",
		}
		fields := make([]string, 0, len(v.Issues))
		for f := range v.Issues {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		var parts []string
		for _, f := range fields {
			if r, ok := reasons[f]; ok {
				parts = append(parts, r)
			} else {
				parts = append(parts, v.Issues[f]+".")
			}
		}
		return &discord.TraderItemInvalid{Reason: strings.Join(parts, " ")}
	case errors.Is(err, repository.ErrShopProductNotFound):
		return discord.ErrTraderItemGone
	}
	return traderErr(err)
}

func (b *traderBackend) TraderAddItem(ctx context.Context, c discord.PanelCaller, installationID int64, in discord.TraderItemInput) (discord.TraderItem, error) {
	row, scope, err := b.itemScope(ctx, c, installationID)
	if err != nil {
		return discord.TraderItem{}, err
	}
	categoryID, err := b.categoryFor(ctx, scope, in.Category)
	if err != nil {
		return discord.TraderItem{}, err
	}
	create := shop.ProductCreate{Name: in.Name, Description: in.Description, CategoryID: categoryID, PricePoints: in.Price, ProductType: "ITEM",
		StockMode: shop.StockUnlimited}
	if in.Stock != nil {
		stock := *in.Stock
		create.StockMode, create.StockQuantity = shop.StockFinite, &stock
	}
	p, err := b.a.Shop.CreateProduct(ctx, scope, create)
	if err != nil {
		return discord.TraderItem{}, itemErr(err)
	}
	b.audit(ctx, c, row, "TRADER_ITEM_ADD", map[string]any{"product_id": p.ID, "name": p.Name, "price_points": p.PricePoints})
	return b.toTraderItem(ctx, scope, *p, b.mapName(ctx, scope) != ""), nil
}

func (b *traderBackend) TraderEditItem(ctx context.Context, c discord.PanelCaller, installationID, productID int64, in discord.TraderItemEdit) (discord.TraderItem, error) {
	row, scope, err := b.itemScope(ctx, c, installationID)
	if err != nil {
		return discord.TraderItem{}, err
	}
	up := shop.ProductUpdate{Name: in.Name, Description: in.Description, PricePoints: in.Price, IsActive: in.Active, IsFeatured: in.Featured}
	var changed []string
	if in.Name != nil || in.Description != nil || in.Price != nil {
		changed = append(changed, "details")
	}
	if in.Category != nil {
		id, err := b.categoryFor(ctx, scope, *in.Category)
		if err != nil {
			return discord.TraderItem{}, err
		}
		if id == nil {
			up.CategoryID = shop.Optional[int64]{Set: true, Null: true}
		} else {
			up.CategoryID = shop.Optional[int64]{Set: true, Value: *id}
		}
	}
	switch {
	case in.Unlimited:
		mode := shop.StockUnlimited
		up.StockMode = &mode
		changed = append(changed, "stock")
	case in.Stock != nil:
		mode, stock := shop.StockFinite, *in.Stock
		up.StockMode, up.StockQuantity = &mode, &stock
		changed = append(changed, "stock")
	}
	switch {
	case in.ClearLimit:
		up.PurchaseLimit = shop.Optional[int]{Set: true, Null: true}
	case in.Limit != nil:
		up.PurchaseLimit = shop.Optional[int]{Set: true, Value: *in.Limit}
	}
	if in.DropOff != nil {
		policy := shop.PolicyManualPickup
		if *in.DropOff {
			policy = shop.PolicyManualCoordinate
		}
		up.DeliveryPolicy = &policy
		changed = append(changed, "delivery")
	}
	if in.Active != nil {
		changed = append(changed, "active")
	}
	if in.Featured != nil {
		changed = append(changed, "featured")
	}
	p, _, err := b.a.Shop.UpdateProduct(ctx, scope, productID, up)
	if err != nil {
		return discord.TraderItem{}, itemErr(err)
	}
	b.audit(ctx, c, row, "TRADER_ITEM_EDIT", map[string]any{"product_id": p.ID, "changed": changed, "price_points": p.PricePoints, "is_active": p.IsActive})
	return b.toTraderItem(ctx, scope, *p, b.mapName(ctx, scope) != ""), nil
}
