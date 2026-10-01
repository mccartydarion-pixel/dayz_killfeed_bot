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
	// BannedAt/BanReason are set by the platform owner (Owner Hub); a banned user is refused by
	// every /api/saas route and Discord interaction that resolves an acting user.
	BannedAt  *time.Time
	BanReason string
}

// Banned reports whether the platform owner has banned this account.
func (u *AppUser) Banned() bool { return u != nil && u.BannedAt != nil }

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
RETURNING id, discord_user_id, discord_username, COALESCE(discord_global_name,''), COALESCE(avatar,''), created_at, updated_at, last_login_at, banned_at, COALESCE(ban_reason,'')`

	var out AppUser
	err := r.pool.QueryRow(ctx, q, u.DiscordUserID, u.DiscordUsername, emptyToNil(u.DiscordGlobalName), emptyToNil(u.Avatar)).
		Scan(&out.ID, &out.DiscordUserID, &out.DiscordUsername, &out.DiscordGlobalName, &out.Avatar, &out.CreatedAt, &out.UpdatedAt, &out.LastLoginAt, &out.BannedAt, &out.BanReason)
	if err != nil {
		return nil, fmt.Errorf("upsert discord user: %w", err)
	}
	return &out, nil
}

// EnsureDiscordUser returns the app_users row for a Discord account, creating it when the person
// has never signed in to the website (a Discord button press is their first contact with
// Champion). Unlike UpsertDiscordUser it never stamps last_login_at: pressing a button is not a
// website login. Existing rows keep the profile the website recorded.
func (r *UserRepository) EnsureDiscordUser(ctx context.Context, discordUserID, username, globalName, avatar string) (*AppUser, error) {
	const q = `
INSERT INTO app_users(discord_user_id, discord_username, discord_global_name, avatar)
VALUES($1,$2,$3,$4)
ON CONFLICT(discord_user_id) DO UPDATE SET updated_at = app_users.updated_at
RETURNING id, discord_user_id, discord_username, COALESCE(discord_global_name,''), COALESCE(avatar,''), created_at, updated_at, last_login_at, banned_at, COALESCE(ban_reason,'')`
	var out AppUser
	err := r.pool.QueryRow(ctx, q, discordUserID, username, emptyToNil(globalName), emptyToNil(avatar)).
		Scan(&out.ID, &out.DiscordUserID, &out.DiscordUsername, &out.DiscordGlobalName, &out.Avatar, &out.CreatedAt, &out.UpdatedAt, &out.LastLoginAt, &out.BannedAt, &out.BanReason)
	if err != nil {
		return nil, fmt.Errorf("ensure discord user: %w", err)
	}
	return &out, nil
}

// GetByDiscordID returns the app_users row for a Discord user ID, or nil if
// this Discord account has never signed in to the website.
func (r *UserRepository) GetByDiscordID(ctx context.Context, discordUserID string) (*AppUser, error) {
	const q = `SELECT id, discord_user_id, discord_username, COALESCE(discord_global_name,''), COALESCE(avatar,''), created_at, updated_at, last_login_at, banned_at, COALESCE(ban_reason,'') FROM app_users WHERE discord_user_id=$1`
	var out AppUser
	err := r.pool.QueryRow(ctx, q, discordUserID).
		Scan(&out.ID, &out.DiscordUserID, &out.DiscordUsername, &out.DiscordGlobalName, &out.Avatar, &out.CreatedAt, &out.UpdatedAt, &out.LastLoginAt, &out.BannedAt, &out.BanReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by discord id: %w", err)
	}
	return &out, nil
}
