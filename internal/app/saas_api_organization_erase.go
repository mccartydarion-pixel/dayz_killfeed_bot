package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Self-serve data deletion (docs/SAAS_HTTP_API.md "Erase an organization" and "Unlink my DayZ
// identity").

// codeSubscriptionActive: the organization still has a live paid Stripe subscription; it must be
// canceled before the organization can be erased.
const codeSubscriptionActive = "SUBSCRIPTION_ACTIVE"

// eraseConfirmWord is what the website must send as {"confirm": ...}, exactly.
const eraseConfirmWord = "DELETE"

const eraseTimeout = 30 * time.Second

func (a *App) registerDataDeletionRoutes() {
	if a.HTTPServer == nil {
		return
	}
	a.HTTPServer.Handle("POST /api/saas/organizations/{organizationID}/erase", a.handleEraseOrganization)
	a.HTTPServer.Handle("POST /api/saas/player/identity/unlink", a.handleUnlinkPlayerIdentity)
}

type eraseOrganizationRequest struct {
	Confirm string `json:"confirm"`
}

// handleEraseOrganization is POST /api/saas/organizations/{organizationID}/erase, body
// {"confirm":"DELETE"}: the organization's OWNER permanently erases it
// (repository.EraseOrganization says what is removed and kept). Read-only "view as" sessions are
// refused by requireSaaSServiceAuth (rejectImpersonatedWrite) like every other write.
func (a *App) handleEraseOrganization(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	orgID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	role, ok := a.requireOrganizationMember(w, r, orgID, user.ID)
	if !ok {
		return
	}
	if role != repository.RoleOwner {
		writeSaaSError(w, codeForbidden, "only the organization's OWNER can erase it")
		return
	}
	body, ok := decodeJSONBody[eraseOrganizationRequest](w, r)
	if !ok {
		return
	}
	if body.Confirm != eraseConfirmWord {
		writeSaaSError(w, codeInvalidRequest, `confirm must be exactly "DELETE"`)
		return
	}
	if a.platformOwnerOrganization(orgID, user.DiscordUserID) {
		writeSaaSError(w, codeForbidden, "a platform owner's organization cannot be erased")
		return
	}
	if a.PlatformOwner == nil {
		writeSaaSError(w, codeInternalError, "erase is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), eraseTimeout)
	defer cancel()

	exists, livePaid, serverIDs, err := a.PlatformOwner.OrganizationErasePrecheck(ctx, orgID)
	switch {
	case err != nil:
		slog.Error("component=saas_api", "event", "organization_erase_failed", "organization_id", orgID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not erase the organization")
		return
	case !exists:
		writeSaaSError(w, codeNotFound, "organization not found")
		return
	case livePaid:
		writeSaaSError(w, codeSubscriptionActive, "cancel the paid subscription before erasing this organization")
		return
	}
	// Stop the workers first, outside the transaction, so nothing writes for these servers while
	// they are erased. Should the erase fail, the reconciler restarts the still-active ones.
	for _, id := range serverIDs {
		a.DisconnectServer(id)
	}
	res, err := a.PlatformOwner.EraseOrganization(ctx, orgID, a.configuredGuildID(), user.DiscordUserID)
	switch {
	case errors.Is(err, repository.ErrEraseSubscriptionActive):
		writeSaaSError(w, codeSubscriptionActive, "cancel the paid subscription before erasing this organization")
		return
	case errors.Is(err, repository.ErrEraseBlocked):
		slog.Warn("component=saas_api", "event", "organization_erase_blocked", "organization_id", orgID)
		writeSaaSError(w, codeConflict, "this organization holds records that cannot be erased automatically; contact Champion support")
		return
	case err != nil:
		slog.Error("component=saas_api", "event", "organization_erase_failed", "organization_id", orgID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not erase the organization")
		return
	case res == nil:
		writeSaaSError(w, codeNotFound, "organization not found")
		return
	}
	// Stop again: a reconcile tick between the first stop and the commit could have restarted a
	// worker. The servers are inactive now, so nothing starts them again.
	for _, id := range res.GameServerIDs {
		a.DisconnectServer(id)
	}
	slog.Info("component=saas_api", "event", "organization_erased", "organization_id", orgID)
	writeSaaSJSON(w, http.StatusOK, map[string]bool{"erased": true})
}

func (a *App) configuredGuildID() string {
	if a.Config == nil {
		return ""
	}
	return a.Config.DiscordGuildID
}

// handleUnlinkPlayerIdentity is POST /api/saas/player/identity/unlink: the acting user removes
// their own DayZ gamertag link(s) on every server (the website equivalent of Discord's /unlink).
// Gameplay stats stay; they are no longer tied to the user's Discord account.
func (a *App) handleUnlinkPlayerIdentity(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	if a.Links == nil {
		writeSaaSError(w, codeInternalError, "player links unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	n, err := a.Links.UnlinkAllForDiscordUser(ctx, user.DiscordUserID)
	if err != nil {
		slog.Error("component=saas_api", "event", "player_unlink_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not unlink")
		return
	}
	slog.Info("component=saas_api", "event", "player_identity_unlinked", "links", n)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"unlinked": n})
}
