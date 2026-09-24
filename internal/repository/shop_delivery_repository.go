package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/dayzmap"
)

// Champion Shop Delivery Engine 2.0 persistence (docs/SHOP_DELIVERY.md). Every purchase has exactly
// one shop_deliveries row, written in the purchase's own transaction. Delivery is always performed
// by staff in this phase: the only states are MANUAL_READY, FULFILLED and CANCELLED, and nothing
// here touches a game server.

// Delivery policies (stable strings, also a CHECK constraint).
const (
	DeliveryPolicyManualPickup     = "MANUAL_PICKUP"     // staff arrange the handover; no coordinates
	DeliveryPolicyManualCoordinate = "MANUAL_COORDINATE" // the player gives X/Z; staff deliver there
)

// Delivery statuses. Only the first three exist in Phase 2A (the table's CHECK allows nothing else).
const (
	DeliveryStatusManualReady = "MANUAL_READY"
	DeliveryStatusFulfilled   = "FULFILLED"
	DeliveryStatusCancelled   = "CANCELLED"

	// Reserved for automatic delivery (Phase 2B) - not accepted by the database yet. FILE_STAGED
	// does not mean an item spawned; RESTART_OBSERVED does not mean an item was delivered.
	DeliveryStatusQueued               = "QUEUED"
	DeliveryStatusPreparing            = "PREPARING"
	DeliveryStatusFileStaged           = "FILE_STAGED"
	DeliveryStatusAwaitingRestart      = "AWAITING_RESTART"
	DeliveryStatusRestartObserved      = "RESTART_OBSERVED"
	DeliveryStatusVerificationRequired = "VERIFICATION_REQUIRED"
	DeliveryStatusFailedReview         = "FAILED_REVIEW"
)

// Delivery cancel reasons.
const DeliveryCancelRefunded = "REFUNDED"

var (
	ErrShopDeliveryCoordinatesRequired    = errors.New("this product is delivered to map coordinates: delivery.x and delivery.z are required")
	ErrShopDeliveryCoordinatesNotAccepted = errors.New("this product is a manual pickup: delivery coordinates are not accepted")
	ErrShopDeliveryOutOfBounds            = errors.New("the delivery coordinates are outside the map")
	ErrShopDeliveryMapUnresolved          = errors.New("this server's map is not configured or not supported for coordinate delivery")
	ErrShopDeliveryNotFound               = errors.New("delivery not found")
)

// ShopDelivery is one purchase's delivery record.
type ShopDelivery struct {
	ID, PurchaseID, OrganizationID, InstallationID int64
	GameServerID                                   *int64
	PlayerID                                       int64
	PlayerName                                     string
	DeliveryType, Policy, MapKey                   string
	X, Z                                           *float64
	Status                                         string
	CreatedAt, UpdatedAt                           time.Time
	FulfilledAt, CancelledAt                       *time.Time
	FulfilledByDiscordID, CancelledByDiscordID     string
	CancelReason                                   string
	// Queue/detail reads only.
	PurchaseStatus string
	Items          []ShopPurchaseItem
}

const deliveryCols = `d.id, d.purchase_id, d.organization_id, d.installation_id, d.game_server_id, d.player_id, COALESCE(pl.display_name,''),
d.delivery_type, d.delivery_policy, COALESCE(d.map_key,''), d.coord_x, d.coord_z, d.status, d.created_at, d.updated_at,
d.fulfilled_at, d.cancelled_at, COALESCE(fu.discord_user_id,''), COALESCE(cu.discord_user_id,''), COALESCE(d.cancel_reason,''), sp.status`

const deliveryFrom = ` FROM shop_deliveries d
JOIN shop_purchases sp ON sp.id = d.purchase_id AND sp.installation_id = d.installation_id
JOIN players pl ON pl.id = d.player_id
LEFT JOIN app_users fu ON fu.id = d.fulfilled_by_user_id
LEFT JOIN app_users cu ON cu.id = d.cancelled_by_user_id`

