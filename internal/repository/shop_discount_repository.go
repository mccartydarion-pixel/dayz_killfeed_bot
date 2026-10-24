package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Shop deals and sales (docs/TRADER.md, migration 0144). A DEAL takes a percentage off one item; a
// SALE takes it off a category, or off every item when it has no category. A purchase pays the best
// one running for its item (repository.ShopRepository.Purchase), so a deal or sale is the shop's
// price on the website and in Discord alike.

const (
	DiscountDeal = "DEAL"
	DiscountSale = "SALE"
	// MaxDiscountPercent caps a deal or sale so a typo can't give items away.
	MaxDiscountPercent = 75
	// MaxDiscountDuration is the longest a deal or sale runs.
	MaxDiscountDuration = 30 * 24 * time.Hour
)

var (
	ErrDiscountInvalid  = errors.New("discount: invalid")
	ErrDiscountNotFound = errors.New("discount: not found")
)

// ShopDiscount is one deal or sale.
type ShopDiscount struct {
	ID, OrganizationID, InstallationID int64
	Kind                               string
	ProductID, CategoryID              *int64
	// ProductName and CategoryName are the item's or category's current names ("" for a sale on
	// every item).
	ProductName, CategoryName string
	PercentOff                int
	Title                     string
	StartsAt, EndsAt          time.Time
	EndedAt                   *time.Time
	EndedByDiscordID          string
	CreatedByDiscordID        string
	PostChannelID             string
	PostMessageID             string
	PostEnded                 bool
}

// Running reports whether d takes money off at now.
func (d ShopDiscount) Running(now time.Time) bool {
	return d.EndedAt == nil && !now.Before(d.StartsAt) && now.Before(d.EndsAt)
}

// Covers reports whether d applies to the item productID in categoryID (nil: no category).
func (d ShopDiscount) Covers(productID int64, categoryID *int64) bool {
	switch d.Kind {
	case DiscountDeal:
		return d.ProductID != nil && *d.ProductID == productID
	case DiscountSale:
		return d.CategoryID == nil || categoryID != nil && *d.CategoryID == *categoryID
	}
	return false
}

// BestDiscount is the deal or sale an item is bought under: the biggest percentage off of those
// running, the oldest on a tie. nil when none applies.
func BestDiscount(ds []ShopDiscount, productID int64, categoryID *int64, now time.Time) *ShopDiscount {
	var best *ShopDiscount
	for i := range ds {
		d := &ds[i]
		if !d.Running(now) || !d.Covers(productID, categoryID) {
			continue
		}
		if best == nil || d.PercentOff > best.PercentOff || d.PercentOff == best.PercentOff && d.ID < best.ID {
			best = d
		}
	}
	return best
}

// DiscountedPrice is price with percent off: the points taken off round down, and an item always
// costs at least 1 point.
func DiscountedPrice(price int64, percent int) int64 {
	if percent <= 0 || price <= 0 {
		return price
	}
	if percent > MaxDiscountPercent {
		percent = MaxDiscountPercent
	}
	off := price/100*int64(percent) + price%100*int64(percent)/100
	if out := price - off; out >= 1 {
		return out
	}
	return 1
}

