package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

// Champion Shop API (docs/SHOP.md). Players browse the active catalog and buy with Champion Points
// through their VERIFIED DayZ link; organization OWNER/ADMIN users manage products, categories and
// purchases (a faction role grants nothing). Every route is scoped organization + installation; the
// handlers only translate HTTP - validation, identity and the atomic purchase live in internal/shop
// and repository.ShopRepository, and the Points debit is the economy's one ledger path.

const (
	codeShopProductNotFound   = "SHOP_PRODUCT_NOT_FOUND"
	codeShopProductDisabled   = "SHOP_PRODUCT_DISABLED"
	codeShopCategoryNotFound  = "SHOP_CATEGORY_NOT_FOUND"
	codeOutOfStock            = "OUT_OF_STOCK"
	codePurchaseLimitReached  = "PURCHASE_LIMIT_REACHED"
	codeInvalidQuantity       = "INVALID_QUANTITY"
	codeDuplicatePurchase     = "DUPLICATE_PURCHASE"
	codePurchaseNotFound      = "PURCHASE_NOT_FOUND"
	codeInvalidPurchaseState  = "INVALID_PURCHASE_STATUS"
	codeDeliveryAttemptActive = "DELIVERY_ATTEMPT_ACTIVE"
	codeShopForbidden         = "SHOP_FORBIDDEN"
)

func init() {
	httpStatusForCode[codeShopProductNotFound] = http.StatusNotFound
	httpStatusForCode[codeShopProductDisabled] = http.StatusConflict
	httpStatusForCode[codeShopCategoryNotFound] = http.StatusNotFound
	httpStatusForCode[codeOutOfStock] = http.StatusConflict
	httpStatusForCode[codePurchaseLimitReached] = http.StatusConflict
	httpStatusForCode[codeInvalidQuantity] = http.StatusBadRequest
	httpStatusForCode[codeDuplicatePurchase] = http.StatusConflict
	httpStatusForCode[codePurchaseNotFound] = http.StatusNotFound
	httpStatusForCode[codeInvalidPurchaseState] = http.StatusConflict
	httpStatusForCode[codeDeliveryAttemptActive] = http.StatusConflict
	httpStatusForCode[codeShopForbidden] = http.StatusForbidden
}

func (a *App) registerShopRoutes() {
	if a.saasShopPurchaseLimiter == nil {
		a.saasShopPurchaseLimiter = newSaaSRateLimiter(time.Minute, 10)
	}
	if a.saasShopAdminLimiter == nil {
		a.saasShopAdminLimiter = newSaaSRateLimiter(time.Minute, 30)
	}
	const base = "/api/saas/organizations/{organizationID}/installations/{installationID}/shop"
	h := a.HTTPServer.Handle
	// Player routes.
	h("GET "+base+"/categories", a.handleShopCategories)
	h("GET "+base+"/products", a.handleShopProducts)
	h("GET "+base+"/products/{productID}", a.handleShopProduct)
	h("POST "+base+"/purchases", a.handleShopPurchase)
	h("GET "+base+"/me/purchases", a.handleShopMyPurchases)
	h("GET "+base+"/me/purchases/{purchaseID}", a.handleShopMyPurchase)
	// Admin routes (organization OWNER/ADMIN).
	h("GET "+base+"/admin/categories", a.handleShopAdminCategories)
	h("POST "+base+"/categories", a.handleShopCreateCategory)
	h("PUT "+base+"/categories/{categoryID}", a.handleShopUpdateCategory)
	h("GET "+base+"/admin/products", a.handleShopAdminProducts)
	h("GET "+base+"/admin/products/{productID}", a.handleShopAdminProduct)
	h("POST "+base+"/products", a.handleShopCreateProduct)
	h("PUT "+base+"/products/{productID}", a.handleShopUpdateProduct)
	h("GET "+base+"/admin/purchases", a.handleShopAdminPurchases)
	h("GET "+base+"/admin/purchases/{purchaseID}", a.handleShopAdminPurchase)
	h("POST "+base+"/purchases/{purchaseID}/fulfill", a.handleShopFulfill)
	h("POST "+base+"/purchases/{purchaseID}/refund", a.handleShopRefund)
	a.registerShopDeliveryRoutes(base)
	a.registerShopCanaryRoutes(base)
}

