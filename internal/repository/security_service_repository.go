package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SecurityServiceRepository sells Security Marketplace services for Champion
// Points. Only the Base Raid Alarm can be offered. A purchase debits the
// existing point ledger (TxSecurityPurchase) and records the paid time in the
// same transaction; it never renews by itself.
type SecurityServiceRepository struct{ pool *pgxpool.Pool }

func NewSecurityServiceRepository(pool *pgxpool.Pool) *SecurityServiceRepository {
	return &SecurityServiceRepository{pool: pool}
}

const (
	ServiceBaseRaidAlarm = "BASE_RAID_ALARM"
	TxSecurityPurchase   = "SECURITY_PURCHASE"

	SecurityMaxPricePoints  = 1_000_000_000
	SecurityMaxDurationDays = 90
)

var (
	ErrSecurityOfferUnavailable = errors.New("this service is not for sale on this server")
	ErrSecurityNoVerifiedPlayer = errors.New("link and verify your DayZ account first")
	ErrSecurityInvalidRequest   = errors.New("invalid security purchase")
)

type SecurityScope struct{ InstallationID, GuildID, ServerID int64 }

func (s SecurityScope) valid() bool { return s.InstallationID > 0 && s.GuildID > 0 && s.ServerID > 0 }

type SecurityOffer struct {
	ServiceID    string     `json:"serviceId"`
	Enabled      bool       `json:"enabled"`
	PricePoints  int64      `json:"pricePoints"`
	DurationDays int        `json:"durationDays"`
	Configured   bool       `json:"configured"`
	UpdatedAt    *time.Time `json:"updatedAt,omitempty"`
}

type SecurityPurchase struct {
	ID           int64     `json:"id"`
	ServiceID    string    `json:"serviceId"`
	PlayerID     int64     `json:"playerId"`
	PlayerName   string    `json:"playerName,omitempty"`
	PricePoints  int64     `json:"pricePoints"`
	DurationDays int       `json:"durationDays"`
	StartsAt     time.Time `json:"startsAt"`
	EndsAt       time.Time `json:"endsAt"`
	CreatedAt    time.Time `json:"createdAt"`
}

type SecurityPurchaseResult struct {
	Purchase     SecurityPurchase
	BalanceAfter int64
	Duplicate    bool
}

func validSecurityService(id string) bool { return id == ServiceBaseRaidAlarm }

// GetOffer returns the owner's offer. No row means not for sale.
func (r *SecurityServiceRepository) GetOffer(ctx context.Context, s SecurityScope, serviceID string) (SecurityOffer, error) {
	out := SecurityOffer{ServiceID: serviceID, PricePoints: 1000, DurationDays: 7}
	if r == nil || r.pool == nil || !s.valid() || !validSecurityService(serviceID) {
		return out, ErrSecurityInvalidRequest
	}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `SELECT enabled,price_points,duration_days,updated_at FROM security_service_offers
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND service_id=$4`, s.InstallationID, s.GuildID, s.ServerID, serviceID).
		Scan(&out.Enabled, &out.PricePoints, &out.DurationDays, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Configured, out.UpdatedAt = true, &updated
	return out, nil
}

// SetOffer stores the owner's offer; the foreign keys reject a mismatched scope.
func (r *SecurityServiceRepository) SetOffer(ctx context.Context, s SecurityScope, serviceID string, enabled bool, price int64, days int, actorUserID *int64) (SecurityOffer, error) {
	if r == nil || r.pool == nil || !s.valid() || !validSecurityService(serviceID) ||
		price < 1 || price > SecurityMaxPricePoints || days < 1 || days > SecurityMaxDurationDays {
		return SecurityOffer{}, ErrSecurityInvalidRequest
	}
	out := SecurityOffer{ServiceID: serviceID, Configured: true}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `INSERT INTO security_service_offers
 (installation_id,guild_id,server_id,service_id,enabled,price_points,duration_days,updated_by_user_id,updated_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NOW())
 ON CONFLICT (installation_id,server_id,service_id) DO UPDATE SET enabled=EXCLUDED.enabled,
  price_points=EXCLUDED.price_points,duration_days=EXCLUDED.duration_days,
  updated_by_user_id=EXCLUDED.updated_by_user_id,updated_at=NOW()
 WHERE security_service_offers.guild_id=EXCLUDED.guild_id
 RETURNING enabled,price_points,duration_days,updated_at`,
		s.InstallationID, s.GuildID, s.ServerID, serviceID, enabled, price, days, actorUserID).
		Scan(&out.Enabled, &out.PricePoints, &out.DurationDays, &updated)
	if err != nil {
		return SecurityOffer{}, err
	}
	out.UpdatedAt = &updated
	return out, nil
}

