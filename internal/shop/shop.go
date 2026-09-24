// Package shop is the Champion Shop service (docs/SHOP.md): an installation-scoped product catalog
// bought with Champion Points. It validates, resolves the buyer's VERIFIED DayZ identity through the
// economy, and delegates the atomic purchase / refund to repository.ShopRepository, which writes the
// Champion Points debit through the economy's single ledger path. There is no second currency and no
// second debit mechanism, and nothing here delivers anything: fulfillment is manual in Phase 1.
package shop

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Limits.
const (
	MaxPricePoints    int64 = 1_000_000_000
	MaxQuantity             = 100
	MaxStock          int64 = 1_000_000_000
	MaxPurchaseLimit        = 1000
	MaxNameLen              = 80
	MaxDescriptionLen       = 1000
	MaxCategoryName         = 60
	MaxCategoryDesc         = 500
	MaxReasonLen            = 200
	MaxSortOrder            = 10000
	MaxSearchLen            = 50

	DefaultLimit = 25
	MaxLimit     = 100
	LowStockAt   = 5
)

// Product types and delivery types (stable strings; also CHECK constraints).
const (
	TypeItem    = "ITEM"
	TypeLoadout = "LOADOUT"
	TypeVehicle = "VEHICLE"
	TypeService = "SERVICE"
	TypeCustom  = "CUSTOM"

	DeliveryManual       = "MANUAL"
	DeliveryDiscordRole  = "DISCORD_ROLE"   // reserved: nothing grants roles yet
	DeliveryInGameFuture = "IN_GAME_FUTURE" // reserved: nothing delivers in game yet

	StockUnlimited = "UNLIMITED"
	StockFinite    = "FINITE"

	// Delivery policies (docs/SHOP_DELIVERY.md): both are fulfilled by staff (delivery type MANUAL).
	PolicyManualPickup     = repository.DeliveryPolicyManualPickup
	PolicyManualCoordinate = repository.DeliveryPolicyManualCoordinate
)

var productTypes = map[string]bool{TypeItem: true, TypeLoadout: true, TypeVehicle: true, TypeService: true, TypeCustom: true}

// Purchase statuses (re-exported so callers need one import).
const (
	StatusPending            = repository.ShopStatusPending
	StatusPaid               = repository.ShopStatusPaid
	StatusPendingFulfillment = repository.ShopStatusPendingFulfillment
	StatusFulfilled          = repository.ShopStatusFulfilled
	StatusCancelled          = repository.ShopStatusCancelled
	StatusRefunded           = repository.ShopStatusRefunded
	StatusFailed             = repository.ShopStatusFailed
)

var statuses = map[string]bool{StatusPending: true, StatusPaid: true, StatusPendingFulfillment: true, StatusFulfilled: true, StatusCancelled: true, StatusRefunded: true, StatusFailed: true}

// transitions is the complete state machine. Phase 1 uses PENDING_FULFILLMENT -> FULFILLED and
// {PENDING_FULFILLMENT, FULFILLED} -> REFUNDED; the rest is reserved for automatic delivery and
// unpaid checkout. Anything not listed is invalid.
var transitions = map[string][]string{
	StatusPending:            {StatusPaid, StatusPendingFulfillment, StatusCancelled, StatusFailed},
	StatusPaid:               {StatusPendingFulfillment, StatusFulfilled, StatusRefunded},
	StatusPendingFulfillment: {StatusFulfilled, StatusRefunded},
	StatusFulfilled:          {StatusRefunded},
}

// CanTransition reports whether a purchase may move from one status to another.
func CanTransition(from, to string) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// IsStatus reports whether s is a known purchase status.
func IsStatus(s string) bool { return statuses[s] }

