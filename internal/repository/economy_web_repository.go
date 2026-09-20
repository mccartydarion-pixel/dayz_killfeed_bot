package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Read side of the Champion Points economy for the website API (docs/ECONOMY.md).
// There is no second account table: an "account" is the existing (guild, player)
// row pair - player_points (balance) + point_transactions (ledger) - reached through an
// installation, which fixes the organization, the Discord guild and the DayZ server.
// Nothing here writes a balance; every write goes through applyLedger.

// EconomyScope is what an installation resolves to. GuildID is the balance scope
// (balances are guild-wide, see economy_repository.go); ServerID (0 = no server
// selected yet) is the installation's DayZ server, used only to attribute writes.
type EconomyScope struct {
	OrganizationID, InstallationID int64
	GuildID, ServerID              int64
	Status                         string
}

// InstallationScope resolves organization + installation to the economy scope. found is
// false when the installation does not belong to the organization (another tenant's id).
func (r *EconomyRepository) InstallationScope(ctx context.Context, organizationID, installationID int64) (EconomyScope, bool, error) {
	s := EconomyScope{OrganizationID: organizationID, InstallationID: installationID}
	var server *int64
	err := r.pool.QueryRow(ctx, `
SELECT c.guild_id, i.game_server_id, i.status
FROM installations i JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
WHERE i.id = $1 AND i.organization_id = $2`, installationID, organizationID).Scan(&s.GuildID, &server, &s.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return EconomyScope{}, false, nil
	}
	if err != nil {
		return EconomyScope{}, false, err
	}
	if server != nil {
		s.ServerID = *server
	}
	return s, true, nil
}

