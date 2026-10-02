package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/perkstore"
)

// The perk store (docs/PERK_STORE.md). Players pay Champion Points; the points go to the server
// owner's own balance. Every charge and refund runs through the point ledger in the same
// transaction as the purchase row, so a player is never charged without holding the purchase.

// Ledger types written only here.
const (
	TxPerkPurchase   = "PERK_PURCHASE"    // debit: a player bought or renewed an offer
	TxPerkRefund     = "PERK_REFUND"      // credit: that payment was given back
	TxPerkSale       = "PERK_SALE"        // credit: the owner received a player's payment
	TxPerkSaleRefund = "PERK_SALE_REFUND" // debit: the owner gave a refunded payment back
)

const (
	PerkActive   = "ACTIVE"
	PerkEnded    = "ENDED"
	PerkRefunded = "REFUNDED"
)

var (
	ErrPerkStoreClosed       = errors.New("the perk store is closed")
	ErrPerkOfferNotFound     = errors.New("offer not found")
	ErrPerkOfferLimit        = fmt.Errorf("a server can have up to %d offers", perkstore.MaxOffers)
	ErrPerkTierNotFound      = errors.New("supporter tier not found")
	ErrPerkNotGiftable       = errors.New("this offer cannot be gifted")
	ErrPerkRecipientNotFound = errors.New("that player was not found on this server")
	ErrPerkAlreadyActive     = errors.New("this player already has this offer")
	ErrPerkTierConflict      = errors.New("this player already holds a different supporter tier")
	ErrPerkPurchaseNotFound  = errors.New("purchase not found")
	ErrPerkNotRefundable     = errors.New("there is no payment left to refund on this purchase")
	ErrPerkOwnerCannotRefund = errors.New("the owner's balance cannot cover this refund")
	ErrPerkInvalidRequest    = errors.New("invalid perk store request")
)

// PerkUnavailableError says why an offer cannot be bought (a perkstore.Unavailable* reason).
type PerkUnavailableError struct{ Reason string }

func (e *PerkUnavailableError) Error() string { return perkstore.UnavailableMessage(e.Reason) }

// PerkScope is where a purchase happens.
type PerkScope struct{ GuildID, InstallationID, ServerID int64 }

type PerkSettings struct {
	Enabled   bool `json:"enabled"`
	Shoutouts bool `json:"shoutouts"`
}

// PerkPurchase is one purchase with the names the hubs show.
type PerkPurchase struct {
	ID                int64      `json:"id"`
	OfferID           int64      `json:"offerId"`
	OfferName         string     `json:"offerName"`
	BuyerPlayerID     int64      `json:"buyerPlayerId"`
	BuyerName         string     `json:"buyerName"`
	RecipientPlayerID int64      `json:"recipientPlayerId"`
	RecipientName     string     `json:"recipientName"`
	Gift              bool       `json:"gift"`
	PricePoints       int64      `json:"pricePoints"`
	Billing           string     `json:"billing"`
	DurationDays      int        `json:"durationDays"`
	VIPTierID         *int64     `json:"vipTierId"`
	VIPTierName       string     `json:"vipTierName,omitempty"`
	PriorityQueue     bool       `json:"priorityQueue"`
	CustomPerk        string     `json:"customPerk"`
	Status            string     `json:"status"`
	EndReason         string     `json:"endReason,omitempty"`
	AutoRenew         bool       `json:"autoRenew"`
	Renewals          int        `json:"renewals"`
	StartedAt         time.Time  `json:"startedAt"`
	ExpiresAt         *time.Time `json:"expiresAt"`
	EndedAt           *time.Time `json:"endedAt,omitempty"`
	PerksApplied      bool       `json:"perksApplied"`
	PerksError        string     `json:"perksError,omitempty"`
	CustomDone        bool       `json:"customDone"`
	CreatedAt         time.Time  `json:"createdAt"`

	GuildID        int64  `json:"-"`
	InstallationID int64  `json:"-"`
	ServerID       int64  `json:"-"`
	VIPMemberID    *int64 `json:"-"`
	PriorityName   string `json:"-"`
	PerkAttempts   int    `json:"-"`
	PerksRemoved   bool   `json:"-"`
}

// PerkPurchaseResult is what a purchase returns. Duplicate reports a replay of the same request
// key: nothing was charged again.
type PerkPurchaseResult struct {
	Purchase     PerkPurchase
	BalanceAfter int64
	Duplicate    bool
}

// PerkSupporter is one line of the top-supporters board.
type PerkSupporter struct {
	PlayerID   int64  `json:"-"`
	PlayerName string `json:"playerName"`
	Points     int64  `json:"points"`
	Purchases  int    `json:"purchases"`
}

