package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
)

// C.A.S.E. Phase 2 / Observation API.
//
// This is a read-only, evidence-provenance endpoint over existing durable ADM
// data. It does NOT perform detection, infer cheating, or create cases. In
// particular, hits and shots are not part of the durable data set today.
// Never substitute zero for "not measured" or call event-triggered ADM
// positions a continuous location stream.

type caseTelemetryCoverage struct {
	Kills             string `json:"kills"`
	Positions         string `json:"positions"`
	Hits              string `json:"hits"`
	Shots             string `json:"shots"`
	ControllerInputs  string `json:"controllerInputs"`
	ContinuousGPS     bool   `json:"continuousGps"`
}

type caseTelemetrySummary struct {
	WindowStart           string                `json:"windowStart"`
	WindowEnd             string                `json:"windowEnd"`
	KillEvents24h         int64                 `json:"killEvents24h"`
	LocationSamples24h    int64                 `json:"locationSamples24h"`
	HitEvents24h          *int64                `json:"hitEvents24h"`
	LastKillAt            *string               `json:"lastKillAt"`
	LastLocationSampleAt  *string               `json:"lastLocationSampleAt"`
	Coverage              caseTelemetryCoverage `json:"coverage"`
}

type caseObservedPlayer struct {
	ID   *int64 `json:"playerId"`
	Name string `json:"name"`
}

type caseObservedEvent struct {
	ID              string              `json:"id"`
	Type            string              `json:"type"`
	Timestamp       string              `json:"timestamp"`
	TimestampSource string              `json:"timestampSource"`
	Killer          caseObservedPlayer  `json:"killer"`
	Victim          caseObservedPlayer  `json:"victim"`
	Weapon          string              `json:"weapon"`
	DistanceMeters  *float64            `json:"distanceMeters"`
}

type caseObservationResponse struct {
	Version          string                `json:"version"`
	Mode             string                `json:"mode"`
	Status           string                `json:"status"`
	GeneratedAt      string                `json:"generatedAt"`
	ServerID         int64                 `json:"serverId"`
	Telemetry        caseTelemetrySummary  `json:"telemetry"`
	RecentEvents     []caseObservedEvent   `json:"recentEvents"`
	Cases            []any                 `json:"cases"`
	Alerts           []any                 `json:"alerts"`
	Watchlist        []any                 `json:"watchlist"`
	DetectorsEnabled bool                  `json:"detectorsEnabled"`
	Enforcement      string                `json:"enforcement"`
}

func newCaseObservationResponse(serverID int64, now time.Time) caseObservationResponse {
	return caseObservationResponse{
		Version: "2.0-observation",
		Mode: "OBSERVATION_ONLY",
		Status: "AWAITING_EVENTS",
		GeneratedAt: now.UTC().Format(time.RFC3339),
		ServerID: serverID,
		Telemetry: caseTelemetrySummary{
			WindowStart: now.Add(-24 * time.Hour).UTC().Format(time.RFC3339),
			WindowEnd: now.UTC().Format(time.RFC3339),
			// nil means unknown/unmeasured; zero would falsely claim zero hits.
			HitEvents24h: nil,
			Coverage: caseTelemetryCoverage{
				Kills: "PERSISTED_ADM_EVENTS",
				Positions: "EVENT_TRIGGERED_ADM_SAMPLES",
				Hits: "PARSED_BUT_NOT_DURABLY_STORED",
				Shots: "NOT_AVAILABLE",
				ControllerInputs: "NOT_AVAILABLE",
				ContinuousGPS: false,
			},
		},
		RecentEvents: make([]caseObservedEvent, 0),
		Cases: make([]any, 0),
		Alerts: make([]any, 0),
		Watchlist: make([]any, 0),
		DetectorsEnabled: false,
		Enforcement: "DISABLED",
	}
}

