package app

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/factionhub"
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
	h("GET "+base+"/leaderboard", a.handleFactionLeaderboard)
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

// --- leaderboard -------------------------------------------------------------------------------------

type leaderboardStatsDTO = factionstats.LeaderboardStats

type leaderboardEntryDTO struct {
	Rank      int    `json:"rank"`
	FactionID int64  `json:"factionId"`
	Name      string `json:"name"`
	Tag       string `json:"tag"`
	Slug      string `json:"slug"`
	// Logo is the faction's uploaded logo or null (the Champion default), exactly as on the profile.
	Logo           *factionLogoDTO `json:"logo"`
	FlagKey        *string         `json:"flagKey"`
	ArmbandKey     *string         `json:"armbandKey"`
	PrimaryColor   *string         `json:"primaryColor"`
	SecondaryColor *string         `json:"secondaryColor"`
	MemberCount    int             `json:"memberCount"`
	// Value is the ranked metric: an integer, except for KD which is a decimal.
	Value any `json:"value"`
	// TrackingSince: figures cover events from the faction's first recorded membership period on.
	TrackingSince *string `json:"trackingSince"`
	// HasTrackedActivity is false when the faction has no counted kill, death or bounty.
	HasTrackedActivity bool                `json:"hasTrackedActivity"`
	Stats              leaderboardStatsDTO `json:"stats"`
}

type leaderboardResponse struct {
	Metric     string                `json:"metric"`
	Direction  string                `json:"direction"`
	Items      []leaderboardEntryDTO `json:"items"`
	NextCursor *string               `json:"nextCursor"`
	Limit      int                   `json:"limit"`
	Total      int                   `json:"total"`
	UpdatedAt  string                `json:"updatedAt"`
}

// handleFactionLeaderboard is GET .../factions/leaderboard: the installation's factions ranked by ONE
// explicit metric (metric= KILLS|DEATHS|KD|HEADSHOTS|LONGSHOTS|BEST_STREAK|BOUNTIES_CLAIMED|BOUNTY_VALUE|
// ACHIEVEMENTS, default KILLS; anything else is 400), searchable (q), keyset-paged (limit default 25, max
// 100, cursor). Open to any synced user; scoped to organization + installation; no overall score.
func (a *App) handleFactionLeaderboard(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok || !a.statsService(w) {
		return
	}
	// A malformed query string (e.g. a ';' separator, which Go silently drops) must not turn a bad
	// metric into the default one.
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, "malformed query string")
		return
	}
	metric := factionstats.DefaultLeaderboardMetric
	if raw := strings.TrimSpace(query.Get("metric")); raw != "" {
		m, good := factionstats.ParseLeaderboardMetric(raw)
		if !good {
			writeSaaSError(w, codeInvalidRequest, "metric must be one of KILLS, DEATHS, KD, HEADSHOTS, LONGSHOTS, BEST_STREAK, BOUNTIES_CLAIMED, BOUNTY_VALUE, ACHIEVEMENTS")
			return
		}
		metric = m
	}
	limit, ok := factionLimit(w, r, factionstats.DefaultLeaderboardLimit, factionstats.MaxLeaderboardLimit)
	if !ok {
		return
	}
	search, good := factionhub.NormalizeSearch(query.Get("q"))
	if !good {
		writeSaaSError(w, codeInvalidRequest, "q is too long")
		return
	}
	var cursor *factionstats.LeaderboardCursor
	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		c, valid := factionstats.DecodeLeaderboardCursor(raw)
		if !valid || c.Metric != metric {
			writeSaaSError(w, codeInvalidRequest, "invalid cursor")
			return
		}
		cursor = c
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	page, err := a.FactionHubStats.GetFactionLeaderboard(ctx, fr.orgID, fr.instID, factionstats.LeaderboardQuery{Metric: metric, Search: search, Limit: limit, Cursor: cursor})
	if err != nil {
		factionFailed(w, "load faction leaderboard", err)
		return
	}
	base := a.assetBaseURL()
	resp := leaderboardResponse{Metric: string(page.Metric), Direction: page.Direction, Items: make([]leaderboardEntryDTO, 0, len(page.Items)),
		NextCursor: page.NextCursor, Limit: page.Limit, Total: page.Total, UpdatedAt: page.UpdatedAt}
	for _, e := range page.Items {
		f := e.Faction
		dto := leaderboardEntryDTO{Rank: e.Rank, FactionID: f.FactionID, Name: f.Name, Tag: f.Tag, Slug: f.Slug, Logo: toFactionLogo(f.Logo, base),
			FlagKey: f.FlagKey, ArmbandKey: f.ArmbandKey, PrimaryColor: f.PrimaryColor, SecondaryColor: f.SecondaryColor, MemberCount: f.MemberCount,
			HasTrackedActivity: e.HasTrackedActivity, Stats: e.Stats, TrackingSince: nullableTimeStr(f.TrackingSince)}
		if page.Metric.IsDecimal() {
			dto.Value = e.Value
		} else {
			dto.Value = int64(e.Value)
		}
		resp.Items = append(resp.Items, dto)
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}