// PerkStats is the owner's summary.
type PerkStats struct {
	ActivePurchases int   `json:"activePurchases"`
	Subscribers     int   `json:"subscribers"`
	Points30Days    int64 `json:"points30Days"`
	PointsAllTime   int64 `json:"pointsAllTime"`
}

type PerkStoreRepository struct{ pool *pgxpool.Pool }

func NewPerkStoreRepository(pool *pgxpool.Pool) *PerkStoreRepository {
	return &PerkStoreRepository{pool: pool}
}

// --- settings ---------------------------------------------------------------------------------------

// Settings returns the store switches; a server that never saved them has the store closed.
func (r *PerkStoreRepository) Settings(ctx context.Context, guildID int64) (PerkSettings, error) {
	s := PerkSettings{Shoutouts: true}
	err := r.pool.QueryRow(ctx, `SELECT enabled,shoutouts FROM perk_store_settings WHERE guild_id=$1`, guildID).Scan(&s.Enabled, &s.Shoutouts)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, nil
	}
	return s, err
}

func (r *PerkStoreRepository) SaveSettings(ctx context.Context, guildID int64, s PerkSettings) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO perk_store_settings(guild_id,enabled,shoutouts) VALUES($1,$2,$3)
ON CONFLICT(guild_id) DO UPDATE SET enabled=EXCLUDED.enabled,shoutouts=EXCLUDED.shoutouts,updated_at=NOW()`, guildID, s.Enabled, s.Shoutouts)
	return err
}

// --- offers -----------------------------------------------------------------------------------------

const perkOfferCols = `o.id,o.name,o.description,o.price_points,o.billing,o.duration_days,o.vip_tier_id,COALESCE(t.name,''),o.priority_queue,o.custom_perk,
o.giftable,o.available_from,o.available_until,o.stock_limit,o.sold_count,o.enabled,o.sort_order`

const perkOfferFrom = ` FROM perk_offers o LEFT JOIN vip_tiers t ON t.id=o.vip_tier_id`

func scanPerkOffer(row pgx.Row) (perkstore.Offer, error) {
	var o perkstore.Offer
	err := row.Scan(&o.ID, &o.Name, &o.Description, &o.PricePoints, &o.Billing, &o.DurationDays, &o.VIPTierID, &o.VIPTierName, &o.PriorityQueue, &o.CustomPerk,
		&o.Giftable, &o.AvailableFrom, &o.AvailableUntil, &o.StockLimit, &o.SoldCount, &o.Enabled, &o.SortOrder)
	return o, err
}

// ListOffers returns the server's offers in display order; archived ones are never listed.
func (r *PerkStoreRepository) ListOffers(ctx context.Context, guildID int64) ([]perkstore.Offer, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+perkOfferCols+perkOfferFrom+` WHERE o.guild_id=$1 AND o.archived_at IS NULL ORDER BY o.sort_order,o.id`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []perkstore.Offer{}
	for rows.Next() {
		o, err := scanPerkOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SaveOffer creates the offer (ID 0) or updates it. The offer must already be normalized.
func (r *PerkStoreRepository) SaveOffer(ctx context.Context, guildID int64, o perkstore.Offer) (perkstore.Offer, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return o, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if o.VIPTierID != nil {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM vip_tiers WHERE guild_id=$1 AND id=$2)`, guildID, *o.VIPTierID).Scan(&ok); err != nil {
			return o, err
		}
		if !ok {
			return o, ErrPerkTierNotFound
		}
	}
	id := o.ID
	if id == 0 {
		var n int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM perk_offers WHERE guild_id=$1 AND archived_at IS NULL`, guildID).Scan(&n); err != nil {
			return o, err
		}
		if n >= perkstore.MaxOffers {
			return o, ErrPerkOfferLimit
		}
		err = tx.QueryRow(ctx, `INSERT INTO perk_offers(guild_id,name,description,price_points,billing,duration_days,vip_tier_id,priority_queue,custom_perk,giftable,available_from,available_until,stock_limit,enabled,sort_order)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
			guildID, o.Name, o.Description, o.PricePoints, o.Billing, o.DurationDays, o.VIPTierID, o.PriorityQueue, o.CustomPerk, o.Giftable, o.AvailableFrom, o.AvailableUntil, o.StockLimit, o.Enabled, o.SortOrder).Scan(&id)
		if err != nil {
			return o, err
		}
	} else {
		tag, err := tx.Exec(ctx, `UPDATE perk_offers SET name=$3,description=$4,price_points=$5,billing=$6,duration_days=$7,vip_tier_id=$8,priority_queue=$9,custom_perk=$10,
giftable=$11,available_from=$12,available_until=$13,stock_limit=$14,enabled=$15,sort_order=$16,updated_at=NOW()
WHERE guild_id=$1 AND id=$2 AND archived_at IS NULL`,
			guildID, id, o.Name, o.Description, o.PricePoints, o.Billing, o.DurationDays, o.VIPTierID, o.PriorityQueue, o.CustomPerk, o.Giftable, o.AvailableFrom, o.AvailableUntil, o.StockLimit, o.Enabled, o.SortOrder)
		if err != nil {
			return o, err
		}
		if tag.RowsAffected() == 0 {
			return o, ErrPerkOfferNotFound
		}
	}
	saved, err := scanPerkOffer(tx.QueryRow(ctx, `SELECT `+perkOfferCols+perkOfferFrom+` WHERE o.id=$1`, id))
	if err != nil {
		return o, err
	}
	return saved, tx.Commit(ctx)
}

