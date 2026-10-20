package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

func (a *App) registerInviteRoutes(adminBase string) {
	a.HTTPServer.Handle("GET "+adminBase+"/invites", a.handleInviteReport)
}

// GET .../admin/invites?days=7..365
//
// Which Discord invites bring members who stay, link a game account and play. Joins are
// attributed by internal/discord.InviteTracker; "tracking" says whether attribution is working
// (it needs the bot's Manage Server permission) so an empty report is never mistaken for no joins.
func (a *App) handleInviteReport(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapInvitesView)
	if !ok {
		return
	}
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.Retention) {
		return
	}
	days, ok := queryInt(w, r, "days", 30, 7, 365)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.Invites == nil {
		writeSaaSError(w, codeInternalError, "invite tracking unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	report, err := a.Invites.InviteReport(ctx, ac.scope.GuildID, days, time.Now().UTC())
	if err != nil {
		slog.Warn("component=saas_api", "event", "invite_report_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load invite results")
		return
	}
	ready, problem, lastSync := a.InviteTracker.Status(ac.scope.DiscordGuildID)
	tracking := map[string]any{"ready": ready, "problem": problem}
	if !lastSync.IsZero() {
		tracking["lastSync"] = lastSync.UTC()
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"report": report, "tracking": tracking})
}