// Errors that map to 400.
var (
	ErrInvalidQuantity = errors.New("quantity must be a whole number from 1 to 100")
	ErrInvalidKey      = errors.New("idempotencyKey must be 8-64 characters of A-Z a-z 0-9 . _ : -")
	ErrReasonRequired  = errors.New("a reason is required")
	ErrInvalidStatus   = errors.New("unknown status")
	ErrInvalidCursor   = errors.New("invalid cursor")
	ErrInvalidQuery    = errors.New("search text is too long")
	// ErrInvalidCoordinates: a delivery object without both x and z, or with a non-finite value.
	ErrInvalidCoordinates = errors.New("delivery needs both x and z as finite numbers")
)

// ValidationError is one or more field problems of a product/category input (HTTP 400).
type ValidationError struct{ Issues map[string]string }

func (e *ValidationError) Error() string { return "invalid shop input" }

func (e *ValidationError) add(field, msg string) {
	if e.Issues == nil {
		e.Issues = map[string]string{}
	}
	e.Issues[field] = msg
}

func (e *ValidationError) err() error {
	if len(e.Issues) == 0 {
		return nil
	}
	return e
}

// Store is the persistence the service needs; *repository.ShopRepository implements it.
type Store interface {
	ListCategories(ctx context.Context, org, inst int64, includeInactive bool) ([]repository.ShopCategory, error)
	CreateCategory(ctx context.Context, org, inst int64, in repository.ShopCategoryInput) (*repository.ShopCategory, error)
	UpdateCategory(ctx context.Context, org, inst, id int64, p repository.ShopCategoryPatch) (*repository.ShopCategory, error)
	ListProducts(ctx context.Context, org, inst int64, q repository.ProductQuery) ([]repository.ShopProduct, bool, error)
	GetProduct(ctx context.Context, org, inst, id int64, includeInactive bool) (*repository.ShopProduct, error)
	CreateProduct(ctx context.Context, org, inst int64, in repository.ShopProductInput) (*repository.ShopProduct, error)
	UpdateProduct(ctx context.Context, org, inst, id int64, p repository.ShopProductPatch, validate func(repository.ShopProduct) error) (*repository.ShopProduct, bool, error)
	ListDeliveries(ctx context.Context, org, inst int64, q repository.DeliveryQuery) ([]repository.ShopDelivery, int64, error)
	GetDelivery(ctx context.Context, org, inst, id, playerID int64) (*repository.ShopDelivery, error)
	GetDeliverySettings(ctx context.Context, org, inst int64) (repository.ShopDeliverySettings, error)
	SetDeliveryMap(ctx context.Context, org, inst int64, mapKey string, actorUserID int64) (repository.ShopDeliverySettings, error)
	Purchase(ctx context.Context, p repository.PurchaseParams) (*repository.PurchaseResult, error)
	GetPurchase(ctx context.Context, org, inst, id, playerID int64) (*repository.ShopPurchase, error)
	ListPurchases(ctx context.Context, org, inst int64, q repository.PurchaseQuery) ([]repository.ShopPurchase, int64, error)
	Fulfill(ctx context.Context, org, inst, id, actorUserID int64) (*repository.ShopPurchase, error)
	Refund(ctx context.Context, p repository.RefundParams) (*repository.RefundResult, error)
	ReconcileShop(ctx context.Context, org, inst, guildID int64, limit int) ([]repository.ShopMismatch, error)
}

// Identity resolves the acting user's VERIFIED player (economy.Accounts implements it).
type Identity interface {
	Me(ctx context.Context, scope repository.EconomyScope, discordUserID string) (economy.Account, error)
}

// Announcer receives committed shop transactions (economy.Service implements it; nil-safe).
type Announcer interface{ Announce(economy.Event) }

type Service struct {
	store    Store
	identity Identity
	announce Announcer
}

func NewService(store Store, identity Identity, announce Announcer) *Service {
	return &Service{store: store, identity: identity, announce: announce}
}

// --- text helpers -----------------------------------------------------------------------------------

func cleanLine(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)), " ")
}

