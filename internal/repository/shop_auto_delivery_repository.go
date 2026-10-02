package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The delivery worker's own storage (migration 0101, docs/SHOP_DELIVERY_WORKER_DESIGN.md): the
// owner's switch and the pause, the lease, the write journal, and the reads the worker decides on
// (which orders are deliverable, where, and what the server logs say about a boot). The attempt
// ledger itself stays in ShopAttemptRepository.

var (
	// ErrShopWriteOutstanding: the server has an unfinished or uncertain write. Nothing more is
	// written to it until a person resolves that write.
	ErrShopWriteOutstanding = errors.New("an earlier write to the Champion file is unresolved")
	// ErrShopAutoDeliveryNotFound: the installation is not in this organization.
	ErrShopAutoDeliveryNotFound = errors.New("installation not found in this organization")
)

// Write journal kinds and outcomes.
const (
	ShopWriteStage   = "STAGE"
	ShopWriteUnstage = "UNSTAGE"

	ShopWriteStarted         = "STARTED"
	ShopWriteWrittenVerified = "WRITTEN_VERIFIED"
	ShopWriteNotWritten      = "NOT_WRITTEN"
	ShopWriteUncertain       = "UNCERTAIN"
)

// ShopAutoDeliveryRepository is the worker's store. It shares the Shop pool.
type ShopAutoDeliveryRepository struct{ pool pgxBeginner }

func NewShopAutoDeliveryRepository(pool pgxBeginner) *ShopAutoDeliveryRepository {
	return &ShopAutoDeliveryRepository{pool: pool}
}

// ShopAutoDeliverySettings is one installation's automatic delivery state.
type ShopAutoDeliverySettings struct {
	Enabled      bool
	PausedAt     *time.Time
	PausedReason string
	UpdatedAt    *time.Time
	// MarkerClass is the static object spawned beside each delivered order ("" = no marker).
	MarkerClass string
}

// ShopDefaultMarkerClass is the marker an installation has until its owner changes it (it is also
// the column default of migration 0114).
const ShopDefaultMarkerClass = "StaticObj_Roadblock_Wood_Small"

// Settings reads the installation's switch and pause (all off when it has no row yet).
func (r *ShopAutoDeliveryRepository) Settings(ctx context.Context, org, inst int64) (ShopAutoDeliverySettings, error) {
	var s ShopAutoDeliverySettings
	var found bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM installations WHERE id=$2 AND organization_id=$1)`, org, inst).Scan(&found)
	if err != nil {
		return s, err
	}
	if !found {
		return s, ErrShopAutoDeliveryNotFound
	}
	err = r.pool.QueryRow(ctx, `SELECT enabled, paused_at, COALESCE(paused_reason,''), updated_at, COALESCE(marker_class,'') FROM shop_auto_delivery_settings
 WHERE installation_id=$2 AND organization_id=$1`, org, inst).Scan(&s.Enabled, &s.PausedAt, &s.PausedReason, &s.UpdatedAt, &s.MarkerClass)
	if errors.Is(err, pgx.ErrNoRows) {
		return ShopAutoDeliverySettings{MarkerClass: ShopDefaultMarkerClass}, nil
	}
	return s, err
}

// SetEnabled is the owner's switch. Turning it on never clears a pause.
func (r *ShopAutoDeliveryRepository) SetEnabled(ctx context.Context, org, inst int64, enabled bool, actorUserID int64) error {
	return execShop(ctx, r.pool, `INSERT INTO shop_auto_delivery_settings(installation_id, organization_id, enabled, updated_by_user_id)
VALUES($2,$1,$3,$4)
ON CONFLICT (installation_id) DO UPDATE SET enabled=EXCLUDED.enabled, updated_by_user_id=EXCLUDED.updated_by_user_id, updated_at=NOW()`,
		org, inst, enabled, nullID(actorUserID))
}

// SetMarker is the owner's choice of marker: a DayZ class name, or "" for none. It applies to orders
// planned from now on; an order already staged keeps the marker it was staged with.
func (r *ShopAutoDeliveryRepository) SetMarker(ctx context.Context, org, inst int64, markerClass string, actorUserID int64) error {
	return execShop(ctx, r.pool, `INSERT INTO shop_auto_delivery_settings(installation_id, organization_id, marker_class, updated_by_user_id)