// ArchiveOffer takes an offer off sale for good. What players already hold stays theirs; monthly
// purchases of it end when their paid time runs out.
func (r *PerkStoreRepository) ArchiveOffer(ctx context.Context, guildID, offerID int64) error {
	tag, err := r.pool.Exec(ctx, `UPDATE perk_offers SET archived_at=NOW(),enabled=FALSE WHERE guild_id=$1 AND id=$2 AND archived_at IS NULL`, guildID, offerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrPerkOfferNotFound
	}
	return nil
}

// --- purchases --------------------------------------------------------------------------------------

const perkPurchaseCols = `p.id,p.offer_id,p.offer_name,p.buyer_player_id,COALESCE(b.display_name,''),p.recipient_player_id,COALESCE(rp.display_name,''),
p.price_points,p.billing,p.duration_days,p.vip_tier_id,COALESCE(t.name,''),p.priority_queue,p.custom_perk,p.status,p.end_reason,p.auto_renew,p.renewals,
p.started_at,p.expires_at,p.ended_at,p.perks_applied_at IS NOT NULL,p.perks_error,p.custom_done_at IS NOT NULL,p.created_at,
p.guild_id,p.installation_id,p.server_id,p.vip_member_id,p.priority_name,p.perk_attempts,p.perks_removed_at IS NOT NULL`

const perkPurchaseFrom = ` FROM perk_purchases p LEFT JOIN players b ON b.id=p.buyer_player_id LEFT JOIN players rp ON rp.id=p.recipient_player_id LEFT JOIN vip_tiers t ON t.id=p.vip_tier_id`

func scanPerkPurchase(row pgx.Row) (PerkPurchase, error) {
	var p PerkPurchase
	err := row.Scan(&p.ID, &p.OfferID, &p.OfferName, &p.BuyerPlayerID, &p.BuyerName, &p.RecipientPlayerID, &p.RecipientName,
		&p.PricePoints, &p.Billing, &p.DurationDays, &p.VIPTierID, &p.VIPTierName, &p.PriorityQueue, &p.CustomPerk, &p.Status, &p.EndReason, &p.AutoRenew, &p.Renewals,
		&p.StartedAt, &p.ExpiresAt, &p.EndedAt, &p.PerksApplied, &p.PerksError, &p.CustomDone, &p.CreatedAt,
		&p.GuildID, &p.InstallationID, &p.ServerID, &p.VIPMemberID, &p.PriorityName, &p.PerkAttempts, &p.PerksRemoved)
	p.Gift = p.BuyerPlayerID != p.RecipientPlayerID
	return p, err
}

