package app

import (
	"context"
	"errors"
	"net/http"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// GET /api/saas/player/servers/{installationID}/ranked exposes the acting
// player's own local progress. No player ID or gamertag is accepted from the
// browser. An absent season is a normal not-started state.
func (a *App) handlePlayerServerRanked(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) { return }
	user := a.resolveActingUser(w, r)
	if user == nil { return }
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok { return }
	if a.SaaSPlayer == nil || a.Ranked == nil {
		writeSaaSError(w, codeInternalError, "player Ranked service unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	scope, found, linked, err := a.SaaSPlayer.ResolvePlayerInstallation(ctx, installationID, user.DiscordUserID)
	if err != nil { playerFailed(w, "resolve player installation", err); return }
	if !found { writeSaaSError(w, codeNotFound, "installation not found"); return }
	if !linked { writeSaaSError(w, codePlayerIdentityRequired, "a verified DayZ link is required"); return }
	observed, err := a.SaaSPlayer.HasObservedActivity(ctx, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil { playerFailed(w, "check player installation association", err); return }
	if !observed { writeSaaSError(w, codeNotFound, "installation not found"); return }
	p, err := a.Ranked.ServerPlayerProgress(ctx, scope.GuildID, scope.ServerID, scope.PlayerID)
	if errors.Is(err, repository.ErrRankedIneligible) {
		writeSaaSJSON(w, http.StatusOK, map[string]any{"installationId": installationID, "status": "NOT_STARTED"})
		return
	}
	if err != nil { playerFailed(w, "player Ranked progress", err); return }
	writeSaaSJSON(w, http.StatusOK, map[string]any{"installationId": installationID, "status": "ACTIVE", "progress": p})
}