func (a *App) shopContext(w http.ResponseWriter, r *http.Request, admin bool) (economyRequest, bool) {
	if a.Shop == nil {
		writeSaaSError(w, codeInternalError, "shop unavailable")
		return economyRequest{}, false
	}
	code := ""
	if admin {
		code = codeShopForbidden
	}
	return a.scopedContext(w, r, code)
}

func shopAudit(event string, er economyRequest, attrs ...any) {
	base := []any{"event", event, "organization_id", er.scope.OrganizationID, "installation_id", er.scope.InstallationID, "acting_user_id", er.user.ID}
	slog.Info("component=saas_api", append(base, attrs...)...)
}

// shopFailed maps shop errors to the stable contract; anything else is an economy/identity error.
func shopFailed(w http.ResponseWriter, what string, err error) {
	var ve *shop.ValidationError
	switch {
	case errors.As(err, &ve):
		fields := make([]string, 0, len(ve.Issues))
		for f := range ve.Issues {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		parts := make([]string, 0, len(fields))
		for _, f := range fields {
			parts = append(parts, ve.Issues[f])
		}
		writeSaaSError(w, codeInvalidRequest, strings.Join(parts, "; "))
	case errors.Is(err, repository.ErrShopProductNotFound):
		writeSaaSError(w, codeShopProductNotFound, "product not found")
	case errors.Is(err, repository.ErrShopProductDisabled):
		writeSaaSError(w, codeShopProductDisabled, "this product is not available")
	case errors.Is(err, repository.ErrShopCategoryNotFound):
		writeSaaSError(w, codeShopCategoryNotFound, "category not found")
	case errors.Is(err, repository.ErrShopOutOfStock):
		writeSaaSError(w, codeOutOfStock, "this product is out of stock")
	case errors.Is(err, repository.ErrShopLimitReached):
		writeSaaSError(w, codePurchaseLimitReached, "the purchase limit for this product has been reached")
	case errors.Is(err, shop.ErrInvalidQuantity):
		writeSaaSError(w, codeInvalidQuantity, "quantity must be a whole number from 1 to 100")
	case errors.Is(err, repository.ErrInsufficientFunds):
		writeSaaSError(w, codeInsufficientFunds, "not enough Champion Points for this purchase")
	case errors.Is(err, repository.ErrShopIdempotencyConflict):
		writeSaaSError(w, codeDuplicatePurchase, "this idempotency key was already used for a different purchase")
	case errors.Is(err, repository.ErrShopPurchaseNotFound):
		writeSaaSError(w, codePurchaseNotFound, "purchase not found")
	case errors.Is(err, repository.ErrShopInvalidStatus):
		writeSaaSError(w, codeInvalidPurchaseState, "the purchase is not in a state that allows this")
	case errors.Is(err, repository.ErrShopDeliveryAttemptActive):
		writeSaaSError(w, codeDeliveryAttemptActive, "an automatic delivery attempt may have put this item on the server: it cannot be refunded or fulfilled by hand until the attempt is resolved")
	case errors.Is(err, repository.ErrInvalidLedgerAmount):
		writeSaaSError(w, codeInvalidRequest, "the total price is out of range")
	case errors.Is(err, shop.ErrInvalidKey):
		writeSaaSError(w, codeInvalidRequest, "idempotencyKey must be 8-64 characters of A-Z a-z 0-9 . _ : -")
	case errors.Is(err, shop.ErrReasonRequired):
		writeSaaSError(w, codeInvalidRequest, "a reason is required")
	case errors.Is(err, shop.ErrInvalidStatus):
		writeSaaSError(w, codeInvalidRequest, "unknown status")
	case errors.Is(err, shop.ErrInvalidCursor):
		writeSaaSError(w, codeInvalidRequest, "invalid cursor")
	case errors.Is(err, shop.ErrInvalidQuery):
		writeSaaSError(w, codeInvalidRequest, "q must be at most 50 characters")
	default:
		if !shopDeliveryFailed(w, err) {
			economyFailed(w, what, err)
		}
	}
}

// --- DTOs (docs/SHOP.md "Website handoff") ----------------------------------------------------------------

type shopCategoryRefDTO struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type shopCategoryDTO struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Description  string `json:"description"`
	SortOrder    int    `json:"sortOrder"`
	ProductCount int    `json:"productCount"`
}

