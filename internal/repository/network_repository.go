package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NetworkRepository reads the cross-server network (docs/NETWORK.md): a public directory of the
// installations that opted in (installation_feature_settings.network_listed) and leaderboards
// across them. Every query starts from the listed set, so nothing about an unlisted installation
// - not its name, not a single kill - can appear in any result.
//
// Across servers a player is the pair (platform, DayZ id): the ADM id is the same for one account
// on every server of a platform. Servers that are not listed contribute nothing to a player's total.
type NetworkRepository struct{ pool *pgxpool.Pool }

func NewNetworkRepository(pool *pgxpool.Pool) *NetworkRepository {
	return &NetworkRepository{pool: pool}
}

// listedServers is the opted-in installations with a server selected: the root of every query here.
const listedServers = `
SELECT i.id AS installation_id, gs.id AS server_id, gs.guild_id, COALESCE(NULLIF(gs.display_name, ''), 'DayZ Server') AS server_name,
       gs.platform, s.network_description, COALESCE(c.guild_name, '') AS guild_name
FROM installation_feature_settings s
JOIN installations i ON i.id = s.installation_id AND i.game_server_id IS NOT NULL
JOIN game_servers gs ON gs.id = i.game_server_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
WHERE s.network_listed`

// NetworkServer is one listed server and its public activity figures.
type NetworkServer struct {
	InstallationID  int64
	ServerID        int64
	GuildID         int64
	Name            string
	Platform        string
	Description     string
	DiscordName     string
	PlayersOnline   int // connected and observed in the last five minutes
	Peak24h         int
	ActivePlayers7d int
	Kills7d         int
	TotalKills      int
	TrackedPlayers  int
	LastActivityAt  *time.Time
}

const networkServerSelect = `
SELECT l.installation_id, l.server_id, l.guild_id, l.server_name, l.platform, l.network_description, l.guild_name,
  (SELECT COUNT(*) FROM player_server_activity a WHERE a.server_id=l.server_id AND a.currently_connected AND a.last_observed_at > $1::timestamptz - interval '5 minutes')::int,
  COALESCE((SELECT MAX(peak_players) FROM server_hourly_activity h WHERE h.server_id=l.server_id AND h.hour > $1::timestamptz - interval '24 hours'), 0)::int,
  (SELECT COUNT(DISTINCT player_id) FROM player_daily_activity d WHERE d.server_id=l.server_id AND d.day > ($1::timestamptz AT TIME ZONE 'UTC')::date - 7)::int,
  (SELECT COUNT(*) FROM kills k WHERE k.guild_id=l.guild_id AND k.server_id=l.server_id AND k.created_at > $1::timestamptz - interval '7 days')::int,
  (SELECT COUNT(*) FROM kills k WHERE k.guild_id=l.guild_id AND k.server_id=l.server_id)::int,
  (SELECT COUNT(*) FROM player_server_activity a WHERE a.server_id=l.server_id)::int,
  (SELECT MAX(last_seen_at) FROM player_server_activity a WHERE a.server_id=l.server_id)
FROM (` + listedServers + `) l`

func scanNetworkServer(row pgx.Row) (NetworkServer, error) {
	var s NetworkServer
	err := row.Scan(&s.InstallationID, &s.ServerID, &s.GuildID, &s.Name, &s.Platform, &s.Description, &s.DiscordName,
		&s.PlayersOnline, &s.Peak24h, &s.ActivePlayers7d, &s.Kills7d, &s.TotalKills, &s.TrackedPlayers, &s.LastActivityAt)
	return s, err
}