// Purchase charges the player the current offer price and adds the offer's
// days to their paid time (starting now, or when their current time ends).
// requestKey makes it idempotent: a replay returns the original purchase and
// charges nothing. The debit, the purchase row and the price check happen in
// one transaction.
func (r *SecurityServiceRepository) Purchase(ctx context.Context, s SecurityScope, playerID int64, serviceID, requestKey string) (SecurityPurchaseResult, error) {
	var out SecurityPurchaseResult
	if r == nil || r.pool == nil || !s.valid() || playerID <= 0 || !validSecurityService(serviceID) ||
		len(requestKey) < 8 || len(requestKey) > 80 {
		return out, ErrSecurityInvalidRequest
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Serialize this player's purchases of this service on this installation,
	// so two clicks can't both start from the same end time.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('security_purchase:'||$1::BIGINT::TEXT||':'||$2::BIGINT::TEXT||':'||$3::TEXT,0))`,
		s.InstallationID, playerID, serviceID); err != nil {
		return out, err
	}
	if p, found, err := findSecurityPurchase(ctx, tx, s.InstallationID, playerID, requestKey); err != nil {
		return out, err
	} else if found {
		out.Purchase, out.Duplicate = p, true
		_ = tx.QueryRow(ctx, `SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2`, s.GuildID, playerID).Scan(&out.BalanceAfter)
		return out, tx.Commit(ctx)
	}
	var price int64
	var days int
	err = tx.QueryRow(ctx, `SELECT price_points,duration_days FROM security_service_offers
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND service_id=$4 AND enabled FOR SHARE`,
		s.InstallationID, s.GuildID, s.ServerID, serviceID).Scan(&price, &days)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrSecurityOfferUnavailable
	}
	if err != nil {
		return out, err
	}
	entry, err := applyLedger(ctx, tx, LedgerParams{GuildID: s.GuildID, PlayerID: playerID, ServerID: s.ServerID,
		Type: TxSecurityPurchase, Amount: price, CreatedBy: "SYSTEM",
		ReferenceID: "security:" + strconv.FormatInt(s.InstallationID, 10) + ":" + requestKey,
		Description: fmt.Sprintf("Base Raid Alarm · %d days", days)}, true)
	if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `WITH current AS (
  SELECT MAX(ends_at) AS ends FROM security_service_purchases
  WHERE installation_id=$1 AND player_id=$5 AND service_id=$4 AND ends_at>NOW()
 ), start AS (SELECT GREATEST(NOW(),COALESCE((SELECT ends FROM current),NOW())) AS at)
 INSERT INTO security_service_purchases
 (installation_id,guild_id,server_id,service_id,player_id,price_points,duration_days,starts_at,ends_at,ledger_entry_id,request_key)
 SELECT $1,$2,$3,$4,$5,$6,$7,start.at,start.at+make_interval(days=>$7),$8,$9 FROM start
 RETURNING id,service_id,player_id,price_points,duration_days,starts_at,ends_at,created_at`,
		s.InstallationID, s.GuildID, s.ServerID, serviceID, playerID, price, days, entry.ID, requestKey).
		Scan(&out.Purchase.ID, &out.Purchase.ServiceID, &out.Purchase.PlayerID, &out.Purchase.PricePoints,
			&out.Purchase.DurationDays, &out.Purchase.StartsAt, &out.Purchase.EndsAt, &out.Purchase.CreatedAt)
	if err != nil {
		return out, err
	}
	out.BalanceAfter = entry.BalanceAfter
	return out, tx.Commit(ctx)
}