VALUES($2,$1,NULLIF($3,''),$4)
ON CONFLICT (installation_id) DO UPDATE SET marker_class=EXCLUDED.marker_class, updated_by_user_id=EXCLUDED.updated_by_user_id, updated_at=NOW()`,
		org, inst, markerClass, nullID(actorUserID))
}

// Pause stops automatic delivery for the installation with a reason. The first reason is kept: a
// later pass that fails for a consequence of it must not overwrite the cause.
func (r *ShopAutoDeliveryRepository) Pause(ctx context.Context, org, inst int64, reason string) error {
	reason = clipText(strings.TrimSpace(reason), 500)
	if reason == "" {
		reason = "paused"
	}
	return execShop(ctx, r.pool, `INSERT INTO shop_auto_delivery_settings(installation_id, organization_id, paused_at, paused_reason)
VALUES($2,$1,NOW(),$3)
ON CONFLICT (installation_id) DO UPDATE SET paused_at=NOW(), paused_reason=EXCLUDED.paused_reason, updated_at=NOW()
 WHERE shop_auto_delivery_settings.paused_at IS NULL`, org, inst, reason)
}

// Resume is a person's decision to let the worker continue: it clears the pause, forgets the
// accepted configuration (the worker accepts the current one on its next pass) and marks any
// unresolved write as resolved by that person.
func (r *ShopAutoDeliveryRepository) Resume(ctx context.Context, org, inst, actorUserID int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE shop_auto_delivery_settings SET paused_at=NULL, paused_reason=NULL, config_sha256=NULL, mission_path=NULL,
 updated_by_user_id=$3, updated_at=NOW() WHERE installation_id=$2 AND organization_id=$1`, org, inst, nullID(actorUserID)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE shop_delivery_writes SET resolved_at=NOW(), resolved_by_user_id=$3
 WHERE installation_id=$2 AND organization_id=$1 AND outcome IN ('STARTED','UNCERTAIN') AND resolved_at IS NULL`, org, inst, nullID(actorUserID)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ShopAutoInstallation is one installation the worker may work on, with its verified bindings.
type ShopAutoInstallation struct {
	OrganizationID, InstallationID, GameServerID, GuildRowID int64
	NitradoServiceID, MapKey                                 string
	// ConfigSHA256 and MissionPath are what the worker last accepted ("" = nothing accepted yet).
	ConfigSHA256, MissionPath string
	// UTCOffsetMinutes is the server clock's offset from UTC, learned by Live Sync (nil = unknown).
	UTCOffsetMinutes *int
	// MarkerClass is the marker to stage beside each newly planned order ("" = none).
	MarkerClass string
}

// ClaimInstallations leases the installations that are enabled, not paused, READY and in allowed,
// and whose lease is free, for leaseFor. A second worker gets none of them until the lease ends.
func (r *ShopAutoDeliveryRepository) ClaimInstallations(ctx context.Context, now time.Time, owner string, leaseFor time.Duration, allowed []int64) ([]ShopAutoInstallation, error) {
	rows, err := r.pool.Query(ctx, `
