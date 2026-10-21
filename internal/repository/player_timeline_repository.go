package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlayerTimelineRepository merges everything Champion knows about one player into a single
// newest-first timeline for staff: sessions, kills, deaths, warnings, purchases, staff point
// adjustments, faction moves and name changes. Positions are never included.
type PlayerTimelineRepository struct{ pool *pgxpool.Pool }

func NewPlayerTimelineRepository(pool *pgxpool.Pool) *PlayerTimelineRepository {
	return &PlayerTimelineRepository{pool: pool}
}

// TimelineKinds are the entry kinds, in the order the Client Hub lists its filters.
var TimelineKinds = []string{"CONNECT", "DISCONNECT", "KILL", "DEATH", "DEATH_OTHER", "WARNING", "WARNING_CLEARED", "PURCHASE", "POINTS_ADJUSTED", "FACTION_JOIN", "FACTION_LEAVE", "NAME_CHANGE"}

// TimelineEntry is one moment. Other/Value/Note carry the kind's details (e.g. KILL: victim,
// weapon, distance; WARNING: issuer, -, reason).
type TimelineEntry struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Server string    `json:"server,omitempty"`
	Other  string    `json:"other,omitempty"`
	Value  string    `json:"value,omitempty"`
	Note   string    `json:"note,omitempty"`
}

// PlayerInGuild reports the player's name when the player belongs to the guild.
func (r *PlayerTimelineRepository) PlayerInGuild(ctx context.Context, guildID, playerID int64) (string, bool, error) {
	var name string
	err := r.pool.QueryRow(ctx, `SELECT display_name FROM players WHERE id=$1 AND guild_id=$2`, playerID, guildID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// Timeline returns up to limit entries strictly before `before`, newest first. kinds filters
// (empty = all). Every source is scoped to the guild (and installation for Hub tables).
func (r *PlayerTimelineRepository) Timeline(ctx context.Context, guildID, installationID, playerID int64, before time.Time, limit int, kinds []string) ([]TimelineEntry, error) {
	limit = clampLimit(limit, 100, 500)
	if len(kinds) == 0 {
		kinds = TimelineKinds
	}
	rows, err := r.pool.Query(ctx, `
WITH entries AS (
  (SELECT e.observed_at AS at, e.event_type AS kind, COALESCE(gs.display_name,'') AS server, ''::text AS other, ''::text AS value, ''::text AS note
   FROM player_location_events e LEFT JOIN game_servers gs ON gs.id=e.server_id
   WHERE e.guild_id=$1 AND e.player_id=$3 AND e.event_type IN ('CONNECT','DISCONNECT') AND e.observed_at < $4
   ORDER BY e.observed_at DESC LIMIT $5)
  UNION ALL
  (SELECT k.event_time, 'KILL', COALESCE(gs.display_name,''), COALESCE(v.display_name,''), COALESCE(k.weapon_display,''), COALESCE(round(k.distance::numeric,1)::text,'')
   FROM kills k LEFT JOIN players v ON v.id=k.victim_player_id LEFT JOIN game_servers gs ON gs.id=k.server_id
   WHERE k.guild_id=$1 AND k.killer_player_id=$3 AND k.event_time < $4 ORDER BY k.event_time DESC LIMIT $5)
  UNION ALL
  (SELECT k.event_time, 'DEATH', COALESCE(gs.display_name,''), COALESCE(kp.display_name,''), COALESCE(k.weapon_display,''), COALESCE(round(k.distance::numeric,1)::text,'')
   FROM kills k LEFT JOIN players kp ON kp.id=k.killer_player_id LEFT JOIN game_servers gs ON gs.id=k.server_id
   WHERE k.guild_id=$1 AND k.victim_player_id=$3 AND k.event_time < $4 ORDER BY k.event_time DESC LIMIT $5)
  UNION ALL
  (SELECT d.event_time, 'DEATH_OTHER', '', '', d.death_type, ''
   FROM deaths d WHERE d.guild_id=$1 AND d.player_id=$3 AND d.death_type<>'PVP' AND d.event_time < $4 ORDER BY d.event_time DESC LIMIT $5)
  UNION ALL
  (SELECT w.issued_at, 'WARNING', '', COALESCE(u.discord_username,''), '', w.reason
   FROM player_warnings w LEFT JOIN app_users u ON u.id=w.issued_by_user_id
   WHERE w.guild_id=$1 AND w.player_id=$3 AND w.issued_at < $4 ORDER BY w.issued_at DESC LIMIT $5)
  UNION ALL
  (SELECT w.cleared_at, 'WARNING_CLEARED', '', COALESCE(u.discord_username,''), '', w.reason
   FROM player_warnings w LEFT JOIN app_users u ON u.id=w.cleared_by_user_id
   WHERE w.guild_id=$1 AND w.player_id=$3 AND w.cleared AND w.cleared_at < $4 ORDER BY w.cleared_at DESC LIMIT $5)
  UNION ALL
  (SELECT sp.created_at, 'PURCHASE', COALESCE(gs.display_name,''), '', sp.total_points::text, sp.status
   FROM shop_purchases sp LEFT JOIN game_servers gs ON gs.id=sp.game_server_id
   WHERE sp.installation_id=$2 AND sp.player_id=$3 AND sp.created_at < $4 ORDER BY sp.created_at DESC LIMIT $5)
  UNION ALL
  (SELECT pt.created_at, 'POINTS_ADJUSTED', '', '', pt.amount::text, COALESCE(pt.description,'')
   FROM point_transactions pt WHERE pt.guild_id=$1 AND pt.player_id=$3 AND pt.reason_type IN ('ADMIN_CREDIT','ADMIN_DEBIT') AND pt.created_at < $4
   ORDER BY pt.created_at DESC LIMIT $5)
  UNION ALL
  (SELECT h.joined_at, 'FACTION_JOIN', '', COALESCE(f.name,''), '', ''
   FROM hub_faction_membership_history h LEFT JOIN hub_factions f ON f.id=h.faction_id
   WHERE h.installation_id=$2 AND h.player_identity_id=$3 AND h.joined_at < $4 ORDER BY h.joined_at DESC LIMIT $5)
  UNION ALL
  (SELECT h.left_at, 'FACTION_LEAVE', '', COALESCE(f.name,''), '', ''
   FROM hub_faction_membership_history h LEFT JOIN hub_factions f ON f.id=h.faction_id
   WHERE h.installation_id=$2 AND h.player_identity_id=$3 AND h.left_at < $4 ORDER BY h.left_at DESC LIMIT $5)
  UNION ALL
  (SELECT n.changed_at, 'NAME_CHANGE', '', n.old_name, n.new_name, ''
   FROM player_name_history n WHERE n.guild_id=$1 AND n.player_id=$3 AND n.changed_at < $4 ORDER BY n.changed_at DESC LIMIT $5)
)
SELECT at,kind,server,other,value,note FROM entries WHERE at IS NOT NULL AND kind = ANY($6) ORDER BY at DESC, kind LIMIT $5`,
		guildID, installationID, playerID, before, limit, kinds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TimelineEntry{}
	for rows.Next() {
		var e TimelineEntry
		if err := rows.Scan(&e.At, &e.Kind, &e.Server, &e.Other, &e.Value, &e.Note); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
