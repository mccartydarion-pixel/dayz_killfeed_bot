package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BaseRentRepository backs base rent (docs/BASE_RENT.md): the owner's price,
// players paying ahead for their player-requested bases, the paused check
// (rent owed for more than the 3-day grace period) and reminder bookkeeping.
type BaseRentRepository struct{ pool *pgxpool.Pool }

func NewBaseRentRepository(pool *pgxpool.Pool) *BaseRentRepository {
	return &BaseRentRepository{pool: pool}
}

const (
	TxBaseRent = "BASE_RENT"

	BaseRentGraceDays     = 3
	BaseRentMaxPeriod     = 30
	BaseRentNoticeDueSoon = "DUE_SOON"
	BaseRentNoticePaused  = "PAUSED"
)

var (
	ErrInvalidBaseRent  = errors.New("invalid base rent request")
	ErrBaseRentOff      = errors.New("base rent is not on")
	ErrBaseRentNotOwned = errors.New("that base isn't yours or doesn't pay rent")
)

type BaseRentSettings struct {
	Enabled      bool       `json:"enabled"`
	PricePoints  int64      `json:"pricePoints"`
	PeriodDays   int        `json:"periodDays"`
	Configured   bool       `json:"configured"`
	EnabledSince *time.Time `json:"enabledSince,omitempty"`
	UpdatedAt    *time.Time `json:"updatedAt,omitempty"`
}

// RentedBase is one player-requested base's rent state.
type RentedBase struct {
	BaseID     int64     `json:"baseId"`
	BaseName   string    `json:"baseName"`
	OwnerID    int64     `json:"ownerPlayerId"`
	OwnerName  string    `json:"ownerName,omitempty"`
	DueAt      time.Time `json:"dueAt"`
	GraceUntil time.Time `json:"graceUntil"`
	Paused     bool      `json:"paused"`
	Overdue    bool      `json:"overdue"`
	// Faction is true for a faction mate's base the player may pay rent on.
	Faction bool `json:"faction,omitempty"`
}

type BaseRentPayment struct {
	ID          int64     `json:"id"`
	BaseID      int64     `json:"baseId"`
	BaseName    string    `json:"baseName,omitempty"`
	PlayerID    int64     `json:"playerId"`
	PlayerName  string    `json:"playerName,omitempty"`
	PricePoints int64     `json:"pricePoints"`
	PeriodDays  int       `json:"periodDays"`
	StartsAt    time.Time `json:"startsAt"`
	EndsAt      time.Time `json:"endsAt"`
	CreatedAt   time.Time `json:"createdAt"`
	// Gift is true for rent days the server owner gave for free.
	Gift bool   `json:"gift,omitempty"`
	Note string `json:"note,omitempty"`
}

type BaseRentPayResult struct {
	Payment      BaseRentPayment
	BalanceAfter int64
	Duplicate    bool
	// Set when a faction mate paid for someone else's base (new payments only).
	OwnerID      int64
	OwnerDiscord string
	PayerName    string
}

func (r *BaseRentRepository) ready() bool { return r != nil && r.pool != nil }

// GetSettings returns the rent settings. No row means off and not configured.
func (r *BaseRentRepository) GetSettings(ctx context.Context, s SecurityScope) (BaseRentSettings, error) {
	var out BaseRentSettings
	if !r.ready() || !s.valid() {
		return out, ErrInvalidBaseRent
	}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `SELECT enabled,price_points,period_days,enabled_since,updated_at FROM base_rent_settings
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3`, s.InstallationID, s.GuildID, s.ServerID).
		Scan(&out.Enabled, &out.PricePoints, &out.PeriodDays, &out.EnabledSince, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Configured, out.UpdatedAt = true, &updated
	return out, nil
}

