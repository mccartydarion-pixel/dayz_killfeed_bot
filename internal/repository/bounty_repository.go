package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	BountyActive    = "ACTIVE"
	BountyClaimed   = "CLAIMED"
	BountyExpired   = "EXPIRED"
	BountyCancelled = "CANCELLED"
	BountyAdmin     = "ADMIN"
	BountyAutomatic = "AUTOMATIC"
)

// ErrBountyNotFound is returned when a bounty does not exist, belongs to another
// guild, or is not in a state the operation applies to (e.g. already claimed).
var ErrBountyNotFound = errors.New("bounty not found or not active")

// Bounty is one durable bounty. RewardPoints is the bounty's amount in Champion
// Points (Phase 1 has no cash economy and never deducts anything from anyone).
// ServerID 0 means guild-wide (every pre-existing bounty): it is claimable by a
// kill on any server of the guild. A non-zero ServerID scopes it to that server.
// TargetName is filled only by queries that join players.
type Bounty struct {
	ID, GuildID, SeasonID, TargetPlayerID, RewardPoints int64
	ServerID                                            int64
	CreatedByType, Status, Reason                       string
	CreatedByDiscordUserID                              string
	StartsAt, ExpiresAt, ClaimedAt                      *time.Time
	ClaimedByPlayerID, ClaimedKillID                    *int64
	TargetName                                          string
	// Reward is the ledger entry a claim paid out (set only on the rows ClaimForKill
	// returns): its BalanceAfter is the hunter's balance right after this payout.
	Reward LedgerEntry
}

// BoardEntry is one target on the public bounty board: every active, eligible
// bounty on the target summed (stacking), with the number of bounties behind it.
type BoardEntry struct {
	TargetPlayerID int64
	TargetName     string
	Total, Count   int64
}

// KillClaim describes a durably persisted PvP kill offered to the bounty system.
type KillClaim struct {
	GuildID, ServerID              int64
	VictimPlayerID, KillerPlayerID int64
	KillID, SeasonID               int64
	At                             time.Time
}

type BountyRepository struct{ pool *pgxpool.Pool }

func NewBountyRepository(pool *pgxpool.Pool) *BountyRepository { return &BountyRepository{pool: pool} }

// bountyCols is the column list every bounty read/RETURNING uses; scanBounty
// must stay in step with it.
const bountyCols = `id,guild_id,COALESCE(season_id,0),COALESCE(server_id,0),target_player_id,reward_points,created_by_type,COALESCE(created_by_discord_user_id,''),status,COALESCE(reason,''),starts_at,expires_at,claimed_by_player_id,claimed_kill_id,claimed_at`

func scanBounty(row pgx.Row) (*Bounty, error) {
	var b Bounty
	if err := row.Scan(&b.ID, &b.GuildID, &b.SeasonID, &b.ServerID, &b.TargetPlayerID, &b.RewardPoints, &b.CreatedByType, &b.CreatedByDiscordUserID, &b.Status, &b.Reason, &b.StartsAt, &b.ExpiresAt, &b.ClaimedByPlayerID, &b.ClaimedKillID, &b.ClaimedAt); err != nil {
		return nil, err
	}
	return &b, nil
}

