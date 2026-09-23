package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChannelRoute is one (installation, feature route) -> Discord channel
// mapping. route_key is the stable internal feature identifier (KILLFEED,
// STATS_LEADERBOARDS, ...) - never a display name (see
// internal/app/saas_channel_layout.go's championDestinations for the
// full set). ManagedByChampion is true only for a channel Champion itself
// created or reused via one-click auto-setup; false for a channel the
// customer explicitly pointed a route at via a manual save - this is the
// signal any future "reset Champion channels" feature must use to decide
// what it may safely delete (never a customer-owned channel).
type ChannelRoute struct {
	InstallationID       int64
	RouteKey, ChannelID  string
	ManagedByChampion    bool
	CreatedAt, UpdatedAt time.Time
}

type ChannelRouteRepository struct{ pool *pgxpool.Pool }

func NewChannelRouteRepository(pool *pgxpool.Pool) *ChannelRouteRepository {
	return &ChannelRouteRepository{pool: pool}
}

// ListForInstallation returns every configured route for installationID,
// requiring it belong to organizationID (section 15 tenant isolation -
// same pattern as InstallationRepository.GetSettings).
func (r *ChannelRouteRepository) ListForInstallation(ctx context.Context, organizationID, installationID int64) ([]ChannelRoute, error) {
	const q = `
SELECT cr.installation_id, cr.route_key, cr.channel_id, cr.managed_by_champion, cr.created_at, cr.updated_at
FROM installation_channel_routes cr
JOIN installations i ON i.id = cr.installation_id
WHERE i.organization_id=$1 AND cr.installation_id=$2
ORDER BY cr.route_key`
	rows, err := r.pool.Query(ctx, q, organizationID, installationID)
	if err != nil {
		return nil, fmt.Errorf("list channel routes: %w", err)
	}
	defer rows.Close()
	var out []ChannelRoute
	for rows.Next() {
		var cr ChannelRoute
		if err := rows.Scan(&cr.InstallationID, &cr.RouteKey, &cr.ChannelID, &cr.ManagedByChampion, &cr.CreatedAt, &cr.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, cr)
	}
	return out, rows.Err()
}

// UpsertRoute creates or updates one route, requiring installationID belong
// to organizationID. Used by both one-click auto-setup (managedByChampion
// always true) and the manual save endpoint (managedByChampion always
// false - an explicit customer selection is never treated as Champion-owned,
// even if it happens to match a previously Champion-created channel ID).
func (r *ChannelRouteRepository) UpsertRoute(ctx context.Context, organizationID, installationID int64, routeKey, channelID string, managedByChampion bool) error {
	const q = `
INSERT INTO installation_channel_routes (installation_id, route_key, channel_id, managed_by_champion)
SELECT $2, $3, $4, $5
WHERE EXISTS (SELECT 1 FROM installations WHERE id=$2 AND organization_id=$1)
ON CONFLICT (installation_id, route_key) DO UPDATE SET
    channel_id=EXCLUDED.channel_id, managed_by_champion=EXCLUDED.managed_by_champion, updated_at=NOW()`
	tag, err := r.pool.Exec(ctx, q, organizationID, installationID, routeKey, channelID, managedByChampion)
	if err != nil {
		return fmt.Errorf("upsert channel route: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("upsert channel route: no matching installation for organization %d id %d", organizationID, installationID)
	}
	return nil
}

// DeleteRoute removes one route (section 9 "disable optional routes"),
// requiring installationID belong to organizationID. A no-op, not an error,
// if the route was never configured.
func (r *ChannelRouteRepository) DeleteRoute(ctx context.Context, organizationID, installationID int64, routeKey string) error {
	const q = `
DELETE FROM installation_channel_routes cr
USING installations i
WHERE i.id = cr.installation_id AND i.organization_id=$1 AND cr.installation_id=$2 AND cr.route_key=$3`
	if _, err := r.pool.Exec(ctx, q, organizationID, installationID, routeKey); err != nil {
		return fmt.Errorf("delete channel route: %w", err)
	}
	return nil
}

// InstallationRef identifies one installation.
type InstallationRef struct{ OrganizationID, InstallationID int64 }

// ListInstallationsForGuild returns every installation connected to the
// Discord guild with row id guildRowID (Discord /setup applies the channel
// layout to each of them).
func (r *ChannelRouteRepository) ListInstallationsForGuild(ctx context.Context, guildRowID int64) ([]InstallationRef, error) {
	const q = `
SELECT i.organization_id, i.id
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
WHERE c.guild_id = $1 AND c.organization_id = i.organization_id
ORDER BY i.id`
	rows, err := r.pool.Query(ctx, q, guildRowID)
	if err != nil {
		return nil, fmt.Errorf("list installations for guild: %w", err)
	}
	defer rows.Close()
	var out []InstallationRef
	for rows.Next() {
		var ref InstallationRef
		if err := rows.Scan(&ref.OrganizationID, &ref.InstallationID); err != nil {
			return nil, fmt.Errorf("scan installation ref: %w", err)
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// ListDistinctChannelIDs returns every unique Discord channel ID referenced
// by any configured route for installationID (section 18 - Step 6
// permission-verification readiness: several route keys sharing one channel
// must only be verified once).
func (r *ChannelRouteRepository) ListDistinctChannelIDs(ctx context.Context, organizationID, installationID int64) ([]string, error) {
	const q = `
SELECT DISTINCT cr.channel_id
FROM installation_channel_routes cr
JOIN installations i ON i.id = cr.installation_id
WHERE i.organization_id=$1 AND cr.installation_id=$2
ORDER BY cr.channel_id`
	rows, err := r.pool.Query(ctx, q, organizationID, installationID)
	if err != nil {
		return nil, fmt.Errorf("list distinct channel route ids: %w", err)
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

// ResolveChannel is the runtime lookup: the channel configured for routeKey
// on the installation that owns (guildRowID, serverID) - guildRowID being
// the internal guilds.id and serverID the game_servers.id. It is one joined
// query (installation -> guild connection -> game server -> route), and is
// deliberately NOT organization-parameterised like the dashboard reads:
// the runtime has no acting user, so tenant isolation is enforced
// structurally instead - the server must belong to the same guild as the
// connection, and a server already claimed by an organization must be
// claimed by the SAME organization that owns the installation and the
// connection. A row failing any of those checks never resolves.
// found=false with a nil error means no route is configured.
func (r *ChannelRouteRepository) ResolveChannel(ctx context.Context, guildRowID, serverID int64, routeKey string) (string, bool, error) {
	const q = `
SELECT cr.channel_id
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN game_servers gs ON gs.id = i.game_server_id
JOIN installation_channel_routes cr ON cr.installation_id = i.id
WHERE c.guild_id = $1
  AND i.game_server_id = $2
  AND cr.route_key = $3
  AND gs.guild_id = c.guild_id
  AND c.organization_id = i.organization_id
  AND (gs.organization_id IS NULL OR gs.organization_id = i.organization_id)
ORDER BY i.id
LIMIT 1`
	var channelID string
	err := r.pool.QueryRow(ctx, q, guildRowID, serverID, routeKey).Scan(&channelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve channel route: %w", err)
	}
	return channelID, channelID != "", nil
}