// SetSettings stores the price, period and switch. Turning rent on (from off)
// starts every rented base's clock now, so nothing is paused the moment it's
// switched on.
func (r *BaseRentRepository) SetSettings(ctx context.Context, s SecurityScope, enabled bool, price int64, periodDays int, actorUserID *int64) (BaseRentSettings, error) {
	if !r.ready() || !s.valid() || price < 1 || price > SecurityMaxPricePoints || periodDays < 1 || periodDays > BaseRentMaxPeriod {
		return BaseRentSettings{}, ErrInvalidBaseRent
	}
	out := BaseRentSettings{Configured: true}
	var updated time.Time
	err := r.pool.QueryRow(ctx, `INSERT INTO base_rent_settings
 (installation_id,guild_id,server_id,enabled,price_points,period_days,enabled_since,updated_by_user_id,updated_at)
 VALUES ($1,$2,$3,$4,$5,$6,CASE WHEN $4 THEN NOW() END,$7,NOW())
 ON CONFLICT (installation_id,server_id) DO UPDATE SET enabled=EXCLUDED.enabled,price_points=EXCLUDED.price_points,
  period_days=EXCLUDED.period_days,updated_by_user_id=EXCLUDED.updated_by_user_id,updated_at=NOW(),
  enabled_since=CASE WHEN EXCLUDED.enabled AND NOT base_rent_settings.enabled THEN NOW()
                     WHEN EXCLUDED.enabled THEN base_rent_settings.enabled_since ELSE base_rent_settings.enabled_since END
 WHERE base_rent_settings.guild_id=EXCLUDED.guild_id
 RETURNING enabled,price_points,period_days,enabled_since,updated_at`,
		s.InstallationID, s.GuildID, s.ServerID, enabled, price, periodDays, actorUserID).
		Scan(&out.Enabled, &out.PricePoints, &out.PeriodDays, &out.EnabledSince, &updated)
	if err != nil {
		return BaseRentSettings{}, err
	}
	out.UpdatedAt = &updated
	return out, nil
}

const rentedBaseSelect = `SELECT b.id,b.name,b.owner_player_id,COALESCE(p.display_name,''),base_rent_due_at(b.id) AS due
 FROM case_registered_bases b
 LEFT JOIN players p ON p.guild_id=b.guild_id AND p.id=b.owner_player_id
 WHERE b.installation_id=$1 AND b.guild_id=$2 AND b.server_id=$3 AND base_rent_due_at(b.id) IS NOT NULL`

func scanRentedBases(rows pgx.Rows) ([]RentedBase, error) {
	defer rows.Close()
	now := time.Now()
	out := make([]RentedBase, 0)
	for rows.Next() {
		var b RentedBase
		if err := rows.Scan(&b.BaseID, &b.BaseName, &b.OwnerID, &b.OwnerName, &b.DueAt); err != nil {
			return nil, err
		}
		b.GraceUntil = b.DueAt.Add(BaseRentGraceDays * 24 * time.Hour)
		b.Overdue = now.After(b.DueAt)
		b.Paused = now.After(b.GraceUntil)
		out = append(out, b)
	}
	return out, rows.Err()
}

// PlayerBases lists the rent state of the player's rented bases, then their
// faction mates' (Faction set), which they may also pay. Empty when rent is off.
func (r *BaseRentRepository) PlayerBases(ctx context.Context, s SecurityScope, playerID int64) ([]RentedBase, error) {
	if !r.ready() || !s.valid() || playerID <= 0 {
		return nil, ErrInvalidBaseRent
	}
	// $5 is the player for sameFactionSQL.
	rows, err := r.pool.Query(ctx, rentedBaseSelect+` AND (b.owner_player_id=$4 OR `+sameFactionSQL+`)
 ORDER BY (b.owner_player_id<>$4),b.id`, s.InstallationID, s.GuildID, s.ServerID, playerID, playerID)
	if err != nil {
		return nil, err
	}
	bases, err := scanRentedBases(rows)
	for i := range bases {
		bases[i].Faction = bases[i].OwnerID != playerID
	}
	return bases, err
}

// AllBases lists every rented base on the server, soonest due first, for the owner.
func (r *BaseRentRepository) AllBases(ctx context.Context, s SecurityScope, limit int) ([]RentedBase, error) {
	if !r.ready() || !s.valid() {
		return nil, ErrInvalidBaseRent
	}
	if limit < 1 || limit > 200 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, rentedBaseSelect+` ORDER BY due,b.id LIMIT $4`, s.InstallationID, s.GuildID, s.ServerID, limit)
	if err != nil {
		return nil, err
	}
	return scanRentedBases(rows)
}

