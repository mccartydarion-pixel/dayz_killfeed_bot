package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/caseintel"
)

// ListCaseSessionEvidence returns a bounded newest-ingested window of recorded
// observations involving one player. It does not order across ADM files, turn
// HH:MM:SS into UTC, or join to any other server under the same guild.
//
// Fetching limit+1 marks whether the oldest edge is incomplete; the pure
// reconstruction engine sorts each source's rows by its physical byte offset.
func (r *CaseEvidenceRepository) ListCaseSessionEvidence(ctx context.Context,
	guildID, serverID, playerID int64, beforeID *int64, limit int) ([]caseintel.Event, *int64, error) {
	if limit < 1 || limit > 500 {
		limit = 200
	}
	rows, err := r.pool.Query(ctx, `
	  SELECT id,source_id,source_end_offset,event_type,adm_clock,ingested_at,
	         subject_player_id,actor_player_id,target_player_id,
	         subject_name,actor_name,target_name,
	         subject_x,subject_z,subject_altitude,
	         actor_x,actor_z,actor_altitude,
	         target_x,target_z,target_altitude
	  FROM case_evidence_events
	  WHERE guild_id=$1 AND server_id=$2
	    AND (subject_player_id=$3 OR actor_player_id=$3 OR target_player_id=$3)
	    AND ($4::BIGINT IS NULL OR id<$4)
	  ORDER BY id DESC LIMIT $5
	`, guildID, serverID, playerID, beforeID, limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := make([]caseintel.Event, 0, limit+1)
	for rows.Next() {
		var e caseintel.Event
		if err = rows.Scan(&e.ID, &e.SourceID, &e.SourceEndOffset, &e.Type, &e.ADMClock, &e.IngestedAt,
			&e.SubjectID, &e.ActorID, &e.TargetID, &e.SubjectName, &e.ActorName, &e.TargetName,
			&e.SubjectX, &e.SubjectZ, &e.SubjectAltitude,
			&e.ActorX, &e.ActorZ, &e.ActorAltitude,
			&e.TargetX, &e.TargetZ, &e.TargetAltitude); err != nil {
			return nil, nil, err
		}
		out = append(out, e)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) <= limit {
		return out, nil, nil
	}
	out = out[:limit]
	cursor := out[len(out)-1].ID
	return out, &cursor, nil
}

// CaseServerUTCOffset returns the server's UTC offset in minutes as learned
// from a restart.log line that states it, or nil when none has been learned.
// Scoped by guild AND server so one tenant can never read another's clock.
func (r *CaseEvidenceRepository) CaseServerUTCOffset(ctx context.Context, guildID, serverID int64) (*int, error) {
	var minutes int
	err := r.pool.QueryRow(ctx, `SELECT utc_offset_minutes FROM live_sync_server_clock WHERE guild_id=$1 AND server_id=$2`,
		guildID, serverID).Scan(&minutes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &minutes, nil
}