func stripControl(s string, keepNewline bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' && keepNewline, r == '\t' && keepNewline:
			b.WriteRune(r)
		case r < 0x20, r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func cleanText(s string) string { return strings.TrimSpace(stripControl(s, true)) }

func runes(s string) int { return utf8.RuneCountInString(s) }

var idemKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,64}$`)

// --- product validation -----------------------------------------------------------------------------

// ProductCreate is a new product.
type ProductCreate struct {
	Name           string
	Description    string
	CategoryID     *int64
	PricePoints    int64
	ProductType    string
	DeliveryType   string // default MANUAL
	DeliveryPolicy string // default MANUAL_PICKUP
	IsActive       *bool  // default true
	IsFeatured     bool
	SortOrder      int
	StockMode      string // default UNLIMITED
	StockQuantity  *int64
	PurchaseLimit  *int
}

// ValidateProduct checks a complete product state (used for create and for the merged result of an update).
func ValidateProduct(name, description string, price int64, ptype, delivery, policy, stockMode string, stock *int64, limit *int, sortOrder int) error {
	v := &ValidationError{}
	if n := runes(name); n < 1 || n > MaxNameLen {
		v.add("name", "name must be 1-80 characters")
	}
	if runes(description) > MaxDescriptionLen {
		v.add("description", "description must be at most 1000 characters")
	}
	if price < 1 || price > MaxPricePoints {
		v.add("pricePoints", "pricePoints must be a whole number from 1 to 1,000,000,000")
	}
	if !productTypes[ptype] {
		v.add("productType", "productType must be one of ITEM, LOADOUT, VEHICLE, SERVICE, CUSTOM")
	}
	switch delivery {
	case DeliveryManual:
	case DeliveryDiscordRole, DeliveryInGameFuture:
		v.add("deliveryType", delivery+" is reserved and not implemented yet; only MANUAL fulfillment exists")
	default:
		v.add("deliveryType", "deliveryType must be MANUAL")
	}
	switch policy {
	case PolicyManualPickup, PolicyManualCoordinate:
	default:
		v.add("deliveryPolicy", "deliveryPolicy must be MANUAL_PICKUP or MANUAL_COORDINATE")
	}
	switch stockMode {
	case StockUnlimited:
		if stock != nil {
			v.add("stockQuantity", "stockQuantity must be omitted for UNLIMITED stock")
		}
	case StockFinite:
		if stock == nil || *stock < 0 || *stock > MaxStock {
			v.add("stockQuantity", "FINITE stock needs stockQuantity from 0 to 1,000,000,000")
		}
	default:
		v.add("stockMode", "stockMode must be UNLIMITED or FINITE")
	}
	if limit != nil && (*limit < 1 || *limit > MaxPurchaseLimit) {
		v.add("purchaseLimit", "purchaseLimit must be 1-1000 or null")
	}
	if sortOrder < -MaxSortOrder || sortOrder > MaxSortOrder {
		v.add("sortOrder", "sortOrder is out of range")
	}
	return v.err()
}

// --- categories -------------------------------------------------------------------------------------

func (s *Service) Categories(ctx context.Context, scope repository.EconomyScope, admin bool) ([]repository.ShopCategory, error) {
	return s.store.ListCategories(ctx, scope.OrganizationID, scope.InstallationID, admin)
}

type CategoryCreate struct {
	Name, Description string
	SortOrder         int
}

func validateCategory(name, desc string, sort int) error {
	v := &ValidationError{}
	if n := runes(name); n < 1 || n > MaxCategoryName {
		v.add("name", "name must be 1-60 characters")
	}
	if runes(desc) > MaxCategoryDesc {
		v.add("description", "description must be at most 500 characters")
	}
	if sort < -MaxSortOrder || sort > MaxSortOrder {
		v.add("sortOrder", "sortOrder is out of range")
	}
	return v.err()
}

func (s *Service) CreateCategory(ctx context.Context, scope repository.EconomyScope, in CategoryCreate) (*repository.ShopCategory, error) {
	name, desc := cleanLine(in.Name), cleanText(in.Description)
	if err := validateCategory(name, desc, in.SortOrder); err != nil {
		return nil, err
	}
	return s.store.CreateCategory(ctx, scope.OrganizationID, scope.InstallationID, repository.ShopCategoryInput{Name: name, Description: desc, SortOrder: in.SortOrder})
}

type CategoryUpdate struct {
	Name, Description *string
	SortOrder         *int
	IsActive          *bool
}

func (s *Service) UpdateCategory(ctx context.Context, scope repository.EconomyScope, id int64, in CategoryUpdate) (*repository.ShopCategory, error) {
	p := repository.ShopCategoryPatch{SortOrder: in.SortOrder, IsActive: in.IsActive}
	name, desc, sort := "x", "", 0
	if in.Name != nil {
		n := cleanLine(*in.Name)
		p.Name, name = &n, n
	}
	if in.Description != nil {
		d := cleanText(*in.Description)
		p.Description, desc = &d, d
	}
	if in.SortOrder != nil {
		sort = *in.SortOrder
	}
	if err := validateCategory(name, desc, sort); err != nil {
		return nil, err
	}
	return s.store.UpdateCategory(ctx, scope.OrganizationID, scope.InstallationID, id, p)
}

// --- products ---------------------------------------------------------------------------------------

// ProductList is a query for the catalog.
type ProductList struct {
	CategorySlug string
	Search       string
	Limit        int
	Cursor       string
}

// ProductPage is one page; NextCursor is "" on the last page.
type ProductPage struct {
	Items      []repository.ShopProduct
	NextCursor string
	Limit      int
}

// Products lists the catalog. A player list shows only active products (in active categories); an
// admin list also shows inactive ones.
func (s *Service) Products(ctx context.Context, scope repository.EconomyScope, q ProductList, admin bool) (ProductPage, error) {
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	search := cleanLine(q.Search)
	if runes(search) > MaxSearchLen {
		return ProductPage{}, ErrInvalidQuery
	}
	rq := repository.ProductQuery{CategorySlug: strings.ToLower(strings.TrimSpace(q.CategorySlug)), Search: search, IncludeInactive: admin, Limit: q.Limit}
	if q.Cursor != "" {
		c, ok := repository.DecodeProductCursor(q.Cursor)
		if !ok {
			return ProductPage{}, ErrInvalidCursor
		}
		rq.Cursor = c
	}
	items, more, err := s.store.ListProducts(ctx, scope.OrganizationID, scope.InstallationID, rq)
	if err != nil {
		return ProductPage{}, err
	}
	page := ProductPage{Items: items, Limit: q.Limit}
	if page.Items == nil {
		page.Items = []repository.ShopProduct{}
	}
	if more && len(items) > 0 {
		page.NextCursor = items[len(items)-1].CursorFor().Encode()
	}
	return page, nil
}

func (s *Service) Product(ctx context.Context, scope repository.EconomyScope, id int64, admin bool) (*repository.ShopProduct, error) {
	return s.store.GetProduct(ctx, scope.OrganizationID, scope.InstallationID, id, admin)
}

func (s *Service) CreateProduct(ctx context.Context, scope repository.EconomyScope, in ProductCreate) (*repository.ShopProduct, error) {
	name, desc := cleanLine(in.Name), cleanText(in.Description)
	delivery, stockMode, active := in.DeliveryType, in.StockMode, true
	if delivery == "" {
		delivery = DeliveryManual
	}
	policy := strings.ToUpper(strings.TrimSpace(in.DeliveryPolicy))
	if policy == "" {
		policy = PolicyManualPickup
	}
	if stockMode == "" {
		stockMode = StockUnlimited
	}
	if in.IsActive != nil {
		active = *in.IsActive
	}
	ptype := strings.ToUpper(strings.TrimSpace(in.ProductType))
	if err := ValidateProduct(name, desc, in.PricePoints, ptype, strings.ToUpper(delivery), policy, strings.ToUpper(stockMode), in.StockQuantity, in.PurchaseLimit, in.SortOrder); err != nil {
		return nil, err
	}
	return s.store.CreateProduct(ctx, scope.OrganizationID, scope.InstallationID, repository.ShopProductInput{
		CategoryID: in.CategoryID, Name: name, Description: desc, PricePoints: in.PricePoints, ProductType: ptype, DeliveryType: strings.ToUpper(delivery), DeliveryPolicy: policy,
		SortOrder: in.SortOrder, IsActive: active, IsFeatured: in.IsFeatured, StockMode: strings.ToUpper(stockMode), StockQuantity: in.StockQuantity, PurchaseLimit: in.PurchaseLimit})
}

// Optional is a JSON field that can be absent, null, or a value (so "categoryId": null clears it).
type Optional[T any] struct {
	Set   bool
	Null  bool
	Value T
}

func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}

// ProductUpdate is a partial update; an absent field is unchanged. The tenant, the slug and the
// product type are not editable.
type ProductUpdate struct {
	Name           *string
	Description    *string
	CategoryID     Optional[int64]
	PricePoints    *int64
	DeliveryType   *string
	DeliveryPolicy *string // changes future purchases only; existing orders keep their snapshot
	IsActive       *bool
	IsFeatured     *bool
	SortOrder      *int
	StockMode      *string
	StockQuantity  *int64
	PurchaseLimit  Optional[int]
}

// UpdateProduct applies a partial update under the product's row lock. wasActive lets the caller audit
// a disable. The merged state is validated exactly like a new product.
func (s *Service) UpdateProduct(ctx context.Context, scope repository.EconomyScope, id int64, in ProductUpdate) (updated *repository.ShopProduct, wasActive bool, err error) {
	p := repository.ShopProductPatch{PricePoints: in.PricePoints, IsActive: in.IsActive, IsFeatured: in.IsFeatured, SortOrder: in.SortOrder, StockQuantity: in.StockQuantity}
	if in.Name != nil {
		n := cleanLine(*in.Name)
		p.Name = &n
	}
	if in.Description != nil {
		d := cleanText(*in.Description)
		p.Description = &d
	}
	if in.DeliveryType != nil {
		d := strings.ToUpper(strings.TrimSpace(*in.DeliveryType))
		p.DeliveryType = &d
	}
	if in.StockMode != nil {
		m := strings.ToUpper(strings.TrimSpace(*in.StockMode))
		p.StockMode = &m
	}
	if in.DeliveryPolicy != nil {
		d := strings.ToUpper(strings.TrimSpace(*in.DeliveryPolicy))
		p.DeliveryPolicy = &d
	}
	switch {
	case in.CategoryID.Set && in.CategoryID.Null:
		p.ClearCategory = true
	case in.CategoryID.Set:
		v := in.CategoryID.Value
		p.CategoryID = &v
	}
	switch {
	case in.PurchaseLimit.Set && in.PurchaseLimit.Null:
		p.ClearPurchaseLimit = true
	case in.PurchaseLimit.Set:
		v := in.PurchaseLimit.Value
		p.PurchaseLimit = &v
	}
	validate := func(m repository.ShopProduct) error {
		return ValidateProduct(m.Name, m.Description, m.PricePoints, m.ProductType, m.DeliveryType, m.DeliveryPolicy, m.StockMode, m.StockQuantity, m.PurchaseLimit, m.SortOrder)
	}
	return s.store.UpdateProduct(ctx, scope.OrganizationID, scope.InstallationID, id, p, validate)
}

// --- purchases --------------------------------------------------------------------------------------

// PurchaseRequest is one purchase of a single product (there is no cart in Phase 1).
type PurchaseRequest struct {
	ProductID      int64
	Quantity       int
	IdempotencyKey string
	Delivery       *DeliveryInput // nil = no delivery object in the request
}

// DeliveryInput is the request's delivery object: X/Z map coordinates (no altitude).
type DeliveryInput struct{ X, Z *float64 }

// coordinates validates the shape of a delivery object; whether the product needs one and whether
// the position is on the map is decided under the product lock (repository.ShopRepository.Purchase).
func (d *DeliveryInput) coordinates() (*repository.DeliveryCoordinates, error) {
	if d == nil {
		return nil, nil
	}
	if d.X == nil || d.Z == nil || !finite(*d.X) || !finite(*d.Z) {
		return nil, ErrInvalidCoordinates
	}
	return &repository.DeliveryCoordinates{X: *d.X, Z: *d.Z}, nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// Purchase buys a product with Champion Points for the acting user, through their VERIFIED DayZ link.
// It is one database transaction (see repository.ShopRepository.Purchase). A committed purchase is
// announced to the ECONOMY notifier AFTER the commit; a replay is not.
func (s *Service) Purchase(ctx context.Context, scope repository.EconomyScope, userID int64, discordUserID string, req PurchaseRequest) (*repository.PurchaseResult, error) {
	if req.Quantity < 1 || req.Quantity > MaxQuantity {
		return nil, ErrInvalidQuantity
	}
	if !idemKey.MatchString(strings.TrimSpace(req.IdempotencyKey)) {
		return nil, ErrInvalidKey
	}
	if req.ProductID <= 0 {
		return nil, repository.ErrShopProductNotFound
	}
	if scope.Status == "SUSPENDED" {
		return nil, economy.ErrSuspended
	}
	if scope.ServerID == 0 {
		return nil, economy.ErrNoServer
	}
	coords, err := req.Delivery.coordinates()
	if err != nil {
		return nil, err
	}
	acct, err := s.identity.Me(ctx, scope, discordUserID)
	if err != nil {
		return nil, err
	}
	res, err := s.store.Purchase(ctx, repository.PurchaseParams{OrganizationID: scope.OrganizationID, InstallationID: scope.InstallationID,
		GuildID: scope.GuildID, ServerID: scope.ServerID, UserID: userID, PlayerID: acct.AccountID, ActorDiscordID: discordUserID,
		ProductID: req.ProductID, Quantity: req.Quantity, IdempotencyKey: strings.TrimSpace(req.IdempotencyKey), Delivery: coords})
	if err != nil {
		return nil, err
	}
	if !res.Duplicate && s.announce != nil {
		s.announce.Announce(economy.Event{Type: economy.TypeShopPurchase, GuildID: scope.GuildID, ServerID: scope.ServerID, PlayerName: res.PlayerName,
			Amount: res.Purchase.TotalPoints, Credit: false, BalanceAfter: res.BalanceAfter, Item: res.ItemName})
	}
	return res, nil
}

// PurchaseList filters a purchase list.
type PurchaseList struct {
	PlayerID  int64
	ProductID int64
	Status    string
	Limit     int
	Cursor    string
}

// PurchasePage is one page, newest first; NextCursor is "" on the last page.
type PurchasePage struct {
	Items      []repository.ShopPurchase
	NextCursor string
	Limit      int
}

const purchaseCursorPrefix = "spc1:"

func encodePurchaseCursor(id int64) string {
	return encodeCursor(purchaseCursorPrefix, id)
}

// MyPurchases lists the acting user's own purchases (their verified player only).
func (s *Service) MyPurchases(ctx context.Context, scope repository.EconomyScope, discordUserID string, q PurchaseList) (PurchasePage, error) {
	acct, err := s.identity.Me(ctx, scope, discordUserID)
	if err != nil {
		return PurchasePage{}, err
	}
	q.PlayerID = acct.AccountID
	return s.list(ctx, scope, q)
}

// AdminPurchases lists the installation's purchases (organization admins only).
func (s *Service) AdminPurchases(ctx context.Context, scope repository.EconomyScope, q PurchaseList) (PurchasePage, error) {
	return s.list(ctx, scope, q)
}

func (s *Service) list(ctx context.Context, scope repository.EconomyScope, q PurchaseList) (PurchasePage, error) {
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	status := strings.ToUpper(strings.TrimSpace(q.Status))
	if status != "" && !IsStatus(status) {
		return PurchasePage{}, ErrInvalidStatus
	}
	var before int64
	if q.Cursor != "" {
		id, ok := decodeCursor(purchaseCursorPrefix, q.Cursor)
		if !ok {
			return PurchasePage{}, ErrInvalidCursor
		}
		before = id
	}
	items, next, err := s.store.ListPurchases(ctx, scope.OrganizationID, scope.InstallationID, repository.PurchaseQuery{PlayerID: q.PlayerID, ProductID: q.ProductID, Status: status, Limit: q.Limit, BeforeID: before})
	if err != nil {
		return PurchasePage{}, err
	}
	page := PurchasePage{Items: items, Limit: q.Limit}
	if page.Items == nil {
		page.Items = []repository.ShopPurchase{}
	}
	if next != 0 {
		page.NextCursor = encodePurchaseCursor(next)
	}
	return page, nil
}

// MyPurchase reads one of the acting user's own purchases; another player's is "not found".
func (s *Service) MyPurchase(ctx context.Context, scope repository.EconomyScope, discordUserID string, id int64) (*repository.ShopPurchase, error) {
	acct, err := s.identity.Me(ctx, scope, discordUserID)
	if err != nil {
		return nil, err
	}
	return s.store.GetPurchase(ctx, scope.OrganizationID, scope.InstallationID, id, acct.AccountID)
}

func (s *Service) AdminPurchase(ctx context.Context, scope repository.EconomyScope, id int64) (*repository.ShopPurchase, error) {
	return s.store.GetPurchase(ctx, scope.OrganizationID, scope.InstallationID, id, 0)
}

// Fulfill marks a PENDING_FULFILLMENT purchase FULFILLED (admin only).
func (s *Service) Fulfill(ctx context.Context, scope repository.EconomyScope, actorUserID, purchaseID int64) (*repository.ShopPurchase, error) {
	if scope.Status == "SUSPENDED" {
		return nil, economy.ErrSuspended
	}
	return s.store.Fulfill(ctx, scope.OrganizationID, scope.InstallationID, purchaseID, actorUserID)
}

// Refund credits a purchase back as a compensating SHOP_REFUND transaction (admin only, reason
// required, once per purchase). The reason is stored for admins and never shown to the player.
func (s *Service) Refund(ctx context.Context, scope repository.EconomyScope, actorUserID int64, actorDiscordID string, purchaseID int64, reason string) (*repository.RefundResult, error) {
	reason = cleanLine(reason)
	if reason == "" {
		return nil, ErrReasonRequired
	}
	if runes(reason) > MaxReasonLen {
		reason = string([]rune(reason)[:MaxReasonLen])
	}
	if scope.Status == "SUSPENDED" {
		return nil, economy.ErrSuspended
	}
	res, err := s.store.Refund(ctx, repository.RefundParams{OrganizationID: scope.OrganizationID, InstallationID: scope.InstallationID, GuildID: scope.GuildID, ServerID: scope.ServerID,
		PurchaseID: purchaseID, ActorUserID: actorUserID, ActorDiscordID: actorDiscordID, Reason: reason})
	if err != nil {
		return nil, err
	}
	if s.announce != nil {
		s.announce.Announce(economy.Event{Type: economy.TypeShopRefund, GuildID: scope.GuildID, ServerID: scope.ServerID, PlayerName: res.PlayerName,
			Amount: res.Purchase.TotalPoints, Credit: true, BalanceAfter: res.BalanceAfter, Item: res.ItemName})
	}
	return res, nil
}

// Reconcile is the read-only shop/ledger integrity check (see repository.ReconcileShop).
func (s *Service) Reconcile(ctx context.Context, scope repository.EconomyScope, limit int) ([]repository.ShopMismatch, error) {
	return s.store.ReconcileShop(ctx, scope.OrganizationID, scope.InstallationID, scope.GuildID, limit)
}

// StockState is the player-facing stock state of a product (the exact quantity is admin-only).
func StockState(p repository.ShopProduct) string {
	if p.StockMode != StockFinite || p.StockQuantity == nil {
		return "UNLIMITED"
	}
	switch q := *p.StockQuantity; {
	case q <= 0:
		return "OUT_OF_STOCK"
	case q <= LowStockAt:
		return "LOW_STOCK"
	}
	return "IN_STOCK"
}
