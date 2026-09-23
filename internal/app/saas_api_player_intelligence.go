package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Phase 3 (docs/PLAYER_INTELLIGENCE.md): the authoritative player directory and
// location-history API. Deliberately NOT built on top of the economy account search (task
// section 1) - a.Locations (internal/repository.LocationRepository) is its own dedicated query
// surface against players/player_server_activity/kills/deaths/player_links/faction_members/
// player_warnings/player_location_events.

func (a *App) registerPlayerIntelligenceRoutes(base string) {
	h := a.HTTPServer.Handle
	h("GET "+base+"/players", a.handlePlayerDirectory)
	h("GET "+base+"/players/online", a.handleOnlinePlayers)
	h("GET "+base+"/players/{playerID}/locations/latest", a.handleLatestLocation)
	h("GET "+base+"/players/{playerID}/locations", a.handleLocationHistory)
}

// --- freshness / location DTOs -----------------------------------------------------------------

type locationDTO struct {
	X          float64  `json:"x"`
	Z          float64  `json:"z"`
	Y          *float64 `json:"y,omitempty"`
	EventType  string   `json:"eventType"`
	ObservedAt string   `json:"observedAt"`
	AgeSeconds int64    `json:"ageSeconds"`
	Freshness  string   `json:"freshness"`
}

// toLocationDTO computes age/freshness at read time (task section 5: every exposed location must
// carry observedAt/ageSeconds/freshness - ADM has no continuous GPS, so "current location" is
// always as of its own last observation, never implied to be live unless it actually qualifies).
func toLocationDTO(e *repository.LocationEvent) *locationDTO {
	if e == nil {
		return nil
	}
	age := time.Since(e.ObservedAt)
	if age < 0 {
		age = 0
	}
	return &locationDTO{
		X: e.X, Z: e.Z, Y: e.Y, EventType: e.EventType,
		ObservedAt: e.ObservedAt.UTC().Format(time.RFC3339),
		AgeSeconds: int64(age.Seconds()),
		Freshness:  repository.ClassifyFreshness(age),
	}
}

// --- player directory (task sections 1-2) -----------------------------------------------------

type playerDirectoryEntryDTO struct {
	PlayerID                int64        `json:"playerId"`
	Gamertag                string       `json:"gamertag"`
	DiscordUserID           *string      `json:"discordUserId,omitempty"`
	DiscordDisplayName      *string      `json:"discordDisplayName,omitempty"`
	Linked                  bool         `json:"linked"`
	Online                  bool         `json:"online"`
	LastSeenAt              *string      `json:"lastSeenAt,omitempty"`
	LastConnectedAt         *string      `json:"lastConnectedAt,omitempty"`
	LastDisconnectedAt      *string      `json:"lastDisconnectedAt,omitempty"`
	CurrentSessionStartedAt *string      `json:"currentSessionStartedAt,omitempty"`
	Kills                   int64        `json:"kills"`
	Deaths                  int64        `json:"deaths"`
	FactionID               *int64       `json:"factionId,omitempty"`
	FactionName             *string      `json:"factionName,omitempty"`
	WarningCount            int64        `json:"warningCount"`
	CurrentLocation         *locationDTO `json:"currentLocation,omitempty"`
	LocationFreshness       *string      `json:"locationFreshness,omitempty"`
}

func toPlayerDirectoryEntryDTO(e repository.PlayerDirectoryEntry) playerDirectoryEntryDTO {
	dto := playerDirectoryEntryDTO{
		PlayerID: e.PlayerID, Gamertag: e.Gamertag, DiscordUserID: e.DiscordUserID, DiscordDisplayName: e.DiscordDisplayName,
		Linked: e.Linked, Online: e.Online,
		LastSeenAt: nullableTimeStr(&e.LastSeenAt), LastConnectedAt: nullableTimeStr(e.LastConnectedAt), LastDisconnectedAt: nullableTimeStr(e.LastDisconnectedAt),
		CurrentSessionStartedAt: nullableTimeStr(e.CurrentSessionStartedAt),
		Kills:                   e.Kills, Deaths: e.Deaths, FactionID: e.FactionID, FactionName: e.FactionName, WarningCount: e.WarningCount,
	}
	if loc := toLocationDTO(e.CurrentLocation); loc != nil {
		dto.CurrentLocation = loc
		dto.LocationFreshness = &loc.Freshness
	}
	return dto
}

func parseOptionalBool(raw string) *bool {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return nil
	}
	v := raw == "true" || raw == "1" || raw == "yes"
	if !v && raw != "false" && raw != "0" && raw != "no" {
		return nil
	}
	return &v
}

