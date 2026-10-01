package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/linking"
)

// LinkRepository persists Discord-to-DayZ pending/verified associations.
type LinkRepository struct{ pool *pgxpool.Pool }

func NewLinkRepository(pool *pgxpool.Pool) *LinkRepository { return &LinkRepository{pool: pool} }

func (r *LinkRepository) FindPlayers(ctx context.Context, guildID int64, name string) ([]linking.PlayerCandidate, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, dayz_player_id, display_name FROM players WHERE guild_id=$1 AND LOWER(display_name)=LOWER($2)`, guildID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []linking.PlayerCandidate
	for rows.Next() {
		var p linking.PlayerCandidate
		if err := rows.Scan(&p.ID, &p.DayZID, &p.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *LinkRepository) GetByDiscord(ctx context.Context, guildID int64, discordUserID string) (*linking.LinkRecord, error) {
	return r.get(ctx, `WHERE guild_id=$1 AND discord_user_id=$2`, guildID, discordUserID)
}

func (r *LinkRepository) GetVerifiedPlayerByDiscord(ctx context.Context, guildID int64, discordUserID string) (int64, error) {
	var playerID int64
	err := r.pool.QueryRow(ctx, `SELECT player_id FROM player_links WHERE guild_id=$1 AND discord_user_id=$2 AND status=$3`, guildID, discordUserID, linking.StatusVerified).Scan(&playerID)
	return playerID, err
}

func (r *LinkRepository) GetByPlayer(ctx context.Context, guildID, playerID int64) (*linking.LinkRecord, error) {
	return r.get(ctx, `WHERE guild_id=$1 AND player_id=$2`, guildID, playerID)
}

func (r *LinkRepository) get(ctx context.Context, where string, args ...any) (*linking.LinkRecord, error) {
	q := `SELECT l.guild_id, l.discord_user_id, l.player_id, COALESCE(l.requested_username,''), l.status,
COALESCE((SELECT v.expires_at FROM link_verifications v WHERE v.guild_id=l.guild_id AND v.discord_user_id=l.discord_user_id AND v.player_id=l.player_id ORDER BY v.created_at DESC LIMIT 1), NOW())
FROM player_links l ` + where
	var l linking.LinkRecord
	err := r.pool.QueryRow(ctx, q, args...).Scan(&l.GuildID, &l.DiscordUserID, &l.PlayerID, &l.RequestedName, &l.Status, &l.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (r *LinkRepository) CreatePending(ctx context.Context, l linking.LinkRecord) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,requested_username) VALUES($1,$2,$3,$4,$5)
ON CONFLICT(guild_id,discord_user_id) DO UPDATE SET player_id=EXCLUDED.player_id,status=EXCLUDED.status,requested_username=EXCLUDED.requested_username,updated_at=NOW()`, l.GuildID, l.PlayerID, l.DiscordUserID, l.Status, l.RequestedName)
	if err != nil {
		return fmt.Errorf("create pending link: %w", err)
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO link_verifications(guild_id,discord_user_id,player_id,challenge_hash,status,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, l.GuildID, l.DiscordUserID, l.PlayerID, l.ChallengeHash, l.Status, l.ExpiresAt)
	return err
}

func (r *LinkRepository) Unlink(ctx context.Context, guildID int64, discordUserID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE player_links SET status=$3,updated_at=NOW() WHERE guild_id=$1 AND discord_user_id=$2`, guildID, discordUserID, linking.StatusUnlinked)
	return err
}

// ExpirePending marks stale pending links as EXPIRED.
func (r *LinkRepository) ExpirePending(ctx context.Context) error {
	_, err := r.pool.Exec(ctx, `UPDATE player_links SET status=$1,updated_at=NOW() WHERE status=$2 AND updated_at < NOW() - INTERVAL '10 minutes'`, linking.StatusExpired, linking.StatusPending)
	return err
}

// Verify promotes a guild's pending link for discordUserID to VERIFIED. Called
// after the ADM disconnect/reconnect challenge completes, or an admin's
// manual approval - see LinkVerificationService.complete.
func (r *LinkRepository) Verify(ctx context.Context, guildID int64, discordUserID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE player_links SET status=$1,verified_at=NOW(),updated_at=NOW(),role_sync_status=$5 WHERE guild_id=$2 AND discord_user_id=$3 AND status=$4`, linking.StatusVerified, guildID, discordUserID, linking.StatusPending, linking.RoleSyncPending)
	return err
}

