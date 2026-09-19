package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The Champion Points economy. There is exactly one ledger (point_transactions,
// append-only) and one balance store (player_points), both guild-wide - the same
// tables the bounty rewards and event prizes have always used. Every credit and
// debit in the system goes through applyLedger, so the ledger and the balance
// can never disagree.
//
// player_points holds three numbers per (guild, player):
//   - balance         the spendable balance; credits add, debits subtract, never < 0
//   - lifetime_points earn-only leaderboard score (never decreases)
//   - season_points   earn-only score, reset each season
//
// A credit is "earned" (bounty rewards, event prizes, future system rewards) or
// not (an admin adjustment): only earned credits also raise the leaderboard
// scores. Debits touch the balance only. Season resets never touch the balance.

// MaxLedgerAmount bounds one ledger operation (1e15). Balances are 64-bit, so
// this keeps sums far from overflow, and PostgreSQL raises (aborting the whole
// transaction) rather than wrapping if a total ever did overflow.
const MaxLedgerAmount int64 = 1_000_000_000_000_000

// Ledger transaction types persisted in point_transactions.reason_type. The bounty
// and event strings are the historical ones - they are part of the idempotency key
// of every row already written, so they must never be renamed.
const (
	TxBountyClaim  = "BOUNTY_CLAIM" // displayed as "BOUNTY REWARD"
	TxAdminCredit  = "ADMIN_CREDIT"
	TxAdminDebit   = "ADMIN_DEBIT"
	TxSystemReward = "SYSTEM_REWARD"
)

var (
	// ErrInsufficientFunds is returned by Debit when the balance is below the amount.
	ErrInsufficientFunds = errors.New("insufficient balance")
	// ErrInvalidLedgerAmount is returned for a non-positive or over-large amount.
	ErrInvalidLedgerAmount = errors.New("amount must be between 1 and the maximum")
)

// LedgerParams describes one credit or debit. Amount is always positive; the
// operation (Credit/Debit) decides the sign. ReferenceID (persisted as source_key)
// makes the operation idempotent per (guild, player, Type): replaying it returns
// the original entry with Duplicate=true and changes nothing. SourceID is the
// optional numeric source (e.g. the bounty id). ServerID is attribution only -
// balances are guild-wide.
type LedgerParams struct {
	GuildID, PlayerID  int64
	ServerID, SeasonID int64 // 0 = none
	Type               string
	Amount             int64
	Earned             bool // credits only: also raise the leaderboard scores
	ReferenceID        string
	SourceID           int64
	Description        string
	CreatedBy          string // Discord user id of an admin, or "SYSTEM" - internal, never shown publicly
}

// LedgerEntry is one ledger row. Amount is signed (debits are negative).
type LedgerEntry struct {
	ID                int64
	GuildID, PlayerID int64
	ServerID          int64
	Type              string
	Amount            int64
	BalanceAfter      int64
	ReferenceID       string
	Description       string
	CreatedBy         string
	CreatedAt         time.Time
	Duplicate         bool // the operation had already been applied; nothing changed
}

type EconomyRepository struct{ pool *pgxpool.Pool }

func NewEconomyRepository(pool *pgxpool.Pool) *EconomyRepository {
	return &EconomyRepository{pool: pool}
}

// Credit adds Amount to the player's balance and appends the ledger row, in one
// transaction: both happen or neither does.
func (r *EconomyRepository) Credit(ctx context.Context, p LedgerParams) (LedgerEntry, error) {
	return r.inTx(ctx, p, false)
}

// Debit subtracts Amount only if balance >= Amount (an atomic guard: two racing
// debits can never both spend the same points), otherwise ErrInsufficientFunds.
func (r *EconomyRepository) Debit(ctx context.Context, p LedgerParams) (LedgerEntry, error) {
	return r.inTx(ctx, p, true)
}

func (r *EconomyRepository) inTx(ctx context.Context, p LedgerParams, debit bool) (LedgerEntry, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return LedgerEntry{}, err
	}
	defer tx.Rollback(ctx)
	e, err := applyLedger(ctx, tx, p, debit)
	if err != nil {
		return LedgerEntry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LedgerEntry{}, err
	}
	return e, nil
}