// VerifiedPlayerID returns the DayZ player the Discord user has a VERIFIED link to in
// the guild. A pending or rejected link proves nothing and is not returned.
func (r *EconomyRepository) VerifiedPlayerID(ctx context.Context, guildID int64, discordUserID string) (int64, bool, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT player_id FROM player_links WHERE guild_id=$1 AND discord_user_id=$2 AND status='VERIFIED'`, guildID, discordUserID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// EconomyAccountRow is one player's account: identity plus the current balance.
type EconomyAccountRow struct {
	PlayerID          int64
	PlayerName        string // the DayZ gamertag
	Balance           int64
	UpdatedAt         *time.Time // last balance change; nil = no economy activity yet
	DiscordUserID     string     // the VERIFIED linked Discord account; "" = not linked
	DiscordUsername   string
	DiscordGlobalName string
	Avatar            string
}

const accountSelect = `
SELECT p.id, p.display_name, COALESCE(pp.balance,0), pp.updated_at,
       COALESCE(pl.discord_user_id,''), COALESCE(u.discord_username,''), COALESCE(u.discord_global_name,''), COALESCE(u.avatar,'')
FROM players p
LEFT JOIN player_points pp ON pp.guild_id = p.guild_id AND pp.player_id = p.id
LEFT JOIN player_links pl ON pl.guild_id = p.guild_id AND pl.player_id = p.id AND pl.status = 'VERIFIED'
LEFT JOIN app_users u ON u.discord_user_id = pl.discord_user_id`

func scanAccount(row pgx.Row) (EconomyAccountRow, error) {
	var a EconomyAccountRow
	err := row.Scan(&a.PlayerID, &a.PlayerName, &a.Balance, &a.UpdatedAt, &a.DiscordUserID, &a.DiscordUsername, &a.DiscordGlobalName, &a.Avatar)
	return a, err
}

// Account returns the player's account if the player belongs to the guild.
func (r *EconomyRepository) Account(ctx context.Context, guildID, playerID int64) (EconomyAccountRow, bool, error) {
	a, err := scanAccount(r.pool.QueryRow(ctx, accountSelect+` WHERE p.guild_id=$1 AND p.id=$2`, guildID, playerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return EconomyAccountRow{}, false, nil
	}
	return a, err == nil, err
}

// MaxAccountSearchResults bounds an admin lookup.
const MaxAccountSearchResults = 25

// SearchAccounts finds players of the guild whose gamertag, or whose VERIFIED linked Discord
// username / display name, contains q (case-insensitive; LIKE wildcards in q are literal).
// Exact matches first, then alphabetical; at most limit rows.
func (r *EconomyRepository) SearchAccounts(ctx context.Context, guildID int64, q string, limit int) ([]EconomyAccountRow, error) {
	if limit <= 0 || limit > MaxAccountSearchResults {
		limit = MaxAccountSearchResults
	}
	pattern := "%" + escapeLike(q) + "%"
	rows, err := r.pool.Query(ctx, accountSelect+`
WHERE p.guild_id = $1
  AND (p.display_name ILIKE $2 OR u.discord_username ILIKE $2 OR u.discord_global_name ILIKE $2)
ORDER BY (LOWER(p.display_name) = LOWER($3)) DESC, LOWER(p.display_name), p.id
LIMIT $4`, guildID, pattern, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EconomyAccountRow{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TransactionFilter narrows a ledger page. Types are exact stored transaction types;
// TypePrefix (for example "EVENT_") matches a family of them. Both empty = all.
type TransactionFilter struct {
	Types      []string
	TypePrefix string
}

// Transactions is History with an optional type filter and the same keyset paging
// (newest first, limit clamped to [1, MaxHistoryLimit] with default DefaultHistoryLimit,
// beforeID = the cursor, nextCursor != 0 when older entries remain). It reads at most
// limit+1 rows through idx_point_transactions_history - never the whole ledger.
func (r *EconomyRepository) Transactions(ctx context.Context, guildID, playerID int64, limit int, beforeID int64, f TransactionFilter) (entries []LedgerEntry, nextCursor int64, err error) {
	switch {
	case limit <= 0:
		limit = DefaultHistoryLimit
	case limit > MaxHistoryLimit:
		limit = MaxHistoryLimit
	}
	var types []string
	if len(f.Types) > 0 {
		types = f.Types
	}
	rows, err := r.pool.Query(ctx, `SELECT `+ledgerCols+` FROM point_transactions
WHERE guild_id=$1 AND player_id=$2 AND ($3::bigint=0 OR id<$3)
  AND (($5::text[] IS NULL AND $6::text = '') OR reason_type = ANY($5::text[]) OR ($6::text <> '' AND reason_type LIKE $6 || '%'))
ORDER BY id DESC LIMIT $4`, guildID, playerID, beforeID, limit+1, types, escapeLike(f.TypePrefix))
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

// Reconciliation kinds.
const (
	MismatchBalance = "BALANCE" // player_points.balance != SUM(ledger) or != the newest balance_after
	MismatchChain   = "CHAIN"   // a row whose balance_after != the previous balance_after + its amount
)

// BalanceMismatch is one inconsistency found by Reconcile. Nothing is ever repaired
// automatically; a repair (a compensating SYSTEM_ADJUSTMENT) is a deliberate operator action.
type BalanceMismatch struct {
	Kind          string
	PlayerID      int64
	Balance       int64 // player_points.balance (CHAIN: the row's balance_after)
	LedgerSum     int64 // SUM(amount) (CHAIN: the previous balance_after + amount that was expected)
	TransactionID int64 // CHAIN only
}

// Reconcile verifies, read-only, that every account of the guild is derivable from its
// ledger: the stored balance equals SUM(amount) and the newest balance_after, and every
// row's balance_after equals the previous row's balance_after plus its amount. It returns
// at most limit inconsistencies (0 = 100); an empty result means the guild is consistent.
func (r *EconomyRepository) Reconcile(ctx context.Context, guildID int64, limit int) ([]BalanceMismatch, error) {
	if limit <= 0 {
		limit = 100
	}
	out := []BalanceMismatch{}
	rows, err := r.pool.Query(ctx, `
SELECT COALESCE(pp.player_id, t.player_id), COALESCE(pp.balance,0), COALESCE(t.total,0)
FROM (SELECT player_id, SUM(amount) AS total, (ARRAY_AGG(balance_after ORDER BY id DESC))[1] AS newest
      FROM point_transactions WHERE guild_id = $1 GROUP BY player_id) t
FULL JOIN (SELECT player_id, balance FROM player_points WHERE guild_id = $1) pp ON pp.player_id = t.player_id
WHERE COALESCE(pp.balance,0) <> COALESCE(t.total,0) OR (t.newest IS NOT NULL AND t.newest <> COALESCE(pp.balance,0))
ORDER BY 1 LIMIT $2`, guildID, limit)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		m := BalanceMismatch{Kind: MismatchBalance}
		if err := rows.Scan(&m.PlayerID, &m.Balance, &m.LedgerSum); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) >= limit {
		return out, nil
	}
	chain, err := r.pool.Query(ctx, `
SELECT player_id, id, COALESCE(balance_after,0), prev + amount
FROM (SELECT id, player_id, amount, balance_after,
             COALESCE(LAG(balance_after) OVER (PARTITION BY player_id ORDER BY id), 0) AS prev
      FROM point_transactions WHERE guild_id = $1) x
WHERE balance_after IS NULL OR balance_after <> prev + amount
ORDER BY id LIMIT $2`, guildID, limit-len(out))
	if err != nil {
		return nil, err
	}
	defer chain.Close()
	for chain.Next() {
		m := BalanceMismatch{Kind: MismatchChain}
		if err := chain.Scan(&m.PlayerID, &m.TransactionID, &m.Balance, &m.LedgerSum); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, chain.Err()
}
