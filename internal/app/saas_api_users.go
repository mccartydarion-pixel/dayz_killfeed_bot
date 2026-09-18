package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// userSyncRequest is POST /api/saas/users/sync's request body (section 4).
// Only safe Discord identity fields - never an OAuth token.
type userSyncRequest struct {
	DiscordUserID     string `json:"discord_user_id"`
	DiscordUsername   string `json:"discord_username"`
	DiscordGlobalName string `json:"discord_global_name"`
	Avatar            string `json:"avatar"`
}

// handleUserSync upserts the website's authenticated Discord user into
// app_users, using the existing UserRepository.UpsertDiscordUser logic
// (repository.AppUser round-trip only - no OAuth token ever touches this
// path). Note this endpoint authenticates itself via service auth only: the
// acting-user header is what THIS sync call is establishing, so it is not
// required here (every other SaaS route does require it, and resolves it
// against the row this call just created/updated).
func (a *App) handleUserSync(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	if !enforceRateLimit(w, a.saasSyncLimiter, rateLimitKey(r)) {
		return
	}
	if a.SaaSUsers == nil {
		writeSaaSError(w, codeInternalError, "user directory unavailable")
		return
	}

	var req userSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	req.DiscordUserID = strings.TrimSpace(req.DiscordUserID)
	req.DiscordUsername = strings.TrimSpace(req.DiscordUsername)
	if req.DiscordUserID == "" || req.DiscordUsername == "" {
		writeSaaSError(w, codeInvalidRequest, "discord_user_id and discord_username are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	user, err := a.SaaSUsers.UpsertDiscordUser(ctx, repository.AppUser{
		DiscordUserID:     req.DiscordUserID,
		DiscordUsername:   req.DiscordUsername,
		DiscordGlobalName: strings.TrimSpace(req.DiscordGlobalName),
		Avatar:            strings.TrimSpace(req.Avatar),
	})
	if err != nil {
		slog.Warn("component=saas_api", "msg", "user sync failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not sync user")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toUserSummary(*user))
}