// MarkRoleSynced records the outcome of a Verified-role assignment for a
// VERIFIED link. status is linking.RoleSyncAssigned, RoleSyncFailed or
// RoleSyncMemberGone; a failure increments the attempt count. Idempotent: an
// ASSIGNED link is never moved back to PENDING/FAILED.
func (r *LinkRepository) MarkRoleSynced(ctx context.Context, guildID int64, discordUserID, status, errMsg string) error {
	if len(errMsg) > 300 {
		errMsg = errMsg[:300]
	}
	_, err := r.pool.Exec(ctx, `UPDATE player_links SET
    role_sync_status=$3,
    role_sync_attempts=role_sync_attempts+CASE WHEN $3 IN ($5,$6) THEN 1 ELSE 0 END,
    role_sync_last_attempt_at=NOW(),
    role_synced_at=CASE WHEN $3=$7 THEN NOW() ELSE role_synced_at END,
    role_sync_error=NULLIF($4,''),
    updated_at=NOW()
WHERE guild_id=$1 AND discord_user_id=$2 AND status=$8 AND role_sync_status IS DISTINCT FROM $7`,
		guildID, discordUserID, status, errMsg, linking.RoleSyncFailed, linking.RoleSyncMemberGone, linking.RoleSyncAssigned, linking.StatusVerified)
	return err
}

// ListRoleSyncDue returns Discord user IDs of VERIFIED links whose role is
// still PENDING or FAILED, with fewer than maxAttempts attempts and none in
// the last retryAfter (a flat per-link backoff), oldest first.
func (r *LinkRepository) ListRoleSyncDue(ctx context.Context, guildID int64, maxAttempts int, retryAfter time.Duration, limit int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT discord_user_id FROM player_links
WHERE guild_id=$1 AND status=$2 AND role_sync_status IN ($3,$4) AND role_sync_attempts < $5
  AND (role_sync_last_attempt_at IS NULL OR role_sync_last_attempt_at < NOW() - make_interval(secs => $6))
ORDER BY role_sync_last_attempt_at NULLS FIRST, verified_at
LIMIT $7`, guildID, linking.StatusVerified, linking.RoleSyncPending, linking.RoleSyncFailed, maxAttempts, retryAfter.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RoleSyncCounts reports VERIFIED links by role sync status for health output
// (status "" = untracked, verified before migration 0056).
func (r *LinkRepository) RoleSyncCounts(ctx context.Context, guildID int64) (map[string]int, error) {
	rows, err := r.pool.Query(ctx, `SELECT COALESCE(role_sync_status,''), COUNT(*) FROM player_links WHERE guild_id=$1 AND status=$2 GROUP BY 1`, guildID, linking.StatusVerified)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// RecordChallengeDisconnect marks the first observed disconnect after a
// pending link's challenge was issued. Only the first disconnect counts
// (challenge_disconnect_at IS NULL guard) - later disconnects before a
// reconnect are no-ops, so a player can't "re-arm" the challenge by
// disconnecting repeatedly.
func (r *LinkRepository) RecordChallengeDisconnect(ctx context.Context, guildID, playerID int64, at time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE link_verifications SET challenge_disconnect_at=$3
WHERE guild_id=$1 AND player_id=$2 AND status=$4 AND expires_at > $3 AND challenge_disconnect_at IS NULL`,
		guildID, playerID, at, linking.StatusPending)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CompletePendingChallenge promotes a pending link to VERIFIED once its
// player has both disconnected (per RecordChallengeDisconnect) and now
// reconnected, all within the challenge's expiry window. Both tables are
// updated in one transaction so a link can never end up VERIFIED in one and
// not the other.
func (r *LinkRepository) CompletePendingChallenge(ctx context.Context, guildID, playerID int64, at time.Time) (string, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)

	var discordUserID string
	err = tx.QueryRow(ctx, `UPDATE link_verifications SET status=$1, verified_at=$2
WHERE guild_id=$3 AND player_id=$4 AND status=$5 AND expires_at > $2
  AND challenge_disconnect_at IS NOT NULL AND challenge_disconnect_at < $2
RETURNING discord_user_id`,
		linking.StatusVerified, at, guildID, playerID, linking.StatusPending).Scan(&discordUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	if _, err := tx.Exec(ctx, `UPDATE player_links SET status=$1,verified_at=NOW(),updated_at=NOW(),role_sync_status=$5 WHERE guild_id=$2 AND discord_user_id=$3 AND status=$4`,
		linking.StatusVerified, guildID, discordUserID, linking.StatusPending, linking.RoleSyncPending); err != nil {
		return "", false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return discordUserID, true, nil
}

// GetPendingLinkByPlayerName resolves a pending link by DayZ display name, for
// the admin manual-approval fallback (/admin verify-link).
func (r *LinkRepository) GetPendingLinkByPlayerName(ctx context.Context, guildID int64, name string) (string, int64, bool, error) {
	var discordUserID string
	var playerID int64
	err := r.pool.QueryRow(ctx, `SELECT l.discord_user_id, l.player_id FROM player_links l
JOIN players p ON p.id = l.player_id
WHERE l.guild_id=$1 AND LOWER(p.display_name)=LOWER($2) AND l.status=$3`,
		guildID, name, linking.StatusPending).Scan(&discordUserID, &playerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return discordUserID, playerID, true, nil
}
