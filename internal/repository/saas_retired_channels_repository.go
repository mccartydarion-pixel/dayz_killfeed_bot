package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RetiredChannel is a Champion-managed Discord channel or category that no
// route of its installation uses any more (installation_retired_channels).
// Only rows here may ever be offered for cleanup: ownership is proven by how
// the row was recorded, never by a channel's name.
type RetiredChannel struct {
	InstallationID int64
	ChannelID      string
	Kind           string // CHANNEL | CATEGORY
	Source         string // ROUTE | LEGACY_SETUP
	FormerRoutes   []string
	// LegacyField names the legacy GuildSetup field that pointed at the
	// channel (LEGACY_SETUP only), so cleanup can clear it.
	LegacyField string
	RecordedAt  time.Time
}

type RetiredChannelRepository struct{ pool *pgxpool.Pool }

func NewRetiredChannelRepository(pool *pgxpool.Pool) *RetiredChannelRepository {
	return &RetiredChannelRepository{pool: pool}
}

// Record upserts retired channels for installationID, requiring it belong to
// organizationID.
func (r *RetiredChannelRepository) Record(ctx context.Context, organizationID, installationID int64, rows []RetiredChannel) error {
	const q = `
INSERT INTO installation_retired_channels (installation_id, channel_id, kind, source, former_routes, legacy_field)
SELECT $2, $3, $4, $5, $6, $7
WHERE EXISTS (SELECT 1 FROM installations WHERE id=$2 AND organization_id=$1)
ON CONFLICT (installation_id, channel_id) DO UPDATE SET
    kind=EXCLUDED.kind, source=EXCLUDED.source, former_routes=EXCLUDED.former_routes, legacy_field=EXCLUDED.legacy_field`
	for _, row := range rows {
		routes := row.FormerRoutes
		if routes == nil {
			routes = []string{}
		}
		if _, err := r.pool.Exec(ctx, q, organizationID, installationID, row.ChannelID, row.Kind, row.Source, routes, row.LegacyField); err != nil {
			return fmt.Errorf("record retired channel: %w", err)
		}
	}
	return nil
}

// List returns installationID's retired channels.
func (r *RetiredChannelRepository) List(ctx context.Context, organizationID, installationID int64) ([]RetiredChannel, error) {
	const q = `
SELECT rc.installation_id, rc.channel_id, rc.kind, rc.source, rc.former_routes, rc.legacy_field, rc.recorded_at
FROM installation_retired_channels rc
JOIN installations i ON i.id = rc.installation_id
WHERE i.organization_id=$1 AND rc.installation_id=$2
ORDER BY rc.recorded_at, rc.id`
	rows, err := r.pool.Query(ctx, q, organizationID, installationID)
	if err != nil {
		return nil, fmt.Errorf("list retired channels: %w", err)
	}
	defer rows.Close()
	var out []RetiredChannel
	for rows.Next() {
		var rc RetiredChannel
		if err := rows.Scan(&rc.InstallationID, &rc.ChannelID, &rc.Kind, &rc.Source, &rc.FormerRoutes, &rc.LegacyField, &rc.RecordedAt); err != nil {
			return nil, fmt.Errorf("scan retired channel: %w", err)
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// Forget removes one retired channel row (after cleanup, or when a route uses
// the channel again).
func (r *RetiredChannelRepository) Forget(ctx context.Context, organizationID, installationID int64, channelID string) error {
	const q = `
DELETE FROM installation_retired_channels rc
USING installations i
WHERE i.id = rc.installation_id AND i.organization_id=$1 AND rc.installation_id=$2 AND rc.channel_id=$3`
	if _, err := r.pool.Exec(ctx, q, organizationID, installationID, channelID); err != nil {
		return fmt.Errorf("forget retired channel: %w", err)
	}
	return nil
}

// ChannelReferenced reports whether any installation's route - customer-owned
// or Champion-managed - points at channelID. A referenced channel is never
// cleaned up.
func (r *RetiredChannelRepository) ChannelReferenced(ctx context.Context, channelID string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM installation_channel_routes WHERE channel_id=$1)`, channelID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check channel references: %w", err)
	}
	return exists, nil
}