type adminShopCategoryDTO struct {
	shopCategoryDTO
	IsActive  bool   `json:"isActive"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type shopProductDTO struct {
	ID             int64               `json:"id"`
	Name           string              `json:"name"`
	Slug           string              `json:"slug"`
	Description    string              `json:"description"`
	PricePoints    int64               `json:"pricePoints"`
	Category       *shopCategoryRefDTO `json:"category"`
	ProductType    string              `json:"productType"`
	DeliveryType   string              `json:"deliveryType"`
	DeliveryPolicy string              `json:"deliveryPolicy"`
	IsFeatured     bool                `json:"isFeatured"`
	StockState     string              `json:"stockState"`
	PurchaseLimit  *int                `json:"purchaseLimit"`
}

type adminShopProductDTO struct {
	shopProductDTO
	IsActive      bool   `json:"isActive"`
	StockMode     string `json:"stockMode"`
	StockQuantity *int64 `json:"stockQuantity"`
	SortOrder     int    `json:"sortOrder"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

type shopPurchaseItemDTO struct {
	ProductID       *int64 `json:"productId"`
	ProductName     string `json:"productName"`
	UnitPricePoints int64  `json:"unitPricePoints"`
	Quantity        int    `json:"quantity"`
	LineTotalPoints int64  `json:"lineTotalPoints"`
}

type shopPurchaseDTO struct {
	ID           int64                 `json:"id"`
	Status       string                `json:"status"`
	TotalPoints  int64                 `json:"totalPoints"`
	DeliveryType string                `json:"deliveryType"`
	Items        []shopPurchaseItemDTO `json:"items"`
	CreatedAt    string                `json:"createdAt"`
	PaidAt       *string               `json:"paidAt"`
	FulfilledAt  *string               `json:"fulfilledAt"`
	RefundedAt   *string               `json:"refundedAt"`
	Delivery     *shopDeliveryDTO      `json:"delivery"`
}

type adminShopPurchaseDTO struct {
	shopPurchaseDTO
	Player                   economyPlayerDTO `json:"player"`
	GameServerID             *int64           `json:"gameServerId"`
	RefundReason             *string          `json:"refundReason"`
	FulfilledByDiscordUserID *string          `json:"fulfilledByDiscordUserId"`
	RefundedByDiscordUserID  *string          `json:"refundedByDiscordUserId"`
	// Delivery shadows the player-safe shopPurchaseDTO.Delivery with the admin view.
	Delivery *adminShopDeliveryDTO `json:"delivery"`
}

type economyPlayerDTO struct {
	AccountID int64  `json:"accountId"`
	Gamertag  string `json:"gamertag"`
}

type shopPage[T any] struct {
	Currency   economy.Currency `json:"currency"`
	Items      []T              `json:"items"`
	NextCursor *string          `json:"nextCursor"`
	Limit      int              `json:"limit"`
}

type shopPurchaseResponse[T any] struct {
	Currency         economy.Currency `json:"currency"`
	Purchase         T                `json:"purchase"`
	RemainingBalance int64            `json:"remainingBalance"`
	Duplicate        bool             `json:"duplicate"`
}

func categoryDTO(c repository.ShopCategory) shopCategoryDTO {
	return shopCategoryDTO{ID: c.ID, Name: c.Name, Slug: c.Slug, Description: c.Description, SortOrder: c.SortOrder, ProductCount: c.ProductCount}
}

func adminCategoryDTO(c repository.ShopCategory) adminShopCategoryDTO {
	return adminShopCategoryDTO{shopCategoryDTO: categoryDTO(c), IsActive: c.IsActive, CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: c.UpdatedAt.UTC().Format(time.RFC3339)}
}

func productDTO(p repository.ShopProduct) shopProductDTO {
	d := shopProductDTO{ID: p.ID, Name: p.Name, Slug: p.Slug, Description: p.Description, PricePoints: p.PricePoints, ProductType: p.ProductType,
		DeliveryType: p.DeliveryType, DeliveryPolicy: p.DeliveryPolicy, IsFeatured: p.IsFeatured, StockState: shop.StockState(p), PurchaseLimit: p.PurchaseLimit}
	if p.CategoryID != nil {
		d.Category = &shopCategoryRefDTO{ID: *p.CategoryID, Name: p.CategoryName, Slug: p.CategorySlug}
	}
	return d
}

func adminProductDTO(p repository.ShopProduct) adminShopProductDTO {
	return adminShopProductDTO{shopProductDTO: productDTO(p), IsActive: p.IsActive, StockMode: p.StockMode, StockQuantity: p.StockQuantity, SortOrder: p.SortOrder,
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339)}
}