// sameFactionSQL is true when player $5 and the base owner are active members
// of the same faction.
const sameFactionSQL = `EXISTS (SELECT 1 FROM faction_members me
  JOIN faction_members fo ON fo.guild_id=me.guild_id AND fo.faction_id=me.faction_id AND fo.active
  WHERE me.guild_id=b.guild_id AND me.player_id=$5 AND me.active AND fo.player_id=b.owner_player_id)`

// payableBaseSQL finds a rented base the player may pay rent on: their own,
// or one owned by an active member of their faction.
const payableBaseSQL = `SELECT b.name,b.owner_player_id FROM case_registered_bases b
 WHERE b.id=$1 AND b.installation_id=$2 AND b.guild_id=$3 AND b.server_id=$4
  AND (b.owner_player_id=$5 OR ` + sameFactionSQL + `)
  AND base_rent_due_at(b.id) IS NOT NULL`

// Quote is what one period of rent on a base costs the player, checking they
// may pay it. Nothing is charged.
func (r *BaseRentRepository) Quote(ctx context.Context, s SecurityScope, playerID, baseID int64) (baseName string, price int64, days int, err error) {
	if !r.ready() || !s.valid() || playerID <= 0 || baseID <= 0 {
		return "", 0, 0, ErrInvalidBaseRent
	}
	err = r.pool.QueryRow(ctx, `SELECT price_points,period_days FROM base_rent_settings
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND enabled`, s.InstallationID, s.GuildID, s.ServerID).Scan(&price, &days)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, 0, ErrBaseRentOff
	}
	if err != nil {
		return "", 0, 0, err
	}
	var ownerID int64
	err = r.pool.QueryRow(ctx, payableBaseSQL, baseID, s.InstallationID, s.GuildID, s.ServerID, playerID).Scan(&baseName, &ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, 0, ErrBaseRentNotOwned
	}
	return baseName, price, days, err
}