// handlePlayerDirectory is GET .../admin/players (PLAYER_DIRECTORY_VIEW) - the authoritative
// player directory, real observed data only (task: "Do not fabricate unavailable fields").
func (a *App) handlePlayerDirectory(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerDirectoryView)
	if !ok {
		return
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected for this installation")
		return
	}
	if a.Locations == nil {
		writeSaaSError(w, codeInternalError, "player directory unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	q := r.URL.Query()
	filter := repository.PlayerDirectoryFilter{Query: strings.TrimSpace(q.Get("q")), Online: parseOptionalBool(q.Get("online")), Linked: parseOptionalBool(q.Get("linked"))}
	if cursor := strings.TrimSpace(q.Get("cursor")); cursor != "" {
		if v, err := strconv.ParseInt(cursor, 10, 64); err == nil {
			filter.Before = v
		}
	}
	if lim := strings.TrimSpace(q.Get("limit")); lim != "" {
		if v, err := strconv.Atoi(lim); err == nil {
			filter.Limit = v
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := a.Locations.ListPlayerDirectory(ctx, ac.scope.GuildID, *ac.scope.ServerID, filter)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "player directory query failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list players")
		return
	}
	out := make([]playerDirectoryEntryDTO, 0, len(rows))
	var nextCursor *string
	for _, row := range rows {
		out = append(out, toPlayerDirectoryEntryDTO(row))
	}
	if len(rows) > 0 && filter.Limit > 0 && len(rows) >= filter.Limit {
		c := strconv.FormatInt(rows[len(rows)-1].PlayerID, 10)
		nextCursor = &c
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out, "nextCursor": nextCursor})
}

// handleOnlinePlayers is GET .../admin/players/online (PLAYER_LAST_LOCATION_VIEW - the response
// includes each player's latest known location, so it sits at the location-privileged floor, not
// the plain directory floor). Never marks a disconnected player online (task section 8) - sourced
// directly from player_server_activity.currently_connected via the same repository method the
// directory listing uses with Online=true.
func (a *App) handleOnlinePlayers(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerLastLocationView)
	if !ok {
		return
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected for this installation")
		return
	}
	if a.Locations == nil {
		writeSaaSError(w, codeInternalError, "player directory unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := a.Locations.OnlinePlayers(ctx, ac.scope.GuildID, *ac.scope.ServerID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "online players query failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list online players")
		return
	}
	out := make([]playerDirectoryEntryDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, toPlayerDirectoryEntryDTO(row))
	}
	// Privacy/audit (task section 10): viewing online-player locations is privileged read access.
	a.recordAudit(ctx, ac, "ONLINE_PLAYER_LOCATIONS_VIEWED", "", "", "success", nil, map[string]int{"count": len(out)})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleLatestLocation is GET .../admin/players/{playerID}/locations/latest (PLAYER_LAST_LOCATION_VIEW).
func (a *App) handleLatestLocation(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerLastLocationView)
	if !ok {
		return
	}
	playerID, good := pathInt64(w, r, "playerID")
	if !good {
		return
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected for this installation")
		return
	}
	if a.Locations == nil {
		writeSaaSError(w, codeInternalError, "location history unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	loc, err := a.Locations.LatestLocation(ctx, ac.scope.GuildID, *ac.scope.ServerID, playerID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "latest location query failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not resolve latest location")
		return
	}
	if loc == nil {
		writeSaaSError(w, codeNotFound, "no observed location for this player on this server")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toLocationDTO(loc))
}

// handleLocationHistory is GET .../admin/players/{playerID}/locations (PLAYER_LOCATION_VIEW).
func (a *App) handleLocationHistory(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerLocationView)
	if !ok {
		return
	}
	playerID, good := pathInt64(w, r, "playerID")
	if !good {
		return
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected for this installation")
		return
	}
	if a.Locations == nil {
		writeSaaSError(w, codeInternalError, "location history unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	q := r.URL.Query()
	filter := repository.LocationHistoryFilter{EventType: strings.TrimSpace(strings.ToUpper(q.Get("eventType")))}
	if from := strings.TrimSpace(q.Get("from")); from != "" {
		if t, err := time.Parse(time.RFC3339, from); err == nil {
			filter.From = &t
		}
	}
	if to := strings.TrimSpace(q.Get("to")); to != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			filter.To = &t
		}
	}
	if cursor := strings.TrimSpace(q.Get("cursor")); cursor != "" {
		if v, err := strconv.ParseInt(cursor, 10, 64); err == nil {
			filter.Before = v
		}
	}
	if lim := strings.TrimSpace(q.Get("limit")); lim != "" {
		if v, err := strconv.Atoi(lim); err == nil {
			filter.Limit = v
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := a.Locations.LocationHistory(ctx, ac.scope.GuildID, *ac.scope.ServerID, playerID, filter)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "location history query failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list location history")
		return
	}
	out := make([]locationDTO, 0, len(rows))
	var nextCursor *string
	for i := range rows {
		if dto := toLocationDTO(&rows[i]); dto != nil {
			out = append(out, *dto)
		}
	}
	if len(rows) > 0 && filter.Limit > 0 && len(rows) >= filter.Limit {
		c := strconv.FormatInt(rows[len(rows)-1].ID, 10)
		nextCursor = &c
	}
	// Privacy/audit (task section 10): viewing a player's location history is privileged read access.
	a.recordAudit(ctx, ac, "PLAYER_LOCATION_HISTORY_VIEWED", playerTarget(playerID), "", "success", nil, map[string]int{"count": len(out)})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out, "nextCursor": nextCursor})
}