WITH claimed AS (
    UPDATE shop_auto_delivery_settings s SET lease_owner=$2, lease_until=$3
     WHERE s.installation_id IN (SELECT installation_id FROM shop_auto_delivery_settings
                                  WHERE enabled AND paused_at IS NULL AND installation_id = ANY($4)
                                    AND (lease_until IS NULL OR lease_until <= $1 OR lease_owner = $2)
                                  ORDER BY installation_id FOR UPDATE SKIP LOCKED)
    RETURNING s.installation_id, s.organization_id, COALESCE(s.config_sha256,'') AS config_sha256, COALESCE(s.mission_path,'') AS mission_path,
              COALESCE(s.marker_class,'') AS marker_class
)
SELECT cl.organization_id, cl.installation_id, gs.id, dc.guild_id, gs.provider_service_id, COALESCE(ds.map_key,''),
       cl.config_sha256, cl.mission_path, ck.utc_offset_minutes, cl.marker_class
  FROM claimed cl
  JOIN installations i ON i.id = cl.installation_id AND i.organization_id = cl.organization_id AND i.status = 'READY'
  JOIN game_servers gs ON gs.id = i.game_server_id
  JOIN discord_guild_connections dc ON dc.id = i.discord_guild_connection_id
  LEFT JOIN shop_delivery_settings ds ON ds.installation_id = i.id AND ds.organization_id = i.organization_id
  LEFT JOIN live_sync_server_clock ck ON ck.server_id = gs.id
 ORDER BY cl.installation_id`, now, owner, now.Add(leaseFor), allowed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShopAutoInstallation
	for rows.Next() {
		var a ShopAutoInstallation
		if err := rows.Scan(&a.OrganizationID, &a.InstallationID, &a.GameServerID, &a.GuildRowID, &a.NitradoServiceID, &a.MapKey,
			&a.ConfigSHA256, &a.MissionPath, &a.UTCOffsetMinutes, &a.MarkerClass); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReleaseLease ends this worker's lease on the installation early (at the end of a pass).
func (r *ShopAutoDeliveryRepository) ReleaseLease(ctx context.Context, inst int64, owner string) error {
	return execShop(ctx, r.pool, `UPDATE shop_auto_delivery_settings SET lease_until=NULL, lease_owner=NULL WHERE installation_id=$1 AND lease_owner=$2`, inst, owner)
}

// AcceptConfiguration records the configuration and mission the worker verified and will require
// from now on. It is only written while nothing is accepted yet (after enabling, or after a resume).
func (r *ShopAutoDeliveryRepository) AcceptConfiguration(ctx context.Context, org, inst int64, configSHA256, missionPath string) error {
	return execShop(ctx, r.pool, `UPDATE shop_auto_delivery_settings SET config_sha256=$3, mission_path=$4, updated_at=NOW()
 WHERE installation_id=$2 AND organization_id=$1 AND config_sha256 IS NULL`, org, inst, configSHA256, missionPath)
}

// ShopDeliveryWrite is one journaled write to the Champion spawner file.
type ShopDeliveryWrite struct {
	ID, OrganizationID, InstallationID int64
	Kind                               string
	AttemptIDs                         []string
	BeforeSHA256, PayloadSHA256        string
	BootFile, Outcome                  string
	AfterSHA256                        *string
	Detail, Worker                     string
	StartedAt                          time.Time
	FinishedAt                         *time.Time
}

const writeCols = `id, organization_id, installation_id, kind, attempt_ids, before_sha256, payload_sha256, boot_file, outcome, after_sha256, detail, worker, started_at, finished_at`

func scanWrite(row pgx.Row) (ShopDeliveryWrite, error) {
	var w ShopDeliveryWrite
	err := row.Scan(&w.ID, &w.OrganizationID, &w.InstallationID, &w.Kind, &w.AttemptIDs, &w.BeforeSHA256, &w.PayloadSHA256, &w.BootFile, &w.Outcome,
		&w.AfterSHA256, &w.Detail, &w.Worker, &w.StartedAt, &w.FinishedAt)
	return w, err
}

// BeginWrite journals a write BEFORE it is sent. It fails with ErrShopWriteOutstanding while the
// installation has an unfinished or uncertain write, so a crashed write is never followed by another.
func (r *ShopAutoDeliveryRepository) BeginWrite(ctx context.Context, w ShopDeliveryWrite) (ShopDeliveryWrite, error) {
	out, err := scanWrite(r.pool.QueryRow(ctx, `INSERT INTO shop_delivery_writes(organization_id, installation_id, kind, attempt_ids, before_sha256, payload_sha256, boot_file, worker)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+writeCols, w.OrganizationID, w.InstallationID, w.Kind, w.AttemptIDs, w.BeforeSHA256, w.PayloadSHA256, w.BootFile, w.Worker))
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "uq_shop_delivery_writes_unresolved" {
		return out, ErrShopWriteOutstanding
	}
	return out, err
}

