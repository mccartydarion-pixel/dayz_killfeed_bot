package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Gifted paid time (docs/SECURITY_GIFTS.md): the server owner gives a player
// days of a base service or the Sentinel Pro bundle for free. No Champion
// Points move. Owner only, audited; the player gets a DM.

type securityGiftRequest struct {
	PlayerID       int64  `json:"playerId"`
	ServiceID      string `json:"serviceId"`
	Days           int    `json:"days"`
	Note           string `json:"note"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// handleListSecurityGifts is GET .../admin/case/security-gifts.
func (a *App) handleListSecurityGifts(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	s := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	gifts, err := repository.NewSecurityServiceRepository(a.DB.Pool).RecentGifts(ctx, s, 20)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load gifts")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"gifts": gifts})
}

// handleGiveSecurityGift is POST .../admin/case/security-gifts.
// 201 for a new gift, 200 for a replay of the same idempotency key.
func (a *App) handleGiveSecurityGift(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req securityGiftRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid gift")
		return
	}
	if !repository.SellableSecurityService(req.ServiceID) {
		writeSaaSError(w, codeInvalidRequest, "this service can't be gifted")
		return
	}
	if req.Days < 1 || req.Days > repository.SecurityGiftMaxDays {
		writeSaaSError(w, codeInvalidRequest, "a gift can be 1 to 90 days")
		return
	}
	if !securityKeyRe.MatchString(req.IdempotencyKey) || ac.user == nil {
		writeSaaSError(w, codeInvalidRequest, "idempotencyKey must be 8-64 characters of A-Z a-z 0-9 . _ : -")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	s := repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	name, discordID, err := sales.GiftRecipient(ctx, s.GuildID, req.PlayerID)
	if errors.Is(err, repository.ErrSecurityInvalidRequest) {
		writeSaaSError(w, codeInvalidRequest, "that player isn't on this server")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "the gift couldn't be given right now")
		return
	}
	gift, replay, err := sales.Gift(ctx, s, req.PlayerID, req.ServiceID, req.Days, req.Note, ac.user.ID, "gift-"+req.IdempotencyKey)
	if errors.Is(err, repository.ErrSecurityInvalidRequest) {
		writeSaaSError(w, codeInvalidRequest, "the note can be up to 200 characters")
		return
	}
	if err != nil {
		slog.Warn("component=security_market", "event", "gift_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "the gift couldn't be given right now")
		return
	}
	gift.PlayerName = name
	status := http.StatusOK
	if !replay {
		status = http.StatusCreated
		a.recordAudit(ctx, ac, "SECURITY_GIFT_GIVEN", "security-gift:"+req.ServiceID, "", "success", nil,
			map[string]any{"playerId": req.PlayerID, "serviceId": req.ServiceID, "days": req.Days, "purchaseId": gift.ID})
		a.notifySecurityGift(discordID, s.ServerID, gift, req.Note)
	}
	writeSaaSJSON(w, status, map[string]any{"gift": gift, "duplicate": replay})
}

// notifySecurityGift DMs the player in the background; a closed DM is only logged.
func (a *App) notifySecurityGift(discordUserID string, serverID int64, gift repository.SecurityPurchase, note string) {
	if discordUserID == "" || a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	session := a.Discord.Session()
	msg := discord.SecurityGiftMessage(repository.SecurityServiceLabel(gift.ServiceID), a.serverNameFunc()(serverID), gift.DurationDays, gift.EndsAt, note, a.securityStoreURL())
	go func() {
		ch, err := session.UserChannelCreate(discordUserID)
		if err == nil {
			_, err = session.ChannelMessageSendComplex(ch.ID, msg)
		}
		if err != nil {
			slog.Warn("component=security_market", "event", "gift_dm_failed", "purchase_id", gift.ID, "err", err.Error())
		}
	}()
}
