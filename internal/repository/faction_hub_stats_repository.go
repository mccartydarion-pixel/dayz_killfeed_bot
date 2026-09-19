package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// HubStatsRepository derives Faction Hub competitive figures from Champion's authoritative
// runtime data (docs/FACTION_STATS.md). It never copies or duplicates kill, death or bounty
// events: it reads `kills`, `deaths`, `bounties`, `record_events` and `player_links` and joins
// them to membership PERIODS (hub_faction_membership_history).
//
// Attribution rules, all enforced in SQL:
//   - identity: a member's player is their VERIFIED player_links row on the installation's guild
//     (never a name match). No verified link = no attribution (the member is listed as UNLINKED
//     with zero figures);
//   - time: an event counts only while the player was a member: joined_at <= at < left_at, where
//     at = COALESCE(event_time, created_at);
//   - server: only rows whose server_id is the faction's server (an installation is a guild +
//     server pair; another server of the same guild never mixes in);
//   - a kill of a fellow member of the same faction (a team kill) is not a counted kill;
//   - dedupe is inherited: kills/deaths are UNIQUE(guild, event_fingerprint) at write time and a
//     bounty row is claimed once, so a replayed event cannot count twice.
type HubStatsRepository struct{ pool *pgxpool.Pool }

func NewHubStatsRepository(pool *pgxpool.Pool) *HubStatsRepository {
	return &HubStatsRepository{pool: pool}
}

// HubStatsScope identifies a faction's data: its guild and server context.
type HubStatsScope struct {
	FactionID, OrganizationID, InstallationID int64
	GuildID, ServerID                         int64
	CreatedAt                                 time.Time
}

// Scope resolves the faction within (organization, installation) - the tenant check every stats
// call starts with - and returns the guild and server its figures are read from.
func (r *HubStatsRepository) Scope(ctx context.Context, organizationID, installationID, factionID int64) (HubStatsScope, error) {
	var s HubStatsScope
	err := r.pool.QueryRow(ctx, `
SELECT f.id, f.organization_id, f.installation_id, c.guild_id, f.game_server_id, f.created_at
FROM hub_factions f
JOIN installations i ON i.id = f.installation_id AND i.organization_id = f.organization_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
WHERE f.id=$1 AND f.organization_id=$2 AND f.installation_id=$3`, factionID, organizationID, installationID).
		Scan(&s.FactionID, &s.OrganizationID, &s.InstallationID, &s.GuildID, &s.ServerID, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return HubStatsScope{}, factionhub.ErrNotFound
	}
	if err != nil {
		return HubStatsScope{}, fmt.Errorf("hub stats scope: %w", err)
	}
	return s, nil
}