func collectBounties(rows pgx.Rows) ([]Bounty, error) {
	defer rows.Close()
	var out []Bounty
	for rows.Next() {
		b, err := scanBounty(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// Create inserts an ACTIVE bounty. A second ACTIVE AUTOMATIC bounty on the same
// (guild, target) returns ErrDuplicate (manual bounties may stack freely).
func (r *BountyRepository) Create(ctx context.Context, b Bounty, creator string) (*Bounty, error) {
	row := r.pool.QueryRow(ctx, `INSERT INTO bounties(guild_id,season_id,server_id,target_player_id,created_by_type,created_by_discord_user_id,status,reward_points,reason,starts_at,expires_at) VALUES($1,NULLIF($2,0),NULLIF($3,0),$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11) RETURNING `+bountyCols,
		b.GuildID, b.SeasonID, b.ServerID, b.TargetPlayerID, b.CreatedByType, creator, BountyActive, b.RewardPoints, b.Reason, b.StartsAt, b.ExpiresAt)
	out, err := scanBounty(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	return out, nil
}

// GetActive returns the highest active bounty on a target (guild-wide view). With
// stacking there can be several; use ActiveTotal for the combined value.
func (r *BountyRepository) GetActive(ctx context.Context, guildID, targetID int64) (*Bounty, error) {
	return r.GetActiveAt(ctx, guildID, targetID, time.Now().UTC())
}

func (r *BountyRepository) GetActiveAt(ctx context.Context, guildID, targetID int64, at time.Time) (*Bounty, error) {
	return scanBounty(r.pool.QueryRow(ctx, `SELECT `+bountyCols+` FROM bounties WHERE guild_id=$1 AND target_player_id=$2 AND status='ACTIVE' AND starts_at<=$3 AND (expires_at IS NULL OR $3<expires_at) ORDER BY reward_points DESC,id LIMIT 1`, guildID, targetID, at))
}

// GetActiveAutomatic returns the target's active streak-driven bounty, if any (at
// most one can exist: a unique index enforces it).
func (r *BountyRepository) GetActiveAutomatic(ctx context.Context, guildID, targetID int64) (*Bounty, error) {
	b, err := scanBounty(r.pool.QueryRow(ctx, `SELECT `+bountyCols+` FROM bounties WHERE guild_id=$1 AND target_player_id=$2 AND status='ACTIVE' AND created_by_type='AUTOMATIC' AND starts_at<=NOW() AND (expires_at IS NULL OR expires_at>NOW())`, guildID, targetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// ActiveTotal is the combined amount and number of active bounties on a target
// across the whole guild.
func (r *BountyRepository) ActiveTotal(ctx context.Context, guildID, targetID int64) (total, count int64, err error) {
	err = r.pool.QueryRow(ctx, `SELECT COALESCE(SUM(reward_points),0)::BIGINT, COUNT(*)::BIGINT FROM bounties WHERE guild_id=$1 AND target_player_id=$2 AND status='ACTIVE' AND starts_at<=NOW() AND (expires_at IS NULL OR expires_at>NOW())`, guildID, targetID).Scan(&total, &count)
	return total, count, err
}

// HasActiveAt reports whether playerID is currently wanted on serverID (a
// guild-wide bounty counts on every server). It drives the "wanted" badge on the
// kill card.
func (r *BountyRepository) HasActiveAt(ctx context.Context, guildID, serverID, playerID int64, at time.Time) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bounties WHERE guild_id=$1 AND target_player_id=$2 AND status='ACTIVE' AND starts_at<=$3 AND (expires_at IS NULL OR $3<expires_at) AND (server_id IS NULL OR server_id=NULLIF($4,0)))`, guildID, playerID, at, serverID).Scan(&ok)
	return ok, err
}

// ClaimForKill atomically claims EVERY eligible active bounty on the victim in
// one statement and awards each bounty's points to the killer, all in one
// transaction. Eligible means: same guild, victim is the target, ACTIVE, inside
// its start/expiry window at the kill's time, and either guild-wide or scoped to
// the server the kill happened on - so a bounty for server A can never be claimed
// by a kill on server B.
//
// The claim is a single "UPDATE ... WHERE status='ACTIVE' ... RETURNING": two
// workers (or a retry) racing on the same victim serialise on the row locks, the
// loser re-evaluates the WHERE against the committed CLAIMED rows and matches
// nothing, so a bounty is claimed at most once. Point awards are additionally
// idempotent through point_transactions' unique source key. Self-kills and
// unresolved players never claim.
//
// Callers must only call this for a durably persisted, non-duplicate PvP kill.
func (r *BountyRepository) ClaimForKill(ctx context.Context, c KillClaim) ([]Bounty, error) {
	if c.KillerPlayerID <= 0 || c.VictimPlayerID <= 0 || c.KillerPlayerID == c.VictimPlayerID {
		return nil, nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `UPDATE bounties SET status='CLAIMED',claimed_by_player_id=$3,claimed_kill_id=$4,claimed_at=$5
WHERE guild_id=$1 AND target_player_id=$2 AND status='ACTIVE' AND starts_at<=$5 AND (expires_at IS NULL OR $5<expires_at) AND (server_id IS NULL OR server_id=NULLIF($6,0))
RETURNING `+bountyCols, c.GuildID, c.VictimPlayerID, c.KillerPlayerID, c.KillID, c.At, c.ServerID)
	if err != nil {
		return nil, err
	}
	claimed, err := collectBounties(rows)
	if err != nil {
		return nil, err
	}
	if len(claimed) == 0 {
		return nil, nil
	}
	// One ledger transaction PER bounty (auditable: each bounty id is its own
	// reference), all inside the claim's transaction so the claim and the payout
	// commit together. Same type and key as ever ("BOUNTY_CLAIM", "bounty:<id>"),
	// so replays stay idempotent.
	economy := NewEconomyRepository(r.pool)
	for i := range claimed {
		entry, err := economy.CreditTx(ctx, tx, LedgerParams{
			GuildID: c.GuildID, PlayerID: c.KillerPlayerID, ServerID: c.ServerID, SeasonID: c.SeasonID,
			Type: TxBountyClaim, Amount: claimed[i].RewardPoints, Earned: true,
			ReferenceID: fmt.Sprintf("bounty:%d", claimed[i].ID), SourceID: claimed[i].ID, CreatedBy: "SYSTEM",
		})
		if err != nil {
			return nil, err
		}
		claimed[i].Reward = entry
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claimed, nil
}

// ExpireDue atomically moves every ACTIVE bounty whose expires_at has passed to
// EXPIRED and returns exactly the rows it transitioned - concurrent sweepers each
// get a disjoint set, so an expiry is reported once.
func (r *BountyRepository) ExpireDue(ctx context.Context, now time.Time) ([]Bounty, error) {
	rows, err := r.pool.Query(ctx, `UPDATE bounties SET status='EXPIRED' WHERE status='ACTIVE' AND expires_at IS NOT NULL AND expires_at<=$1 RETURNING `+bountyCols, now)
	if err != nil {
		return nil, err
	}
	return collectBounties(rows)
}

// Expire is ExpireDue without the result.
func (r *BountyRepository) Expire(ctx context.Context, now time.Time) error {
	_, err := r.ExpireDue(ctx, now)
	return err
}

// Upgrade raises an AUTOMATIC bounty's reward (never lowers it).
func (r *BountyRepository) Upgrade(ctx context.Context, guildID, bountyID int64, reward int) error {
	_, err := r.UpgradeReturning(ctx, guildID, bountyID, reward)
	if errors.Is(err, ErrBountyNotFound) {
		return nil // nothing to raise is not an error (matches the historical behaviour)
	}
	return err
}

// UpgradeReturning is Upgrade returning the updated bounty; ErrBountyNotFound when
// nothing changed (not an active automatic bounty of this guild, or not an increase).
func (r *BountyRepository) UpgradeReturning(ctx context.Context, guildID, bountyID int64, reward int) (*Bounty, error) {
	if reward <= 0 {
		return nil, fmt.Errorf("reward must be positive")
	}
	b, err := scanBounty(r.pool.QueryRow(ctx, `UPDATE bounties SET reward_points=$1 WHERE guild_id=$2 AND id=$3 AND status='ACTIVE' AND created_by_type='AUTOMATIC' AND reward_points<$1 RETURNING `+bountyCols, reward, guildID, bountyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBountyNotFound
	}
	return b, err
}

// Increase raises any ACTIVE bounty of the guild to newAmount, atomically and only
// if it really is an increase.
func (r *BountyRepository) Increase(ctx context.Context, guildID, bountyID, newAmount int64) (*Bounty, error) {
	if newAmount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	b, err := scanBounty(r.pool.QueryRow(ctx, `UPDATE bounties SET reward_points=$3 WHERE guild_id=$1 AND id=$2 AND status='ACTIVE' AND reward_points<$3 RETURNING `+bountyCols, guildID, bountyID, newAmount))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBountyNotFound
	}
	return b, err
}

// Cancel moves an ACTIVE bounty of the guild to CANCELLED and returns it;
// ErrBountyNotFound if it is not active in this guild (including already claimed).
func (r *BountyRepository) Cancel(ctx context.Context, guildID, bountyID int64) (*Bounty, error) {
	b, err := scanBounty(r.pool.QueryRow(ctx, `UPDATE bounties SET status='CANCELLED' WHERE guild_id=$1 AND id=$2 AND status='ACTIVE' RETURNING `+bountyCols, guildID, bountyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBountyNotFound
	}
	return b, err
}

// ListActiveForTarget returns every active bounty on one target player (Client Admin Control
// Plane Phase 1's "resetBountyReset": clearing a player's bounty state means cancelling every
// bounty currently active on them, not deleting the ledger/history of past claimed/expired ones).
func (r *BountyRepository) ListActiveForTarget(ctx context.Context, guildID, targetPlayerID int64) ([]Bounty, error) {
	rows, err := r.pool.Query(ctx, `SELECT id FROM bounties WHERE guild_id=$1 AND target_player_id=$2 AND status='ACTIVE'`, guildID, targetPlayerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bounty
	for rows.Next() {
		var b Bounty
		if err := rows.Scan(&b.ID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListActive lists individual active bounties (highest first) with the target's
// display name. Use ListBoard for the stacked, per-target view.
func (r *BountyRepository) ListActive(ctx context.Context, guildID int64, limit int) ([]Bounty, error) {
	rows, err := r.pool.Query(ctx, `SELECT b.id,b.guild_id,COALESCE(b.season_id,0),COALESCE(b.server_id,0),b.target_player_id,b.reward_points,b.created_by_type,COALESCE(b.created_by_discord_user_id,''),b.status,COALESCE(b.reason,''),b.starts_at,b.expires_at,b.claimed_by_player_id,b.claimed_kill_id,b.claimed_at,p.display_name FROM bounties b JOIN players p ON p.id=b.target_player_id WHERE b.guild_id=$1 AND b.status='ACTIVE' AND b.starts_at<=NOW() AND (b.expires_at IS NULL OR b.expires_at>NOW()) ORDER BY b.reward_points DESC,b.id LIMIT $2`, guildID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bounty
	for rows.Next() {
		var b Bounty
		if err := rows.Scan(&b.ID, &b.GuildID, &b.SeasonID, &b.ServerID, &b.TargetPlayerID, &b.RewardPoints, &b.CreatedByType, &b.CreatedByDiscordUserID, &b.Status, &b.Reason, &b.StartsAt, &b.ExpiresAt, &b.ClaimedByPlayerID, &b.ClaimedKillID, &b.ClaimedAt, &b.TargetName); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

const boardSelect = `SELECT b.target_player_id,p.display_name,SUM(b.reward_points)::BIGINT AS total,COUNT(*)::BIGINT AS n
FROM bounties b JOIN players p ON p.id=b.target_player_id
WHERE b.guild_id=$1 AND b.status='ACTIVE' AND b.starts_at<=NOW() AND (b.expires_at IS NULL OR b.expires_at>NOW())`

const boardTail = ` GROUP BY b.target_player_id,p.display_name ORDER BY total DESC,MIN(b.id) LIMIT `

func scanBoard(rows pgx.Rows) ([]BoardEntry, error) {
	defer rows.Close()
	var out []BoardEntry
	for rows.Next() {
		var e BoardEntry
		if err := rows.Scan(&e.TargetPlayerID, &e.TargetName, &e.Total, &e.Count); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListBoard is the public board for the given servers: the top targets by combined
// active bounty, counting guild-wide bounties plus those scoped to any of
// serverIDs. Bounties scoped to other servers never appear.
func (r *BountyRepository) ListBoard(ctx context.Context, guildID int64, serverIDs []int64, limit int) ([]BoardEntry, error) {
	rows, err := r.pool.Query(ctx, boardSelect+` AND (b.server_id IS NULL OR b.server_id = ANY($2))`+boardTail+`$3`, guildID, serverIDs, limit)
	if err != nil {
		return nil, err
	}
	return scanBoard(rows)
}

// ListBoardAll is the board across every server of the guild.
func (r *BountyRepository) ListBoardAll(ctx context.Context, guildID int64, limit int) ([]BoardEntry, error) {
	rows, err := r.pool.Query(ctx, boardSelect+boardTail+`$2`, guildID, limit)
	if err != nil {
		return nil, err
	}
	return scanBoard(rows)
}

// PlayerName returns a player's display name if the player belongs to guildID.
func (r *BountyRepository) PlayerName(ctx context.Context, guildID, playerID int64) (string, bool, error) {
	var name string
	err := r.pool.QueryRow(ctx, `SELECT display_name FROM players WHERE id=$1 AND guild_id=$2`, playerID, guildID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return name, err == nil, err
}

// ServerScope returns the guild and (nullable) organization a game server belongs
// to; found=false when there is no such server.
func (r *BountyRepository) ServerScope(ctx context.Context, serverID int64) (guildID int64, organizationID *int64, found bool, err error) {
	err = r.pool.QueryRow(ctx, `SELECT guild_id,organization_id FROM game_servers WHERE id=$1`, serverID).Scan(&guildID, &organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil
	}
	return guildID, organizationID, err == nil, err
}
