package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ChannelRoute is one (installation, feature route) -> Discord channel
// mapping. route_key is the stable internal feature identifier (KILLFEED,
// STATS_LEADERBOARDS, ...) - never a display name (see
// internal/app/saas_api_channel_routes.go's championRouteBlueprint for the
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