// Servers returns every listed server, busiest first (active players this week, then kills).
func (r *NetworkRepository) Servers(ctx context.Context, now time.Time, platform string, limit int) ([]NetworkServer, error) {
	rows, err := r.pool.Query(ctx, networkServerSelect+` WHERE ($2 = '' OR l.platform = $2) ORDER BY 10 DESC, 11 DESC, l.installation_id LIMIT $3`,
		now.UTC(), platform, clampLimit(limit, 50, 200))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NetworkServer{}
	for rows.Next() {
		s, err := scanNetworkServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Server returns one listed server, or (nil, nil) when the installation is not listed.
func (r *NetworkRepository) Server(ctx context.Context, now time.Time, installationID int64) (*NetworkServer, error) {
	s, err := scanNetworkServer(r.pool.QueryRow(ctx, networkServerSelect+` WHERE l.installation_id = $2`, now.UTC(), installationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// NetworkRank is one leaderboard row. Value is kills, metres or seconds depending on the board.
type NetworkRank struct {
	Platform   string
	PlayerName string
	Value      float64
	Servers    int    // KILLS: listed servers the kills were made on
	ServerName string // LONGEST_KILL / LONGEST_LIFE: where it happened
	Weapon     string // LONGEST_KILL
	At         *time.Time
}

// Network leaderboards.
const (
	NetworkBoardKills       = "KILLS"
	NetworkBoardLongestKill = "LONGEST_KILL"
	NetworkBoardLongestLife = "LONGEST_LIFE"
)

// Leaderboard ranks players across the listed servers. installationID > 0 restricts the board to
// that one listed server; platform "" means every platform; since nil means all time.
func (r *NetworkRepository) Leaderboard(ctx context.Context, board, platform string, installationID int64, since *time.Time, limit int) ([]NetworkRank, error) {
	listed := `(` + listedServers + `) l`
	scope := ` ($1 = '' OR l.platform = $1) AND ($2::bigint = 0 OR l.installation_id = $2) `
	var q string
	switch board {
	case NetworkBoardKills:
		// One row per (platform, DayZ id): the same account on several listed servers adds up, and
		// carries the name it was most recently seen under.
		q = `
SELECT l.platform, (ARRAY_AGG(p.display_name ORDER BY p.last_seen_at DESC, p.id DESC))[1], COUNT(*)::float8, COUNT(DISTINCT l.server_id)::int, '', '', NULL::timestamptz
FROM ` + listed + `
JOIN kills k ON k.guild_id = l.guild_id AND k.server_id = l.server_id
JOIN players p ON p.id = k.killer_player_id
WHERE` + scope + `AND k.victim_player_id IS DISTINCT FROM k.killer_player_id
  AND ($3::timestamptz IS NULL OR COALESCE(k.event_time, k.created_at) >= $3)
GROUP BY l.platform, p.dayz_player_id
ORDER BY 3 DESC, 2 LIMIT $4`
	case NetworkBoardLongestKill:
		q = `
SELECT l.platform, p.display_name, k.distance::float8, 1, l.server_name, COALESCE(k.weapon_display, k.weapon_raw, ''), COALESCE(k.event_time, k.created_at)
FROM ` + listed + `
JOIN kills k ON k.guild_id = l.guild_id AND k.server_id = l.server_id
JOIN players p ON p.id = k.killer_player_id
WHERE` + scope + `AND k.distance IS NOT NULL AND k.victim_player_id IS DISTINCT FROM k.killer_player_id
  AND ($3::timestamptz IS NULL OR COALESCE(k.event_time, k.created_at) >= $3)
ORDER BY k.distance DESC, k.id LIMIT $4`
	case NetworkBoardLongestLife:
		q = `
SELECT l.platform, p.display_name, pl.playtime_seconds::float8, 1, l.server_name, '', pl.ended_at
FROM ` + listed + `
JOIN player_lives pl ON pl.guild_id = l.guild_id AND pl.server_id = l.server_id
JOIN players p ON p.id = pl.player_id
WHERE` + scope + `AND pl.playtime_seconds IS NOT NULL AND pl.playtime_seconds > 0
  AND ($3::timestamptz IS NULL OR pl.ended_at >= $3)
ORDER BY pl.playtime_seconds DESC, pl.id LIMIT $4`
	default:
		return nil, fmt.Errorf("unknown network board %q", board)
	}
	rows, err := r.pool.Query(ctx, q, platform, installationID, since, clampLimit(limit, 25, 100))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NetworkRank{}
	for rows.Next() {
		var n NetworkRank
		if err := rows.Scan(&n.Platform, &n.PlayerName, &n.Value, &n.Servers, &n.ServerName, &n.Weapon, &n.At); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