// Pay charges the player one period of rent for their base and extends its
// paid time (from now, or from when the current paid time ends). requestKey
// makes it idempotent. The debit and the payment row are one transaction.
func (r *BaseRentRepository) Pay(ctx context.Context, s SecurityScope, playerID, baseID int64, requestKey string) (BaseRentPayResult, error) {
	var out BaseRentPayResult
	// "gift-" keys belong to the owner's rent gifts.
	if !r.ready() || !s.valid() || playerID <= 0 || baseID <= 0 || len(requestKey) < 8 || len(requestKey) > 80 || strings.HasPrefix(requestKey, "gift-") {
		return out, ErrInvalidBaseRent
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('base_rent:'||$1::BIGINT::TEXT,0))`, baseID); err != nil {
		return out, err
	}
	var dup BaseRentPayment
	err = tx.QueryRow(ctx, `SELECT id,base_id,player_id,price_points,period_days,starts_at,ends_at,created_at FROM base_rent_payments
 WHERE installation_id=$1 AND player_id=$2 AND request_key=$3`, s.InstallationID, playerID, requestKey).
		Scan(&dup.ID, &dup.BaseID, &dup.PlayerID, &dup.PricePoints, &dup.PeriodDays, &dup.StartsAt, &dup.EndsAt, &dup.CreatedAt)
	if err == nil {
		out.Payment, out.Duplicate = dup, true
		_ = tx.QueryRow(ctx, `SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2`, s.GuildID, playerID).Scan(&out.BalanceAfter)
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	var price int64
	var days int
	err = tx.QueryRow(ctx, `SELECT price_points,period_days FROM base_rent_settings
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND enabled FOR SHARE`, s.InstallationID, s.GuildID, s.ServerID).Scan(&price, &days)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrBaseRentOff
	}
	if err != nil {
		return out, err
	}
	var baseName string
	var ownerID int64
	err = tx.QueryRow(ctx, payableBaseSQL, baseID, s.InstallationID, s.GuildID, s.ServerID, playerID).Scan(&baseName, &ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrBaseRentNotOwned
	}
	if err != nil {
		return out, err
	}
	entry, err := applyLedger(ctx, tx, LedgerParams{GuildID: s.GuildID, PlayerID: playerID, ServerID: s.ServerID,
		Type: TxBaseRent, Amount: price, CreatedBy: "SYSTEM",
		ReferenceID: "base-rent:" + strconv.FormatInt(s.InstallationID, 10) + ":" + requestKey,
		Description: fmt.Sprintf("Rent for %s · %d days", baseRaidClip(baseName, 80), days)}, true)
	if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `WITH start AS (
  SELECT GREATEST(NOW(),COALESCE((SELECT MAX(ends_at) FROM base_rent_payments WHERE base_id=$4),NOW())) AS at)
 INSERT INTO base_rent_payments
 (installation_id,guild_id,server_id,base_id,player_id,price_points,period_days,starts_at,ends_at,ledger_entry_id,request_key)
 SELECT $1,$2,$3,$4,$5,$6,$7,start.at,start.at+make_interval(days=>$7),$8,$9 FROM start
 RETURNING id,base_id,player_id,price_points,period_days,starts_at,ends_at,created_at`,
		s.InstallationID, s.GuildID, s.ServerID, baseID, playerID, price, days, entry.ID, requestKey).
		Scan(&out.Payment.ID, &out.Payment.BaseID, &out.Payment.PlayerID, &out.Payment.PricePoints, &out.Payment.PeriodDays,
			&out.Payment.StartsAt, &out.Payment.EndsAt, &out.Payment.CreatedAt)
	if err != nil {
		return out, err
	}
	out.Payment.BaseName = baseName
	out.BalanceAfter = entry.BalanceAfter
	if ownerID != playerID {
		// A faction mate paid: say who, so the base owner can be told.
		out.OwnerID = ownerID
		_ = tx.QueryRow(ctx, `SELECT COALESCE((SELECT display_name FROM players WHERE guild_id=$1 AND id=$2),''),
  COALESCE((SELECT discord_user_id FROM player_links WHERE guild_id=$1 AND player_id=$3 AND status='VERIFIED'),'')`,
			s.GuildID, playerID, ownerID).Scan(&out.PayerName, &out.OwnerDiscord)
	}
	return out, tx.Commit(ctx)
}

// BaseRentGiftMaxDays bounds one rent gift.
const BaseRentGiftMaxDays = 90

// RentGiftResult is a gift and who to tell.
type RentGiftResult struct {
	Payment      BaseRentPayment
	Duplicate    bool
	OwnerDiscord string
	OwnerName    string
}

// Gift gives a rented base free rent days: no Champion Points move. It stacks
// like a payment, counts for the base's owner and is idempotent on requestKey.
func (r *BaseRentRepository) Gift(ctx context.Context, s SecurityScope, baseID int64, days int, note string, giverUserID int64, requestKey string) (RentGiftResult, error) {
	var out RentGiftResult
	note = strings.TrimSpace(note)
	if !r.ready() || !s.valid() || baseID <= 0 || days < 1 || days > BaseRentGiftMaxDays || giverUserID <= 0 ||
		len([]rune(note)) > 200 || len(requestKey) < 8 || len(requestKey) > 80 {
		return out, ErrInvalidBaseRent
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('base_rent:'||$1::BIGINT::TEXT,0))`, baseID); err != nil {
		return out, err
	}
	var ownerID int64
	err = tx.QueryRow(ctx, `SELECT b.name,b.owner_player_id,COALESCE(p.display_name,''),COALESCE(l.discord_user_id,'')
 FROM case_registered_bases b
 LEFT JOIN players p ON p.guild_id=b.guild_id AND p.id=b.owner_player_id
 LEFT JOIN player_links l ON l.guild_id=b.guild_id AND l.player_id=b.owner_player_id AND l.status='VERIFIED'
 WHERE b.id=$1 AND b.installation_id=$2 AND b.guild_id=$3 AND b.server_id=$4 AND base_rent_due_at(b.id) IS NOT NULL`,
		baseID, s.InstallationID, s.GuildID, s.ServerID).Scan(&out.Payment.BaseName, &ownerID, &out.OwnerName, &out.OwnerDiscord)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrBaseRentNotOwned
	}
	if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT id,base_id,player_id,price_points,period_days,starts_at,ends_at,created_at,gift_note FROM base_rent_payments
 WHERE installation_id=$1 AND player_id=$2 AND request_key=$3`, s.InstallationID, ownerID, requestKey).
		Scan(&out.Payment.ID, &out.Payment.BaseID, &out.Payment.PlayerID, &out.Payment.PricePoints, &out.Payment.PeriodDays,
			&out.Payment.StartsAt, &out.Payment.EndsAt, &out.Payment.CreatedAt, &out.Payment.Note)
	if err == nil {
		out.Payment.Gift, out.Duplicate = true, true
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	err = tx.QueryRow(ctx, `WITH start AS (
  SELECT GREATEST(NOW(),COALESCE((SELECT MAX(ends_at) FROM base_rent_payments WHERE base_id=$4),NOW())) AS at)
 INSERT INTO base_rent_payments
 (installation_id,guild_id,server_id,base_id,player_id,price_points,period_days,starts_at,ends_at,gifted_by_user_id,gift_note,request_key)
 SELECT $1,$2,$3,$4,$5,0,$6,start.at,start.at+make_interval(days=>$6),$7,$8,$9 FROM start
 RETURNING id,base_id,player_id,price_points,period_days,starts_at,ends_at,created_at`,
		s.InstallationID, s.GuildID, s.ServerID, baseID, ownerID, days, giverUserID, note, requestKey).
		Scan(&out.Payment.ID, &out.Payment.BaseID, &out.Payment.PlayerID, &out.Payment.PricePoints, &out.Payment.PeriodDays,
			&out.Payment.StartsAt, &out.Payment.EndsAt, &out.Payment.CreatedAt)
	if err != nil {
		return out, err
	}
	out.Payment.Gift, out.Payment.Note = true, note
	return out, tx.Commit(ctx)
}

// RecentPayments lists the newest rent payments on one server, for the owner.
func (r *BaseRentRepository) RecentPayments(ctx context.Context, s SecurityScope, limit int) ([]BaseRentPayment, error) {
	if !r.ready() || !s.valid() {
		return nil, ErrInvalidBaseRent
	}
	if limit < 1 || limit > 50 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `SELECT rp.id,rp.base_id,b.name,rp.player_id,COALESCE(p.display_name,''),rp.price_points,rp.period_days,
  rp.starts_at,rp.ends_at,rp.created_at,rp.ledger_entry_id IS NULL,rp.gift_note
 FROM base_rent_payments rp
 JOIN case_registered_bases b ON b.id=rp.base_id
 LEFT JOIN players p ON p.guild_id=rp.guild_id AND p.id=rp.player_id
 WHERE rp.installation_id=$1 AND rp.guild_id=$2 AND rp.server_id=$3
 ORDER BY rp.created_at DESC,rp.id DESC LIMIT $4`, s.InstallationID, s.GuildID, s.ServerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BaseRentPayment, 0)
	for rows.Next() {
		var p BaseRentPayment
		if err := rows.Scan(&p.ID, &p.BaseID, &p.BaseName, &p.PlayerID, &p.PlayerName, &p.PricePoints, &p.PeriodDays, &p.StartsAt, &p.EndsAt, &p.CreatedAt, &p.Gift, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RentNotice is one reminder to send.
type RentNotice struct {
	Kind          string
	BaseID        int64
	BaseName      string
	ServerID      int64
	DueAt         time.Time
	PricePoints   int64
	PeriodDays    int
	DiscordUserID string
}

// DueNotices lists reminders not yet sent: rent due within a day, and bases
// that just became paused. Each is sent once per due date.
func (r *BaseRentRepository) DueNotices(ctx context.Context, limit int) ([]RentNotice, error) {
	if !r.ready() {
		return nil, ErrInvalidBaseRent
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `WITH rented AS (
  SELECT b.id,b.name,b.server_id,b.guild_id,b.owner_player_id,base_rent_due_at(b.id) AS due,rs.price_points,rs.period_days
  FROM case_registered_bases b
  JOIN base_rent_settings rs ON rs.installation_id=b.installation_id AND rs.server_id=b.server_id AND rs.enabled
  WHERE b.state<>'REVOKED' AND EXISTS (SELECT 1 FROM case_base_requests rq WHERE rq.base_id=b.id AND rq.status='APPROVED')
 ), candidates AS (
  SELECT 'DUE_SOON' AS kind,* FROM rented WHERE due > NOW() AND due <= NOW() + INTERVAL '1 day'
  UNION ALL
  SELECT 'PAUSED',* FROM rented WHERE due + make_interval(days => $2) < NOW()
 )
 SELECT c.kind,c.id,c.name,c.server_id,c.due,c.price_points,c.period_days,COALESCE(l.discord_user_id,'')
 FROM candidates c
 LEFT JOIN player_links l ON l.guild_id=c.guild_id AND l.player_id=c.owner_player_id AND l.status='VERIFIED'
 WHERE NOT EXISTS (SELECT 1 FROM base_rent_notices n WHERE n.base_id=c.id AND n.kind=c.kind AND n.due_at=c.due)
 ORDER BY c.due LIMIT $1`, limit, BaseRentGraceDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RentNotice
	for rows.Next() {
		var n RentNotice
		if err := rows.Scan(&n.Kind, &n.BaseID, &n.BaseName, &n.ServerID, &n.DueAt, &n.PricePoints, &n.PeriodDays, &n.DiscordUserID); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkNotice records that a reminder was handled (sent or not), so it's never repeated.
func (r *BaseRentRepository) MarkNotice(ctx context.Context, n RentNotice) error {
	if !r.ready() || n.BaseID <= 0 || (n.Kind != BaseRentNoticeDueSoon && n.Kind != BaseRentNoticePaused) {
		return ErrInvalidBaseRent
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO base_rent_notices(base_id,kind,due_at) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, n.BaseID, n.Kind, n.DueAt)
	return err
}

// BaseRentSummary is a server's rent numbers for the owner's sales summary.
type BaseRentSummary struct {
	Enabled    bool  `json:"enabled"`
	Payments   int   `json:"payments"`
	Points     int64 `json:"points"`
	Payers     int   `json:"payers"`
	GiftedDays int   `json:"giftedDays"`
	Rented     int   `json:"rented"`
	PaidUp     int   `json:"paidUp"`
	Overdue    int   `json:"overdue"`
	Paused     int   `json:"paused"`
}

// Summary counts rent paid since the given time (Champion Points, payments,
// paying players; gifted rent days separately) and where every rented base
// stands right now.
func (r *BaseRentRepository) Summary(ctx context.Context, s SecurityScope, since time.Time) (BaseRentSummary, error) {
	var out BaseRentSummary
	if !r.ready() || !s.valid() || since.IsZero() {
		return out, ErrInvalidBaseRent
	}
	settings, err := r.GetSettings(ctx, s)
	if err != nil {
		return out, err
	}
	out.Enabled = settings.Enabled
	if err := r.pool.QueryRow(ctx, `SELECT
  COUNT(*) FILTER (WHERE ledger_entry_id IS NOT NULL),
  COALESCE(SUM(price_points) FILTER (WHERE ledger_entry_id IS NOT NULL),0)::BIGINT,
  COUNT(DISTINCT player_id) FILTER (WHERE ledger_entry_id IS NOT NULL),
  COALESCE(SUM(period_days) FILTER (WHERE ledger_entry_id IS NULL),0)::INT
 FROM base_rent_payments WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND created_at>=$4`,
		s.InstallationID, s.GuildID, s.ServerID, since).Scan(&out.Payments, &out.Points, &out.Payers, &out.GiftedDays); err != nil {
		return out, err
	}
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*),
  COUNT(*) FILTER (WHERE due>NOW()),
  COUNT(*) FILTER (WHERE due<=NOW() AND due+make_interval(days=>$4)>=NOW()),
  COUNT(*) FILTER (WHERE due+make_interval(days=>$4)<NOW())
 FROM (SELECT base_rent_due_at(b.id) AS due FROM case_registered_bases b
  WHERE b.installation_id=$1 AND b.guild_id=$2 AND b.server_id=$3) t WHERE due IS NOT NULL`,
		s.InstallationID, s.GuildID, s.ServerID, BaseRentGraceDays).Scan(&out.Rented, &out.PaidUp, &out.Overdue, &out.Paused); err != nil {
		return out, err
	}
	return out, nil
}

