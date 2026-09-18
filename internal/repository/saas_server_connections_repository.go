package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SaaSServerRepository claims and lists game_servers rows for an
// organization. It reuses the existing game_servers table (see GameServer in
// server_repository.go) instead of a separate dayz_server_connections table
// - the guild-scoped ServerRepository used by the live bot runtime is
// untouched by this type and its methods.
type SaaSServerRepository struct{ pool *pgxpool.Pool }

func NewSaaSServerRepository(pool *pgxpool.Pool) *SaaSServerRepository {
	return &SaaSServerRepository{pool: pool}
}

// ClaimForOrganization sets organization_id on an already-existing
// game_servers row, identified by guildID+providerServiceID (matching
// ServerRepository.FindByGuildAndService). The server row itself must
// already exist - created by the bot's own Nitrado connect flow - this only
// records SaaS ownership over it. Returns nil if no such server exists.
func (r *SaaSServerRepository) ClaimForOrganization(ctx context.Context, organizationID, guildID int64, providerServiceID string) (*GameServer, error) {
	const q = `
UPDATE game_servers SET organization_id=$1, updated_at=NOW()
WHERE guild_id=$2 AND provider_service_id=$3
RETURNING id, guild_id, provider, provider_service_id, game, platform, COALESCE(display_name,''), status, active, created_at, updated_at, organization_id`
	var s GameServer
	err := r.pool.QueryRow(ctx, q, organizationID, guildID, providerServiceID).
		Scan(&s.ID, &s.GuildID, &s.Provider, &s.ProviderServiceID, &s.Game, &s.Platform, &s.DisplayName, &s.Status, &s.Active, &s.CreatedAt, &s.UpdatedAt, &s.OrganizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim server for organization: %w", err)
	}
	return &s, nil
}

// UpsertForInstallation creates or updates a game_servers row for a
// customer-selected DayZ console service (section 7 of the console DayZ
// backend task): guildID is the installation's own resolved guilds.id (via
// its discord_guild_connection - see loadInstallationGuildSnowflake in
// internal/app/saas_api_discord.go), so the same unique constraint
// (guild_id, provider, provider_service_id) ServerRepository.UpsertGameServer
// already uses applies here too - this never creates a second row for a
// service the bot's own /setup flow already connected. organization_id and
// platform are always set explicitly here, unlike the guild-scoped
// UpsertGameServer (which predates the SaaS platform column's purpose).
func (r *SaaSServerRepository) UpsertForInstallation(ctx context.Context, organizationID, guildID int64, s GameServer) (*GameServer, error) {
	// organization_id is only overwritten when the existing row is
	// unclaimed (NULL) or already claimed by this same organization - never
	// when a DIFFERENT organization already owns it. Callers must check the
	// returned row's OrganizationID against the caller's organizationID and
	// treat a mismatch as a conflict (never silently proceed with a server
	// owned by someone else - section 6 of the console DayZ backend task).
	const q = `
INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, active, organization_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT(guild_id,provider,provider_service_id) DO UPDATE SET
    game=EXCLUDED.game,
    platform=EXCLUDED.platform,
    display_name=EXCLUDED.display_name,
    status=EXCLUDED.status,
    active=EXCLUDED.active,
    organization_id=CASE
        WHEN game_servers.organization_id IS NULL OR game_servers.organization_id=EXCLUDED.organization_id
        THEN EXCLUDED.organization_id
        ELSE game_servers.organization_id
    END,
    updated_at=NOW()
RETURNING id, guild_id, provider, provider_service_id, game, platform, COALESCE(display_name,''), status, active, created_at, updated_at, organization_id`
	var out GameServer
	err := r.pool.QueryRow(ctx, q, guildID, s.Provider, s.ProviderServiceID, s.Game, s.Platform, s.DisplayName, s.Status, s.Active, organizationID).
		Scan(&out.ID, &out.GuildID, &out.Provider, &out.ProviderServiceID, &out.Game, &out.Platform, &out.DisplayName, &out.Status, &out.Active, &out.CreatedAt, &out.UpdatedAt, &out.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("upsert server for installation: %w", err)
	}
	return &out, nil
}

// ListByOrganization returns every game_servers row claimed by organizationID.
func (r *SaaSServerRepository) ListByOrganization(ctx context.Context, organizationID int64) ([]GameServer, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, guild_id, provider, provider_service_id, game, platform, COALESCE(display_name,''), status, active, created_at, updated_at, organization_id FROM game_servers WHERE organization_id=$1 ORDER BY id`, organizationID)
	if err != nil {
		return nil, fmt.Errorf("list servers for organization: %w", err)
	}
	defer rows.Close()
	var out []GameServer
	for rows.Next() {
		var s GameServer
		if err := rows.Scan(&s.ID, &s.GuildID, &s.Provider, &s.ProviderServiceID, &s.Game, &s.Platform, &s.DisplayName, &s.Status, &s.Active, &s.CreatedAt, &s.UpdatedAt, &s.OrganizationID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetScoped returns one game_servers row, requiring it belong to
// organizationID (section 15 tenant isolation).
func (r *SaaSServerRepository) GetScoped(ctx context.Context, organizationID, serverID int64) (*GameServer, error) {
	const q = `SELECT id, guild_id, provider, provider_service_id, game, platform, COALESCE(display_name,''), status, active, created_at, updated_at, organization_id FROM game_servers WHERE organization_id=$1 AND id=$2`
	var s GameServer
	err := r.pool.QueryRow(ctx, q, organizationID, serverID).
		Scan(&s.ID, &s.GuildID, &s.Provider, &s.ProviderServiceID, &s.Game, &s.Platform, &s.DisplayName, &s.Status, &s.Active, &s.CreatedAt, &s.UpdatedAt, &s.OrganizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get scoped server: %w", err)
	}
	return &s, nil
}