// CreditTx is Credit inside the caller's transaction, so a caller (the bounty
// claim, event finalization) can make its own change and the payout atomic. It
// uses a savepoint: a duplicate reference undoes only the payout's own writes.
func (r *EconomyRepository) CreditTx(ctx context.Context, tx pgx.Tx, p LedgerParams) (LedgerEntry, error) {
	return applyLedger(ctx, tx, p, false)
}

// DebitTx is Debit inside the caller's transaction.
func (r *EconomyRepository) DebitTx(ctx context.Context, tx pgx.Tx, p LedgerParams) (LedgerEntry, error) {
	return applyLedger(ctx, tx, p, true)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const ledgerCols = `id,guild_id,player_id,COALESCE(server_id,0),reason_type,amount,COALESCE(balance_after,0),COALESCE(source_key,''),COALESCE(description,''),COALESCE(created_by,''),created_at`

func scanLedger(row pgx.Row) (LedgerEntry, error) {
	var e LedgerEntry
	err := row.Scan(&e.ID, &e.GuildID, &e.PlayerID, &e.ServerID, &e.Type, &e.Amount, &e.BalanceAfter, &e.ReferenceID, &e.Description, &e.CreatedBy, &e.CreatedAt)
	return e, err
}

func findLedgerEntry(ctx context.Context, q querier, p LedgerParams) (LedgerEntry, bool, error) {
	e, err := scanLedger(q.QueryRow(ctx, `SELECT `+ledgerCols+` FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND reason_type=$3 AND source_key=$4`, p.GuildID, p.PlayerID, p.Type, p.ReferenceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return LedgerEntry{}, false, nil
	}
	return e, err == nil, err
}

func applyLedger(ctx context.Context, tx pgx.Tx, p LedgerParams, debit bool) (LedgerEntry, error) {
	if p.Amount <= 0 || p.Amount > MaxLedgerAmount {
		return LedgerEntry{}, ErrInvalidLedgerAmount
	}
	if p.GuildID <= 0 || p.PlayerID <= 0 || p.Type == "" {
		return LedgerEntry{}, fmt.Errorf("ledger operation needs a guild, a player and a type")
	}

	// Replay fast path: an operation with this reference was already applied.
	if p.ReferenceID != "" {
		if e, found, err := findLedgerEntry(ctx, tx, p); err != nil {
			return LedgerEntry{}, err
		} else if found {
			e.Duplicate = true
			return e, nil
		}
	}

	// Everything below runs in a savepoint so that losing a same-reference race
	// undoes only this operation's own balance change, never the caller's work.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return LedgerEntry{}, err
	}
	defer sp.Rollback(ctx) // no-op once committed

	var balance int64
	if debit {
		// The atomic guard: the row lock taken by this UPDATE serialises every
		// concurrent credit/debit on the player, and balance>=amount is re-checked
		// against the committed value after any wait - no lost update, no double spend.
		err = sp.QueryRow(ctx, `UPDATE player_points SET balance=balance-$3,updated_at=NOW() WHERE guild_id=$1 AND player_id=$2 AND balance>=$3 RETURNING balance`, p.GuildID, p.PlayerID, p.Amount).Scan(&balance)
		if errors.Is(err, pgx.ErrNoRows) {
			return LedgerEntry{}, ErrInsufficientFunds
		}
	} else {
		var earned int64
		if p.Earned {
			earned = p.Amount
		}
		err = sp.QueryRow(ctx, `INSERT INTO player_points(guild_id,player_id,balance,lifetime_points,season_points) VALUES($1,$2,$3,$4,$4)
ON CONFLICT(guild_id,player_id) DO UPDATE SET balance=player_points.balance+EXCLUDED.balance,lifetime_points=player_points.lifetime_points+EXCLUDED.lifetime_points,season_points=player_points.season_points+EXCLUDED.season_points,updated_at=NOW()
RETURNING balance`, p.GuildID, p.PlayerID, p.Amount, earned).Scan(&balance)
	}
	if err != nil {
		return LedgerEntry{}, err
	}

	signed := p.Amount
	if debit {
		signed = -p.Amount
	}
	e := LedgerEntry{GuildID: p.GuildID, PlayerID: p.PlayerID, ServerID: p.ServerID, Type: p.Type, Amount: signed, BalanceAfter: balance, ReferenceID: p.ReferenceID, Description: p.Description, CreatedBy: p.CreatedBy}
	err = sp.QueryRow(ctx, `INSERT INTO point_transactions(guild_id,season_id,server_id,player_id,amount,balance_after,reason_type,source_id,source_key,description,created_by)
VALUES($1,NULLIF($2,0),NULLIF($3,0),$4,$5,$6,$7,NULLIF($8,0),NULLIF($9,''),NULLIF($10,''),NULLIF($11,'')) ON CONFLICT DO NOTHING RETURNING id,created_at`,
		p.GuildID, p.SeasonID, p.ServerID, p.PlayerID, signed, balance, p.Type, p.SourceID, p.ReferenceID, p.Description, p.CreatedBy).Scan(&e.ID, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Lost a race with the same reference: undo our balance change and return
		// the entry the winner recorded.
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return LedgerEntry{}, rbErr
		}
		existing, found, findErr := findLedgerEntry(ctx, tx, p)
		if findErr != nil {
			return LedgerEntry{}, findErr
		}
		if !found {
			return LedgerEntry{}, fmt.Errorf("ledger insert conflicted but no entry was found")
		}
		existing.Duplicate = true
		return existing, nil
	}
	if err != nil {
		return LedgerEntry{}, err
	}
	if err := sp.Commit(ctx); err != nil {
		return LedgerEntry{}, err
	}
	return e, nil
}