func (r *PerkStoreRepository) collectPurchases(rows pgx.Rows, err error) ([]PerkPurchase, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PerkPurchase{}
	for rows.Next() {
		p, err := scanPerkPurchase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPurchase returns one purchase of the guild.
func (r *PerkStoreRepository) GetPurchase(ctx context.Context, guildID, purchaseID int64) (PerkPurchase, error) {
	p, err := scanPerkPurchase(r.pool.QueryRow(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.guild_id=$1 AND p.id=$2`, guildID, purchaseID))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrPerkPurchaseNotFound
	}
	return p, err
}

// ListPurchases returns the guild's purchases: active first, then the most recent.
func (r *PerkStoreRepository) ListPurchases(ctx context.Context, guildID int64, limit int) ([]PerkPurchase, error) {
	return r.collectPurchases(r.pool.Query(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.guild_id=$1
ORDER BY (p.status='ACTIVE') DESC, p.created_at DESC, p.id DESC LIMIT $2`, guildID, clampLimit(limit, 100, 500)))
}

// ListForPlayer returns what a player bought or was given.
func (r *PerkStoreRepository) ListForPlayer(ctx context.Context, guildID, playerID int64, limit int) ([]PerkPurchase, error) {
	return r.collectPurchases(r.pool.Query(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.guild_id=$1 AND (p.buyer_player_id=$2 OR p.recipient_player_id=$2)
ORDER BY (p.status='ACTIVE') DESC, p.created_at DESC, p.id DESC LIMIT $3`, guildID, playerID, clampLimit(limit, 50, 200)))
}

// ownerPlayer is the in-game character of the organization's owner in this guild: who receives
// what players pay. ok=false when the owner has no verified link there.
func ownerPlayer(ctx context.Context, tx pgx.Tx, s PerkScope) (int64, bool, error) {
	var id int64
	err := tx.QueryRow(ctx, `SELECT pl.player_id FROM installations i
JOIN organizations o ON o.id=i.organization_id
JOIN app_users u ON u.id=o.owner_user_id
JOIN player_links pl ON pl.guild_id=$2 AND pl.discord_user_id=u.discord_user_id AND pl.status='VERIFIED'
WHERE i.id=$1 LIMIT 1`, s.InstallationID, s.GuildID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// OwnerLinked reports whether the owner has a character to receive points on.
func (r *PerkStoreRepository) OwnerLinked(ctx context.Context, s PerkScope) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	_, ok, err := ownerPlayer(ctx, tx, s)
	return ok, err
}

// charge debits the buyer, credits the owner (when they have a character) and records the charge.
func charge(ctx context.Context, tx pgx.Tx, s PerkScope, purchaseID, buyerID, amount int64, kind, ref, description string) (LedgerEntry, error) {
	entry, err := applyLedger(ctx, tx, LedgerParams{GuildID: s.GuildID, PlayerID: buyerID, ServerID: s.ServerID, Type: TxPerkPurchase,
		Amount: amount, CreatedBy: "SYSTEM", ReferenceID: ref, Description: description}, true)
	if err != nil {
		return entry, err
	}
	var owner *int64
	if id, ok, err := ownerPlayer(ctx, tx, s); err != nil {
		return entry, err
	} else if ok {
		if _, err := applyLedger(ctx, tx, LedgerParams{GuildID: s.GuildID, PlayerID: id, ServerID: s.ServerID, Type: TxPerkSale,
			Amount: amount, CreatedBy: "SYSTEM", ReferenceID: ref, Description: description}, false); err != nil {
			return entry, err
		}
		owner = &id
		if id == buyerID {
			// The owner bought from their own store: the balance the caller reports is the one
			// after the points came back.
			if err := tx.QueryRow(ctx, `SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2`, s.GuildID, buyerID).Scan(&entry.BalanceAfter); err != nil {
				return entry, err
			}
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO perk_charges(guild_id,purchase_id,buyer_player_id,owner_player_id,amount,kind) VALUES($1,$2,$3,$4,$5,$6)`,
		s.GuildID, purchaseID, buyerID, owner, amount, kind)
	return entry, err
}

// Purchase charges buyer the offer's current price and records the purchase for recipient (the
// buyer, or another player when it is a gift). requestKey makes it idempotent. The price check,
// the stock, the debit and the purchase row are one transaction.
func (r *PerkStoreRepository) Purchase(ctx context.Context, s PerkScope, buyerID, recipientID, offerID int64, requestKey string, now time.Time) (PerkPurchaseResult, error) {
	var out PerkPurchaseResult
	if r == nil || r.pool == nil || s.GuildID <= 0 || s.InstallationID <= 0 || buyerID <= 0 || recipientID <= 0 || offerID <= 0 || len(requestKey) < 8 || len(requestKey) > 80 {
		return out, ErrPerkInvalidRequest
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// One purchase of an offer at a time, so limited stock can never be oversold.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('perk_offer:'||$1::BIGINT::TEXT,0))`, offerID); err != nil {
		return out, err
	}
	var existing int64
	err = tx.QueryRow(ctx, `SELECT id FROM perk_purchases WHERE guild_id=$1 AND buyer_player_id=$2 AND request_key=$3`, s.GuildID, buyerID, requestKey).Scan(&existing)
	if err == nil {
		p, err := scanPerkPurchase(tx.QueryRow(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.id=$1`, existing))
		if err != nil {
			return out, err
		}
		out.Purchase, out.Duplicate = p, true
		_ = tx.QueryRow(ctx, `SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2`, s.GuildID, buyerID).Scan(&out.BalanceAfter)
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}

	var open bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT enabled FROM perk_store_settings WHERE guild_id=$1),FALSE)`, s.GuildID).Scan(&open); err != nil {
		return out, err
	}
	if !open {
		return out, ErrPerkStoreClosed
	}
	offer, err := scanPerkOffer(tx.QueryRow(ctx, `SELECT `+perkOfferCols+perkOfferFrom+` WHERE o.guild_id=$1 AND o.id=$2 AND o.archived_at IS NULL FOR UPDATE OF o`, s.GuildID, offerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrPerkOfferNotFound
	}
	if err != nil {
		return out, err
	}
	if reason := offer.Unavailable(now); reason != "" {
		return out, &PerkUnavailableError{Reason: reason}
	}
	gift := recipientID != buyerID
	if gift && !offer.Giftable {
		return out, ErrPerkNotGiftable
	}
	var recipientName string
	err = tx.QueryRow(ctx, `SELECT display_name FROM players WHERE guild_id=$1 AND id=$2`, s.GuildID, recipientID).Scan(&recipientName)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrPerkRecipientNotFound
	}
	if err != nil {
		return out, err
	}
	var holds bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM perk_purchases WHERE guild_id=$1 AND recipient_player_id=$2 AND offer_id=$3 AND status='ACTIVE')`, s.GuildID, recipientID, offerID).Scan(&holds); err != nil {
		return out, err
	}
	if holds {
		return out, ErrPerkAlreadyActive
	}
	if offer.VIPTierID != nil {
		var other bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM vip_members WHERE guild_id=$1 AND player_id=$2 AND tier_id<>$3 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>$4))`,
			s.GuildID, recipientID, *offer.VIPTierID, now).Scan(&other); err != nil {
			return out, err
		}
		if other {
			return out, ErrPerkTierConflict
		}
	}

	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO perk_purchases(guild_id,installation_id,server_id,offer_id,offer_name,buyer_player_id,recipient_player_id,price_points,billing,duration_days,
vip_tier_id,priority_queue,custom_perk,auto_renew,started_at,expires_at,request_key)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id`,
		s.GuildID, s.InstallationID, s.ServerID, offer.ID, offer.Name, buyerID, recipientID, offer.PricePoints, offer.Billing, offer.DurationDays,
		offer.VIPTierID, offer.PriorityQueue, offer.CustomPerk, offer.Billing == perkstore.BillingMonthly, now,
		perkstore.Expiry(offer.Billing, offer.DurationDays, now), requestKey).Scan(&id)
	if err != nil {
		return out, err
	}
	description := offer.Name
	if gift {
		description += " (gift for " + recipientName + ")"
	}
	entry, err := charge(ctx, tx, s, id, buyerID, offer.PricePoints, "PURCHASE", "perk:"+strconv.FormatInt(id, 10), description)
	if err != nil {
		return out, err
	}
	if _, err := tx.Exec(ctx, `UPDATE perk_offers SET sold_count=sold_count+1 WHERE id=$1`, offer.ID); err != nil {
		return out, err
	}
	out.Purchase, err = scanPerkPurchase(tx.QueryRow(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.id=$1`, id))
	if err != nil {
		return out, err
	}
	out.BalanceAfter = entry.BalanceAfter
	return out, tx.Commit(ctx)
}

