package app

import (
	"context"
	"net/http"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/factionstats"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Faction Hub Phase 5 (docs/FACTION_STATS.md): competitive statistics, achievements and public
// activity. Read-only and public within the authenticated Champion experience: any synced user
// may read them (no faction or organization membership), and every read is scoped by
// organization + installation + faction - another tenant's ids are a 404, and no query ever
// aggregates across installations. The handlers contain no statistics logic; everything is
// computed by internal/factionstats.

func (a *App) registerFactionStatsRoutes() {
	const base = "/api/saas/organizations/{organizationID}/installations/{installationID}/factions"
	h := a.HTTPServer.Handle
	h("GET "+base+"/{factionID}/stats", a.handleFactionStats)
	h("GET "+base+"/{factionID}/activity", a.handleFactionActivity)
	h("GET "+base+"/{factionID}/achievements", a.handleFactionAchievements)
}

// statsService reports whether the stats service is wired (it needs the database).
func (a *App) statsService(w http.ResponseWriter) bool {
	if a.FactionHubStats == nil {
		writeSaaSError(w, codeInternalError, "faction statistics unavailable")
		return false
	}
	return true
}

// factionStatsChanged drops a faction's cached figures after a membership or role change, so
// the next read reflects it immediately (kills, deaths and bounty claims invalidate through the
// killfeed hook instead).
func (a *App) factionStatsChanged(fr factionRequest, factionID int64) {
	if a.FactionHubStats != nil {
		a.FactionHubStats.Invalidate(fr.orgID, fr.instID, factionID)
	}
}

// handleFactionStats is GET .../factions/{factionID}/stats: {summary, memberContributions, updatedAt}.
func (a *App) handleFactionStats(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok || !a.statsService(w) {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	st, err := a.FactionHubStats.GetFactionStats(ctx, fr.orgID, fr.instID, factionID)
	if err != nil {
		factionFailed(w, "load faction stats", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, st)
}

// handleFactionActivity is GET .../factions/{factionID}/activity: public-safe events, newest
// first. limit (default 20, max 100) and cursor (the previous page's nextCursor).
func (a *App) handleFactionActivity(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok || !a.statsService(w) {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	limit, ok := factionLimit(w, r, factionstats.DefaultActivityLimit, factionstats.MaxActivityLimit)
	if !ok {
		return
	}
	var cursor *repository.HubActivityCursor
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		c, good := factionstats.DecodeCursor(raw)
		if !good {
			writeSaaSError(w, codeInvalidRequest, "invalid cursor")
			return
		}
		cursor = c
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	page, err := a.FactionHubStats.GetFactionRecentActivity(ctx, fr.orgID, fr.instID, factionID, limit, cursor)
	if err != nil {
		factionFailed(w, "load faction activity", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, page)
}

type achievementsResponse struct {
	Items         []factionstats.Achievement `json:"items"`
	UnlockedCount int                        `json:"unlockedCount"`
	Total         int                        `json:"total"`
}

// handleFactionAchievements is GET .../factions/{factionID}/achievements: every system-defined
// achievement with its unlocked state, unlockedAt and progress/target.
func (a *App) handleFactionAchievements(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok || !a.statsService(w) {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	items, err := a.FactionHubStats.GetFactionAchievements(ctx, fr.orgID, fr.instID, factionID)
	if err != nil {
		factionFailed(w, "load faction achievements", err)
		return
	}
	resp := achievementsResponse{Items: items, Total: len(items)}
	for _, it := range items {
		if it.Unlocked {
			resp.UnlockedCount++
		}
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}
