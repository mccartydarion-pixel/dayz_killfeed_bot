package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Champion Shop persistence (docs/SHOP.md). Every statement is scoped by organization + installation.
// A purchase debits Champion Points through the ONE economy write path (applyLedger, the same
// function admin debits use) inside the purchase's own transaction, so the product lock, the stock
// decrement, the ledger row, the balance change, the purchase and its item snapshot commit or roll
// back together. There is no second debit mechanism.
//
// Lock order (deadlock-free by construction):
//
//	purchase:  idempotency pre-check -> product row FOR UPDATE -> idempotency re-check -> new purchase row
//	           -> player_points row (debit) -> stock update -> item snapshot
//	refund:    purchase row FOR UPDATE -> product row (restock) -> player_points row (credit)
//	fulfill:   purchase row only
//	admin ops: player_points row only
//
// so every path takes product/purchase locks before the balance lock, never the reverse.

// Purchase statuses (stable strings, also a CHECK constraint).
const (
	ShopStatusPending            = "PENDING"             // reserved: unpaid checkout does not exist yet
	ShopStatusPaid               = "PAID"                // reserved: an automatically delivered product
	ShopStatusPendingFulfillment = "PENDING_FULFILLMENT" // paid, waiting for an admin to hand it over
	ShopStatusFulfilled          = "FULFILLED"
	ShopStatusCancelled          = "CANCELLED" // reserved
	ShopStatusRefunded           = "REFUNDED"
	ShopStatusFailed             = "FAILED" // reserved
)

// Ledger types and reference of a shop payment.
const (
	TxShopPurchase = "SHOP_PURCHASE"
	TxShopRefund   = "SHOP_REFUND"
)

// ShopPurchaseRef is the ledger source_key of a purchase's debit and of its refund credit (they
// differ by ledger type, so each can exist exactly once).
func ShopPurchaseRef(purchaseID int64) string { return fmt.Sprintf("purchase:%d", purchaseID) }

var (
	ErrShopProductNotFound     = errors.New("product not found")
	ErrShopProductDisabled     = errors.New("product is not available")
	ErrShopOutOfStock          = errors.New("product is out of stock")
	ErrShopLimitReached        = errors.New("purchase limit reached")
	ErrShopPurchaseNotFound    = errors.New("purchase not found")
	ErrShopInvalidStatus       = errors.New("the purchase is not in a state that allows this")
	ErrShopIdempotencyConflict = errors.New("idempotency key was already used for a different purchase")
	ErrShopCategoryNotFound    = errors.New("category not found")
	errShopKeyRaced            = errors.New("idempotency key inserted concurrently")
)

// countedStatuses are the purchases that count against a per-player purchase limit: paid ones,
// including delivered. A refunded, cancelled or failed purchase frees the limit again.
const shopCountedStatuses = `('PAID','PENDING_FULFILLMENT','FULFILLED')`

type ShopRepository struct{ pool *pgxpool.Pool }

func NewShopRepository(pool *pgxpool.Pool) *ShopRepository { return &ShopRepository{pool: pool} }

// --- models -----------------------------------------------------------------------------------------

type ShopCategory struct {
	ID, OrganizationID, InstallationID int64
	Name, Slug, Description            string
	SortOrder                          int
	IsActive                           bool
	ProductCount                       int // active products (list only)
	CreatedAt, UpdatedAt               time.Time
}

type ShopProduct struct {
	ID, OrganizationID, InstallationID int64
	CategoryID                         *int64
	CategoryName, CategorySlug         string
	Name, Slug, Description            string
	PricePoints                        int64
	ProductType, DeliveryType          string
	SortOrder                          int
	IsActive, IsFeatured               bool
	StockMode                          string
	StockQuantity                      *int64
	PurchaseLimit                      *int
	CreatedAt, UpdatedAt               time.Time
}

type ShopPurchaseItem struct {
	ProductID       *int64
	ProductName     string
	UnitPricePoints int64
	Quantity        int
	LineTotalPoints int64
}

type ShopPurchase struct {
	ID, OrganizationID, InstallationID int64
	GameServerID                       *int64
	UserID                             *int64
	PlayerID                           int64
	PlayerName                         string
	Status                             string
	TotalPoints                        int64
	DeliveryType                       string
	IdempotencyKey                     string
	PaidAt, FulfilledAt                *time.Time
	CancelledAt, RefundedAt            *time.Time
	FulfilledByDiscordID               string
	RefundedByDiscordID                string
	RefundReason                       string
	CreatedAt, UpdatedAt               time.Time
	Items                              []ShopPurchaseItem
}

// --- helpers ----------------------------------------------------------------------------------------

func shopSlugify(name, fallback string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 50 {
		s = strings.Trim(s[:50], "-")
	}
	if s == "" {
		return fallback
	}
	return s
}

func slugCandidate(base string, n int) string {
	if n <= 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, n)
}

func uniqueConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}

// --- categories -------------------------------------------------------------------------------------