func purchaseDTO(p repository.ShopPurchase) shopPurchaseDTO {
	d := shopPurchaseDTO{ID: p.ID, Status: p.Status, TotalPoints: p.TotalPoints, DeliveryType: p.DeliveryType, Items: make([]shopPurchaseItemDTO, 0, len(p.Items)),
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339), PaidAt: nullableTimeStr(p.PaidAt), FulfilledAt: nullableTimeStr(p.FulfilledAt), RefundedAt: nullableTimeStr(p.RefundedAt)}
	for _, it := range p.Items {
		d.Items = append(d.Items, shopPurchaseItemDTO{ProductID: it.ProductID, ProductName: it.ProductName, UnitPricePoints: it.UnitPricePoints, Quantity: it.Quantity, LineTotalPoints: it.LineTotalPoints})
	}
	if p.Delivery != nil {
		dd := deliveryDTO(*p.Delivery)
		d.Delivery = &dd
	}
	return d
}

func adminPurchaseDTO(p repository.ShopPurchase) adminShopPurchaseDTO {
	out := adminShopPurchaseDTO{shopPurchaseDTO: purchaseDTO(p), Player: economyPlayerDTO{AccountID: p.PlayerID, Gamertag: p.PlayerName}, GameServerID: p.GameServerID,
		RefundReason: optStr(p.RefundReason), FulfilledByDiscordUserID: optStr(p.FulfilledByDiscordID), RefundedByDiscordUserID: optStr(p.RefundedByDiscordID)}
	if p.Delivery != nil {
		dd := adminDeliveryDTO(*p.Delivery)
		out.Delivery = &dd
	}
	return out
}

// --- query helpers ----------------------------------------------------------------------------------

func shopQuery(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery) // a malformed query (e.g. a ';') is 400, never silently ignored
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, "malformed query string")
		return nil, false
	}
	return q, true
}

func optionalID(w http.ResponseWriter, q url.Values, name string) (int64, bool) {
	raw := strings.TrimSpace(q.Get(name))
	if raw == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeSaaSError(w, codeInvalidRequest, "invalid "+name)
		return 0, false
	}
	return id, true
}

// --- catalog (player) -------------------------------------------------------------------------------

func (a *App) handleShopCategories(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	cats, err := a.Shop.Categories(ctx, er.scope, false)
	if err != nil {
		shopFailed(w, "list shop categories", err)
		return
	}
	items := make([]shopCategoryDTO, 0, len(cats))
	for _, c := range cats {
		items = append(items, categoryDTO(c))
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Currency economy.Currency  `json:"currency"`
		Items    []shopCategoryDTO `json:"items"`
	}{economy.ChampionPoints, items})
}