// ListScopes pages through every faction (reconciliation), ordered by id.
func (r *HubStatsRepository) ListScopes(ctx context.Context, afterID int64, limit int) ([]HubStatsScope, error) {
	rows, err := r.pool.Query(ctx, `
SELECT f.id, f.organization_id, f.installation_id, c.guild_id, f.game_server_id, f.created_at
FROM hub_factions f
JOIN installations i ON i.id = f.installation_id AND i.organization_id = f.organization_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
WHERE f.id > $1 ORDER BY f.id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("hub stats list scopes: %w", err)
	}
	defer rows.Close()
	var out []HubStatsScope
	for rows.Next() {
		var s HubStatsScope
		if err := rows.Scan(&s.FactionID, &s.OrganizationID, &s.InstallationID, &s.GuildID, &s.ServerID, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ScopesForPlayers returns the factions whose CURRENT members include one of the given players
// (by verified link) on the given guild and server: the factions a kill by those players can
// affect. Used to decide which factions to re-evaluate after a kill.
func (r *HubStatsRepository) ScopesForPlayers(ctx context.Context, guildID, serverID int64, playerIDs []int64) ([]HubStatsScope, error) {
	if len(playerIDs) == 0 {
		return nil, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT DISTINCT f.id, f.organization_id, f.installation_id, c.guild_id, f.game_server_id, f.created_at
FROM hub_faction_members m
JOIN hub_factions f ON f.id = m.faction_id
JOIN installations i ON i.id = f.installation_id AND i.organization_id = f.organization_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id AND c.guild_id = $1
JOIN app_users u ON u.id = m.user_id
JOIN player_links pl ON pl.guild_id = $1 AND pl.discord_user_id = u.discord_user_id AND pl.status = 'VERIFIED'
WHERE f.game_server_id = $2 AND pl.player_id = ANY($3)`, guildID, serverID, playerIDs)
	if err != nil {
		return nil, fmt.Errorf("hub stats scopes for players: %w", err)
	}
	defer rows.Close()
	var out []HubStatsScope
	for rows.Next() {
		var s HubStatsScope
		if err := rows.Scan(&s.FactionID, &s.OrganizationID, &s.InstallationID, &s.GuildID, &s.ServerID, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// The shared CTE prefix. Parameters: $1 installation id, $2 guild id, $3 faction id (0 = every faction of
// the installation - the leaderboard; a faction id = one faction - the profile). ONE definition serves both,
// so a leaderboard value can never differ from the profile value.
//
//	windows  one row per membership PERIOD with the member's live VERIFIED player (NULL = unlinked)
//	wp       the distinct linked players (drives index-friendly kill/death lookups)
//	ka       kills on this guild+server in which a faction player is killer or victim
//	fk       COUNTED kills: killer in a period at that time, and the victim not a fellow member then
//	dv       COUNTED deaths (the project's death definition: rows of the deaths table)
//	vv       PvP kills in which a member was the victim (only used to break kill streaks)
//	bc       claimed bounties earned by a counted kill
const hubStatsCTE = `
windows AS (
  SELECT h.id AS wid, h.faction_id, f.game_server_id AS server_id, h.user_id, h.joined_at, h.left_at, pl.player_id
  FROM hub_faction_membership_history h
  JOIN hub_factions f ON f.id = h.faction_id
  JOIN app_users u ON u.id = h.user_id
  LEFT JOIN player_links pl ON pl.guild_id = $2 AND pl.discord_user_id = u.discord_user_id AND pl.status = 'VERIFIED'
  WHERE h.installation_id = $1 AND ($3::bigint = 0 OR h.faction_id = $3)
),
wp AS (SELECT DISTINCT player_id, server_id FROM windows WHERE player_id IS NOT NULL),
ka AS (
  SELECT k.id, k.server_id, k.killer_player_id, k.victim_player_id, COALESCE(k.event_time, k.created_at) AS at, k.headshot, k.longshot, k.distance, k.weapon_display
  FROM kills k WHERE k.guild_id = $2 AND k.server_id IN (SELECT server_id FROM wp) AND k.killer_player_id IN (SELECT player_id FROM wp)
  UNION
  SELECT k.id, k.server_id, k.killer_player_id, k.victim_player_id, COALESCE(k.event_time, k.created_at) AS at, k.headshot, k.longshot, k.distance, k.weapon_display
  FROM kills k WHERE k.guild_id = $2 AND k.server_id IN (SELECT server_id FROM wp) AND k.victim_player_id IN (SELECT player_id FROM wp)
),
fk AS (
  SELECT ka.id AS kill_id, w.wid, w.faction_id, w.user_id, w.player_id, ka.at, ka.headshot, ka.longshot, ka.distance, ka.weapon_display, ka.victim_player_id
  FROM ka
  JOIN windows w ON w.player_id = ka.killer_player_id AND w.server_id = ka.server_id AND ka.at >= w.joined_at AND (w.left_at IS NULL OR ka.at < w.left_at)
  WHERE NOT EXISTS (
    SELECT 1 FROM windows v
    WHERE v.faction_id = w.faction_id AND v.player_id = ka.victim_player_id AND ka.at >= v.joined_at AND (v.left_at IS NULL OR ka.at < v.left_at))
),
dv AS (
  SELECT d.id AS death_id, w.wid, w.faction_id, w.user_id, COALESCE(d.event_time, d.created_at) AS at
  FROM deaths d
  JOIN windows w ON w.player_id = d.player_id AND w.server_id = d.server_id
   AND COALESCE(d.event_time, d.created_at) >= w.joined_at AND (w.left_at IS NULL OR COALESCE(d.event_time, d.created_at) < w.left_at)
  WHERE d.guild_id = $2 AND d.server_id IN (SELECT server_id FROM wp) AND d.player_id IN (SELECT player_id FROM wp)
),
vv AS (
  SELECT ka.id AS kill_id, w.wid, w.faction_id, ka.at
  FROM ka JOIN windows w ON w.player_id = ka.victim_player_id AND w.server_id = ka.server_id AND ka.at >= w.joined_at AND (w.left_at IS NULL OR ka.at < w.left_at)
),
bc AS (
  SELECT b.id AS bounty_id, fk.kill_id, fk.wid, fk.faction_id, fk.user_id, fk.at, b.reward_points
  FROM bounties b
  JOIN fk ON fk.kill_id = b.claimed_kill_id AND fk.player_id = b.claimed_by_player_id
  WHERE b.guild_id = $2 AND b.status = 'CLAIMED'
)`

// hubStreakCTE adds the streak CTEs to hubStatsCTE. A streak is the run of counted kills between
// two death events (a death row, or being the victim of a PvP kill) INSIDE one membership period:
// a streak never carries across a leave/rejoin. At the same instant a kill sorts before a death.
const hubStreakCTE = `,
sev AS (
  SELECT wid, at, 0 AS kind, kill_id AS id FROM fk
  UNION ALL SELECT wid, at, 1, death_id FROM dv
  UNION ALL SELECT wid, at, 1, kill_id FROM vv
),
sg AS (
  SELECT wid, kind, SUM(kind) OVER (PARTITION BY wid ORDER BY at, kind, id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS g FROM sev
),
sl AS (SELECT wid, g, COUNT(*) FILTER (WHERE kind = 0) AS len FROM sg GROUP BY wid, g),
cur AS (
  SELECT DISTINCT ON (sl.wid) sl.wid, sl.len
  FROM sl JOIN windows w ON w.wid = sl.wid AND w.left_at IS NULL
  ORDER BY sl.wid, sl.g DESC
)`

// HubMemberStats is one member's (or former member's) counted figures.
type HubMemberStats struct {
	UserID                                      int64
	DiscordUserID, Username, GlobalName, Avatar string
	MemberID                                    *int64  // current membership id; nil for a former member
	Role                                        *string // current role; nil for a former member
	Active                                      bool
	Linked                                      bool // a VERIFIED player link exists on this guild
	Gamertag                                    *string
	JoinedAt                                    time.Time // start of the current (or latest) membership period
	Kills, Deaths, Headshots, Longshots         int64
	Bounties, BountyValue                       int64
	BestStreak, CurrentStreak                   int
}

// HubStatsResult is everything the stats endpoints need, from one grouped query.
type HubStatsResult struct {
	Members       []HubMemberStats
	MemberCount   int
	LinkedCount   int
	TrackingSince *time.Time
}

// ComputeStats returns the per-member counted figures of one faction. It runs ONE grouped
// statement (no per-member queries); the faction summary is the sum of the rows plus the best/
// current streak maxima, computed by the service.
func (r *HubStatsRepository) ComputeStats(ctx context.Context, s HubStatsScope) (*HubStatsResult, error) {
	rows, err := r.pool.Query(ctx, `WITH `+hubStatsCTE+hubStreakCTE+`,
uk AS (SELECT user_id, COUNT(*) AS kills, COUNT(*) FILTER (WHERE headshot) AS hs, COUNT(*) FILTER (WHERE longshot) AS ls FROM fk GROUP BY user_id),
ud AS (SELECT user_id, COUNT(*) AS deaths FROM dv GROUP BY user_id),
ub AS (SELECT user_id, COUNT(*) AS n, COALESCE(SUM(reward_points),0) AS v FROM bc GROUP BY user_id),
us AS (SELECT w.user_id, MAX(sl.len) AS best FROM sl JOIN windows w ON w.wid = sl.wid GROUP BY w.user_id),
uc AS (SELECT w.user_id, MAX(cur.len) AS cur FROM cur JOIN windows w ON w.wid = cur.wid GROUP BY w.user_id),
uw AS (SELECT user_id, MAX(joined_at) AS last_joined, MAX(player_id) AS player_id FROM windows GROUP BY user_id),
everyone AS (SELECT user_id FROM windows UNION SELECT user_id FROM hub_faction_members WHERE faction_id = $3)
SELECT u.id, u.discord_user_id, u.discord_username, COALESCE(u.discord_global_name,''), COALESCE(u.avatar,''),
       m.id, m.role_key, (m.id IS NOT NULL) AS active,
       (pl.player_id IS NOT NULL) AS linked, p.display_name,
       COALESCE(m.joined_at, uw.last_joined, $4::timestamptz) AS joined_at,
       COALESCE(uk.kills,0), COALESCE(ud.deaths,0), COALESCE(uk.hs,0), COALESCE(uk.ls,0),
       COALESCE(ub.n,0), COALESCE(ub.v,0), COALESCE(us.best,0), COALESCE(uc.cur,0)
FROM everyone e
JOIN app_users u ON u.id = e.user_id
LEFT JOIN hub_faction_members m ON m.faction_id = $3 AND m.user_id = e.user_id
LEFT JOIN uw ON uw.user_id = e.user_id
LEFT JOIN player_links pl ON pl.guild_id = $2 AND pl.discord_user_id = u.discord_user_id AND pl.status = 'VERIFIED'
LEFT JOIN players p ON p.id = pl.player_id
LEFT JOIN uk ON uk.user_id = e.user_id
LEFT JOIN ud ON ud.user_id = e.user_id
LEFT JOIN ub ON ub.user_id = e.user_id
LEFT JOIN us ON us.user_id = e.user_id
LEFT JOIN uc ON uc.user_id = e.user_id
WHERE m.id IS NOT NULL OR COALESCE(uk.kills,0) + COALESCE(ud.deaths,0) + COALESCE(ub.n,0) > 0
ORDER BY COALESCE(uk.kills,0) DESC, COALESCE(ud.deaths,0) ASC, m.id ASC NULLS LAST, u.id ASC`,
		s.InstallationID, s.GuildID, s.FactionID, s.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("hub faction stats: %w", err)
	}
	defer rows.Close()
	out := &HubStatsResult{Members: []HubMemberStats{}}
	for rows.Next() {
		var m HubMemberStats
		if err := rows.Scan(&m.UserID, &m.DiscordUserID, &m.Username, &m.GlobalName, &m.Avatar, &m.MemberID, &m.Role, &m.Active, &m.Linked, &m.Gamertag,
			&m.JoinedAt, &m.Kills, &m.Deaths, &m.Headshots, &m.Longshots, &m.Bounties, &m.BountyValue, &m.BestStreak, &m.CurrentStreak); err != nil {
			return nil, fmt.Errorf("hub faction stats scan: %w", err)
		}
		if m.Active {
			out.MemberCount++
			if m.Linked {
				out.LinkedCount++
			}
		}
		out.Members = append(out.Members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var since *time.Time
	if err := r.pool.QueryRow(ctx, `SELECT MIN(joined_at) FROM hub_faction_membership_history WHERE faction_id=$1`, s.FactionID).Scan(&since); err != nil {
		return nil, fmt.Errorf("hub stats tracking since: %w", err)
	}
	out.TrackingSince = since
	return out, nil
}

// --- achievements ---------------------------------------------------------------------------

// HubUnlock is one recorded achievement unlock.
type HubUnlock struct {
	Key        string
	UnlockedAt time.Time
	Metadata   map[string]any
}

// Unlocks returns the faction's recorded unlocks.
func (r *HubStatsRepository) Unlocks(ctx context.Context, factionID int64) (map[string]HubUnlock, error) {
	rows, err := r.pool.Query(ctx, `SELECT achievement_key, unlocked_at, metadata_json FROM hub_faction_achievement_unlocks WHERE faction_id=$1`, factionID)
	if err != nil {
		return nil, fmt.Errorf("hub unlocks: %w", err)
	}
	defer rows.Close()
	out := map[string]HubUnlock{}
	for rows.Next() {
		var u HubUnlock
		var raw []byte
		if err := rows.Scan(&u.Key, &u.UnlockedAt, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &u.Metadata)
		}
		out[u.Key] = u
	}
	return out, rows.Err()
}

// InsertUnlock records an unlock exactly once: the (faction, achievement) unique key makes
// concurrent evaluators converge on one row. inserted is true only for the caller that won.
func (r *HubStatsRepository) InsertUnlock(ctx context.Context, s HubStatsScope, key string, at time.Time, metadata map[string]any) (inserted bool, err error) {
	var raw []byte
	if metadata != nil {
		if raw, err = json.Marshal(metadata); err != nil {
			return false, err
		}
	}
	var id int64
	err = r.pool.QueryRow(ctx, `
INSERT INTO hub_faction_achievement_unlocks(organization_id, installation_id, faction_id, achievement_key, unlocked_at, metadata_json)
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT (faction_id, achievement_key) DO NOTHING
RETURNING id`, s.OrganizationID, s.InstallationID, s.FactionID, key, at, raw).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("hub insert unlock: %w", err)
	}
	return true, nil
}

// NthEventTime returns when the faction's n-th counted event of a kind happened (the moment a
// count achievement was really earned): metric is "kills", "headshots", "longshots" or "bounties".
// nil when there are fewer than n events.
func (r *HubStatsRepository) NthEventTime(ctx context.Context, s HubStatsScope, metric string, n int) (*time.Time, error) {
	var q string
	switch metric {
	case "kills":
		q = `SELECT at FROM fk ORDER BY at, kill_id OFFSET $4 LIMIT 1`
	case "headshots":
		q = `SELECT at FROM fk WHERE headshot ORDER BY at, kill_id OFFSET $4 LIMIT 1`
	case "longshots":
		q = `SELECT at FROM fk WHERE longshot ORDER BY at, kill_id OFFSET $4 LIMIT 1`
	case "bounties":
		q = `SELECT at FROM bc ORDER BY at, bounty_id OFFSET $4 LIMIT 1`
	default:
		return nil, fmt.Errorf("unknown metric %q", metric)
	}
	var at time.Time
	err := r.pool.QueryRow(ctx, `WITH `+hubStatsCTE+` `+q, s.InstallationID, s.GuildID, s.FactionID, n-1).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hub nth event time: %w", err)
	}
	return &at, nil
}

// --- activity --------------------------------------------------------------------------------

// Activity source ranks: they break ties between events at the same instant (keyset order is
// occurred_at, source, id - all descending).
const (
	ActivitySrcHub    = 1
	ActivitySrcKill   = 2
	ActivitySrcBounty = 3
	ActivitySrcRecord = 4
	ActivitySrcUnlock = 5
)

// HubActivityCursor is the keyset position of the last event of the previous page.
type HubActivityCursor struct {
	At  time.Time
	Src int
	ID  int64
}

// HubActivityRow is one raw activity event; the service turns it into the public DTO.
type HubActivityRow struct {
	At   time.Time
	Src  int
	ID   int64
	Type string

	// The member the event is about (identity resolved through the live verified link), and the
	// actor for events with one (leadership transfer: the previous leader).
	SubjectDiscordID, SubjectUsername, SubjectGlobalName, SubjectAvatar string
	SubjectGamertag                                                     *string
	ActorUsername, ActorGlobalName                                      *string

	Detail        *string  // a role key for role events
	VictimName    *string  // kills, bounties
	Weapon        *string  // kills
	Distance      *float64 // kills, metres
	Headshot      bool
	Longshot      bool
	RewardPoints  int64 // bounties: total for the kill
	BountyCount   int64 // bounties: how many stacked bounties the kill claimed
	RecordType    *string
	AchievementID *string
}

// Activity returns up to limit events newest first, after cursor (nil = first page), and
// whether more exist. Combat events (kills, bounty claims, server records) are derived from the
// authoritative tables through the SAME attribution rules as the stats; hub events and
// achievement unlocks come from their own tables. Each source is limited to limit+1 rows before
// the merge, so a page never scans a faction's whole history.
func (r *HubStatsRepository) Activity(ctx context.Context, s HubStatsScope, limit int, cursor *HubActivityCursor) (rows []HubActivityRow, more bool, err error) {
	var cAt *time.Time
	var cSrc, cID int64
	if cursor != nil {
		cAt, cSrc, cID = &cursor.At, int64(cursor.Src), cursor.ID
	}
	// $4 cursor time (NULL = none), $5 cursor source, $6 cursor id, $7 per-source limit.
	const q = `WITH ` + hubStatsCTE + `,
hub AS (
  SELECT a.occurred_at AS at, 1 AS src, a.id AS id, a.event_type AS type, a.subject_user_id AS subj, a.actor_user_id AS actor, a.detail AS detail,
         NULL::text AS victim, NULL::text AS weapon, NULL::double precision AS distance, FALSE AS headshot, FALSE AS longshot,
         0::bigint AS reward, 0::bigint AS n, NULL::text AS rtype, NULL::text AS akey
  FROM hub_faction_activity a
  WHERE a.faction_id = $3 AND ($4::timestamptz IS NULL OR (a.occurred_at, 1, a.id) < ($4, $5::int, $6::bigint))
  ORDER BY a.occurred_at DESC, a.id DESC LIMIT $7
),
kev AS (
  SELECT fk.at AS at, 2 AS src, fk.kill_id AS id,
         CASE WHEN fk.headshot THEN 'HEADSHOT' WHEN fk.longshot THEN 'LONGSHOT' ELSE 'KILL' END AS type,
         fk.user_id AS subj, NULL::bigint AS actor, NULL::text AS detail,
         vp.display_name AS victim, fk.weapon_display AS weapon, fk.distance AS distance, fk.headshot AS headshot, fk.longshot AS longshot,
         0::bigint AS reward, 0::bigint AS n, NULL::text AS rtype, NULL::text AS akey
  FROM fk LEFT JOIN players vp ON vp.id = fk.victim_player_id
  WHERE ($4::timestamptz IS NULL OR (fk.at, 2, fk.kill_id) < ($4, $5::int, $6::bigint))
  ORDER BY fk.at DESC, fk.kill_id DESC LIMIT $7
),
bev AS (
  SELECT bc.at AS at, 3 AS src, bc.kill_id AS id, 'BOUNTY_CLAIMED' AS type, bc.user_id AS subj, NULL::bigint AS actor, NULL::text AS detail,
         MAX(vp.display_name) AS victim, NULL::text AS weapon, NULL::double precision AS distance, FALSE AS headshot, FALSE AS longshot,
         SUM(bc.reward_points)::bigint AS reward, COUNT(*)::bigint AS n, NULL::text AS rtype, NULL::text AS akey
  FROM bc
  JOIN fk ON fk.kill_id = bc.kill_id
  LEFT JOIN players vp ON vp.id = fk.victim_player_id
  WHERE ($4::timestamptz IS NULL OR (bc.at, 3, bc.kill_id) < ($4, $5::int, $6::bigint))
  GROUP BY bc.at, bc.kill_id, bc.user_id
  ORDER BY bc.at DESC, bc.kill_id DESC LIMIT $7
),
rev AS (
  SELECT re.created_at AS at, 4 AS src, re.id AS id, 'SERVER_RECORD' AS type, w.user_id AS subj, NULL::bigint AS actor, NULL::text AS detail,
         NULL::text AS victim, NULL::text AS weapon, NULL::double precision AS distance, FALSE AS headshot, FALSE AS longshot,
         0::bigint AS reward, 0::bigint AS n, re.record_type AS rtype, NULL::text AS akey
  FROM record_events re
  JOIN windows w ON w.player_id = re.player_id AND re.created_at >= w.joined_at AND (w.left_at IS NULL OR re.created_at < w.left_at)
  JOIN kills rk ON rk.id = re.kill_id AND rk.guild_id = $2 AND rk.server_id = w.server_id
  WHERE re.guild_id = $2 AND re.player_id IN (SELECT player_id FROM wp)
    AND ($4::timestamptz IS NULL OR (re.created_at, 4, re.id) < ($4, $5::int, $6::bigint))
  ORDER BY re.created_at DESC, re.id DESC LIMIT $7
),
uev AS (
  SELECT u.unlocked_at AS at, 5 AS src, u.id AS id, 'ACHIEVEMENT_UNLOCKED' AS type, NULL::bigint AS subj, NULL::bigint AS actor, NULL::text AS detail,
         NULL::text AS victim, NULL::text AS weapon, NULL::double precision AS distance, FALSE AS headshot, FALSE AS longshot,
         0::bigint AS reward, 0::bigint AS n, NULL::text AS rtype, u.achievement_key AS akey
  FROM hub_faction_achievement_unlocks u
  WHERE u.faction_id = $3 AND ($4::timestamptz IS NULL OR (u.unlocked_at, 5, u.id) < ($4, $5::int, $6::bigint))
  ORDER BY u.unlocked_at DESC, u.id DESC LIMIT $7
),
allev AS (
  SELECT * FROM hub UNION ALL SELECT * FROM kev UNION ALL SELECT * FROM bev UNION ALL SELECT * FROM rev UNION ALL SELECT * FROM uev
)
SELECT x.at, x.src, x.id, x.type,
       COALESCE(su.discord_user_id,''), COALESCE(su.discord_username,''), COALESCE(su.discord_global_name,''), COALESCE(su.avatar,''), sp.display_name,
       au.discord_username, au.discord_global_name,
       x.detail, x.victim, x.weapon, x.distance, x.headshot, x.longshot, x.reward, x.n, x.rtype, x.akey
FROM allev x
LEFT JOIN app_users su ON su.id = x.subj
LEFT JOIN player_links spl ON spl.guild_id = $2 AND spl.discord_user_id = su.discord_user_id AND spl.status = 'VERIFIED'
LEFT JOIN players sp ON sp.id = spl.player_id
LEFT JOIN app_users au ON au.id = x.actor
ORDER BY x.at DESC, x.src DESC, x.id DESC
LIMIT $8`
	res, err := r.pool.Query(ctx, q, s.InstallationID, s.GuildID, s.FactionID, cAt, cSrc, cID, limit+1, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("hub faction activity: %w", err)
	}
	defer res.Close()
	for res.Next() {
		var a HubActivityRow
		if err := res.Scan(&a.At, &a.Src, &a.ID, &a.Type, &a.SubjectDiscordID, &a.SubjectUsername, &a.SubjectGlobalName, &a.SubjectAvatar, &a.SubjectGamertag,
			&a.ActorUsername, &a.ActorGlobalName, &a.Detail, &a.VictimName, &a.Weapon, &a.Distance, &a.Headshot, &a.Longshot, &a.RewardPoints, &a.BountyCount,
			&a.RecordType, &a.AchievementID); err != nil {
			return nil, false, fmt.Errorf("hub faction activity scan: %w", err)
		}
		rows = append(rows, a)
	}
	if err := res.Err(); err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		rows, more = rows[:limit], true
	}
	return rows, more, nil
}

// --- leaderboard ---------------------------------------------------------------------------------

// HubLeaderboardRow is one faction's public metadata and every leaderboard metric, computed with the
// SAME attribution CTE as the faction profile (hubStatsCTE with faction filter 0), so a leaderboard
// value cannot differ from the profile value.
type HubLeaderboardRow struct {
	FactionID                                                  int64
	Name, Tag, Slug                                            string
	FlagKey, ArmbandKey, PrimaryColor, SecondaryColor          *string
	Logo                                                       *factionhub.Asset
	MemberCount                                                int
	Kills, Deaths, Headshots, Longshots, Bounties, BountyValue int64
	BestStreak                                                 int
	Achievements                                               int
	TrackingSince                                              *time.Time
}

// HubLeaderboardData is every faction of one installation with its figures.
type HubLeaderboardData struct {
	GuildID, ServerID int64 // the installation's guild and current server (cache invalidation scope)
	Rows              []HubLeaderboardRow
}

// Leaderboard returns every faction of the installation with all metrics, in ONE grouped statement
// (no per-faction queries), scoped by organization + installation. The installation must belong to the
// organization (factionhub.ErrNotFound otherwise). Ordering and ranking are the service's job.
func (r *HubStatsRepository) Leaderboard(ctx context.Context, organizationID, installationID int64) (*HubLeaderboardData, error) {
	out := &HubLeaderboardData{Rows: []HubLeaderboardRow{}}
	var server *int64
	err := r.pool.QueryRow(ctx, `
SELECT c.guild_id, i.game_server_id
FROM installations i JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
WHERE i.id = $1 AND i.organization_id = $2`, installationID, organizationID).Scan(&out.GuildID, &server)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, factionhub.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("hub leaderboard scope: %w", err)
	}
	if server != nil {
		out.ServerID = *server
	}
	rows, err := r.pool.Query(ctx, `WITH `+hubStatsCTE+hubStreakCTE+`,
fa AS (SELECT faction_id, COUNT(*) AS kills, COUNT(*) FILTER (WHERE headshot) AS hs, COUNT(*) FILTER (WHERE longshot) AS ls FROM fk GROUP BY faction_id),
da AS (SELECT faction_id, COUNT(*) AS n FROM dv GROUP BY faction_id),
ba AS (SELECT faction_id, COUNT(*) AS n, COALESCE(SUM(reward_points),0) AS v FROM bc GROUP BY faction_id),
sa AS (SELECT w.faction_id, MAX(sl.len) AS best FROM sl JOIN windows w ON w.wid = sl.wid GROUP BY w.faction_id),
ma AS (SELECT faction_id, COUNT(*) AS n FROM hub_faction_members WHERE installation_id = $1 GROUP BY faction_id),
ua AS (SELECT faction_id, COUNT(*) AS n FROM hub_faction_achievement_unlocks WHERE installation_id = $1 GROUP BY faction_id),
ta AS (SELECT faction_id, MIN(joined_at) AS t FROM hub_faction_membership_history WHERE installation_id = $1 GROUP BY faction_id)
SELECT f.id, f.name, f.tag, f.slug, f.flag_key, f.armband_key, f.primary_color, f.secondary_color,
       la.id, la.public_id::text, la.storage_key, la.content_type, la.size_bytes, la.width, la.height, la.original_filename, la.created_at,
       COALESCE(ma.n,0), COALESCE(fa.kills,0), COALESCE(da.n,0), COALESCE(fa.hs,0), COALESCE(fa.ls,0),
       COALESCE(ba.n,0), COALESCE(ba.v,0), COALESCE(sa.best,0), COALESCE(ua.n,0), ta.t
FROM hub_factions f
LEFT JOIN hub_faction_assets la ON la.id = f.logo_asset_id
LEFT JOIN ma ON ma.faction_id = f.id
LEFT JOIN fa ON fa.faction_id = f.id
LEFT JOIN da ON da.faction_id = f.id
LEFT JOIN ba ON ba.faction_id = f.id
LEFT JOIN sa ON sa.faction_id = f.id
LEFT JOIN ua ON ua.faction_id = f.id
LEFT JOIN ta ON ta.faction_id = f.id
WHERE f.installation_id = $1 AND f.organization_id = $4
ORDER BY f.id`, installationID, out.GuildID, int64(0), organizationID)
	if err != nil {
		return nil, fmt.Errorf("hub leaderboard: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var lr HubLeaderboardRow
		var la struct {
			id                                      *int64
			publicID, storageKey, contentType, name *string
			size, width, height                     *int
			created                                 *time.Time
		}
		if err := rows.Scan(&lr.FactionID, &lr.Name, &lr.Tag, &lr.Slug, &lr.FlagKey, &lr.ArmbandKey, &lr.PrimaryColor, &lr.SecondaryColor,
			&la.id, &la.publicID, &la.storageKey, &la.contentType, &la.size, &la.width, &la.height, &la.name, &la.created,
			&lr.MemberCount, &lr.Kills, &lr.Deaths, &lr.Headshots, &lr.Longshots, &lr.Bounties, &lr.BountyValue, &lr.BestStreak, &lr.Achievements, &lr.TrackingSince); err != nil {
			return nil, fmt.Errorf("hub leaderboard scan: %w", err)
		}
		if la.id != nil {
			lr.Logo = &factionhub.Asset{ID: *la.id, PublicID: *la.publicID, FactionID: lr.FactionID, StorageKey: *la.storageKey, ContentType: *la.contentType,
				SizeBytes: *la.size, Width: *la.width, Height: *la.height, OriginalFilename: *la.name, CreatedAt: *la.created}
		}
		out.Rows = append(out.Rows, lr)
	}
	return out, rows.Err()
}

// LeaderboardScope returns the installation's guild and current server (the invalidation scope of its
// leaderboard cache), verifying the installation belongs to the organization.
func (r *HubStatsRepository) LeaderboardScope(ctx context.Context, organizationID, installationID int64) (guildID, serverID int64, err error) {
	var server *int64
	err = r.pool.QueryRow(ctx, `
SELECT c.guild_id, i.game_server_id
FROM installations i JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
WHERE i.id = $1 AND i.organization_id = $2`, installationID, organizationID).Scan(&guildID, &server)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, factionhub.ErrNotFound
	}
	if err != nil {
		return 0, 0, fmt.Errorf("hub leaderboard scope: %w", err)
	}
	if server != nil {
		serverID = *server
	}
	return guildID, serverID, nil
}
