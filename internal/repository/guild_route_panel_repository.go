package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GuildRoutePanel records that the persistent panel for (guild, routeKey)
// currently lives as MessageID in ChannelID.
type GuildRoutePanel struct {
	ChannelID, MessageID string
}

// GuildRoutePanelRepository persists which message holds a routed panel in
// which channel, so restarts and route changes never post a second copy.
type GuildRoutePanelRepository struct{ pool *pgxpool.Pool }

func NewGuildRoutePanelRepository(pool *pgxpool.Pool) *GuildRoutePanelRepository {
	return &GuildRoutePanelRepository{pool: pool}
}

// List returns every panel message recorded for (guildRowID, routeKey).
func (r *GuildRoutePanelRepository) List(ctx context.Context, guildRowID int64, routeKey string) ([]GuildRoutePanel, error) {
	rows, err := r.pool.Query(ctx, `SELECT channel_id, message_id FROM guild_route_panels WHERE guild_id=$1 AND route_key=$2 ORDER BY channel_id`, guildRowID, routeKey)
	if err != nil {
		return nil, fmt.Errorf("list guild route panels: %w", err)
	}
	defer rows.Close()
	var out []GuildRoutePanel
	for rows.Next() {
		var p GuildRoutePanel
		if err := rows.Scan(&p.ChannelID, &p.MessageID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Upsert records (or replaces) the panel message for one routed channel.
func (r *GuildRoutePanelRepository) Upsert(ctx context.Context, guildRowID int64, routeKey, channelID, messageID string) error {
	const q = `
INSERT INTO guild_route_panels (guild_id, route_key, channel_id, message_id)
VALUES ($1,$2,$3,$4)
ON CONFLICT (guild_id, route_key, channel_id) DO UPDATE SET message_id=EXCLUDED.message_id, updated_at=NOW()`
	if _, err := r.pool.Exec(ctx, q, guildRowID, routeKey, channelID, messageID); err != nil {
		return fmt.Errorf("upsert guild route panel: %w", err)
	}
	return nil
}

// Delete forgets the panel recorded for one routed channel (a no-op if none).
func (r *GuildRoutePanelRepository) Delete(ctx context.Context, guildRowID int64, routeKey, channelID string) error {
	if _, err := r.pool.Exec(ctx, `DELETE FROM guild_route_panels WHERE guild_id=$1 AND route_key=$2 AND channel_id=$3`, guildRowID, routeKey, channelID); err != nil {
		return fmt.Errorf("delete guild route panel: %w", err)
	}
	return nil
}