// Renew charges a monthly purchase that ran out for its next period and returns it with the new
// end. ErrInsufficientFunds and a PerkUnavailableError (the offer is gone or switched off) mean it
// was not renewed and the caller should end it.
func (r *PerkStoreRepository) Renew(ctx context.Context, guildID, purchaseID int64, now time.Time) (PerkPurchase, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return PerkPurchase{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var price int64
	var buyer, installation, server int64
	var renewals int
	var expires time.Time
	var name string
	var onSale bool
	err = tx.QueryRow(ctx, `SELECT p.price_points,p.buyer_player_id,p.installation_id,p.server_id,p.renewals,p.expires_at,p.offer_name,
(o.archived_at IS NULL AND o.enabled)
FROM perk_purchases p JOIN perk_offers o ON o.id=p.offer_id
WHERE p.guild_id=$1 AND p.id=$2 AND p.status='ACTIVE' AND p.billing='MONTHLY' AND p.auto_renew AND p.expires_at IS NOT NULL AND p.expires_at<=$3 FOR UPDATE OF p`,
		guildID, purchaseID, now).Scan(&price, &buyer, &installation, &server, &renewals, &expires, &name, &onSale)
	if errors.Is(err, pgx.ErrNoRows) {
		return PerkPurchase{}, ErrPerkPurchaseNotFound
	}
	if err != nil {
		return PerkPurchase{}, err
	}
	if !onSale {
		return PerkPurchase{}, &PerkUnavailableError{Reason: perkstore.UnavailableDisabled}
	}
	s := PerkScope{GuildID: guildID, InstallationID: installation, ServerID: server}
	ref := "perk:" + strconv.FormatInt(purchaseID, 10) + ":renew:" + strconv.Itoa(renewals+1)
	if _, err := charge(ctx, tx, s, purchaseID, buyer, price, "RENEWAL", ref, name+" (monthly renewal)"); err != nil {
		return PerkPurchase{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE perk_purchases SET expires_at=$2,renewals=renewals+1 WHERE id=$1`, purchaseID, perkstore.NextRenewal(expires, now)); err != nil {
		return PerkPurchase{}, err
	}
	p, err := scanPerkPurchase(tx.QueryRow(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.id=$1`, purchaseID))
	if err != nil {
		return PerkPurchase{}, err
	}
	return p, tx.Commit(ctx)
}

// End closes an active purchase without giving points back.
func (r *PerkStoreRepository) End(ctx context.Context, guildID, purchaseID int64, reason string, now time.Time) (PerkPurchase, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE perk_purchases SET status='ENDED',end_reason=$3,ended_at=$4,auto_renew=FALSE WHERE guild_id=$1 AND id=$2 AND status='ACTIVE'`, guildID, purchaseID, reason, now)
	if err != nil {
		return PerkPurchase{}, err
	}
	if tag.RowsAffected() == 0 {
		return PerkPurchase{}, ErrPerkPurchaseNotFound
	}
	return r.GetPurchase(ctx, guildID, purchaseID)
}

// SetAutoRenew turns a monthly purchase's renewal on or off. Only the buyer pays, so only the
// buyer may change it (buyerID 0 = staff).
func (r *PerkStoreRepository) SetAutoRenew(ctx context.Context, guildID, purchaseID, buyerID int64, on bool) (PerkPurchase, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE perk_purchases SET auto_renew=$4 WHERE guild_id=$1 AND id=$2 AND status='ACTIVE' AND billing='MONTHLY' AND ($3=0 OR buyer_player_id=$3)`,
		guildID, purchaseID, buyerID, on)
	if err != nil {
		return PerkPurchase{}, err
	}
	if tag.RowsAffected() == 0 {
		return PerkPurchase{}, ErrPerkPurchaseNotFound
	}
	return r.GetPurchase(ctx, guildID, purchaseID)
}

