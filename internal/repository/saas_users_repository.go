package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AppUser is the durable mapping for a Discord-authenticated website user.
// It never stores a Discord OAuth access/refresh token - those belong to the
// website's own session store, not the shared Champion schema.
type AppUser struct {
	ID                                         int64
	DiscordUserID                              string
	DiscordUsername, DiscordGlobalName, Avatar string
	CreatedAt, UpdatedAt                       time.Time
	LastLoginAt                                *time.Time
}

// UserRepository persists website users keyed by their unique Discord ID.
type UserRepository struct{ pool *pgxpool.Pool }

func NewUserRepository(pool *pgxpool.Pool) *UserRepository { return &UserRepository{pool: pool} }

// UpsertDiscordUser inserts or updates the durable record for a
// Discord-authenticated website user and stamps last_login_at. Call this on
// every successful website sign-in.
func (r *UserRepository) UpsertDiscordUser(ctx context.Context, u AppUser) (*AppUser, error) {
	const q = `
INSERT INTO app_users(discord_user_id, discord_username, discord_global_name, avatar, last_login_at)
VALUES($1,$2,$3,$4,NOW())
ON CONFLICT(discord_user_id) DO UPDATE SET
    discord_username=EXCLUDED.discord_username,
    discord_global_name=EXCLUDED.discord_global_name,
    avatar=EXCLUDED.avatar,
    last_login_at=NOW(),
    updated_at=NOW()
RETURNING id, discord_user_id, discord_username, COALESCE(discord_global_name,''), COALESCE(avatar,''), created_at, updated_at, last_login_at`

	var out AppUser
	err := r.pool.QueryRow(ctx, q, u.DiscordUserID, u.DiscordUsername, emptyToNil(u.DiscordGlobalName), emptyToNil(u.Avatar)).
		Scan(&out.ID, &out.DiscordUserID, &out.DiscordUsername, &out.DiscordGlobalName, &out.Avatar, &out.CreatedAt, &out.UpdatedAt, &out.LastLoginAt)
	if err != nil {
		return nil, fmt.Errorf("upsert discord user: %w", err)
	}
	return &out, nil
}

// GetByDiscordID returns the app_users row for a Discord user ID, or nil if
// this Discord account has never signed in to the website.
func (r *UserRepository) GetByDiscordID(ctx context.Context, discordUserID string) (*AppUser, error) {
	const q = `SELECT id, discord_user_id, discord_username, COALESCE(discord_global_name,''), COALESCE(avatar,''), created_at, updated_at, last_login_at FROM app_users WHERE discord_user_id=$1`
	var out AppUser
	err := r.pool.QueryRow(ctx, q, discordUserID).
		Scan(&out.ID, &out.DiscordUserID, &out.DiscordUsername, &out.DiscordGlobalName, &out.Avatar, &out.CreatedAt, &out.UpdatedAt, &out.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by discord id: %w", err)
	}
	return &out, nil
}
