package repository

import (
	"context"
	"errors"
	"time"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
)

// Read-only inputs for the Base Boosting shadow run (caseintel.ShadowBaseBoost).

// ListBaseShadowBuilds returns the newest retained "Placed"/"Built" ADM lines
// on one server, with each builder's current faction. The bool reports that
// older lines exist beyond the limit.
func (r *CaseBaseRegistrationRepository) ListBaseShadowBuilds(ctx context.Context, guildID, serverID int64, limit int) ([]caseintel.ShadowBuildEvent, bool, error) {
	if r == nil || r.pool == nil || guildID <= 0 || serverID <= 0 {
		return nil, false, errors.New("invalid C.A.S.E. base scope")
	}
	if limit < 1 || limit > 1000 {
		limit = 500
	}
	rows, err := r.pool.Query(ctx, `SELECT e.id,e.source_id,e.source_end_offset,e.event_type,e.adm_clock,e.ingested_at,
  e.subject_player_id,e.subject_name,e.subject_x,e.subject_z,e.subject_altitude,e.build_action,e.build_object,fm.faction_id
 FROM case_evidence_events e
 LEFT JOIN faction_members fm ON fm.guild_id=e.guild_id AND fm.player_id=e.subject_player_id AND fm.active
 WHERE e.guild_id=$1 AND e.server_id=$2 AND e.event_type='BUILD_ACTION' AND e.build_action IN ('Placed','Built')
 ORDER BY e.id DESC LIMIT $3`, guildID, serverID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]caseintel.ShadowBuildEvent, 0, limit)
	for rows.Next() {
		var b caseintel.ShadowBuildEvent
		e := &b.Event
		if err := rows.Scan(&e.ID, &e.SourceID, &e.SourceEndOffset, &e.Type, &e.ADMClock, &e.IngestedAt,
			&e.SubjectID, &e.SubjectName, &e.SubjectX, &e.SubjectZ, &e.SubjectAltitude, &b.Action, &b.Object, &b.FactionID); err != nil {
			return nil, false, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(out) > limit
	if truncated {
		out = out[:limit]
	}
	return out, truncated, nil
}

// baseBoostReadinessWindow bounds the readiness scan to the server's newest
// evidence rows (an index range on idx_case_evidence_server_id), so the setup
// page never scans a server's whole history.
const baseBoostReadinessWindow = 20000

// BaseBoostReadiness is what the Base Boosting test run has to work with.
type BaseBoostReadiness struct {
	BuildLines  int        // Placed/Built lines since the cutoff, within the newest evidence rows
	LastBuildAt *time.Time // newest Placed/Built line within the newest evidence rows
	Bases       int        // registered, non-withdrawn bases
}

// BaseBoostReadiness counts recent retained build lines and the server's
// registered bases, for the anti-cheat setup checklist.
func (r *CaseBaseRegistrationRepository) BaseBoostReadiness(ctx context.Context, installationID, guildID, serverID int64, since time.Time) (BaseBoostReadiness, error) {
	var out BaseBoostReadiness
	if r == nil || r.pool == nil || installationID <= 0 || guildID <= 0 || serverID <= 0 {
		return out, errors.New("invalid C.A.S.E. base scope")
	}
	err := r.pool.QueryRow(ctx, `SELECT
  COUNT(*) FILTER (WHERE recent.event_type='BUILD_ACTION' AND recent.build_action IN ('Placed','Built') AND recent.ingested_at>=$4),
  MAX(recent.ingested_at) FILTER (WHERE recent.event_type='BUILD_ACTION' AND recent.build_action IN ('Placed','Built')),
  (SELECT COUNT(*) FROM case_registered_bases WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND state<>'REVOKED')
 FROM (SELECT event_type,build_action,ingested_at FROM case_evidence_events
  WHERE guild_id=$2 AND server_id=$3 ORDER BY id DESC LIMIT $5) recent`,
		installationID, guildID, serverID, since, baseBoostReadinessWindow).Scan(&out.BuildLines, &out.LastBuildAt, &out.Bases)
	return out, err
}

// ListBaseShadowBases returns this server's registered, non-withdrawn bases
// with the owner's current faction and the friend-list grants active now.
// For the shadow run an owner-registered base counts as verified.
func (r *CaseBaseRegistrationRepository) ListBaseShadowBases(ctx context.Context, installationID, guildID, serverID int64) ([]caseintel.RegisteredBase, error) {
	if r == nil || r.pool == nil || installationID <= 0 || guildID <= 0 || serverID <= 0 {
		return nil, errors.New("invalid C.A.S.E. base scope")
	}
	rows, err := r.pool.Query(ctx, `SELECT b.id,b.owner_player_id,b.center_x,b.center_z,b.radius,b.created_at,ofm.faction_id
 FROM case_registered_bases b
 LEFT JOIN faction_members ofm ON ofm.guild_id=b.guild_id AND ofm.player_id=b.owner_player_id AND ofm.active
 WHERE b.installation_id=$1 AND b.guild_id=$2 AND b.server_id=$3 AND b.state<>'REVOKED'
 ORDER BY b.id`, installationID, guildID, serverID)
	if err != nil {
		return nil, err
	}
	byID := map[int64]int{}
	var out []caseintel.RegisteredBase
	for rows.Next() {
		var id int64
		var b caseintel.RegisteredBase
		if err := rows.Scan(&id, &b.OwnerPlayerID, &b.X, &b.Z, &b.Radius, &b.RegisteredAt, &b.OwnerFactionID); err != nil {
			rows.Close()
			return nil, err
		}
		b.BaseID, b.Verified = caseintel.BaseIDLabel(id), true
		byID[id] = len(out)
		out = append(out, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	grants, err := r.pool.Query(ctx, `SELECT base_id,player_id,faction_id FROM case_base_authorizations
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3
  AND valid_from<=NOW() AND (valid_until IS NULL OR valid_until>NOW())`, installationID, guildID, serverID)
	if err != nil {
		return nil, err
	}
	defer grants.Close()
	for grants.Next() {
		var baseID int64
		var player, faction *int64
		if err := grants.Scan(&baseID, &player, &faction); err != nil {
			return nil, err
		}
		i, ok := byID[baseID]
		if !ok {
			continue
		}
		if player != nil {
			out[i].AuthorizedPlayers = append(out[i].AuthorizedPlayers, *player)
		}
		if faction != nil {
			out[i].AuthorizedFactions = append(out[i].AuthorizedFactions, *faction)
		}
	}
	return out, grants.Err()
}