func (a *App) listProducts(w http.ResponseWriter, r *http.Request, admin bool) {
	er, ok := a.shopContext(w, r, admin)
	if !ok {
		return
	}
	q, ok := shopQuery(w, r)
	if !ok {
		return
	}
	limit, ok := factionLimit(w, r, shop.DefaultLimit, shop.MaxLimit)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	page, err := a.Shop.Products(ctx, er.scope, shop.ProductList{CategorySlug: q.Get("category"), Search: q.Get("q"), Limit: limit, Cursor: strings.TrimSpace(q.Get("cursor"))}, admin)
	if err != nil {
		shopFailed(w, "list shop products", err)
		return
	}
	if admin {
		out := shopPage[adminShopProductDTO]{Currency: economy.ChampionPoints, Items: make([]adminShopProductDTO, 0, len(page.Items)), NextCursor: optStr(page.NextCursor), Limit: page.Limit}
		for _, p := range page.Items {
			out.Items = append(out.Items, adminProductDTO(p))
		}
		writeSaaSJSON(w, http.StatusOK, out)
		return
	}
	out := shopPage[shopProductDTO]{Currency: economy.ChampionPoints, Items: make([]shopProductDTO, 0, len(page.Items)), NextCursor: optStr(page.NextCursor), Limit: page.Limit}
	for _, p := range page.Items {
		out.Items = append(out.Items, productDTO(p))
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

func (a *App) handleShopProducts(w http.ResponseWriter, r *http.Request) { a.listProducts(w, r, false) }
func (a *App) handleShopAdminProducts(w http.ResponseWriter, r *http.Request) {
	a.listProducts(w, r, true)
}

func (a *App) getProduct(w http.ResponseWriter, r *http.Request, admin bool) {
	er, ok := a.shopContext(w, r, admin)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "productID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	p, err := a.Shop.Product(ctx, er.scope, id, admin)
	if err != nil {
		shopFailed(w, "load shop product", err)
		return
	}
	if admin {
		writeSaaSJSON(w, http.StatusOK, struct {
			Currency economy.Currency    `json:"currency"`
			Product  adminShopProductDTO `json:"product"`
		}{economy.ChampionPoints, adminProductDTO(*p)})
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Currency economy.Currency `json:"currency"`
		Product  shopProductDTO   `json:"product"`
	}{economy.ChampionPoints, productDTO(*p)})
}

func (a *App) handleShopProduct(w http.ResponseWriter, r *http.Request) { a.getProduct(w, r, false) }
func (a *App) handleShopAdminProduct(w http.ResponseWriter, r *http.Request) {
	a.getProduct(w, r, true)
}

// --- purchase (player) ------------------------------------------------------------------------------

type shopPurchaseBody struct {
	ProductID      json.Number       `json:"productId"`
	Quantity       json.Number       `json:"quantity"`
	IdempotencyKey string            `json:"idempotencyKey"`
	Delivery       *shopDeliveryBody `json:"delivery"`
}

// shopDeliveryBody is the purchase's delivery object: X/Z map coordinates. There is no y.
type shopDeliveryBody struct {
	X *float64 `json:"x"`
	Z *float64 `json:"z"`
}

// handleShopPurchase is POST .../shop/purchases: buy one product with Champion Points, atomically.
// 201 for a new purchase, 200 for an idempotent replay (the original purchase, nothing charged).
func (a *App) handleShopPurchase(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, false)
	if !ok || !enforceRateLimit(w, a.saasShopPurchaseLimiter, rateLimitKey(r)) {
		return
	}
	var body shopPurchaseBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	productID, err := strconv.ParseInt(body.ProductID.String(), 10, 64)
	if err != nil || productID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "productId must be a positive whole number")
		return
	}
	qty, err := strconv.Atoi(body.Quantity.String())
	if err != nil {
		writeSaaSError(w, codeInvalidQuantity, "quantity must be a whole number from 1 to 100")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	req := shop.PurchaseRequest{ProductID: productID, Quantity: qty, IdempotencyKey: body.IdempotencyKey}
	if body.Delivery != nil {
		req.Delivery = &shop.DeliveryInput{X: body.Delivery.X, Z: body.Delivery.Z}
	}
	res, err := a.Shop.Purchase(ctx, er.scope, er.user.ID, er.user.DiscordUserID, req)
	if err != nil {
		shopFailed(w, "shop purchase", err)
		return
	}
	shopAudit("shop_purchase_created", er, "purchase_id", res.Purchase.ID, "product_id", productID, "quantity", qty, "total_points", res.Purchase.TotalPoints, "duplicate", res.Duplicate,
		"delivery_policy", deliveryPolicyOf(res.Purchase))
	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	writeSaaSJSON(w, status, shopPurchaseResponse[shopPurchaseDTO]{Currency: economy.ChampionPoints, Purchase: purchaseDTO(res.Purchase), RemainingBalance: res.BalanceAfter, Duplicate: res.Duplicate})
}