type ShopCategoryInput struct {
	Name, Description string
	SortOrder         int
}

type ShopCategoryPatch struct {
	Name, Description *string
	SortOrder         *int
	IsActive          *bool
}

const categoryCols = `c.id, c.organization_id, c.installation_id, c.name, c.slug, c.description, c.sort_order, c.is_active, c.created_at, c.updated_at`

func scanCategory(row pgx.Row, withCount bool) (ShopCategory, error) {
	var c ShopCategory
	dest := []any{&c.ID, &c.OrganizationID, &c.InstallationID, &c.Name, &c.Slug, &c.Description, &c.SortOrder, &c.IsActive, &c.CreatedAt, &c.UpdatedAt}
	if withCount {
		dest = append(dest, &c.ProductCount)
	}
	return c, row.Scan(dest...)
}

// ListCategories returns the installation's categories (active ones only unless includeInactive) with
// the number of active products in each, in one query.
func (r *ShopRepository) ListCategories(ctx context.Context, org, inst int64, includeInactive bool) ([]ShopCategory, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+categoryCols+`,
  (SELECT COUNT(*) FROM shop_products p WHERE p.category_id = c.id AND p.installation_id = c.installation_id AND p.is_active)::int
FROM shop_categories c
WHERE c.organization_id = $1 AND c.installation_id = $2 AND ($3 OR c.is_active)
ORDER BY c.sort_order, LOWER(c.name), c.id`, org, inst, includeInactive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShopCategory{}
	for rows.Next() {
		c, err := scanCategory(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *ShopRepository) CreateCategory(ctx context.Context, org, inst int64, in ShopCategoryInput) (*ShopCategory, error) {
	base := shopSlugify(in.Name, "category")
	for n := 1; n <= 60; n++ {
		slug := slugCandidate(base, n)
		c, err := scanCategory(r.pool.QueryRow(ctx, `INSERT INTO shop_categories(organization_id, installation_id, name, slug, description, sort_order)
SELECT $1::bigint, i.id, $3::text, $4::text, $5::text, $6::int FROM installations i WHERE i.id = $2 AND i.organization_id = $1
RETURNING id, organization_id, installation_id, name, slug, description, sort_order, is_active, created_at, updated_at`, org, inst, in.Name, slug, in.Description, in.SortOrder), false)
		switch {
		case err == nil:
			return &c, nil
		case errors.Is(err, pgx.ErrNoRows):
			return nil, ErrShopCategoryNotFound
		case uniqueConstraint(err) == "uq_shop_categories_installation_slug":
			continue
		default:
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not allocate a category slug")
}

func (r *ShopRepository) UpdateCategory(ctx context.Context, org, inst, id int64, p ShopCategoryPatch) (*ShopCategory, error) {
	c, err := scanCategory(r.pool.QueryRow(ctx, `UPDATE shop_categories c SET
  name = COALESCE($4, c.name), description = COALESCE($5, c.description), sort_order = COALESCE($6, c.sort_order),
  is_active = COALESCE($7, c.is_active), updated_at = NOW()
WHERE c.id = $3 AND c.organization_id = $1 AND c.installation_id = $2
RETURNING `+categoryCols, org, inst, id, p.Name, p.Description, p.SortOrder, p.IsActive), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShopCategoryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// --- products ---------------------------------------------------------------------------------------

const productCols = `p.id, p.organization_id, p.installation_id, p.category_id, COALESCE(c.name,''), COALESCE(c.slug,''),
p.name, p.slug, p.description, p.price_points, p.product_type, p.delivery_type, p.sort_order, p.is_active, p.is_featured,
p.stock_mode, p.stock_quantity, p.purchase_limit, p.created_at, p.updated_at`

const productFrom = ` FROM shop_products p LEFT JOIN shop_categories c ON c.id = p.category_id AND c.installation_id = p.installation_id`

func scanProduct(row pgx.Row) (ShopProduct, error) {
	var p ShopProduct
	err := row.Scan(&p.ID, &p.OrganizationID, &p.InstallationID, &p.CategoryID, &p.CategoryName, &p.CategorySlug,
		&p.Name, &p.Slug, &p.Description, &p.PricePoints, &p.ProductType, &p.DeliveryType, &p.SortOrder, &p.IsActive, &p.IsFeatured,
		&p.StockMode, &p.StockQuantity, &p.PurchaseLimit, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// ProductQuery selects a catalog page. Admin lists include inactive products; a player list shows only
// active products whose category (if any) is active too.
type ProductQuery struct {
	CategorySlug    string
	Search          string
	IncludeInactive bool
	Limit           int
	Cursor          *ProductCursor
}

// ProductCursor is the sort key of the last product of the previous page.
type ProductCursor struct {
	NotFeatured bool
	SortOrder   int
	NameLower   string
	ID          int64
}

func (c ProductCursor) Encode() string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(append([]byte("sp1:"), raw...))
}

func DecodeProductCursor(s string) (*ProductCursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(raw), "sp1:") {
		return nil, false
	}
	var c ProductCursor
	if err := json.Unmarshal(raw[4:], &c); err != nil || c.ID <= 0 {
		return nil, false
	}
	return &c, true
}

// ListProducts returns one page in the deterministic order featured first, sort_order, name, id.
// The category name/slug come from the same joined query (no per-product lookups). more reports
// whether another page exists.
func (r *ShopRepository) ListProducts(ctx context.Context, org, inst int64, q ProductQuery) (items []ShopProduct, more bool, err error) {
	if q.Limit <= 0 {
		q.Limit = 25
	}
	var cur ProductCursor
	hasCursor := q.Cursor != nil
	if hasCursor {
		cur = *q.Cursor
	}
	pattern := ""
	if s := strings.TrimSpace(q.Search); s != "" {
		pattern = "%" + escapeLike(s) + "%"
	}
	rows, err := r.pool.Query(ctx, `SELECT `+productCols+productFrom+`
WHERE p.organization_id = $1 AND p.installation_id = $2
  AND ($3 OR (p.is_active AND (p.category_id IS NULL OR c.is_active)))
  AND ($4 = '' OR c.slug = $4)
  AND ($5 = '' OR p.name ILIKE $5 OR p.description ILIKE $5)
  AND (NOT $6 OR (NOT p.is_featured, p.sort_order, LOWER(p.name), p.id) > ($7, $8, $9, $10))
ORDER BY (NOT p.is_featured), p.sort_order, LOWER(p.name), p.id
LIMIT $11`, org, inst, q.IncludeInactive, q.CategorySlug, pattern, hasCursor, cur.NotFeatured, cur.SortOrder, cur.NameLower, cur.ID, q.Limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, false, err
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(items) > q.Limit {
		items, more = items[:q.Limit], true
	}
	return items, more, nil
}

// CursorFor builds the cursor that continues after p.
func (p ShopProduct) CursorFor() ProductCursor {
	return ProductCursor{NotFeatured: !p.IsFeatured, SortOrder: p.SortOrder, NameLower: strings.ToLower(p.Name), ID: p.ID}
}

// GetProduct loads one product of the installation. A player read (includeInactive=false) treats an
// inactive product, or one in an inactive category, as not found.
func (r *ShopRepository) GetProduct(ctx context.Context, org, inst, id int64, includeInactive bool) (*ShopProduct, error) {
	p, err := scanProduct(r.pool.QueryRow(ctx, `SELECT `+productCols+productFrom+`
WHERE p.id = $3 AND p.organization_id = $1 AND p.installation_id = $2 AND ($4 OR (p.is_active AND (p.category_id IS NULL OR c.is_active)))`, org, inst, id, includeInactive))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShopProductNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

type ShopProductInput struct {
	CategoryID                *int64
	Name, Description         string
	PricePoints               int64
	ProductType, DeliveryType string
	SortOrder                 int
	IsActive, IsFeatured      bool
	StockMode                 string
	StockQuantity             *int64
	PurchaseLimit             *int
}

func (r *ShopRepository) CreateProduct(ctx context.Context, org, inst int64, in ShopProductInput) (*ShopProduct, error) {
	base := shopSlugify(in.Name, "product")
	for n := 1; n <= 60; n++ {
		slug := slugCandidate(base, n)
		var id int64
		err := r.pool.QueryRow(ctx, `INSERT INTO shop_products(organization_id, installation_id, category_id, name, slug, description, price_points, product_type, delivery_type,
  sort_order, is_active, is_featured, stock_mode, stock_quantity, purchase_limit)
SELECT $1::bigint, i.id, $3::bigint, $4::text, $5::text, $6::text, $7::bigint, $8::text, $9::text, $10::int, $11::bool, $12::bool, $13::text, $14::bigint, $15::int FROM installations i WHERE i.id = $2 AND i.organization_id = $1
RETURNING id`, org, inst, in.CategoryID, in.Name, slug, in.Description, in.PricePoints, in.ProductType, in.DeliveryType,
			in.SortOrder, in.IsActive, in.IsFeatured, in.StockMode, in.StockQuantity, in.PurchaseLimit).Scan(&id)
		switch {
		case err == nil:
			return r.GetProduct(ctx, org, inst, id, true)
		case errors.Is(err, pgx.ErrNoRows):
			return nil, ErrShopProductNotFound
		case uniqueConstraint(err) == "uq_shop_products_installation_slug":
			continue
		case isForeignKeyViolation(err):
			return nil, ErrShopCategoryNotFound // the category is not one of this installation's
		default:
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not allocate a product slug")
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// ShopProductPatch is a partial update. A nil field is unchanged; ClearCategory / ClearPurchaseLimit
// set the column to NULL. The merged result must stay valid (the service validates it and the table
// CHECKs enforce it).
type ShopProductPatch struct {
	Name, Description    *string
	CategoryID           *int64
	ClearCategory        bool
	PricePoints          *int64
	DeliveryType         *string
	SortOrder            *int
	IsActive, IsFeatured *bool
	StockMode            *string
	StockQuantity        *int64
	PurchaseLimit        *int
	ClearPurchaseLimit   bool
}

// UpdateProduct applies the patch under a row lock (so a concurrent purchase sees either the old or the
// new product, never a mix) and returns the updated product and whether it was enabled before.
// Tenant columns and the slug are never editable.
func (r *ShopRepository) UpdateProduct(ctx context.Context, org, inst, id int64, p ShopProductPatch, validate func(merged ShopProduct) error) (updated *ShopProduct, wasActive bool, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	cur, err := scanProduct(tx.QueryRow(ctx, `SELECT `+productCols+productFrom+`
WHERE p.id = $3 AND p.organization_id = $1 AND p.installation_id = $2 FOR UPDATE OF p`, org, inst, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrShopProductNotFound
	}
	if err != nil {
		return nil, false, err
	}
	m := cur
	if p.Name != nil {
		m.Name = *p.Name
	}
	if p.Description != nil {
		m.Description = *p.Description
	}
	if p.PricePoints != nil {
		m.PricePoints = *p.PricePoints
	}
	if p.DeliveryType != nil {
		m.DeliveryType = *p.DeliveryType
	}
	if p.SortOrder != nil {
		m.SortOrder = *p.SortOrder
	}
	if p.IsActive != nil {
		m.IsActive = *p.IsActive
	}
	if p.IsFeatured != nil {
		m.IsFeatured = *p.IsFeatured
	}
	if p.CategoryID != nil {
		m.CategoryID = p.CategoryID
	}
	if p.ClearCategory {
		m.CategoryID = nil
	}
	if p.StockMode != nil {
		m.StockMode = *p.StockMode
		if m.StockMode == "UNLIMITED" {
			m.StockQuantity = nil
		}
	}
	if p.StockQuantity != nil {
		q := *p.StockQuantity
		m.StockQuantity = &q
	}
	if p.PurchaseLimit != nil {
		l := *p.PurchaseLimit
		m.PurchaseLimit = &l
	}
	if p.ClearPurchaseLimit {
		m.PurchaseLimit = nil
	}
	if validate != nil {
		if err := validate(m); err != nil {
			return nil, false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE shop_products SET category_id=$4, name=$5, description=$6, price_points=$7, delivery_type=$8, sort_order=$9,
  is_active=$10, is_featured=$11, stock_mode=$12, stock_quantity=$13, purchase_limit=$14, updated_at=NOW()
WHERE id=$3 AND organization_id=$1 AND installation_id=$2`, org, inst, id, m.CategoryID, m.Name, m.Description, m.PricePoints, m.DeliveryType, m.SortOrder,
		m.IsActive, m.IsFeatured, m.StockMode, m.StockQuantity, m.PurchaseLimit); err != nil {
		if isForeignKeyViolation(err) {
			return nil, false, ErrShopCategoryNotFound
		}
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	out, err := r.GetProduct(ctx, org, inst, id, true)
	return out, cur.IsActive, err
}

// --- purchases --------------------------------------------------------------------------------------

// PurchaseParams is one purchase. The caller (the service) has already resolved the acting user's
// VERIFIED player identity and the installation's guild/server.
type PurchaseParams struct {
	OrganizationID, InstallationID int64
	GuildID, ServerID              int64 // ServerID 0 = none
	UserID, PlayerID               int64
	ActorDiscordID                 string
	ProductID                      int64
	Quantity                       int
	IdempotencyKey                 string
}

// PurchaseResult is the committed purchase. BalanceAfter is the authoritative balance right after
// the debit (for a replay: the account's current balance).
type PurchaseResult struct {
	Purchase     ShopPurchase
	BalanceAfter int64
	Duplicate    bool
	PlayerName   string
	ItemName     string
}

const purchaseCols = `sp.id, sp.organization_id, sp.installation_id, sp.game_server_id, sp.user_id, sp.player_id, COALESCE(pl.display_name,''),
sp.status, sp.total_points, sp.delivery_type, sp.idempotency_key, sp.paid_at, sp.fulfilled_at, sp.cancelled_at, sp.refunded_at,
COALESCE(fu.discord_user_id,''), COALESCE(ru.discord_user_id,''), COALESCE(sp.refund_reason,''), sp.created_at, sp.updated_at`

const purchaseFrom = ` FROM shop_purchases sp
JOIN players pl ON pl.id = sp.player_id
LEFT JOIN app_users fu ON fu.id = sp.fulfilled_by_user_id
LEFT JOIN app_users ru ON ru.id = sp.refunded_by_user_id`

func scanPurchase(row pgx.Row) (ShopPurchase, error) {
	var p ShopPurchase
	err := row.Scan(&p.ID, &p.OrganizationID, &p.InstallationID, &p.GameServerID, &p.UserID, &p.PlayerID, &p.PlayerName,
		&p.Status, &p.TotalPoints, &p.DeliveryType, &p.IdempotencyKey, &p.PaidAt, &p.FulfilledAt, &p.CancelledAt, &p.RefundedAt,
		&p.FulfilledByDiscordID, &p.RefundedByDiscordID, &p.RefundReason, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

type shopQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// loadItems fills the item snapshots of the given purchases with ONE query.
func loadItems(ctx context.Context, q shopQuerier, purchases []ShopPurchase) error {
	if len(purchases) == 0 {
		return nil
	}
	ids := make([]int64, len(purchases))
	idx := make(map[int64]int, len(purchases))
	for i, p := range purchases {
		ids[i] = p.ID
		idx[p.ID] = i
	}
	rows, err := q.Query(ctx, `SELECT purchase_id, product_id, product_name, unit_price_points, quantity, line_total_points FROM shop_purchase_items WHERE purchase_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		var it ShopPurchaseItem
		if err := rows.Scan(&pid, &it.ProductID, &it.ProductName, &it.UnitPricePoints, &it.Quantity, &it.LineTotalPoints); err != nil {
			return err
		}
		purchases[idx[pid]].Items = append(purchases[idx[pid]].Items, it)
	}
	return rows.Err()
}

// GetPurchase loads one purchase of the installation. playerID != 0 restricts it to that player's own
// purchases (a player reading another player's purchase is "not found").
func (r *ShopRepository) GetPurchase(ctx context.Context, org, inst, id, playerID int64) (*ShopPurchase, error) {
	return getPurchase(ctx, r.pool, org, inst, id, playerID, false)
}

func getPurchase(ctx context.Context, q shopQuerier, org, inst, id, playerID int64, lock bool) (*ShopPurchase, error) {
	sql := `SELECT ` + purchaseCols + purchaseFrom + ` WHERE sp.id = $3 AND sp.organization_id = $1 AND sp.installation_id = $2 AND ($4::bigint = 0 OR sp.player_id = $4)`
	if lock {
		sql += ` FOR UPDATE OF sp`
	}
	p, err := scanPurchase(q.QueryRow(ctx, sql, org, inst, id, playerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShopPurchaseNotFound
	}
	if err != nil {
		return nil, err
	}
	list := []ShopPurchase{p}
	if err := loadItems(ctx, q, list); err != nil {
		return nil, err
	}
	return &list[0], nil
}

func findByKey(ctx context.Context, q shopQuerier, p PurchaseParams) (*ShopPurchase, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM shop_purchases WHERE installation_id = $1 AND user_id = $2 AND idempotency_key = $3`, p.InstallationID, p.UserID, p.IdempotencyKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return getPurchase(ctx, q, p.OrganizationID, p.InstallationID, id, 0, false)
}

// sameRequest reports whether a stored purchase is the one the request describes.
func sameRequest(sp *ShopPurchase, p PurchaseParams) bool {
	if sp.PlayerID != p.PlayerID || len(sp.Items) != 1 {
		return false
	}
	it := sp.Items[0]
	return it.ProductID != nil && *it.ProductID == p.ProductID && it.Quantity == p.Quantity
}

// Purchase buys Quantity of one product for the player, atomically (see the file comment). Errors:
// ErrShopProductNotFound, ErrShopProductDisabled, ErrShopOutOfStock, ErrShopLimitReached,
// ErrInsufficientFunds (nothing is written), ErrShopIdempotencyConflict. A repeated idempotency key
// returns the original purchase with Duplicate=true and charges nothing.
func (r *ShopRepository) Purchase(ctx context.Context, p PurchaseParams) (*PurchaseResult, error) {
	for attempt := 0; attempt < 3; attempt++ {
		res, err := r.purchaseOnce(ctx, p)
		if errors.Is(err, errShopKeyRaced) {
			continue // the other insert has committed by now: the pre-check finds it
		}
		return res, err
	}
	return nil, fmt.Errorf("purchase kept racing on its idempotency key")
}

func (r *ShopRepository) replay(ctx context.Context, q shopQuerier, existing *ShopPurchase, p PurchaseParams) (*PurchaseResult, error) {
	if !sameRequest(existing, p) {
		return nil, ErrShopIdempotencyConflict
	}
	var balance int64
	if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2),0)`, p.GuildID, p.PlayerID).Scan(&balance); err != nil {
		return nil, err
	}
	res := &PurchaseResult{Purchase: *existing, BalanceAfter: balance, Duplicate: true, PlayerName: existing.PlayerName}
	if len(existing.Items) > 0 {
		res.ItemName = existing.Items[0].ProductName
	}
	return res, nil
}

func (r *ShopRepository) purchaseOnce(ctx context.Context, p PurchaseParams) (*PurchaseResult, error) {
	// Fast path: a retry of an already-committed purchase returns the original even if the product has
	// since sold out or been disabled.
	if existing, err := findByKey(ctx, r.pool, p); err != nil {
		return nil, err
	} else if existing != nil {
		return r.replay(ctx, r.pool, existing, p)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// 1. Lock the product: every buyer of it serialises here (stock, price and limit are decided
	//    against committed, locked state).
	prod, err := scanProduct(tx.QueryRow(ctx, `SELECT `+productCols+productFrom+`
WHERE p.id = $3 AND p.organization_id = $1 AND p.installation_id = $2 FOR UPDATE OF p`, p.OrganizationID, p.InstallationID, p.ProductID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShopProductNotFound
	}
	if err != nil {
		return nil, err
	}
	// 2. Re-check the key under the lock: a request that waited for the lock finds the winner's purchase.
	if existing, err := findByKey(ctx, tx, p); err != nil {
		return nil, err
	} else if existing != nil {
		return r.replay(ctx, tx, existing, p)
	}
	// 3. Validate against the locked product.
	var categoryActive = true
	if prod.CategoryID != nil {
		if err := tx.QueryRow(ctx, `SELECT is_active FROM shop_categories WHERE id=$1 AND installation_id=$2`, *prod.CategoryID, p.InstallationID).Scan(&categoryActive); err != nil {
			return nil, err
		}
	}
	if !prod.IsActive || !categoryActive {
		return nil, ErrShopProductDisabled
	}
	if prod.StockMode == "FINITE" && (prod.StockQuantity == nil || *prod.StockQuantity < int64(p.Quantity)) {
		return nil, ErrShopOutOfStock
	}
	if prod.PurchaseLimit != nil {
		var bought int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(pi.quantity),0) FROM shop_purchase_items pi JOIN shop_purchases sp ON sp.id = pi.purchase_id
WHERE sp.installation_id = $1 AND sp.player_id = $2 AND pi.product_id = $3 AND sp.status IN `+shopCountedStatuses, p.InstallationID, p.PlayerID, p.ProductID).Scan(&bought); err != nil {
			return nil, err
		}
		if bought+int64(p.Quantity) > int64(*prod.PurchaseLimit) {
			return nil, ErrShopLimitReached
		}
	}
	total, ok := mulSafe(prod.PricePoints, int64(p.Quantity))
	if !ok || total > MaxLedgerAmount {
		return nil, ErrInvalidLedgerAmount
	}

	// 4. The purchase row (paid in this very transaction, waiting for manual fulfillment).
	var server any
	if p.ServerID != 0 {
		server = p.ServerID
	}
	var purchaseID int64
	err = tx.QueryRow(ctx, `INSERT INTO shop_purchases(organization_id, installation_id, game_server_id, user_id, player_id, status, total_points, delivery_type, idempotency_key, paid_at)
VALUES($1,$2,$3,$4,$5,'PENDING_FULFILLMENT',$6,$7,$8,NOW())
ON CONFLICT ON CONSTRAINT uq_shop_purchases_idempotency DO NOTHING RETURNING id`,
		p.OrganizationID, p.InstallationID, server, p.UserID, p.PlayerID, total, prod.DeliveryType, p.IdempotencyKey).Scan(&purchaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errShopKeyRaced
	}
	if err != nil {
		return nil, err
	}
	// 5. Debit Champion Points through the one economy write path. Insufficient funds aborts everything.
	desc := clipText(fmt.Sprintf("Shop: %s x%d", prod.Name, p.Quantity), 200)
	entry, err := applyLedger(ctx, tx, LedgerParams{GuildID: p.GuildID, PlayerID: p.PlayerID, ServerID: p.ServerID, Type: TxShopPurchase, Amount: total,
		ReferenceID: ShopPurchaseRef(purchaseID), SourceID: purchaseID, Description: desc, CreatedBy: p.ActorDiscordID}, true)
	if err != nil {
		return nil, err
	}
	// 6. Stock, then the immutable name/price snapshot.
	if prod.StockMode == "FINITE" {
		if _, err := tx.Exec(ctx, `UPDATE shop_products SET stock_quantity = stock_quantity - $3, updated_at = NOW() WHERE id = $1 AND installation_id = $2`, p.ProductID, p.InstallationID, p.Quantity); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO shop_purchase_items(purchase_id, product_id, product_name, unit_price_points, quantity, line_total_points) VALUES($1,$2,$3,$4,$5,$6)`,
		purchaseID, p.ProductID, prod.Name, prod.PricePoints, p.Quantity, total); err != nil {
		return nil, err
	}
	purchase, err := getPurchase(ctx, tx, p.OrganizationID, p.InstallationID, purchaseID, 0, false)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &PurchaseResult{Purchase: *purchase, BalanceAfter: entry.BalanceAfter, PlayerName: purchase.PlayerName, ItemName: prod.Name}, nil
}

func mulSafe(a, b int64) (int64, bool) {
	if a <= 0 || b <= 0 {
		return 0, false
	}
	if a > (1<<62)/b {
		return 0, false
	}
	return a * b, true
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// PurchaseQuery filters a purchase list (newest first, keyset by id).
type PurchaseQuery struct {
	PlayerID  int64 // 0 = any (admin)
	ProductID int64
	Status    string
	Limit     int
	BeforeID  int64
}

// ListPurchases returns up to Limit purchases with their item snapshots (two queries in total, no N+1).
func (r *ShopRepository) ListPurchases(ctx context.Context, org, inst int64, q PurchaseQuery) (items []ShopPurchase, next int64, err error) {
	if q.Limit <= 0 {
		q.Limit = 25
	}
	rows, err := r.pool.Query(ctx, `SELECT `+purchaseCols+purchaseFrom+`
WHERE sp.organization_id = $1 AND sp.installation_id = $2
  AND ($3::bigint = 0 OR sp.player_id = $3)
  AND ($4 = '' OR sp.status = $4)
  AND ($5::bigint = 0 OR EXISTS (SELECT 1 FROM shop_purchase_items pi WHERE pi.purchase_id = sp.id AND pi.product_id = $5))
  AND ($6::bigint = 0 OR sp.id < $6)
ORDER BY sp.id DESC LIMIT $7`, org, inst, q.PlayerID, q.Status, q.ProductID, q.BeforeID, q.Limit+1)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		p, err := scanPurchase(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		items = append(items, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(items) > q.Limit {
		items = items[:q.Limit]
		next = items[len(items)-1].ID
	}
	if err := loadItems(ctx, r.pool, items); err != nil {
		return nil, 0, err
	}
	return items, next, nil
}

// Fulfill marks a PENDING_FULFILLMENT purchase FULFILLED and records the admin. Any other status is
// ErrShopInvalidStatus (also for a concurrent second fulfill).
func (r *ShopRepository) Fulfill(ctx context.Context, org, inst, id, actorUserID int64) (*ShopPurchase, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE shop_purchases SET status='FULFILLED', fulfilled_at=NOW(), fulfilled_by_user_id=$4, updated_at=NOW()
WHERE id=$3 AND organization_id=$1 AND installation_id=$2 AND status='PENDING_FULFILLMENT'`, org, inst, id, actorUserID)
	if err != nil {
		return nil, err
	}
	p, err := r.GetPurchase(ctx, org, inst, id, 0)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrShopInvalidStatus
	}
	return p, nil
}

// RefundParams is one refund.
type RefundParams struct {
	OrganizationID, InstallationID int64
	GuildID, ServerID              int64
	PurchaseID                     int64
	ActorUserID                    int64
	ActorDiscordID                 string
	Reason                         string
}

// RefundResult is the committed refund.
type RefundResult struct {
	Purchase     ShopPurchase
	BalanceAfter int64
	PlayerName   string
	ItemName     string
}

// Refund credits the purchase's total back as a compensating SHOP_REFUND ledger row (the original
// debit is never touched), marks the purchase REFUNDED and - only if the product was not yet handed
// over - returns finite stock. It runs under the purchase row lock, so of concurrent refunds exactly
// one succeeds and the others get ErrShopInvalidStatus; the ledger's own uniqueness of
// (SHOP_REFUND, purchase:<id>) is a second guard. Allowed from PENDING_FULFILLMENT, PAID and FULFILLED.
func (r *ShopRepository) Refund(ctx context.Context, p RefundParams) (*RefundResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	purchase, err := getPurchase(ctx, tx, p.OrganizationID, p.InstallationID, p.PurchaseID, 0, true)
	if err != nil {
		return nil, err
	}
	switch purchase.Status {
	case ShopStatusPendingFulfillment, ShopStatusPaid, ShopStatusFulfilled:
	default:
		return nil, ErrShopInvalidStatus
	}
	itemName := ""
	for _, it := range purchase.Items {
		if itemName == "" {
			itemName = it.ProductName
		}
		// Product lock before the balance lock (see the lock order). A delivered product is not restocked.
		if purchase.Status != ShopStatusFulfilled && it.ProductID != nil {
			if _, err := tx.Exec(ctx, `UPDATE shop_products SET stock_quantity = stock_quantity + $3, updated_at = NOW()
WHERE id = $1 AND installation_id = $2 AND stock_mode = 'FINITE' AND stock_quantity + $3 <= 1000000000`, *it.ProductID, p.InstallationID, it.Quantity); err != nil {
				return nil, err
			}
		}
	}
	entry, err := applyLedger(ctx, tx, LedgerParams{GuildID: p.GuildID, PlayerID: purchase.PlayerID, ServerID: p.ServerID, Type: TxShopRefund, Amount: purchase.TotalPoints,
		ReferenceID: ShopPurchaseRef(purchase.ID), SourceID: purchase.ID, Description: clipText("Shop refund: "+p.Reason, 200), CreatedBy: p.ActorDiscordID}, false)
	if err != nil {
		return nil, err
	}
	if entry.Duplicate {
		return nil, ErrShopInvalidStatus // already refunded according to the ledger
	}
	if _, err := tx.Exec(ctx, `UPDATE shop_purchases SET status='REFUNDED', refunded_at=NOW(), refunded_by_user_id=$4, refund_reason=$5, updated_at=NOW()
WHERE id=$3 AND organization_id=$1 AND installation_id=$2`, p.OrganizationID, p.InstallationID, p.PurchaseID, p.ActorUserID, p.Reason); err != nil {
		return nil, err
	}
	updated, err := getPurchase(ctx, tx, p.OrganizationID, p.InstallationID, p.PurchaseID, 0, false)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &RefundResult{Purchase: *updated, BalanceAfter: entry.BalanceAfter, PlayerName: updated.PlayerName, ItemName: itemName}, nil
}

// --- reconciliation ---------------------------------------------------------------------------------

// Shop reconciliation kinds.
const (
	ShopMismatchMissingDebit     = "MISSING_DEBIT"     // a paid/delivered/refunded purchase has no matching SHOP_PURCHASE debit
	ShopMismatchMissingRefund    = "MISSING_REFUND"    // a REFUNDED purchase has no matching SHOP_REFUND credit
	ShopMismatchUnexpectedRefund = "UNEXPECTED_REFUND" // a SHOP_REFUND credit exists for a purchase that is not REFUNDED
	ShopMismatchOrphanDebit      = "ORPHAN_DEBIT"      // a SHOP_PURCHASE ledger row without a purchase
	ShopMismatchItemTotal        = "ITEM_TOTAL"        // the item snapshots do not add up to the purchase total
)

type ShopMismatch struct {
	Kind       string
	PurchaseID int64
	Detail     string
}

// ReconcileShop verifies, read-only, that the shop and the ledger agree for one installation's guild
// (guildID): every paid purchase has exactly its debit, every refunded purchase exactly its refund credit,
// no ledger shop row is orphaned and item snapshots sum to the total. It never repairs anything.
func (r *ShopRepository) ReconcileShop(ctx context.Context, org, inst, guildID int64, limit int) ([]ShopMismatch, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
SELECT kind, purchase_id, detail FROM (
  SELECT 'MISSING_DEBIT' AS kind, sp.id AS purchase_id, 'no matching SHOP_PURCHASE debit of -' || sp.total_points AS detail
  FROM shop_purchases sp
  WHERE sp.organization_id=$1 AND sp.installation_id=$2 AND sp.status IN ('PAID','PENDING_FULFILLMENT','FULFILLED','REFUNDED')
    AND NOT EXISTS (SELECT 1 FROM point_transactions t WHERE t.guild_id=$3 AND t.player_id=sp.player_id AND t.reason_type='SHOP_PURCHASE'
                    AND t.source_key='purchase:' || sp.id AND t.amount = -sp.total_points)
  UNION ALL
  SELECT 'MISSING_REFUND', sp.id, 'no matching SHOP_REFUND credit of ' || sp.total_points
  FROM shop_purchases sp
  WHERE sp.organization_id=$1 AND sp.installation_id=$2 AND sp.status='REFUNDED'
    AND NOT EXISTS (SELECT 1 FROM point_transactions t WHERE t.guild_id=$3 AND t.player_id=sp.player_id AND t.reason_type='SHOP_REFUND'
                    AND t.source_key='purchase:' || sp.id AND t.amount = sp.total_points)
  UNION ALL
  SELECT 'UNEXPECTED_REFUND', sp.id, 'a SHOP_REFUND credit exists but the purchase is ' || sp.status
  FROM shop_purchases sp
  WHERE sp.organization_id=$1 AND sp.installation_id=$2 AND sp.status <> 'REFUNDED'
    AND EXISTS (SELECT 1 FROM point_transactions t WHERE t.guild_id=$3 AND t.player_id=sp.player_id AND t.reason_type='SHOP_REFUND' AND t.source_key='purchase:' || sp.id)
  UNION ALL
  SELECT 'ORPHAN_DEBIT', COALESCE(t.source_id,0), 'ledger row ' || t.id || ' has no purchase'
  FROM point_transactions t
  WHERE t.guild_id=$3 AND t.reason_type='SHOP_PURCHASE' AND t.server_id IN (SELECT game_server_id FROM installations WHERE id=$2)
    AND NOT EXISTS (SELECT 1 FROM shop_purchases sp WHERE 'purchase:' || sp.id = t.source_key AND sp.player_id = t.player_id)
  UNION ALL
  SELECT 'ITEM_TOTAL', sp.id, 'items sum to ' || COALESCE(s.total,0) || ' but the purchase total is ' || sp.total_points
  FROM shop_purchases sp LEFT JOIN (SELECT purchase_id, SUM(line_total_points) AS total FROM shop_purchase_items GROUP BY purchase_id) s ON s.purchase_id = sp.id
  WHERE sp.organization_id=$1 AND sp.installation_id=$2 AND COALESCE(s.total,0) <> sp.total_points
) x ORDER BY purchase_id, kind LIMIT $4`, org, inst, guildID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShopMismatch{}
	for rows.Next() {
		var m ShopMismatch
		if err := rows.Scan(&m.Kind, &m.PurchaseID, &m.Detail); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
