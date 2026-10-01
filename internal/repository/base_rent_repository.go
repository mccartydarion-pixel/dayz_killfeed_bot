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
}

type BaseRentPayResult struct {
	Payment      BaseRentPayment
	BalanceAfter int64
	Duplicate    bool
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

// PlayerBases lists the rent state of the player's rented bases (empty when rent is off).
func (r *BaseRentRepository) PlayerBases(ctx context.Context, s SecurityScope, playerID int64) ([]RentedBase, error) {
	if !r.ready() || !s.valid() || playerID <= 0 {
		return nil, ErrInvalidBaseRent
	}
	rows, err := r.pool.Query(ctx, rentedBaseSelect+` AND b.owner_player_id=$4 ORDER BY b.id`, s.InstallationID, s.GuildID, s.ServerID, playerID)
	if err != nil {
		return nil, err
	}
	return scanRentedBases(rows)
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

// payableBaseSQL finds a rented base the player may pay rent on: their own.
const payableBaseSQL = `SELECT b.name FROM case_registered_bases b
 WHERE b.id=$1 AND b.installation_id=$2 AND b.guild_id=$3 AND b.server_id=$4 AND b.owner_player_id=$5
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
	err = r.pool.QueryRow(ctx, payableBaseSQL, baseID, s.InstallationID, s.GuildID, s.ServerID, playerID).Scan(&baseName)
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
	if !r.ready() || !s.valid() || playerID <= 0 || baseID <= 0 || len(requestKey) < 8 || len(requestKey) > 80 {
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
	err = tx.QueryRow(ctx, payableBaseSQL, baseID, s.InstallationID, s.GuildID, s.ServerID, playerID).Scan(&baseName)
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
  rp.starts_at,rp.ends_at,rp.created_at
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
		if err := rows.Scan(&p.ID, &p.BaseID, &p.BaseName, &p.PlayerID, &p.PlayerName, &p.PricePoints, &p.PeriodDays, &p.StartsAt, &p.EndsAt, &p.CreatedAt); err != nil {
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