// Balance is the player's spendable balance; a player with no economy activity
// has 0. It does not check that the player exists (the service does).
func (r *EconomyRepository) Balance(ctx context.Context, guildID, playerID int64) (int64, error) {
	var b int64
	err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2),0)`, guildID, playerID).Scan(&b)
	return b, err
}

const (
	DefaultHistoryLimit = 10
	MaxHistoryLimit     = 50
)

// History returns the player's ledger newest first. It is keyset-paginated and
// hard-bounded: limit is clamped to [1, MaxHistoryLimit] (0 -> DefaultHistoryLimit)
// and beforeID (0 = start from the newest) is the cursor. nextCursor is non-zero
// when older entries remain.
func (r *EconomyRepository) History(ctx context.Context, guildID, playerID int64, limit int, beforeID int64) (entries []LedgerEntry, nextCursor int64, err error) {
	switch {
	case limit <= 0:
		limit = DefaultHistoryLimit
	case limit > MaxHistoryLimit:
		limit = MaxHistoryLimit
	}
	rows, err := r.pool.Query(ctx, `SELECT `+ledgerCols+` FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND ($3=0 OR id<$3) ORDER BY id DESC LIMIT $4`, guildID, playerID, beforeID, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanLedger(rows)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(entries) > limit {
		entries = entries[:limit]
		nextCursor = entries[len(entries)-1].ID
	}
	return entries, nextCursor, nil
}

// PlayerName returns a player's display name if the player belongs to guildID
// (the tenant check every economy operation starts with).
func (r *EconomyRepository) PlayerName(ctx context.Context, guildID, playerID int64) (string, bool, error) {
	var name string
	err := r.pool.QueryRow(ctx, `SELECT display_name FROM players WHERE id=$1 AND guild_id=$2`, playerID, guildID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return name, err == nil, err
}

// ServerScope returns the guild and (nullable) organization a game server belongs to.
func (r *EconomyRepository) ServerScope(ctx context.Context, serverID int64) (guildID int64, organizationID *int64, found bool, err error) {
	err = r.pool.QueryRow(ctx, `SELECT guild_id,organization_id FROM game_servers WHERE id=$1`, serverID).Scan(&guildID, &organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil
	}
	return guildID, organizationID, err == nil, err
}