func scanDelivery(row pgx.Row) (ShopDelivery, error) {
	var d ShopDelivery
	err := row.Scan(&d.ID, &d.PurchaseID, &d.OrganizationID, &d.InstallationID, &d.GameServerID, &d.PlayerID, &d.PlayerName,
		&d.DeliveryType, &d.Policy, &d.MapKey, &d.X, &d.Z, &d.Status, &d.CreatedAt, &d.UpdatedAt,
		&d.FulfilledAt, &d.CancelledAt, &d.FulfilledByDiscordID, &d.CancelledByDiscordID, &d.CancelReason, &d.PurchaseStatus)
	return d, err
}

// purchaseDeliveryMap validates the request's delivery instructions against the locked product's
// policy and returns the map key to snapshot ("" for a pickup). It runs inside the purchase
// transaction, before anything is written.
func purchaseDeliveryMap(ctx context.Context, q shopQuerier, p PurchaseParams, policy string) (string, error) {
	if policy != DeliveryPolicyManualCoordinate {
		if p.Delivery != nil {
			return "", ErrShopDeliveryCoordinatesNotAccepted
		}
		return "", nil
	}
	if p.Delivery == nil {
		return "", ErrShopDeliveryCoordinatesRequired
	}
	var key string
	err := q.QueryRow(ctx, `SELECT COALESCE(map_key,'') FROM shop_delivery_settings WHERE installation_id=$1 AND organization_id=$2`, p.InstallationID, p.OrganizationID).Scan(&key)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	m, ok := dayzmap.Lookup(key)
	if !ok {
		return "", ErrShopDeliveryMapUnresolved
	}
	if !m.Contains(p.Delivery.X, p.Delivery.Z) {
		return "", ErrShopDeliveryOutOfBounds
	}
	return m.Key, nil
}

func insertDelivery(ctx context.Context, tx pgx.Tx, p PurchaseParams, purchaseID int64, policy, mapKey string) error {
	var server, mk, x, z any
	if p.ServerID != 0 {
		server = p.ServerID
	}
	if policy == DeliveryPolicyManualCoordinate {
		mk, x, z = mapKey, p.Delivery.X, p.Delivery.Z
	}
	_, err := tx.Exec(ctx, `INSERT INTO shop_deliveries(purchase_id, organization_id, installation_id, game_server_id, player_id, delivery_policy, map_key, coord_x, coord_z, status)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'MANUAL_READY')`, purchaseID, p.OrganizationID, p.InstallationID, server, p.PlayerID, policy, mk, x, z)
	return err
}

// healDelivery creates purchaseID's delivery record from the purchase's own state when it is
// missing (a purchase written by an older instance during a rolling deploy). No-op otherwise.
func healDelivery(ctx context.Context, tx pgx.Tx, purchaseID int64) error {
	_, err := tx.Exec(ctx, database.ShopDeliveryHealSQL, purchaseID)
	return err
}

