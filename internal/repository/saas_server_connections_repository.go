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