func findSecurityPurchase(ctx context.Context, q querier, installationID, playerID int64, requestKey string) (SecurityPurchase, bool, error) {
	var p SecurityPurchase
	err := q.QueryRow(ctx, `SELECT id,service_id,player_id,price_points,duration_days,starts_at,ends_at,created_at
 FROM security_service_purchases WHERE installation_id=$1 AND player_id=$2 AND request_key=$3`, installationID, playerID, requestKey).
		Scan(&p.ID, &p.ServiceID, &p.PlayerID, &p.PricePoints, &p.DurationDays, &p.StartsAt, &p.EndsAt, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

// ActiveUntil returns when the player's paid time (including stacked
// purchases) ends, or nil when none is active.
func (r *SecurityServiceRepository) ActiveUntil(ctx context.Context, installationID, playerID int64, serviceID string) (*time.Time, error) {
	if r == nil || r.pool == nil || installationID <= 0 || playerID <= 0 {
		return nil, ErrSecurityInvalidRequest
	}
	var until *time.Time
	// Stacked purchases chain end to start, so the latest end is when the
	// player's paid time runs out, as long as one of them has started.
	err := r.pool.QueryRow(ctx, `SELECT CASE WHEN bool_or(starts_at<=NOW()) THEN MAX(ends_at) END FROM security_service_purchases
 WHERE installation_id=$1 AND player_id=$2 AND service_id=$3 AND ends_at>NOW()`,
		installationID, playerID, serviceID).Scan(&until)
	return until, err
}

// RecentSales lists the newest purchases on one server, for the owner.
func (r *SecurityServiceRepository) RecentSales(ctx context.Context, s SecurityScope, limit int) ([]SecurityPurchase, int, error) {
	if r == nil || r.pool == nil || !s.valid() {
		return nil, 0, ErrSecurityInvalidRequest
	}
	if limit < 1 || limit > 50 {
		limit = 10
	}
	var active int
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(DISTINCT player_id) FROM security_service_purchases
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND starts_at<=NOW() AND ends_at>NOW()`,
		s.InstallationID, s.GuildID, s.ServerID).Scan(&active); err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT p.id,p.service_id,p.player_id,COALESCE(pl.display_name,''),p.price_points,p.duration_days,
  p.starts_at,p.ends_at,p.created_at
 FROM security_service_purchases p LEFT JOIN players pl ON pl.guild_id=p.guild_id AND pl.id=p.player_id
 WHERE p.installation_id=$1 AND p.guild_id=$2 AND p.server_id=$3
 ORDER BY p.created_at DESC,p.id DESC LIMIT $4`, s.InstallationID, s.GuildID, s.ServerID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]SecurityPurchase, 0)
	for rows.Next() {
		var p SecurityPurchase
		if err := rows.Scan(&p.ID, &p.ServiceID, &p.PlayerID, &p.PlayerName, &p.PricePoints, &p.DurationDays, &p.StartsAt, &p.EndsAt, &p.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, p)
	}
	return out, active, rows.Err()
}

// SecurityExpiry is a paid period that ended with nothing after it.
type SecurityExpiry struct {
	PurchaseID     int64
	PlayerID       int64
	DiscordUserID  string
	ServiceID      string
	ServerID       int64
	InstallationID int64
}

// DueExpiries lists ended purchases not yet announced, where the player has no
// later paid time. Purchases followed by more paid time are marked without a DM.
func (r *SecurityServiceRepository) DueExpiries(ctx context.Context, limit int) ([]SecurityExpiry, error) {
	if r == nil || r.pool == nil {
		return nil, ErrSecurityInvalidRequest
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	if _, err := r.pool.Exec(ctx, `UPDATE security_service_purchases p SET expiry_notified_at=NOW()
 WHERE p.expiry_notified_at IS NULL AND p.ends_at<=NOW() AND EXISTS (
  SELECT 1 FROM security_service_purchases q WHERE q.installation_id=p.installation_id AND q.player_id=p.player_id
   AND q.service_id=p.service_id AND q.ends_at>NOW())`); err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT p.id,p.player_id,COALESCE(l.discord_user_id,''),p.service_id,p.server_id,p.installation_id
 FROM security_service_purchases p
 LEFT JOIN player_links l ON l.guild_id=p.guild_id AND l.player_id=p.player_id AND l.status='VERIFIED'
 WHERE p.expiry_notified_at IS NULL AND p.ends_at<=NOW()
 ORDER BY p.ends_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecurityExpiry
	for rows.Next() {
		var e SecurityExpiry
		if err := rows.Scan(&e.PurchaseID, &e.PlayerID, &e.DiscordUserID, &e.ServiceID, &e.ServerID, &e.InstallationID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkExpiryNotified records that the expiry was handled (DM sent or not possible).
func (r *SecurityServiceRepository) MarkExpiryNotified(ctx context.Context, purchaseID int64) error {
	if r == nil || r.pool == nil || purchaseID <= 0 {
		return ErrSecurityInvalidRequest
	}
	_, err := r.pool.Exec(ctx, `UPDATE security_service_purchases SET expiry_notified_at=NOW() WHERE id=$1 AND expiry_notified_at IS NULL`, purchaseID)
	return err
}