// FinishWrite records the verified outcome of a journaled write. Only a STARTED write can be finished.
func (r *ShopAutoDeliveryRepository) FinishWrite(ctx context.Context, id int64, outcome, afterSHA256, detail string) error {
	if outcome != ShopWriteWrittenVerified && outcome != ShopWriteNotWritten && outcome != ShopWriteUncertain {
		return errors.New("shop: unknown write outcome")
	}
	var one int
	err := r.pool.QueryRow(ctx, `UPDATE shop_delivery_writes SET outcome=$2, after_sha256=NULLIF($3,''), detail=$4, finished_at=NOW()
 WHERE id=$1 AND outcome='STARTED' RETURNING 1`, id, outcome, afterSHA256, clipText(detail, 500)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("shop: the write is not open")
	}
	return err
}

// UnresolvedWrite returns the installation's unfinished or uncertain write, if it has one.
func (r *ShopAutoDeliveryRepository) UnresolvedWrite(ctx context.Context, inst int64) (*ShopDeliveryWrite, error) {
	w, err := scanWrite(r.pool.QueryRow(ctx, `SELECT `+writeCols+` FROM shop_delivery_writes
 WHERE installation_id=$1 AND outcome IN ('STARTED','UNCERTAIN') AND resolved_at IS NULL`, inst))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// ShopAutoCandidate is an open order the worker could deliver: an automatic product bought at a
// position the server itself logged for that buyer.
type ShopAutoCandidate struct {
	DeliveryID, PurchaseID, PlayerID int64
	ClassName                        string
	AltitudeY                        float64
	DropSourceFile                   string
	DropSourceOffset                 int64
}

// dropPointTolerance is how far a delivery coordinate may differ from the logged position it was
// taken from (the ADM prints one decimal; the purchase stores what the site sent back).
const dropPointTolerance = 0.05

// Candidates lists up to limit open orders the worker may plan, oldest first. An order qualifies
// when all of this holds:
//   - the delivery and its purchase are open, and it is a coordinate delivery;
//   - it has exactly one item line, of a product that is automatic and has a class name now;
//   - it has no attempt, or only attempts proven to have spawned nothing (ABANDONED, UNSTAGED);
//   - the buyer's own logged position matches the delivery coordinates and carries an altitude and
//     its ADM source. The newest such observation is the drop point.
func (r *ShopAutoDeliveryRepository) Candidates(ctx context.Context, org, inst, serverID int64, limit int) ([]ShopAutoCandidate, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, `
SELECT d.id, d.purchase_id, d.player_id, p.class_name, loc.y, loc.source_file, loc.source_offset
  FROM shop_deliveries d
  JOIN shop_purchases sp ON sp.id = d.purchase_id AND sp.installation_id = d.installation_id
  JOIN LATERAL (SELECT MIN(product_id) AS product_id, COUNT(*) AS lines, SUM(quantity) AS units
                  FROM shop_purchase_items WHERE purchase_id = d.purchase_id) it ON TRUE
  JOIN shop_products p ON p.id = it.product_id AND p.installation_id = d.installation_id
  JOIN LATERAL (SELECT e.y, e.source_file, e.source_offset FROM player_location_events e
                 WHERE e.player_id = d.player_id AND e.server_id = $3 AND e.y IS NOT NULL
                   AND e.source_file LIKE '%.ADM' AND e.source_offset > 0
                   AND abs(e.x - d.coord_x) <= $5 AND abs(e.z - d.coord_z) <= $5
                 ORDER BY e.observed_at DESC LIMIT 1) loc ON TRUE
 WHERE d.organization_id = $1 AND d.installation_id = $2 AND d.game_server_id = $3
   AND d.status = 'MANUAL_READY' AND d.delivery_policy = 'MANUAL_COORDINATE' AND sp.status = 'PENDING_FULFILLMENT'
   AND it.lines = 1 AND it.units BETWEEN 1 AND 10
   AND p.auto_delivery AND p.class_name IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM shop_delivery_attempts a WHERE a.delivery_id = d.id AND a.state NOT IN ('ABANDONED','UNSTAGED'))
 ORDER BY d.id LIMIT $4`, org, inst, serverID, limit, dropPointTolerance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShopAutoCandidate
	for rows.Next() {
		var c ShopAutoCandidate
		if err := rows.Scan(&c.DeliveryID, &c.PurchaseID, &c.PlayerID, &c.ClassName, &c.AltitudeY, &c.DropSourceFile, &c.DropSourceOffset); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ShopBootLog is what the server's own RPT says about the time between two boots.
type ShopBootLog struct {
	// CentralEconomySeen: the boot reached the Central Economy (the spawner has run by then).
	CentralEconomySeen bool
	// SpawnerError: the RPT has an object-spawner error naming the Champion file.
	SpawnerError bool
}

// BootLog reads Live Sync's RPT records for the server between from and to (to zero = no upper bound).
func (r *ShopAutoDeliveryRepository) BootLog(ctx context.Context, serverID int64, from, to time.Time) (ShopBootLog, error) {
	var out ShopBootLog
	var until any
	if !to.IsZero() {
		until = to
	}
	err := r.pool.QueryRow(ctx, `
SELECT COALESCE(bool_or(category = 'CENTRAL_ECONOMY'), FALSE),
       COALESCE(bool_or(category = 'OBJECT_SPAWNER_ERROR' AND payload->>'missingFile' LIKE '%champion_shop_delivery.json'), FALSE)
  FROM live_sync_records
 WHERE server_id = $1 AND category IN ('CENTRAL_ECONOMY','OBJECT_SPAWNER_ERROR')
   AND COALESCE(source_utc, detected_at) >= $2 AND ($3::timestamptz IS NULL OR COALESCE(source_utc, detected_at) < $3)`,
		serverID, from, until).Scan(&out.CentralEconomySeen, &out.SpawnerError)
	return out, err
}

// ShopPlayerPosition is a buyer's latest logged position.
type ShopPlayerPosition struct {
	X, Z, AltitudeY float64
	ObservedAt      time.Time
	SourceFile      string
}

// LatestPosition is the player's newest logged position with an altitude in the server's CURRENT
// boot session (ok=false when there is none, or the session has ended).
func (r *ShopAutoDeliveryRepository) LatestPosition(ctx context.Context, serverID, playerID int64) (ShopPlayerPosition, bool, error) {
	var p ShopPlayerPosition
	err := r.pool.QueryRow(ctx, `
SELECT e.x, e.z, e.y, e.observed_at, e.source_file
  FROM player_location_events e JOIN server_adm_sessions s ON s.server_id = e.server_id AND s.adm_file = e.source_file
 WHERE e.server_id = $1 AND e.player_id = $2 AND e.y IS NOT NULL AND e.source_offset > 0 AND s.ended_at IS NULL
 ORDER BY e.observed_at DESC LIMIT 1`, serverID, playerID).Scan(&p.X, &p.Z, &p.AltitudeY, &p.ObservedAt, &p.SourceFile)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

// Ticket origin for tickets the delivery worker opens (migration 0101).
const TicketViaSystem = "SYSTEM"

// ticketSystemOpener is stored as the opener of a SYSTEM ticket whose buyer has no verified Discord
// account. It is not a Discord id, and the Discord side never treats it as one.
const ticketSystemOpener = "system"

// OpenForDelivered opens the buyer's confirmation for an order the delivery worker delivered. The
// purchase is still PENDING_FULFILLMENT at that point (it is fulfilled by the buyer's answer), so
// the trigger of migration 0087 has not opened one. Opening it twice is a no-op.
func (r *ShopConfirmationRepository) OpenForDelivered(ctx context.Context, org, inst, purchaseID int64) error {
	return execShop(ctx, r.pool, `INSERT INTO shop_order_confirmations(purchase_id, organization_id, installation_id, player_id)
SELECT id, organization_id, installation_id, player_id FROM shop_purchases
 WHERE id=$3 AND organization_id=$1 AND installation_id=$2 AND status='PENDING_FULFILLMENT'
ON CONFLICT (purchase_id) DO NOTHING`, org, inst, purchaseID)
}

// OpenSystemTicket opens a ticket for an order the worker could not deliver with certainty, so a
// person decides. If the order already has an open ticket, that one is returned unchanged.
func (r *ShopConfirmationRepository) OpenSystemTicket(ctx context.Context, org, inst, purchaseID int64, reason string) (ShopOrderTicket, error) {
	reason = clipText(strings.TrimSpace(reason), 1000)
	if reason == "" {
		reason = "Automatic delivery needs a person to check this order."
	}
	t, err := scanTicket(r.pool.QueryRow(ctx, `INSERT INTO shop_order_tickets(organization_id, installation_id, purchase_id, player_id, opened_by_discord_id, opened_via, reason)
SELECT sp.organization_id, sp.installation_id, sp.id, sp.player_id,
       COALESCE((SELECT pl.discord_user_id FROM installations i
                   JOIN discord_guild_connections dc ON dc.id = i.discord_guild_connection_id
                   JOIN player_links pl ON pl.guild_id = dc.guild_id AND pl.player_id = sp.player_id AND pl.status = 'VERIFIED'
                  WHERE i.id = sp.installation_id), $5),
       'SYSTEM', $4
  FROM shop_purchases sp WHERE sp.id=$3 AND sp.organization_id=$1 AND sp.installation_id=$2
ON CONFLICT (purchase_id) WHERE status = 'OPEN' DO NOTHING
RETURNING `+ticketCols, org, inst, purchaseID, reason, ticketSystemOpener))
	if !errors.Is(err, ErrShopTicketNotFound) {
		return t, err
	}
	return scanTicket(r.pool.QueryRow(ctx, `SELECT `+ticketCols+` FROM shop_order_tickets
 WHERE purchase_id=$3 AND organization_id=$1 AND installation_id=$2 AND status='OPEN'`, org, inst, purchaseID))
}

// ErrShopProductNotCoordinate: only a coordinate product can be delivered automatically.
var ErrShopProductNotCoordinate = errors.New("only a coordinate-delivery product can be delivered automatically")

// ShopProductAutoDelivery is a product's automatic-delivery setting.
type ShopProductAutoDelivery struct {
	ProductID      int64
	AutoDelivery   bool
	ClassName      string
	DeliveryPolicy string
}

// ProductAutoDelivery reads a product's automatic-delivery setting.
func (r *ShopAutoDeliveryRepository) ProductAutoDelivery(ctx context.Context, org, inst, productID int64) (ShopProductAutoDelivery, error) {
	p := ShopProductAutoDelivery{ProductID: productID}
	err := r.pool.QueryRow(ctx, `SELECT auto_delivery, COALESCE(class_name,''), delivery_policy FROM shop_products
 WHERE id=$3 AND organization_id=$1 AND installation_id=$2`, org, inst, productID).Scan(&p.AutoDelivery, &p.ClassName, &p.DeliveryPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrShopProductNotFound
	}
	return p, err
}

// SetProductAutoDelivery is the owner's per-product switch. className "" keeps no class name (and
// is only valid with auto=false). Switching it on requires a coordinate product.
func (r *ShopAutoDeliveryRepository) SetProductAutoDelivery(ctx context.Context, org, inst, productID int64, auto bool, className string) (ShopProductAutoDelivery, error) {
	p := ShopProductAutoDelivery{ProductID: productID}
	err := r.pool.QueryRow(ctx, `UPDATE shop_products SET auto_delivery=$4, class_name=NULLIF($5,''), updated_at=NOW()
 WHERE id=$3 AND organization_id=$1 AND installation_id=$2 AND (NOT $4 OR delivery_policy='MANUAL_COORDINATE')
 RETURNING auto_delivery, COALESCE(class_name,''), delivery_policy`, org, inst, productID, auto, className).Scan(&p.AutoDelivery, &p.ClassName, &p.DeliveryPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := r.ProductAutoDelivery(ctx, org, inst, productID); gerr != nil {
			return p, gerr
		}
		return p, ErrShopProductNotCoordinate
	}
	return p, err
}

// AttemptsForPurchase lists every delivery attempt of one purchase, oldest first.
func (r *ShopAttemptRepository) AttemptsForPurchase(ctx context.Context, org, inst, purchaseID int64) ([]ShopAttempt, error) {
	return r.list(ctx, `SELECT `+prefixed("a", attemptCols)+` FROM shop_delivery_attempts a JOIN shop_deliveries sd ON sd.id = a.delivery_id
 WHERE sd.purchase_id=$3 AND a.organization_id=$1 AND a.installation_id=$2 ORDER BY a.id`, org, inst, purchaseID)
}

// prefixed qualifies a comma-separated column list with a table alias.
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