// loadDeliveries attaches each purchase's delivery record with ONE query.
func loadDeliveries(ctx context.Context, q shopQuerier, purchases []ShopPurchase) error {
	if len(purchases) == 0 {
		return nil
	}
	ids := make([]int64, len(purchases))
	idx := make(map[int64]int, len(purchases))
	for i, p := range purchases {
		ids[i] = p.ID
		idx[p.ID] = i
	}
	rows, err := q.Query(ctx, `SELECT `+deliveryCols+deliveryFrom+` WHERE d.purchase_id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return err
		}
		if i, ok := idx[d.PurchaseID]; ok && d.InstallationID == purchases[i].InstallationID {
			dd := d
			purchases[i].Delivery = &dd
		}
	}
	return rows.Err()
}

// DeliveryQuery filters the delivery queue (newest first, keyset by id).
type DeliveryQuery struct {
	Status   string // "" = any
	Policy   string // "" = any
	PlayerID int64  // 0 = any
	Limit    int
	BeforeID int64
}

// ListDeliveries is the installation's delivery queue with each order's item snapshot (two queries,
// no N+1). next is the id to continue before, 0 on the last page.
func (r *ShopRepository) ListDeliveries(ctx context.Context, org, inst int64, q DeliveryQuery) (items []ShopDelivery, next int64, err error) {
	if q.Limit <= 0 {
		q.Limit = 25
	}
	rows, err := r.pool.Query(ctx, `SELECT `+deliveryCols+deliveryFrom+`
WHERE d.organization_id = $1 AND d.installation_id = $2
  AND ($3 = '' OR d.status = $3)
  AND ($4 = '' OR d.delivery_policy = $4)
  AND ($5::bigint = 0 OR d.player_id = $5)
  AND ($6::bigint = 0 OR d.id < $6)
ORDER BY d.id DESC LIMIT $7`, org, inst, q.Status, q.Policy, q.PlayerID, q.BeforeID, q.Limit+1)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		items = append(items, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(items) > q.Limit {
		items = items[:q.Limit]
		next = items[len(items)-1].ID
	}
	if err := loadDeliveryItems(ctx, r.pool, items); err != nil {
		return nil, 0, err
	}
	return items, next, nil
}

// GetDelivery loads one delivery of the installation. playerID != 0 restricts it to that player's
// own deliveries (anyone else's is "not found").
func (r *ShopRepository) GetDelivery(ctx context.Context, org, inst, id, playerID int64) (*ShopDelivery, error) {
	d, err := scanDelivery(r.pool.QueryRow(ctx, `SELECT `+deliveryCols+deliveryFrom+`
WHERE d.id = $3 AND d.organization_id = $1 AND d.installation_id = $2 AND ($4::bigint = 0 OR d.player_id = $4)`, org, inst, id, playerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShopDeliveryNotFound
	}
	if err != nil {
		return nil, err
	}
	list := []ShopDelivery{d}
	if err := loadDeliveryItems(ctx, r.pool, list); err != nil {
		return nil, err
	}
	return &list[0], nil
}

func loadDeliveryItems(ctx context.Context, q shopQuerier, deliveries []ShopDelivery) error {
	if len(deliveries) == 0 {
		return nil
	}
	ids := make([]int64, len(deliveries))
	byPurchase := make(map[int64]int, len(deliveries))
	for i, d := range deliveries {
		ids[i] = d.PurchaseID
		byPurchase[d.PurchaseID] = i
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
		deliveries[byPurchase[pid]].Items = append(deliveries[byPurchase[pid]].Items, it)
	}
	return rows.Err()
}

// ShopDeliverySettings is an installation's delivery configuration.
type ShopDeliverySettings struct {
	MapKey             string // "" = not configured
	UpdatedAt          *time.Time
	UpdatedByDiscordID string
}

// GetDeliverySettings returns the installation's delivery settings (zero value when never set).
func (r *ShopRepository) GetDeliverySettings(ctx context.Context, org, inst int64) (ShopDeliverySettings, error) {
	var s ShopDeliverySettings
	var at time.Time
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(s.map_key,''), s.updated_at, COALESCE(u.discord_user_id,'')
FROM shop_delivery_settings s LEFT JOIN app_users u ON u.id = s.updated_by_user_id
WHERE s.installation_id=$2 AND s.organization_id=$1`, org, inst).Scan(&s.MapKey, &at, &s.UpdatedByDiscordID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ShopDeliverySettings{}, nil
	}
	if err != nil {
		return ShopDeliverySettings{}, err
	}
	s.UpdatedAt = &at
	return s, nil
}

// SetDeliveryMap stores the installation's map key ("" clears it). The caller validates the key
// against the verified map catalog. Existing deliveries keep their own snapshotted map.
func (r *ShopRepository) SetDeliveryMap(ctx context.Context, org, inst int64, mapKey string, actorUserID int64) (ShopDeliverySettings, error) {
	var key any
	if mapKey != "" {
		key = mapKey
	}
	tag, err := r.pool.Exec(ctx, `INSERT INTO shop_delivery_settings(installation_id, organization_id, map_key, updated_by_user_id)
SELECT i.id, i.organization_id, $3, $4 FROM installations i WHERE i.id = $2 AND i.organization_id = $1
ON CONFLICT (installation_id) DO UPDATE SET map_key = EXCLUDED.map_key, updated_by_user_id = EXCLUDED.updated_by_user_id, updated_at = NOW()`, org, inst, key, actorUserID)
	if err != nil {
		return ShopDeliverySettings{}, err
	}
	if tag.RowsAffected() == 0 {
		return ShopDeliverySettings{}, ErrShopProductNotFound // the installation is not this organization's
	}
	return r.GetDeliverySettings(ctx, org, inst)
}
