package app

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func (a *App) registerPlayerTimelineRoutes(adminBase string) {
	a.HTTPServer.Handle("GET "+adminBase+"/players/{playerID}/timeline", a.handlePlayerTimeline)
}

// GET .../admin/players/{playerID}/timeline?before=RFC3339&limit=1..500&kinds=KILL,DEATH,...
//
// One player's history for staff, newest first: sessions, kills, deaths, warnings, purchases,
// staff point adjustments, faction moves and name changes. Page with `before` = the last entry's
// time. Map positions are never part of the timeline.
func (a *App) handlePlayerTimeline(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerDirectoryView)
	if !ok {
		return
	}
	playerID, ok := pathInt64(w, r, "playerID")
	if !ok {
		return
	}
	limit, ok := queryInt(w, r, "limit", 100, 1, 500)
	if !ok {
		return
	}
	before := time.Now().UTC().Add(time.Minute)
	if raw := strings.TrimSpace(r.URL.Query().Get("before")); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeSaaSError(w, codeInvalidRequest, "before must be an RFC 3339 time")
			return
		}
		before = parsed
	}
	var kinds []string
	if raw := strings.TrimSpace(r.URL.Query().Get("kinds")); raw != "" {
		allowed := map[string]bool{}
		for _, k := range repository.TimelineKinds {
			allowed[k] = true
		}
		for _, k := range strings.Split(raw, ",") {
			k = strings.ToUpper(strings.TrimSpace(k))
			if !allowed[k] {
				writeSaaSError(w, codeInvalidRequest, "unknown timeline kind "+k)
				return
			}
			kinds = append(kinds, k)
		}
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.PlayerTimeline == nil {
		writeSaaSError(w, codeInternalError, "player timeline unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	name, found, err := a.PlayerTimeline.PlayerInGuild(ctx, ac.scope.GuildID, playerID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the player")
		return
	}
	if !found {
		writeSaaSError(w, codeNotFound, "player not found")
		return
	}
	entries, err := a.PlayerTimeline.Timeline(ctx, ac.scope.GuildID, ac.scope.InstallationID, playerID, before, limit, kinds)
	if err != nil {
		slog.Warn("component=saas_api", "event", "player_timeline_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the timeline")
		return
	}
	resp := map[string]any{"playerId": playerID, "name": name, "items": entries, "kinds": repository.TimelineKinds}
	if len(entries) == limit {
		resp["nextBefore"] = entries[len(entries)-1].At
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}
