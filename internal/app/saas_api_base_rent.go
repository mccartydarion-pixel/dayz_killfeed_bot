package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Base rent (docs/BASE_RENT.md). The server owner sets one rent price and
// period for bases registered from players' requests (bases the owner adds
// stay free). Players pay ahead from the Security Store; nothing is taken
// automatically. 3 days after rent runs out the base is paused: its base
// services stop until rent is paid. The base is kept.

type baseRentSettingsRequest struct {
	Enabled     bool  `json:"enabled"`
	PricePoints int64 `json:"pricePoints"`
	PeriodDays  int   `json:"periodDays"`
}

func (a *App) ownerRentScope(w http.ResponseWriter, r *http.Request, write bool) (adminActor, repository.SecurityScope, bool) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return ac, repository.SecurityScope{}, false
	}
	limiter := a.saasAdminReadLimiter
	if write {
		limiter = a.saasAdminActionLimiter
	}
	if !enforceRateLimit(w, limiter, rateLimitKey(r)) {
		return ac, repository.SecurityScope{}, false
	}
	return ac, repository.SecurityScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}, true
}

// handleGetBaseRent is GET .../admin/case/base-rent.
func (a *App) handleGetBaseRent(w http.ResponseWriter, r *http.Request) {
	_, s, ok := a.ownerRentScope(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	repo := repository.NewBaseRentRepository(a.DB.Pool)
	settings, err := repo.GetSettings(ctx, s)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load base rent")
		return
	}
	bases, err := repo.AllBases(ctx, s, 100)
	if err != nil {
		slog.Warn("component=base_rent", "event", "list_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load base rent")
		return
	}
	payments, err := repo.RecentPayments(ctx, s, 20)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load base rent")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings, "bases": bases, "payments": payments, "graceDays": repository.BaseRentGraceDays})
}

