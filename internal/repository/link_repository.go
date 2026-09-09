package repository

import (
	"context"
	"errors"
	"fmt"

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

// Verify is intentionally not exposed by the user-facing service until a real
// ownership proof exists. It is reserved for a future authenticated verifier.
func (r *LinkRepository) Verify(ctx context.Context, guildID int64, discordUserID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE player_links SET status=$1,verified_at=NOW(),updated_at=NOW() WHERE guild_id=$2 AND discord_user_id=$3 AND status=$4`, linking.StatusVerified, guildID, discordUserID, linking.StatusPending)
	return err
}