// Refund gives the latest payment of a purchase back to the buyer, takes it back from whoever
// received it, and closes the purchase. ErrPerkOwnerCannotRefund when the receiver no longer has
// the points.
func (r *PerkStoreRepository) Refund(ctx context.Context, guildID, purchaseID int64, actor string, now time.Time) (PerkPurchase, int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return PerkPurchase{}, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status, name, kind string
	var buyer, offer, server int64
	err = tx.QueryRow(ctx, `SELECT status,offer_name,buyer_player_id,offer_id,server_id FROM perk_purchases WHERE guild_id=$1 AND id=$2 FOR UPDATE`, guildID, purchaseID).
		Scan(&status, &name, &buyer, &offer, &server)
	if errors.Is(err, pgx.ErrNoRows) {
		return PerkPurchase{}, 0, ErrPerkPurchaseNotFound
	}
	if err != nil {
		return PerkPurchase{}, 0, err
	}
	if status == PerkRefunded {
		return PerkPurchase{}, 0, ErrPerkNotRefundable
	}
	var chargeID, amount int64
	var owner *int64
	err = tx.QueryRow(ctx, `SELECT id,amount,owner_player_id,kind FROM perk_charges WHERE purchase_id=$1 AND refunded_at IS NULL ORDER BY id DESC LIMIT 1 FOR UPDATE`, purchaseID).
		Scan(&chargeID, &amount, &owner, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return PerkPurchase{}, 0, ErrPerkNotRefundable
	}
	if err != nil {
		return PerkPurchase{}, 0, err
	}
	ref := "perk-charge:" + strconv.FormatInt(chargeID, 10)
	if owner != nil {
		_, err := applyLedger(ctx, tx, LedgerParams{GuildID: guildID, PlayerID: *owner, ServerID: server, Type: TxPerkSaleRefund,
			Amount: amount, CreatedBy: actor, ReferenceID: ref, Description: name + " (refund)"}, true)
		if errors.Is(err, ErrInsufficientFunds) {
			return PerkPurchase{}, 0, ErrPerkOwnerCannotRefund
		}
		if err != nil {
			return PerkPurchase{}, 0, err
		}
	}
	if _, err := applyLedger(ctx, tx, LedgerParams{GuildID: guildID, PlayerID: buyer, ServerID: server, Type: TxPerkRefund,
		Amount: amount, CreatedBy: actor, ReferenceID: ref, Description: name + " (refund)"}, false); err != nil {
		return PerkPurchase{}, 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE perk_charges SET refunded_at=$2 WHERE id=$1`, chargeID, now); err != nil {
		return PerkPurchase{}, 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE perk_purchases SET status='REFUNDED',end_reason='REFUNDED',ended_at=COALESCE(ended_at,$2),auto_renew=FALSE WHERE id=$1`, purchaseID, now); err != nil {
		return PerkPurchase{}, 0, err
	}
	if kind == "PURCHASE" {
		// The unit goes back on sale.
		if _, err := tx.Exec(ctx, `UPDATE perk_offers SET sold_count=GREATEST(sold_count-1,0) WHERE id=$1`, offer); err != nil {
			return PerkPurchase{}, 0, err
		}
	}
	p, err := scanPerkPurchase(tx.QueryRow(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.id=$1`, purchaseID))
	if err != nil {
		return PerkPurchase{}, 0, err
	}
	return p, amount, tx.Commit(ctx)
}

// --- perk bookkeeping -------------------------------------------------------------------------------

// MarkApplied records the outcome of handing out a purchase's perks. problem == "" means every
// perk is in place; otherwise it is kept for staff and the worker tries again.
func (r *PerkStoreRepository) MarkApplied(ctx context.Context, purchaseID int64, vipMemberID *int64, priorityName, problem string) error {
	_, err := r.pool.Exec(ctx, `UPDATE perk_purchases SET
perks_applied_at=CASE WHEN $4='' THEN NOW() ELSE perks_applied_at END,
perks_error=$4, perk_attempts=perk_attempts+1,
vip_member_id=COALESCE($2,vip_member_id),
priority_name=CASE WHEN $3<>'' THEN $3 ELSE priority_name END
WHERE id=$1`, purchaseID, vipMemberID, priorityName, problem)
	return err
}

// MarkRemoved records the outcome of taking a closed purchase's perks away.
func (r *PerkStoreRepository) MarkRemoved(ctx context.Context, purchaseID int64, problem string) error {
	_, err := r.pool.Exec(ctx, `UPDATE perk_purchases SET
perks_removed_at=CASE WHEN $2='' THEN NOW() ELSE perks_removed_at END, perks_error=$2, perk_attempts=perk_attempts+1 WHERE id=$1`, purchaseID, problem)
	return err
}

// MarkCustomDone records that staff handed out the purchase's custom perk.
func (r *PerkStoreRepository) MarkCustomDone(ctx context.Context, guildID, purchaseID int64, by string) (PerkPurchase, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE perk_purchases SET custom_done_at=NOW(),custom_done_by=$3 WHERE guild_id=$1 AND id=$2 AND custom_perk<>'' AND custom_done_at IS NULL`, guildID, purchaseID, by)
	if err != nil {
		return PerkPurchase{}, err
	}
	if tag.RowsAffected() == 0 {
		return PerkPurchase{}, ErrPerkPurchaseNotFound
	}
	return r.GetPurchase(ctx, guildID, purchaseID)
}

// DueRenewals are monthly purchases whose paid time ran out and that renew.
func (r *PerkStoreRepository) DueRenewals(ctx context.Context, guildID int64, now time.Time, limit int) ([]PerkPurchase, error) {
	return r.collectPurchases(r.pool.Query(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.guild_id=$1 AND p.status='ACTIVE' AND p.billing='MONTHLY' AND p.auto_renew
AND p.expires_at IS NOT NULL AND p.expires_at<=$2 ORDER BY p.expires_at LIMIT $3`, guildID, now, limit))
}

// DueExpiries are active purchases whose time ran out and that do not renew.
func (r *PerkStoreRepository) DueExpiries(ctx context.Context, guildID int64, now time.Time, limit int) ([]PerkPurchase, error) {
	return r.collectPurchases(r.pool.Query(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.guild_id=$1 AND p.status='ACTIVE' AND NOT (p.billing='MONTHLY' AND p.auto_renew)
AND p.expires_at IS NOT NULL AND p.expires_at<=$2 ORDER BY p.expires_at LIMIT $3`, guildID, now, limit))
}

// PendingApply are active purchases whose perks are not all in place yet and that still have tries left.
func (r *PerkStoreRepository) PendingApply(ctx context.Context, guildID int64, limit int) ([]PerkPurchase, error) {
	return r.collectPurchases(r.pool.Query(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.guild_id=$1 AND p.status='ACTIVE' AND p.perks_applied_at IS NULL AND p.perk_attempts<$2
ORDER BY p.id LIMIT $3`, guildID, perkstore.MaxPerkAttempts, limit))
}

// PendingRemove are closed purchases whose perks are still in place and that still have tries left.
func (r *PerkStoreRepository) PendingRemove(ctx context.Context, guildID int64, limit int) ([]PerkPurchase, error) {
	return r.collectPurchases(r.pool.Query(ctx, `SELECT `+perkPurchaseCols+perkPurchaseFrom+` WHERE p.guild_id=$1 AND p.status<>'ACTIVE' AND p.perks_removed_at IS NULL AND p.perk_attempts<$2
ORDER BY p.id LIMIT $3`, guildID, 2*perkstore.MaxPerkAttempts, limit))
}

// PriorityStillPaid reports whether the player holds another active purchase that grants priority.
func (r *PerkStoreRepository) PriorityStillPaid(ctx context.Context, guildID, playerID, exceptPurchaseID int64) (bool, error) {
	var yes bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM perk_purchases WHERE guild_id=$1 AND recipient_player_id=$2 AND id<>$3 AND status='ACTIVE' AND priority_queue)`,
		guildID, playerID, exceptPurchaseID).Scan(&yes)
	return yes, err
}

// PriorityNameFromStore returns the name the store itself put on the priority list for this
// player, "" when every entry of theirs was already there (added by staff), so it is never removed.
func (r *PerkStoreRepository) PriorityNameFromStore(ctx context.Context, guildID, playerID int64) (string, error) {
	var name string
	err := r.pool.QueryRow(ctx, `SELECT priority_name FROM perk_purchases WHERE guild_id=$1 AND recipient_player_id=$2 AND priority_name<>'' ORDER BY id DESC LIMIT 1`, guildID, playerID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// ClearPriorityName forgets that the store put this player on the priority list (after removing them).
func (r *PerkStoreRepository) ClearPriorityName(ctx context.Context, guildID, playerID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE perk_purchases SET priority_name='' WHERE guild_id=$1 AND recipient_player_id=$2 AND priority_name<>''`, guildID, playerID)
	return err
}

// InstallationServer returns the organization and game server behind an installation.
func (r *PerkStoreRepository) InstallationServer(ctx context.Context, installationID int64) (organizationID, serverID int64, err error) {
	var server *int64
	err = r.pool.QueryRow(ctx, `SELECT organization_id,game_server_id FROM installations WHERE id=$1`, installationID).Scan(&organizationID, &server)
	if server != nil {
		serverID = *server
	}
	return organizationID, serverID, err
}

// --- board and summary ------------------------------------------------------------------------------

// TopSupporters ranks players by the points they spent in the store since since (nil = ever).
// Refunded payments do not count.
func (r *PerkStoreRepository) TopSupporters(ctx context.Context, guildID int64, since *time.Time, limit int) ([]PerkSupporter, error) {
	rows, err := r.pool.Query(ctx, `SELECT c.buyer_player_id,COALESCE(p.display_name,''),SUM(c.amount)::BIGINT,COUNT(*)::INT
FROM perk_charges c JOIN players p ON p.id=c.buyer_player_id
WHERE c.guild_id=$1 AND c.refunded_at IS NULL AND ($2::TIMESTAMPTZ IS NULL OR c.created_at>=$2)
GROUP BY c.buyer_player_id,p.display_name ORDER BY SUM(c.amount) DESC, MIN(c.created_at) LIMIT $3`, guildID, since, clampLimit(limit, 10, 50))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PerkSupporter{}
	for rows.Next() {
		var s PerkSupporter
		if err := rows.Scan(&s.PlayerID, &s.PlayerName, &s.Points, &s.Purchases); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *PerkStoreRepository) Stats(ctx context.Context, guildID int64, now time.Time) (PerkStats, error) {
	var s PerkStats
	err := r.pool.QueryRow(ctx, `SELECT
(SELECT COUNT(*) FROM perk_purchases WHERE guild_id=$1 AND status='ACTIVE')::INT,
(SELECT COUNT(*) FROM perk_purchases WHERE guild_id=$1 AND status='ACTIVE' AND billing='MONTHLY' AND auto_renew)::INT,
COALESCE((SELECT SUM(amount) FROM perk_charges WHERE guild_id=$1 AND refunded_at IS NULL AND created_at>=$2),0)::BIGINT,
COALESCE((SELECT SUM(amount) FROM perk_charges WHERE guild_id=$1 AND refunded_at IS NULL),0)::BIGINT`, guildID, now.AddDate(0, 0, -30)).
		Scan(&s.ActivePurchases, &s.Subscribers, &s.Points30Days, &s.PointsAllTime)
	return s, err
}

// PerkRecipient is a player a gift can go to.
type PerkRecipient struct {
	PlayerID int64  `json:"playerId"`
	Name     string `json:"name"`
}

// SearchRecipients finds players of the guild by in-game name, for gifting.
func (r *PerkStoreRepository) SearchRecipients(ctx context.Context, guildID int64, q string, limit int) ([]PerkRecipient, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,display_name FROM players WHERE guild_id=$1 AND display_name ILIKE $2
ORDER BY (LOWER(display_name)=LOWER($3)) DESC, LOWER(display_name), id LIMIT $4`, guildID, "%"+escapeLike(q)+"%", q, clampLimit(limit, 10, 25))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PerkRecipient{}
	for rows.Next() {
		var p PerkRecipient
		if err := rows.Scan(&p.PlayerID, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// StoreOpenForInstallation reports whether the perk store of the installation's Discord server is
// open (the channel layout only makes the donations channel for a server that sells something).
func (r *PerkStoreRepository) StoreOpenForInstallation(ctx context.Context, installationID int64) (bool, error) {
	var open bool
	err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT s.enabled FROM installations i
JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id
JOIN perk_store_settings s ON s.guild_id=c.guild_id WHERE i.id=$1),FALSE)`, installationID).Scan(&open)
	return open, err
}
