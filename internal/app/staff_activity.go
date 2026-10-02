package app

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/staffactivity"
)

func (a *App) registerStaffActivityRoutes(adminBase string) {
	a.HTTPServer.Handle("GET "+adminBase+"/staff-activity", a.handleStaffActivity)
}

type staffSummary struct {
	ActorDiscordID string           `json:"actorDiscordId"`
	ActorName      string           `json:"actorName"`
	Actions        int64            `json:"actions"`
	Failed         int64            `json:"failed"`
	LastAt         time.Time        `json:"lastAt"`
	ByCategory     map[string]int64 `json:"byCategory"`
}

type staffCategoryDTO struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// GET .../admin/staff-activity?days=1..90&actor=<discord id>&category=<key>&before=<id>&limit=1..200
//
// Who on the staff did what and when: a per-person summary for the window, actions per day, and
// the filtered list of actions (newest first, paged by id). Built on the admin audit log; the
// before/after snapshots stay in the full audit log.
func (a *App) handleStaffActivity(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapStaffActivityView)
	if !ok {
		return
	}
	days, ok := queryInt(w, r, "days", 30, 1, 90)
	if !ok {
		return
	}
	limit, ok := queryInt(w, r, "limit", 50, 1, 200)
	if !ok {
		return
	}
	filter := repository.StaffActivityFilter{Limit: limit, ActorDiscordID: strings.TrimSpace(r.URL.Query().Get("actor"))}
	if len(filter.ActorDiscordID) > 32 {
		writeSaaSError(w, codeInvalidRequest, "actor must be a Discord user id")
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("before")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeSaaSError(w, codeInvalidRequest, "before must be an entry id")
			return
		}
		filter.BeforeID = id
	}
	if key := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("category"))); key != "" {
		// First match wins: a category excludes every pattern of the categories before it.
		found := key == staffactivity.Other
		for _, c := range staffactivity.Categories {
			if c.Key == key {
				filter.Include, found = c.Patterns, true
				break
			}
			filter.Exclude = append(filter.Exclude, c.Patterns...)
		}
		if !found {
			writeSaaSError(w, codeInvalidRequest, "unknown category")
			return
		}
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.StaffActivity == nil {
		writeSaaSError(w, codeInternalError, "staff activity unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	filter.Since = time.Now().UTC().AddDate(0, 0, -days)
	counts, err := a.StaffActivity.ActionCounts(ctx, ac.scope.OrganizationID, ac.scope.InstallationID, filter.Since)
	if err != nil {
		slog.Warn("component=saas_api", "event", "staff_activity_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load staff activity")
		return
	}
	daily, err := a.StaffActivity.Daily(ctx, ac.scope.OrganizationID, ac.scope.InstallationID, filter.Since)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load staff activity")
		return
	}
	items, err := a.StaffActivity.Entries(ctx, ac.scope.OrganizationID, ac.scope.InstallationID, filter)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load staff activity")
		return
	}
	for i := range items {
		items[i].Category = staffactivity.Of(items[i].Action)
	}
	staff := map[string]*staffSummary{}
	for _, c := range counts {
		s := staff[c.ActorDiscordID]
		if s == nil {
			s = &staffSummary{ActorDiscordID: c.ActorDiscordID, ActorName: c.ActorName, ByCategory: map[string]int64{}}
			staff[c.ActorDiscordID] = s
		}
		s.Actions += c.Count
		s.Failed += c.Failed
		s.ByCategory[staffactivity.Of(c.Action)] += c.Count
		if c.LastAt.After(s.LastAt) {
			s.LastAt = c.LastAt
		}
	}
	summaries := make([]staffSummary, 0, len(staff))
	for _, s := range staff {
		summaries = append(summaries, *s)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Actions != summaries[j].Actions {
			return summaries[i].Actions > summaries[j].Actions
		}
		return summaries[i].ActorDiscordID < summaries[j].ActorDiscordID
	})
	categories := []staffCategoryDTO{}
	for _, c := range staffactivity.Categories {
		categories = append(categories, staffCategoryDTO{c.Key, c.Label})
	}
	categories = append(categories, staffCategoryDTO{staffactivity.Other, "Other"})
	resp := map[string]any{"days": days, "staff": summaries, "daily": daily, "items": items, "categories": categories}
	if len(items) == limit {
		resp["nextBefore"] = items[len(items)-1].ID
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}