// PausedDigest is one server's newly paused bases for the daily staff notice.
type PausedDigest struct {
	InstallationID int64
	GuildID        int64
	ServerID       int64
	Bases          []PausedBase
}

// PausedBase is one base paused for unpaid rent.
type PausedBase struct {
	BaseID    int64
	BaseName  string
	OwnerName string
	PausedAt  time.Time
}

// PausedDigests lists, per server, bases paused since that server's last
// notice (or in the last day), for servers not told in the last ~24 hours.
func (r *BaseRentRepository) PausedDigests(ctx context.Context) ([]PausedDigest, error) {
	if !r.ready() {
		return nil, ErrInvalidBaseRent
	}
	rows, err := r.pool.Query(ctx, `WITH paused AS (
  SELECT b.installation_id,b.guild_id,b.server_id,b.id,b.name,COALESCE(p.display_name,'') AS owner,
   base_rent_due_at(b.id)+make_interval(days=>$1) AS paused_at
  FROM case_registered_bases b
  LEFT JOIN players p ON p.guild_id=b.guild_id AND p.id=b.owner_player_id
  WHERE base_rent_due_at(b.id)+make_interval(days=>$1) < NOW()
 )
 SELECT pa.installation_id,pa.guild_id,pa.server_id,pa.id,pa.name,pa.owner,pa.paused_at
 FROM paused pa
 LEFT JOIN base_rent_digests d ON d.installation_id=pa.installation_id AND d.server_id=pa.server_id
 WHERE (d.last_sent_at IS NULL OR d.last_sent_at < NOW()-INTERVAL '23 hours 45 minutes')
  AND pa.paused_at > COALESCE(d.last_sent_at, NOW()-INTERVAL '1 day')
 ORDER BY pa.installation_id,pa.server_id,pa.paused_at,pa.id`, BaseRentGraceDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PausedDigest
	for rows.Next() {
		var inst, guild, server int64
		var b PausedBase
		if err := rows.Scan(&inst, &guild, &server, &b.BaseID, &b.BaseName, &b.OwnerName, &b.PausedAt); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].InstallationID != inst || out[n-1].ServerID != server {
			out = append(out, PausedDigest{InstallationID: inst, GuildID: guild, ServerID: server})
		}
		out[len(out)-1].Bases = append(out[len(out)-1].Bases, b)
	}
	return out, rows.Err()
}