func caseISO(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func (a *App) registerAntiCheatRoutes(base string) {
	a.HTTPServer.Handle("GET "+base+"/anti-cheat/overview", a.handleAntiCheatOverview)
}

// handleAntiCheatOverview does not read other servers under the same Discord
// guild: both guild_id and server_id are mandatory in EVERY data query.
// Caller identity and effective role are resolved server-side by requireCapability.
func (a *App) handleAntiCheatOverview(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerDirectoryView)
	if !ok {
		return
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected for this installation")
		return
	}
	if a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeInternalError, "C.A.S.E. telemetry unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}

	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)
	out := newCaseObservationResponse(*ac.scope.ServerID, now)
	var lastKill, lastLocation *time.Time

	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()

	// A processed kill without a source timestamp is not silently presented as
	// a source-timed kill. The 24h count uses event_time only; the list below
	// preserves timestampSource to distinguish event_time and created_at.
	err := a.DB.Pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM kills
			 WHERE guild_id=$1 AND server_id=$2 AND event_time >= $3 AND event_time <= $4),
			(SELECT COUNT(*) FROM player_location_events
			 WHERE guild_id=$1 AND server_id=$2 AND observed_at >= $3 AND observed_at <= $4),
			(SELECT MAX(event_time) FROM kills WHERE guild_id=$1 AND server_id=$2),
			(SELECT MAX(observed_at) FROM player_location_events WHERE guild_id=$1 AND server_id=$2)
	`, ac.scope.GuildID, *ac.scope.ServerID, from, now).
		Scan(&out.Telemetry.KillEvents24h, &out.Telemetry.LocationSamples24h,
			&lastKill, &lastLocation)
	if err != nil {
		slog.Warn("component=case", "event", "telemetry_read_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load C.A.S.E. telemetry")
		return
	}
	out.Telemetry.LastKillAt = caseISO(lastKill)
	out.Telemetry.LastLocationSampleAt = caseISO(lastLocation)
	if out.Telemetry.KillEvents24h > 0 || out.Telemetry.LocationSamples24h > 0 {
		out.Status = "COLLECTING"
	}

	rows, err := a.DB.Pool.Query(ctx, `
		SELECT k.id, k.event_time, k.created_at,
		       k.killer_player_id, COALESCE(killer.display_name, ''),
		       k.victim_player_id, COALESCE(victim.display_name, ''),
		       COALESCE(k.weapon_display, ''), k.distance
		  FROM kills k
		  LEFT JOIN players killer ON killer.id=k.killer_player_id AND killer.guild_id=k.guild_id
		  LEFT JOIN players victim ON victim.id=k.victim_player_id AND victim.guild_id=k.guild_id
		 WHERE k.guild_id=$1 AND k.server_id=$2
		 ORDER BY k.id DESC
		 LIMIT 20
	`, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil {
		slog.Warn("component=case", "event", "observations_read_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load C.A.S.E. observations")
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var at *time.Time
		var ingestedAt time.Time
		var killerID, victimID *int64
		var killerName, victimName, weapon string
		var distance *float64
		if err := rows.Scan(&id, &at, &ingestedAt, &killerID, &killerName,
			&victimID, &victimName, &weapon, &distance); err != nil {
			slog.Warn("component=case", "event", "observation_scan_failed", "err", err.Error())
			writeSaaSError(w, codeInternalError, "could not read C.A.S.E. observations")
			return
		}
		source := "ADM_EVENT_TIME"
		if at == nil {
			at = &ingestedAt
			source = "INGESTED_AT"
		}
		out.RecentEvents = append(out.RecentEvents, caseObservedEvent{
			ID: fmt.Sprintf("kill-%d", id), Type: "PLAYER_KILL",
			Timestamp: at.UTC().Format(time.RFC3339), TimestampSource: source,
			Killer: caseObservedPlayer{ID: killerID, Name: killerName},
			Victim: caseObservedPlayer{ID: victimID, Name: victimName},
			Weapon: weapon, DistanceMeters: distance,
		})
	}
	if err := rows.Err(); err != nil {
		slog.Warn("component=case", "event", "observations_rows_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not read C.A.S.E. observations")
		return
	}
	a.recordAudit(ctx, ac, "CASE_TELEMETRY_VIEWED", "", "", "success", nil, map[string]int{"events": len(out.RecentEvents)})
	writeSaaSJSON(w, http.StatusOK, out)
}