func (a *App) purchaseList(w http.ResponseWriter, r *http.Request, admin bool) {
	er, ok := a.shopContext(w, r, admin)
	if !ok {
		return
	}
	q, ok := shopQuery(w, r)
	if !ok {
		return
	}
	limit, ok := factionLimit(w, r, shop.DefaultLimit, shop.MaxLimit)
	if !ok {
		return
	}
	list := shop.PurchaseList{Status: q.Get("status"), Limit: limit, Cursor: strings.TrimSpace(q.Get("cursor"))}
	if list.ProductID, ok = optionalID(w, q, "productId"); !ok {
		return
	}
	if admin {
		if list.PlayerID, ok = optionalID(w, q, "playerId"); !ok {
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	var page shop.PurchasePage
	var err error
	if admin {
		page, err = a.Shop.AdminPurchases(ctx, er.scope, list)
	} else {
		page, err = a.Shop.MyPurchases(ctx, er.scope, er.user.DiscordUserID, list)
	}
	if err != nil {
		shopFailed(w, "list shop purchases", err)
		return
	}
	if admin {
		out := shopPage[adminShopPurchaseDTO]{Currency: economy.ChampionPoints, Items: make([]adminShopPurchaseDTO, 0, len(page.Items)), NextCursor: optStr(page.NextCursor), Limit: page.Limit}
		for _, p := range page.Items {
			out.Items = append(out.Items, adminPurchaseDTO(p))
		}
		writeSaaSJSON(w, http.StatusOK, out)
		return
	}
	out := shopPage[shopPurchaseDTO]{Currency: economy.ChampionPoints, Items: make([]shopPurchaseDTO, 0, len(page.Items)), NextCursor: optStr(page.NextCursor), Limit: page.Limit}
	for _, p := range page.Items {
		out.Items = append(out.Items, purchaseDTO(p))
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

func (a *App) handleShopMyPurchases(w http.ResponseWriter, r *http.Request) {
	a.purchaseList(w, r, false)
}
func (a *App) handleShopAdminPurchases(w http.ResponseWriter, r *http.Request) {
	a.purchaseList(w, r, true)
}

func (a *App) purchaseDetail(w http.ResponseWriter, r *http.Request, admin bool) {
	er, ok := a.shopContext(w, r, admin)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	var p *repository.ShopPurchase
	var err error
	if admin {
		p, err = a.Shop.AdminPurchase(ctx, er.scope, id)
	} else {
		p, err = a.Shop.MyPurchase(ctx, er.scope, er.user.DiscordUserID, id) // another player's purchase is "not found"
	}
	if err != nil {
		shopFailed(w, "load shop purchase", err)
		return
	}
	if admin {
		writeSaaSJSON(w, http.StatusOK, struct {
			Currency economy.Currency     `json:"currency"`
			Purchase adminShopPurchaseDTO `json:"purchase"`
		}{economy.ChampionPoints, adminPurchaseDTO(*p)})
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Currency economy.Currency `json:"currency"`
		Purchase shopPurchaseDTO  `json:"purchase"`
	}{economy.ChampionPoints, purchaseDTO(*p)})
}

func (a *App) handleShopMyPurchase(w http.ResponseWriter, r *http.Request) {
	a.purchaseDetail(w, r, false)
}
func (a *App) handleShopAdminPurchase(w http.ResponseWriter, r *http.Request) {
	a.purchaseDetail(w, r, true)
}

// --- admin: categories ------------------------------------------------------------------------------

func (a *App) handleShopAdminCategories(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	cats, err := a.Shop.Categories(ctx, er.scope, true)
	if err != nil {
		shopFailed(w, "list shop categories", err)
		return
	}
	items := make([]adminShopCategoryDTO, 0, len(cats))
	for _, c := range cats {
		items = append(items, adminCategoryDTO(c))
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Currency economy.Currency       `json:"currency"`
		Items    []adminShopCategoryDTO `json:"items"`
	}{economy.ChampionPoints, items})
}

type shopCategoryBody struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	SortOrder   *int    `json:"sortOrder"`
	IsActive    *bool   `json:"isActive"`
}

func (a *App) handleShopCreateCategory(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	var body shopCategoryBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	in := shop.CategoryCreate{}
	if body.Name != nil {
		in.Name = *body.Name
	}
	if body.Description != nil {
		in.Description = *body.Description
	}
	if body.SortOrder != nil {
		in.SortOrder = *body.SortOrder
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	c, err := a.Shop.CreateCategory(ctx, er.scope, in)
	if err != nil {
		shopFailed(w, "create shop category", err)
		return
	}
	shopAudit("shop_category_created", er, "category_id", c.ID)
	writeSaaSJSON(w, http.StatusCreated, struct {
		Category adminShopCategoryDTO `json:"category"`
	}{adminCategoryDTO(*c)})
}

func (a *App) handleShopUpdateCategory(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	id, ok := pathInt64(w, r, "categoryID")
	if !ok {
		return
	}
	var body shopCategoryBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	c, err := a.Shop.UpdateCategory(ctx, er.scope, id, shop.CategoryUpdate{Name: body.Name, Description: body.Description, SortOrder: body.SortOrder, IsActive: body.IsActive})
	if err != nil {
		shopFailed(w, "update shop category", err)
		return
	}
	shopAudit("shop_category_updated", er, "category_id", c.ID, "is_active", c.IsActive)
	writeSaaSJSON(w, http.StatusOK, struct {
		Category adminShopCategoryDTO `json:"category"`
	}{adminCategoryDTO(*c)})
}

// --- admin: products --------------------------------------------------------------------------------

type shopProductCreateBody struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	CategoryID     *int64 `json:"categoryId"`
	PricePoints    int64  `json:"pricePoints"`
	ProductType    string `json:"productType"`
	DeliveryType   string `json:"deliveryType"`
	DeliveryPolicy string `json:"deliveryPolicy"`
	IsActive       *bool  `json:"isActive"`
	IsFeatured     bool   `json:"isFeatured"`
	SortOrder      int    `json:"sortOrder"`
	StockMode      string `json:"stockMode"`
	StockQuantity  *int64 `json:"stockQuantity"`
	PurchaseLimit  *int   `json:"purchaseLimit"`
}

func (a *App) handleShopCreateProduct(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	var b shopProductCreateBody
	if !decodeFactionBody(w, r, &b) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	p, err := a.Shop.CreateProduct(ctx, er.scope, shop.ProductCreate{Name: b.Name, Description: b.Description, CategoryID: b.CategoryID, PricePoints: b.PricePoints,
		ProductType: b.ProductType, DeliveryType: b.DeliveryType, DeliveryPolicy: b.DeliveryPolicy, IsActive: b.IsActive, IsFeatured: b.IsFeatured, SortOrder: b.SortOrder,
		StockMode: b.StockMode, StockQuantity: b.StockQuantity, PurchaseLimit: b.PurchaseLimit})
	if err != nil {
		shopFailed(w, "create shop product", err)
		return
	}
	shopAudit("shop_product_created", er, "product_id", p.ID, "price_points", p.PricePoints)
	writeSaaSJSON(w, http.StatusCreated, struct {
		Product adminShopProductDTO `json:"product"`
	}{adminProductDTO(*p)})
}

type shopProductUpdateBody struct {
	Name           *string              `json:"name"`
	Description    *string              `json:"description"`
	CategoryID     shop.Optional[int64] `json:"categoryId"`
	PricePoints    *int64               `json:"pricePoints"`
	DeliveryType   *string              `json:"deliveryType"`
	DeliveryPolicy *string              `json:"deliveryPolicy"`
	IsActive       *bool                `json:"isActive"`
	IsFeatured     *bool                `json:"isFeatured"`
	SortOrder      *int                 `json:"sortOrder"`
	StockMode      *string              `json:"stockMode"`
	StockQuantity  *int64               `json:"stockQuantity"`
	PurchaseLimit  shop.Optional[int]   `json:"purchaseLimit"`
}

// handleShopUpdateProduct is PUT .../shop/products/{productID}: every field is optional (absent = unchanged;
// categoryId / purchaseLimit accept null to clear). The tenant, the slug and the product type are not editable,
// and unknown keys (an image URL, an installation id) are rejected.
func (a *App) handleShopUpdateProduct(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	id, ok := pathInt64(w, r, "productID")
	if !ok {
		return
	}
	var b shopProductUpdateBody
	if !decodeFactionBody(w, r, &b) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	p, wasActive, err := a.Shop.UpdateProduct(ctx, er.scope, id, shop.ProductUpdate{Name: b.Name, Description: b.Description, CategoryID: b.CategoryID, PricePoints: b.PricePoints,
		DeliveryType: b.DeliveryType, DeliveryPolicy: b.DeliveryPolicy, IsActive: b.IsActive, IsFeatured: b.IsFeatured, SortOrder: b.SortOrder, StockMode: b.StockMode, StockQuantity: b.StockQuantity, PurchaseLimit: b.PurchaseLimit})
	if err != nil {
		shopFailed(w, "update shop product", err)
		return
	}
	event := "shop_product_updated"
	if wasActive && !p.IsActive {
		event = "shop_product_disabled"
	}
	shopAudit(event, er, "product_id", p.ID, "price_points", p.PricePoints, "is_active", p.IsActive, "delivery_policy", p.DeliveryPolicy)
	writeSaaSJSON(w, http.StatusOK, struct {
		Product adminShopProductDTO `json:"product"`
	}{adminProductDTO(*p)})
}

// --- admin: fulfillment and refunds -----------------------------------------------------------------

func (a *App) handleShopFulfill(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	p, err := a.Shop.Fulfill(ctx, er.scope, er.user.ID, id)
	if err != nil {
		shopFailed(w, "fulfill shop purchase", err)
		return
	}
	shopAudit("shop_purchase_fulfilled", er, "purchase_id", p.ID)
	writeSaaSJSON(w, http.StatusOK, struct {
		Currency economy.Currency     `json:"currency"`
		Purchase adminShopPurchaseDTO `json:"purchase"`
	}{economy.ChampionPoints, adminPurchaseDTO(*p)})
}

type shopRefundBody struct {
	Reason string `json:"reason"`
}

// handleShopRefund is POST .../shop/purchases/{purchaseID}/refund: a compensating SHOP_REFUND credit, once per
// purchase (a second or concurrent refund is 409 INVALID_PURCHASE_STATUS). The reason is required and is shown
// to admins only.
func (a *App) handleShopRefund(w http.ResponseWriter, r *http.Request) {
	er, ok := a.shopContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasShopAdminLimiter, rateLimitKey(r)) {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	var body shopRefundBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	res, err := a.Shop.Refund(ctx, er.scope, er.user.ID, er.user.DiscordUserID, id, body.Reason)
	if err != nil {
		shopFailed(w, "refund shop purchase", err)
		return
	}
	shopAudit("shop_purchase_refunded", er, "purchase_id", res.Purchase.ID, "total_points", res.Purchase.TotalPoints)
	writeSaaSJSON(w, http.StatusOK, shopPurchaseResponse[adminShopPurchaseDTO]{Currency: economy.ChampionPoints, Purchase: adminPurchaseDTO(res.Purchase), RemainingBalance: res.BalanceAfter})
}