// MarkDigest records that a server got its paused-bases notice now.
func (r *BaseRentRepository) MarkDigest(ctx context.Context, installationID, serverID int64) error {
	if !r.ready() || installationID <= 0 || serverID <= 0 {
		return ErrInvalidBaseRent
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO base_rent_digests(installation_id,server_id,last_sent_at) VALUES ($1,$2,NOW())
 ON CONFLICT (installation_id,server_id) DO UPDATE SET last_sent_at=NOW()`, installationID, serverID)
	return err
}

// PlayerPayments lists the newest rent payments and gifts on the player's
// bases and their faction mates' bases, with who paid.
func (r *BaseRentRepository) PlayerPayments(ctx context.Context, s SecurityScope, playerID int64, limit int) ([]BaseRentPayment, error) {
	if !r.ready() || !s.valid() || playerID <= 0 {
		return nil, ErrInvalidBaseRent
	}
	if limit < 1 || limit > 50 {
		limit = 20
	}
	// $5 is the player for sameFactionSQL.
	rows, err := r.pool.Query(ctx, `SELECT rp.id,rp.base_id,b.name,rp.player_id,COALESCE(p.display_name,''),rp.price_points,rp.period_days,
  rp.starts_at,rp.ends_at,rp.created_at,rp.ledger_entry_id IS NULL,rp.gift_note
 FROM base_rent_payments rp
 JOIN case_registered_bases b ON b.id=rp.base_id
 LEFT JOIN players p ON p.guild_id=rp.guild_id AND p.id=rp.player_id
 WHERE rp.installation_id=$1 AND rp.guild_id=$2 AND rp.server_id=$3
  AND (b.owner_player_id=$4 OR `+sameFactionSQL+`)
 ORDER BY rp.created_at DESC,rp.id DESC LIMIT $6`, s.InstallationID, s.GuildID, s.ServerID, playerID, playerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BaseRentPayment, 0)
	for rows.Next() {
		var p BaseRentPayment
		if err := rows.Scan(&p.ID, &p.BaseID, &p.BaseName, &p.PlayerID, &p.PlayerName, &p.PricePoints, &p.PeriodDays, &p.StartsAt, &p.EndsAt, &p.CreatedAt, &p.Gift, &p.Note); err != nil {
			return nil, err
		}
		if p.Gift {
			// Players don't see which staff account gave it; the payment row holds the base owner.
			p.PlayerName = ""
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RentFreeBase is a player-requested base the owner made rent-free.
type RentFreeBase struct {
	BaseID    int64     `json:"baseId"`
	BaseName  string    `json:"baseName"`
	OwnerID   int64     `json:"ownerPlayerId"`
	OwnerName string    `json:"ownerName,omitempty"`
	Note      string    `json:"note,omitempty"`
	Since     time.Time `json:"since"`
}

// requestedBaseSQL is true for a player-requested base in scope ($1-$4: base, installation, guild, server).
const requestedBaseSQL = `EXISTS (SELECT 1 FROM case_registered_bases b
 WHERE b.id=$1 AND b.installation_id=$2 AND b.guild_id=$3 AND b.server_id=$4 AND b.state<>'REVOKED'
  AND EXISTS (SELECT 1 FROM case_base_requests rq WHERE rq.base_id=b.id AND rq.status='APPROVED'))`

// SetRentFree makes a player-requested base rent-free, or charges it rent
// again (its clock restarts now). Only bases registered from a request.
func (r *BaseRentRepository) SetRentFree(ctx context.Context, s SecurityScope, baseID int64, free bool, note string, actorUserID *int64) error {
	note = strings.TrimSpace(note)
	if !r.ready() || !s.valid() || baseID <= 0 || len([]rune(note)) > 200 {
		return ErrInvalidBaseRent
	}
	var ok bool
	if err := r.pool.QueryRow(ctx, `SELECT `+requestedBaseSQL, baseID, s.InstallationID, s.GuildID, s.ServerID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrBaseRentNotOwned
	}
	_, err := r.pool.Exec(ctx, `INSERT INTO base_rent_exemptions(base_id,installation_id,exempt,note,changed_by_user_id,changed_at)
 VALUES ($1,$2,$3,$4,$5,NOW())
 ON CONFLICT (base_id) DO UPDATE SET exempt=EXCLUDED.exempt,note=EXCLUDED.note,changed_by_user_id=EXCLUDED.changed_by_user_id,
  changed_at=CASE WHEN base_rent_exemptions.exempt=EXCLUDED.exempt THEN base_rent_exemptions.changed_at ELSE NOW() END`,
		baseID, s.InstallationID, free, note, actorUserID)
	return err
}

// RentFreeBases lists the server's rent-free bases.
func (r *BaseRentRepository) RentFreeBases(ctx context.Context, s SecurityScope) ([]RentFreeBase, error) {
	if !r.ready() || !s.valid() {
		return nil, ErrInvalidBaseRent
	}
	rows, err := r.pool.Query(ctx, `SELECT b.id,b.name,b.owner_player_id,COALESCE(p.display_name,''),ex.note,ex.changed_at
 FROM base_rent_exemptions ex
 JOIN case_registered_bases b ON b.id=ex.base_id
 LEFT JOIN players p ON p.guild_id=b.guild_id AND p.id=b.owner_player_id
 WHERE ex.exempt AND b.installation_id=$1 AND b.guild_id=$2 AND b.server_id=$3 AND b.state<>'REVOKED'
 ORDER BY b.name,b.id`, s.InstallationID, s.GuildID, s.ServerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RentFreeBase, 0)
	for rows.Next() {
		var b RentFreeBase
		if err := rows.Scan(&b.BaseID, &b.BaseName, &b.OwnerID, &b.OwnerName, &b.Note, &b.Since); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
