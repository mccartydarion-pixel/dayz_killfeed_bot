package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ServerNameRepository is the storage side of the Nitrado name sync (docs/SERVER_NAME_SYNC.md).
type ServerNameRepository struct{ pool *pgxpool.Pool }

func NewServerNameRepository(pool *pgxpool.Pool) *ServerNameRepository {
	return &ServerNameRepository{pool: pool}
}

// ServerNameTarget is one server the sync reads the Nitrado name of.
type ServerNameTarget struct {
	ServerID, GuildID int64
	OrganizationID    *int64
	ProviderServiceID string
}

// ListSyncTargets lists every active Nitrado server with a service ID. A server whose
// installations are all suspended is left out; one with no installation (connected with the
// Discord /server command only) is included.
func (r *ServerNameRepository) ListSyncTargets(ctx context.Context) ([]ServerNameTarget, error) {
	rows, err := r.pool.Query(ctx, `
SELECT gs.id, gs.guild_id, gs.organization_id, gs.provider_service_id
FROM game_servers gs
WHERE gs.active AND UPPER(gs.provider)='NITRADO' AND COALESCE(gs.provider_service_id,'')<>''
  AND (NOT EXISTS (SELECT 1 FROM installations i WHERE i.game_server_id=gs.id)
       OR EXISTS (SELECT 1 FROM installations i WHERE i.game_server_id=gs.id AND i.suspended_at IS NULL))
ORDER BY gs.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServerNameTarget
	for rows.Next() {
		var t ServerNameTarget
		if err := rows.Scan(&t.ServerID, &t.GuildID, &t.OrganizationID, &t.ProviderServiceID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RecordProviderName stores name as the server's last seen Nitrado name and, unless the owner
// typed a name in Champion, makes it the display name. It reports whether the display name
// changed. An empty name is refused (a name is never blanked); an unknown or inactive server is a
// no-op. The custom check happens in the same statement as the write, so a rename racing with the
// sync is never overwritten.
//
// The first time a Nitrado name is seen for a server whose custom name is exactly that name, the
// server stops being custom: Champion could not know at rename time that the owner had typed the
// Nitrado name.
func (r *ServerNameRepository) RecordProviderName(ctx context.Context, serverID int64, name string) (bool, error) {
	if name == "" {
		return false, errors.New("provider name is empty")
	}
	var changed bool
	err := r.pool.QueryRow(ctx, `
WITH old AS (
    SELECT id, COALESCE(display_name,'') AS display_name, display_name_custom, provider_display_name
    FROM game_servers WHERE id=$1 AND active FOR UPDATE
)
UPDATE game_servers gs SET
    provider_display_name=$2,
    display_name=CASE WHEN old.display_name_custom THEN gs.display_name ELSE $2 END,
    display_name_custom=old.display_name_custom AND NOT (old.provider_display_name IS NULL AND old.display_name=$2),
    updated_at=CASE WHEN NOT old.display_name_custom AND old.display_name<>$2 THEN NOW() ELSE gs.updated_at END
FROM old WHERE gs.id=old.id
RETURNING (NOT old.display_name_custom AND old.display_name<>$2)`, serverID, name).Scan(&changed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return changed, err
}
