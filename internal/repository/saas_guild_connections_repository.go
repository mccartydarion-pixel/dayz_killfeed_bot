package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DiscordGuildConnection records which organization owns an already-connected
// guilds row for the SaaS dashboard. It deliberately does not re-store the
// Discord guild snowflake or channel config - guilds.discord_guild_id and
// guilds' channel columns remain authoritative for those (see
// docs/SAAS_SCHEMA.md); GuildID here is the internal guilds.id foreign key.
type DiscordGuildConnection struct {
	ID                                int64
	OrganizationID                    int64
	GuildID                           int64
	GuildName, GuildIcon              string
	BotInstalled, PermissionsVerified bool
	ConnectedAt, UpdatedAt            time.Time
}

type GuildConnectionRepository struct{ pool *pgxpool.Pool }

func NewGuildConnectionRepository(pool *pgxpool.Pool) *GuildConnectionRepository {
	return &GuildConnectionRepository{pool: pool}
}

// Upsert creates or updates the organization's ownership record for an
// already-connected guilds row (GuildID is guilds.id - resolve it via
// GuildRepository.GetGuild first). guild_id is UNIQUE, so a guild can only
// ever be claimed by one organization at a time.
func (r *GuildConnectionRepository) Upsert(ctx context.Context, c DiscordGuildConnection) (*DiscordGuildConnection, error) {
	const q = `
INSERT INTO discord_guild_connections(organization_id, guild_id, guild_name, guild_icon, bot_installed, permissions_verified)
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(guild_id) DO UPDATE SET
    guild_name=EXCLUDED.guild_name,
    guild_icon=EXCLUDED.guild_icon,
    bot_installed=EXCLUDED.bot_installed,
    permissions_verified=EXCLUDED.permissions_verified,
    updated_at=NOW()
RETURNING id, organization_id, guild_id, COALESCE(guild_name,''), COALESCE(guild_icon,''), bot_installed, permissions_verified, connected_at, updated_at`

	var out DiscordGuildConnection
	err := r.pool.QueryRow(ctx, q, c.OrganizationID, c.GuildID, emptyToNil(c.GuildName), emptyToNil(c.GuildIcon), c.BotInstalled, c.PermissionsVerified).
		Scan(&out.ID, &out.OrganizationID, &out.GuildID, &out.GuildName, &out.GuildIcon, &out.BotInstalled, &out.PermissionsVerified, &out.ConnectedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert guild connection: %w", err)
	}
	return &out, nil
}

// ListByOrganization returns every guild connection owned by organizationID.
func (r *GuildConnectionRepository) ListByOrganization(ctx context.Context, organizationID int64) ([]DiscordGuildConnection, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, organization_id, guild_id, COALESCE(guild_name,''), COALESCE(guild_icon,''), bot_installed, permissions_verified, connected_at, updated_at FROM discord_guild_connections WHERE organization_id=$1 ORDER BY id`, organizationID)
	if err != nil {
		return nil, fmt.Errorf("list guild connections: %w", err)
	}
	defer rows.Close()
	var out []DiscordGuildConnection
	for rows.Next() {
		var c DiscordGuildConnection
		if err := rows.Scan(&c.ID, &c.OrganizationID, &c.GuildID, &c.GuildName, &c.GuildIcon, &c.BotInstalled, &c.PermissionsVerified, &c.ConnectedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateBotInstalled refreshes only bot_installed for guildID's connection,
// requiring it belong to organizationID. Deliberately narrow rather than
// reusing Upsert: Upsert's ON CONFLICT clause unconditionally overwrites
// guild_name/guild_icon/permissions_verified from whatever is in the passed
// struct, so calling it with only BotInstalled set would silently wipe those
// other fields back to empty/false. Used by the live bot-presence refresh in
// handleVerifyInstallation (internal/app/saas_api_discord.go).
func (r *GuildConnectionRepository) UpdateBotInstalled(ctx context.Context, organizationID, guildID int64, installed bool) error {
	if _, err := r.pool.Exec(ctx, `UPDATE discord_guild_connections SET bot_installed=$3, updated_at=NOW() WHERE organization_id=$1 AND guild_id=$2`, organizationID, guildID, installed); err != nil {
		return fmt.Errorf("update bot_installed: %w", err)
	}
	return nil
}

// GetByGuildID returns the connection claiming guildID (guilds.id),
// regardless of which organization owns it, or nil if unclaimed.
// Deliberately NOT organization-scoped: it exists specifically so a caller
// can detect a cross-tenant conflict (a guild already claimed by a
// DIFFERENT organization) before Upsert would otherwise silently reassign
// it - see handleConnectDiscordGuild in internal/app/saas_api_discord.go.
func (r *GuildConnectionRepository) GetByGuildID(ctx context.Context, guildID int64) (*DiscordGuildConnection, error) {
	const q = `SELECT id, organization_id, guild_id, COALESCE(guild_name,''), COALESCE(guild_icon,''), bot_installed, permissions_verified, connected_at, updated_at FROM discord_guild_connections WHERE guild_id=$1`
	var c DiscordGuildConnection
	err := r.pool.QueryRow(ctx, q, guildID).
		Scan(&c.ID, &c.OrganizationID, &c.GuildID, &c.GuildName, &c.GuildIcon, &c.BotInstalled, &c.PermissionsVerified, &c.ConnectedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get guild connection by guild id: %w", err)
	}
	return &c, nil
}

// GetScoped returns one guild connection, requiring it belong to
// organizationID - the tenant-isolation-safe lookup (section 15): a caller
// can never resolve another organization's connection by ID guessing.
func (r *GuildConnectionRepository) GetScoped(ctx context.Context, organizationID, connectionID int64) (*DiscordGuildConnection, error) {
	const q = `SELECT id, organization_id, guild_id, COALESCE(guild_name,''), COALESCE(guild_icon,''), bot_installed, permissions_verified, connected_at, updated_at FROM discord_guild_connections WHERE organization_id=$1 AND id=$2`
	var c DiscordGuildConnection
	err := r.pool.QueryRow(ctx, q, organizationID, connectionID).
		Scan(&c.ID, &c.OrganizationID, &c.GuildID, &c.GuildName, &c.GuildIcon, &c.BotInstalled, &c.PermissionsVerified, &c.ConnectedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get scoped guild connection: %w", err)
	}
	return &c, nil
}