// handleSetBaseRent is PUT .../admin/case/base-rent.
func (a *App) handleSetBaseRent(w http.ResponseWriter, r *http.Request) {
	ac, s, ok := a.ownerRentScope(w, r, true)
	if !ok {
		return
	}
	var req baseRentSettingsRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid rent settings")
		return
	}
	if req.PricePoints < 1 || req.PricePoints > repository.SecurityMaxPricePoints {
		writeSaaSError(w, codeInvalidRequest, "rent must be between 1 and 1,000,000,000 Champion Points")
		return
	}
	if req.PeriodDays < 1 || req.PeriodDays > repository.BaseRentMaxPeriod {
		writeSaaSError(w, codeInvalidRequest, "the rent period must be 1 to 30 days")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	settings, err := repository.NewBaseRentRepository(a.DB.Pool).SetSettings(ctx, s, req.Enabled, req.PricePoints, req.PeriodDays, actor)
	if err != nil {
		slog.Warn("component=base_rent", "event", "set_failed", "err", err.Error())
		writeSaaSError(w, codeInvalidRequest, "could not save rent settings")
		return
	}
	a.recordAudit(ctx, ac, "BASE_RENT_SAVED", "base-rent", "", "success", nil,
		map[string]any{"enabled": settings.Enabled, "pricePoints": settings.PricePoints, "periodDays": settings.PeriodDays})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

type baseRentGiftRequest struct {
	BaseID         int64  `json:"baseId"`
	Days           int    `json:"days"`
	Note           string `json:"note"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// handleGiftBaseRent is POST .../admin/case/base-rent/gift: free rent days for
// one rented base. No Champion Points move. 201 new, 200 replay.
func (a *App) handleGiftBaseRent(w http.ResponseWriter, r *http.Request) {
	ac, s, ok := a.ownerRentScope(w, r, true)
	if !ok {
		return
	}
	var req baseRentGiftRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid rent gift")
		return
	}
	if req.BaseID <= 0 || req.Days < 1 || req.Days > repository.BaseRentGiftMaxDays {
		writeSaaSError(w, codeInvalidRequest, "choose a base and 1 to 90 days")
		return
	}
	if !securityKeyRe.MatchString(req.IdempotencyKey) || ac.user == nil {
		writeSaaSError(w, codeInvalidRequest, "idempotencyKey must be 8-64 characters of A-Z a-z 0-9 . _ : -")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	res, err := repository.NewBaseRentRepository(a.DB.Pool).Gift(ctx, s, req.BaseID, req.Days, req.Note, ac.user.ID, "gift-"+req.IdempotencyKey)
	switch {
	case errors.Is(err, repository.ErrBaseRentNotOwned):
		writeSaaSError(w, codeConflict, "that base doesn't pay rent")
		return
	case errors.Is(err, repository.ErrInvalidBaseRent):
		writeSaaSError(w, codeInvalidRequest, "the note can be up to 200 characters")
		return
	case err != nil:
		slog.Warn("component=base_rent", "event", "gift_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "the rent gift couldn't be given right now")
		return
	}
	status := http.StatusOK
	if !res.Duplicate {
		status = http.StatusCreated
		a.recordAudit(ctx, ac, "BASE_RENT_GIFTED", "base-rent", "", "success", nil,
			map[string]any{"baseId": req.BaseID, "days": req.Days, "paymentId": res.Payment.ID})
		a.notifyRentGift(res, s.ServerID)
	}
	writeSaaSJSON(w, status, map[string]any{"gift": res.Payment, "duplicate": res.Duplicate})
}

// notifyRentGift DMs the base owner in the background; a closed DM is only logged.
func (a *App) notifyRentGift(res repository.RentGiftResult, serverID int64) {
	if res.OwnerDiscord == "" || a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	session := a.Discord.Session()
	msg := discord.BaseRentGiftMessage(res.Payment.BaseName, a.serverName(serverID), res.Payment.PeriodDays, res.Payment.EndsAt, res.Payment.Note, a.securityStoreURL())
	go func() {
		ch, err := session.UserChannelCreate(res.OwnerDiscord)
		if err == nil {
			_, err = session.ChannelMessageSendComplex(ch.ID, msg)
		}
		if err != nil {
			slog.Warn("component=base_rent", "event", "gift_dm_failed", "payment_id", res.Payment.ID, "err", err.Error())
		}
	}()
}

type playerBaseRentResponse struct {
	Enabled     bool                    `json:"enabled"`
	PricePoints int64                   `json:"pricePoints,omitempty"`
	PeriodDays  int                     `json:"periodDays,omitempty"`
	GraceDays   int                     `json:"graceDays"`
	Bases       []repository.RentedBase `json:"bases"`
	// History is the newest payments and gifts on their and their faction's bases.
	History []repository.BaseRentPayment `json:"history"`
}

type playerBaseRentPayBody struct {
	BaseID         int64  `json:"baseId"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// handleGetPlayerBaseRent is GET .../security-marketplace/base-rent: the
// signed-in player's rented bases and when rent is due.
func (a *App) handleGetPlayerBaseRent(w http.ResponseWriter, r *http.Request) {
	s, playerID, ctx, cancel, ok := a.playerBaseRequestContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	scope := repository.SecurityScope{InstallationID: s.InstallationID, GuildID: s.GuildID, ServerID: s.ServerID}
	repo := repository.NewBaseRentRepository(a.DB.Pool)
	settings, err := repo.GetSettings(ctx, scope)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load your rent")
		return
	}
	out := playerBaseRentResponse{Enabled: settings.Enabled, GraceDays: repository.BaseRentGraceDays, Bases: []repository.RentedBase{}}
	if out.History, err = repo.PlayerPayments(ctx, scope, playerID, 20); err != nil {
		writeSaaSError(w, codeInternalError, "could not load your rent")
		return
	}
	if settings.Enabled {
		out.PricePoints, out.PeriodDays = settings.PricePoints, settings.PeriodDays
		if out.Bases, err = repo.PlayerBases(ctx, scope, playerID); err != nil {
			writeSaaSError(w, codeInternalError, "could not load your rent")
			return
		}
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

// handlePayPlayerBaseRent is POST .../security-marketplace/base-rent: pay one
// period of rent for one of the player's bases. 201 new, 200 replay.
func (a *App) handlePayPlayerBaseRent(w http.ResponseWriter, r *http.Request) {
	var body playerBaseRentPayBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	if body.BaseID <= 0 || !securityKeyRe.MatchString(body.IdempotencyKey) {
		writeSaaSError(w, codeInvalidRequest, "choose a base; idempotencyKey must be 8-64 characters of A-Z a-z 0-9 . _ : -")
		return
	}
	s, playerID, ctx, cancel, ok := a.playerBaseRequestContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	if a.saasShopPurchaseLimiter != nil && !enforceRateLimit(w, a.saasShopPurchaseLimiter, rateLimitKey(r)) {
		return
	}
	scope := repository.SecurityScope{InstallationID: s.InstallationID, GuildID: s.GuildID, ServerID: s.ServerID}
	res, err := repository.NewBaseRentRepository(a.DB.Pool).Pay(ctx, scope, playerID, body.BaseID, body.IdempotencyKey)
	switch {
	case errors.Is(err, repository.ErrInsufficientFunds):
		writeSaaSError(w, codeInsufficientFunds, "you don't have enough Champion Points")
		return
	case errors.Is(err, repository.ErrBaseRentOff):
		writeSaaSError(w, codeConflict, "this server doesn't charge base rent")
		return
	case errors.Is(err, repository.ErrBaseRentNotOwned):
		writeSaaSError(w, codeConflict, "that base isn't yours or your faction's, or doesn't pay rent")
		return
	case err != nil:
		slog.Error("component=base_rent", "msg", "pay failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "the rent couldn't be paid; nothing was charged")
		return
	}
	slog.Info("component=base_rent", "event", "paid", "installation_id", s.InstallationID, "base_id", body.BaseID,
		"payment_id", res.Payment.ID, "duplicate", res.Duplicate)
	a.notifyRentPaidForOwner(res, s.ServerID)
	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	writeSaaSJSON(w, status, map[string]any{"payment": res.Payment, "remainingBalance": res.BalanceAfter, "duplicate": res.Duplicate})
}

// notifyRentPaidForOwner DMs a base owner when a faction mate paid their rent
// (new payments only), in the background.
func (a *App) notifyRentPaidForOwner(res repository.BaseRentPayResult, serverID int64) {
	if res.Duplicate || res.OwnerID == 0 || res.OwnerDiscord == "" || a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	session := a.Discord.Session()
	msg := discord.BaseRentPaidForYouMessage(res.PayerName, res.Payment.BaseName, a.serverName(serverID), res.Payment.PeriodDays, res.Payment.EndsAt)
	go func() {
		ch, err := session.UserChannelCreate(res.OwnerDiscord)
		if err == nil {
			_, err = session.ChannelMessageSendComplex(ch.ID, msg)
		}
		if err != nil {
			slog.Warn("component=base_rent", "event", "paid_for_dm_failed", "payment_id", res.Payment.ID, "err", err.Error())
		}
	}()
}

// startBaseRentReminders sends rent reminders every 10 minutes: one a day
// before rent is due, and one when a base becomes paused.
func (a *App) startBaseRentReminders(ctx context.Context) {
	if a.DB == nil || a.DB.Pool == nil || a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	session := a.Discord.Session()
	send := func(userID string, msg *discordgo.MessageSend) error {
		ch, err := session.UserChannelCreate(userID)
		if err != nil {
			return err
		}
		_, err = session.ChannelMessageSendComplex(ch.ID, msg)
		return err
	}
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.sendBaseRentReminders(ctx, send)
				if a.AdminAlerts != nil {
					a.sendRentPauseDigests(ctx, a.AdminAlerts)
				}
			}
		}
	}()
}

func (a *App) sendBaseRentReminders(ctx context.Context, send func(string, *discordgo.MessageSend) error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=base_rent", "msg", "reminder panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	repo := repository.NewBaseRentRepository(a.DB.Pool)
	notices, err := repo.DueNotices(ctx, 100)
	if err != nil {
		slog.Warn("component=base_rent", "event", "notices_failed", "err", err.Error())
		return
	}
	for _, n := range notices {
		if n.DiscordUserID != "" && send != nil {
			msg := discord.BaseRentNoticeMessage(n.Kind == repository.BaseRentNoticeDueSoon, n.BaseName, a.serverName(n.ServerID),
				n.DueAt, n.PricePoints, n.PeriodDays, a.securityStoreURL())
			if err := send(n.DiscordUserID, msg); err != nil {
				slog.Warn("component=base_rent", "event", "reminder_dm_failed", "base_id", n.BaseID, "err", err.Error())
			}
		}
		// One attempt only, so nobody is messaged twice.
		if err := repo.MarkNotice(ctx, n); err != nil {
			slog.Warn("component=base_rent", "event", "mark_notice_failed", "base_id", n.BaseID, "err", err.Error())
		}
	}
}

// rentAlertPublisher is the part of the staff alert publisher the digest uses.
type rentAlertPublisher interface{ Publish(discord.AdminAlert) }

// sendRentPauseDigests posts, at most once a day per server, a staff notice
// listing bases paused for unpaid rent since the last one. It is information
// only: nothing happens to the players.
func (a *App) sendRentPauseDigests(ctx context.Context, pub rentAlertPublisher) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=base_rent", "msg", "digest panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	if pub == nil {
		return
	}
	repo := repository.NewBaseRentRepository(a.DB.Pool)
	digests, err := repo.PausedDigests(ctx)
	if err != nil {
		slog.Warn("component=base_rent", "event", "digest_failed", "err", err.Error())
		return
	}
	for _, d := range digests {
		pub.Publish(rentPauseAlert(d))
		if err := repo.MarkDigest(ctx, d.InstallationID, d.ServerID); err != nil {
			slog.Warn("component=base_rent", "event", "mark_digest_failed", "server_id", d.ServerID, "err", err.Error())
		}
	}
}

// rentPauseAlert lists up to 10 newly paused bases with their owners.
func rentPauseAlert(d repository.PausedDigest) discord.AdminAlert {
	lines := make([]string, 0, 11)
	for i, b := range d.Bases {
		if i == 10 {
			lines = append(lines, fmt.Sprintf("…and %d more", len(d.Bases)-10))
			break
		}
		owner := b.OwnerName
		if owner == "" {
			owner = "unknown player"
		}
		lines = append(lines, "• **"+presentation.SafeName(b.BaseName, 64)+"** ("+presentation.SafeName(owner, 40)+")")
	}
	word := "bases were"
	if len(d.Bases) == 1 {
		word = "base was"
	}
	return discord.AdminAlert{GuildRowID: d.GuildID, ServerID: d.ServerID, Kind: discord.AlertKindRentPaused, Severity: discord.AlertInfo,
		Headline: "BASES PAUSED FOR RENT",
		Detail: fmt.Sprintf("%d %s paused for unpaid rent since the last notice. Their base services are off until rent is paid; the bases are kept and nothing else happens to the players.\n%s",
			len(d.Bases), word, strings.Join(lines, "\n")),
		Fields: [][2]string{{"Where to look", "Anti-cheat → Bases tab → Base rent"}}}
}