// runningDiscountFor is the best deal or sale running now for the item, read inside the purchase
// transaction.
func runningDiscountFor(ctx context.Context, q shopQuerier, installationID, productID int64, categoryID *int64) (*ShopDiscount, error) {
	var d ShopDiscount
	err := q.QueryRow(ctx, `SELECT id, percent_off FROM shop_discounts
WHERE installation_id=$1 AND ended_at IS NULL AND starts_at<=NOW() AND ends_at>NOW()
  AND ((kind='DEAL' AND product_id=$2) OR (kind='SALE' AND (category_id IS NULL OR category_id=$3)))
ORDER BY percent_off DESC, id ASC LIMIT 1`, installationID, productID, categoryID).Scan(&d.ID, &d.PercentOff)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ShopDiscountRepository stores deals and sales.
type ShopDiscountRepository struct{ pool *pgxpool.Pool }

func NewShopDiscountRepository(pool *pgxpool.Pool) *ShopDiscountRepository {
	return &ShopDiscountRepository{pool: pool}
}

const discountCols = `d.id, d.organization_id, d.installation_id, d.kind, d.product_id, d.category_id, COALESCE(p.name,''), COALESCE(c.name,''),
 d.percent_off, d.title, d.starts_at, d.ends_at, d.ended_at, d.ended_by_discord_id, d.created_by_discord_id, d.post_channel_id, d.post_message_id, d.post_ended`

const discountFrom = ` FROM shop_discounts d LEFT JOIN shop_products p ON p.id = d.product_id LEFT JOIN shop_categories c ON c.id = d.category_id`

func scanDiscount(row pgx.Row) (ShopDiscount, error) {
	var d ShopDiscount
	err := row.Scan(&d.ID, &d.OrganizationID, &d.InstallationID, &d.Kind, &d.ProductID, &d.CategoryID, &d.ProductName, &d.CategoryName,
		&d.PercentOff, &d.Title, &d.StartsAt, &d.EndsAt, &d.EndedAt, &d.EndedByDiscordID, &d.CreatedByDiscordID, &d.PostChannelID, &d.PostMessageID, &d.PostEnded)
	return d, err
}

func (r *ShopDiscountRepository) list(ctx context.Context, where string, args ...any) ([]ShopDiscount, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+discountCols+discountFrom+` WHERE `+where+` ORDER BY d.ends_at, d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShopDiscount
	for rows.Next() {
		d, err := scanDiscount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ShopDiscountInput is a new deal or sale.
type ShopDiscountInput struct {
	OrganizationID, InstallationID int64
	Kind                           string
	ProductID, CategoryID          *int64
	PercentOff                     int
	Title                          string
	Duration                       time.Duration
	CreatedByDiscordID             string
}

// Create starts a deal or sale now. The item or category must be the installation's.
func (r *ShopDiscountRepository) Create(ctx context.Context, in ShopDiscountInput) (*ShopDiscount, error) {
	if in.PercentOff < 1 || in.PercentOff > MaxDiscountPercent || in.Duration < time.Minute || in.Duration > MaxDiscountDuration {
		return nil, ErrDiscountInvalid
	}
	switch in.Kind {
	case DiscountDeal:
		if in.ProductID == nil || in.CategoryID != nil {
			return nil, ErrDiscountInvalid
		}
	case DiscountSale:
		if in.ProductID != nil {
			return nil, ErrDiscountInvalid
		}
	default:
		return nil, ErrDiscountInvalid
	}
	var id int64
	err := r.pool.QueryRow(ctx, `INSERT INTO shop_discounts(organization_id, installation_id, kind, product_id, category_id, percent_off, title, ends_at, created_by_discord_id)
SELECT $1, $2, $3, $4, $5, $6, $7, NOW() + make_interval(secs => $8), $9
WHERE ($4::bigint IS NULL OR EXISTS (SELECT 1 FROM shop_products WHERE id=$4 AND installation_id=$2 AND organization_id=$1))
  AND ($5::bigint IS NULL OR EXISTS (SELECT 1 FROM shop_categories WHERE id=$5 AND installation_id=$2 AND organization_id=$1))
RETURNING id`, in.OrganizationID, in.InstallationID, in.Kind, in.ProductID, in.CategoryID, in.PercentOff, in.Title, in.Duration.Seconds(), in.CreatedByDiscordID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDiscountNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, in.InstallationID, id)
}

// Get is one of installationID's deals or sales.
func (r *ShopDiscountRepository) Get(ctx context.Context, installationID, id int64) (*ShopDiscount, error) {
	d, err := scanDiscount(r.pool.QueryRow(ctx, `SELECT `+discountCols+discountFrom+` WHERE d.installation_id=$1 AND d.id=$2`, installationID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDiscountNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// Running lists installationID's deals and sales running now, soonest ending first.
func (r *ShopDiscountRepository) Running(ctx context.Context, installationID int64) ([]ShopDiscount, error) {
	return r.list(ctx, `d.installation_id=$1 AND d.ended_at IS NULL AND d.starts_at<=NOW() AND d.ends_at>NOW()`, installationID)
}

// End stops one deal or sale now. changed is false when it had already ended.
func (r *ShopDiscountRepository) End(ctx context.Context, installationID, id int64, byDiscordID string) (d *ShopDiscount, changed bool, err error) {
	tag, err := r.pool.Exec(ctx, `UPDATE shop_discounts SET ended_at=NOW(), ended_by_discord_id=$3
WHERE installation_id=$1 AND id=$2 AND ended_at IS NULL AND ends_at>NOW()`, installationID, id, byDiscordID)
	if err != nil {
		return nil, false, err
	}
	d, err = r.Get(ctx, installationID, id)
	return d, tag.RowsAffected() > 0, err
}

// EndAll stops every running deal and sale of installationID and returns them.
func (r *ShopDiscountRepository) EndAll(ctx context.Context, installationID int64, byDiscordID string) ([]ShopDiscount, error) {
	rows, err := r.pool.Query(ctx, `UPDATE shop_discounts SET ended_at=NOW(), ended_by_discord_id=$2
WHERE installation_id=$1 AND ended_at IS NULL AND ends_at>NOW() RETURNING id`, installationID, byDiscordID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return r.list(ctx, `d.installation_id=$1 AND d.id = ANY($2)`, installationID, ids)
}

// SetPost records the Discord post announcing a deal or sale.
func (r *ShopDiscountRepository) SetPost(ctx context.Context, installationID, id int64, channelID, messageID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE shop_discounts SET post_channel_id=$3, post_message_id=$4 WHERE installation_id=$1 AND id=$2`, installationID, id, channelID, messageID)
	return err
}

// PostsToEnd lists installationID's deals and sales that are over but whose post still shows them
// running.
func (r *ShopDiscountRepository) PostsToEnd(ctx context.Context, installationID int64) ([]ShopDiscount, error) {
	return r.list(ctx, `d.installation_id=$1 AND d.post_message_id<>'' AND NOT d.post_ended AND (d.ended_at IS NOT NULL OR d.ends_at<=NOW())`, installationID)
}

// MarkPostEnded records that a deal's or sale's post now shows it over.
func (r *ShopDiscountRepository) MarkPostEnded(ctx context.Context, installationID, id int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE shop_discounts SET post_ended=TRUE WHERE installation_id=$1 AND id=$2`, installationID, id)
	return err
}
