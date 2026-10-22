package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FactionHubMembershipRepository answers which Faction Hub faction players belong to on a game
// server, through their verified gamertag link (live_faction_members).
type FactionHubMembershipRepository struct{ pool *pgxpool.Pool }

func NewFactionHubMembershipRepository(pool *pgxpool.Pool) *FactionHubMembershipRepository {
	return &FactionHubMembershipRepository{pool: pool}
}

// FactionsOnServer maps each of playerIDs that is in a Faction Hub faction on the installation
// of serverID to that faction's id, in one query. Players in no faction are absent.
func (r *FactionHubMembershipRepository) FactionsOnServer(ctx context.Context, guildID, serverID int64, playerIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 || len(playerIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT DISTINCT ON (lfm.player_id) lfm.player_id, lfm.faction_id
FROM live_faction_members lfm
JOIN installations i ON i.id = lfm.installation_id AND i.game_server_id = $2
WHERE lfm.guild_id = $1 AND lfm.player_id = ANY($3)
ORDER BY lfm.player_id, lfm.faction_id`, guildID, serverID, playerIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var player, faction int64
		if err := rows.Scan(&player, &faction); err != nil {
			return nil, err
		}
		out[player] = faction
	}
	return out, rows.Err()
}
