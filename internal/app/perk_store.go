package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/perkstore"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// The perk store (docs/PERK_STORE.md): the Player Hub's Donate tab and the Client Hub's control
// hub. Players spend Champion Points on perks that do not affect gameplay; the points go to the
// server owner's balance. A purchase is recorded and charged first; its perks (supporter tier,
// priority queue) are handed out right after and retried by the scheduler when that fails.

const (
	perkReasonExpired      = "EXPIRED"
	perkReasonNoCredits    = "INSUFFICIENT_CREDITS"
	perkReasonOfferRemoved = "OFFER_REMOVED"
	perkReasonStaff        = "STAFF_ENDED"
	perkWorkBatch          = 25
)

var perkKeyRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,64}$`)

func (a *App) registerPerkStoreAdminRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/perks", a.handlePerkStoreAdmin)
	h("PUT "+adminBase+"/perks/settings", a.handleSavePerkSettings)
	h("PUT "+adminBase+"/perks/offers", a.handleSavePerkOffer)
	h("DELETE "+adminBase+"/perks/offers/{offerID}", a.handleArchivePerkOffer)
	h("POST "+adminBase+"/perks/purchases/{purchaseID}/refund", a.handleRefundPerkPurchase)
	h("POST "+adminBase+"/perks/purchases/{purchaseID}/end", a.handleEndPerkPurchase)
	h("POST "+adminBase+"/perks/purchases/{purchaseID}/custom-done", a.handlePerkCustomDone)
	h("POST "+adminBase+"/perks/purchases/{purchaseID}/retry", a.handleRetryPerks)
}

func (a *App) registerPerkStoreRoutes() {
	const base = "/api/saas/organizations/{organizationID}/installations/{installationID}/perks"
	a.HTTPServer.Handle("GET "+base, a.handlePerkStore)
	a.HTTPServer.Handle("GET "+base+"/recipients", a.handlePerkRecipients)
	a.HTTPServer.Handle("POST "+base+"/purchases", a.handlePerkPurchase)
	a.HTTPServer.Handle("POST "+base+"/purchases/{purchaseID}/auto-renew", a.handlePerkAutoRenew)
}

func adminPerkScope(ac adminActor) repository.PerkScope {
	s := repository.PerkScope{GuildID: ac.scope.GuildID, InstallationID: ac.scope.InstallationID}
	if ac.scope.ServerID != nil {
		s.ServerID = *ac.scope.ServerID
	}
	return s
}

// perkBoards is the top-supporters board: this calendar month and all time.
type perkBoards struct {
	Month   []repository.PerkSupporter `json:"month"`
	AllTime []repository.PerkSupporter `json:"allTime"`
}

func (a *App) perkBoards(ctx context.Context, guildID int64, now time.Time, limit int) perkBoards {
	out := perkBoards{Month: []repository.PerkSupporter{}, AllTime: []repository.PerkSupporter{}}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if rows, err := a.Perks.TopSupporters(ctx, guildID, &month, limit); err == nil {
		out.Month = rows
	}
	if rows, err := a.Perks.TopSupporters(ctx, guildID, nil, limit); err == nil {
		out.AllTime = rows
	}
	return out
}

// --- Client Hub ------------------------------------------------------------------------------------

// perkAdmin runs the gate every control-hub route shares.
func (a *App) perkAdmin(w http.ResponseWriter, r *http.Request, capability permissions.Capability, write bool) (adminActor, bool) {
	ac, ok := a.requireCapability(w, r, capability)
	if !ok {
		return ac, false
	}
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.PerkStore) {
		return ac, false
	}
	if a.Perks == nil {
		writeSaaSError(w, codeInternalError, "the perk store is unavailable")
		return ac, false
	}
	limiter := a.saasAdminReadLimiter
	if write {
		limiter = a.saasAdminActionLimiter
	}
	if !enforceRateLimit(w, limiter, rateLimitKey(r)) {
		return ac, false
	}
	return ac, true
}

type perkTierOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (a *App) handlePerkStoreAdmin(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.perkAdmin(w, r, permissions.CapPerksView, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()
	guildID := ac.scope.GuildID
	settings, err := a.Perks.Settings(ctx, guildID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the perk store")
		return
	}
	offers, err := a.Perks.ListOffers(ctx, guildID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the offers")
		return
	}
	purchases, err := a.Perks.ListPurchases(ctx, guildID, 200)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the purchases")
		return
	}
	stats, err := a.Perks.Stats(ctx, guildID, now)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the store summary")
		return
	}
	tiers := []perkTierOption{}
	if a.VIP != nil {
		if rows, err := a.VIP.ListTiers(ctx, guildID); err == nil {
			for _, t := range rows {
				tiers = append(tiers, perkTierOption{ID: t.ID, Name: t.Name})
			}
		}
	}
	ownerLinked, _ := a.Perks.OwnerLinked(ctx, adminPerkScope(ac))
	writeSaaSJSON(w, http.StatusOK, map[string]any{
		"currency": economy.ChampionPoints, "settings": settings, "ownerLinked": ownerLinked, "offers": offers, "tiers": tiers,
		"purchases": purchases, "stats": stats, "topSupporters": a.perkBoards(ctx, guildID, now, 10),
	})
}

func (a *App) handleSavePerkSettings(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.perkAdmin(w, r, permissions.CapPerksManage, true)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[repository.PerkSettings](w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	if err := a.Perks.SaveSettings(ctx, ac.scope.GuildID, req); err != nil {
		writeSaaSError(w, codeInternalError, "could not save the store settings")
		return
	}
	a.recordAudit(ctx, ac, "PERK_STORE_SETTINGS", "", "", "success", nil, req)
	if req.Enabled {
		// Making a Discord channel can take longer than the website waits for a save, so it runs
		// on its own (inline only in tests).
		ensure := func() {
			layoutCtx, cancelLayout := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
			defer cancelLayout()
			a.ensurePerkChannel(layoutCtx, ac.scope.OrganizationID, ac.scope.InstallationID)
		}
		if a.perkInline {
			ensure()
		} else {
			go ensure()
		}
	}
	writeSaaSJSON(w, http.StatusOK, req)
}

func (a *App) handleSavePerkOffer(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.perkAdmin(w, r, permissions.CapPerksManage, true)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[perkstore.Offer](w, r)
	if !ok {
		return
	}
	offer, err := perkstore.Normalize(req)
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	saved, err := a.Perks.SaveOffer(ctx, ac.scope.GuildID, offer)
	switch {
	case errors.Is(err, repository.ErrPerkOfferNotFound):
		writeSaaSError(w, codeNotFound, err.Error())
		return
	case errors.Is(err, repository.ErrPerkTierNotFound), errors.Is(err, repository.ErrPerkOfferLimit):
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	case err != nil:
		slog.Warn("component=perk_store", "event", "offer_save_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save the offer")
		return
	}
	a.recordAudit(ctx, ac, "PERK_OFFER_SAVE", fmt.Sprintf("perk_offer:%d", saved.ID), saved.Name, "success", nil, saved)
	writeSaaSJSON(w, http.StatusOK, saved)
}

func (a *App) handleArchivePerkOffer(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.perkAdmin(w, r, permissions.CapPerksManage, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "offerID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	err := a.Perks.ArchiveOffer(ctx, ac.scope.GuildID, id)
	if errors.Is(err, repository.ErrPerkOfferNotFound) {
		writeSaaSError(w, codeNotFound, err.Error())
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not remove the offer")
		return
	}
	a.recordAudit(ctx, ac, "PERK_OFFER_ARCHIVE", fmt.Sprintf("perk_offer:%d", id), "", "success", nil, nil)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"archived": id})
}

func (a *App) handleRefundPerkPurchase(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.perkAdmin(w, r, permissions.CapPerksManage, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer cancel()
	p, amount, err := a.Perks.Refund(ctx, ac.scope.GuildID, id, ac.user.DiscordUserID, time.Now().UTC())
	switch {
	case errors.Is(err, repository.ErrPerkPurchaseNotFound):
		writeSaaSError(w, codeNotFound, err.Error())
		return
	case errors.Is(err, repository.ErrPerkNotRefundable):
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	case errors.Is(err, repository.ErrPerkOwnerCannotRefund):
		writeSaaSError(w, codeInsufficientFunds, "the owner's balance cannot cover this refund")
		return
	case err != nil:
		slog.Warn("component=perk_store", "event", "refund_failed", "purchase_id", id, "err", err.Error())
		writeSaaSError(w, codeInternalError, "the refund could not be completed; nothing changed")
		return
	}
	a.removePerks(ctx, p)
	a.recordAudit(ctx, ac, "PERK_REFUND", playerTarget(p.BuyerPlayerID), p.OfferName, "success", nil, map[string]any{"purchaseId": p.ID, "points": amount})
	p, _ = a.Perks.GetPurchase(ctx, ac.scope.GuildID, id)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"purchase": p, "refundedPoints": amount})
}

func (a *App) handleEndPerkPurchase(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.perkAdmin(w, r, permissions.CapPerksManage, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer cancel()
	p, err := a.Perks.End(ctx, ac.scope.GuildID, id, perkReasonStaff, time.Now().UTC())
	if errors.Is(err, repository.ErrPerkPurchaseNotFound) {
		writeSaaSError(w, codeNotFound, "purchase not found or already ended")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not end the purchase")
		return
	}
	a.removePerks(ctx, p)
	a.recordAudit(ctx, ac, "PERK_END", playerTarget(p.RecipientPlayerID), p.OfferName, "success", nil, map[string]any{"purchaseId": p.ID})
	p, _ = a.Perks.GetPurchase(ctx, ac.scope.GuildID, id)
	writeSaaSJSON(w, http.StatusOK, p)
}

func (a *App) handlePerkCustomDone(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.perkAdmin(w, r, permissions.CapPerksView, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	p, err := a.Perks.MarkCustomDone(ctx, ac.scope.GuildID, id, ac.user.DiscordUserID)
	if errors.Is(err, repository.ErrPerkPurchaseNotFound) {
		writeSaaSError(w, codeNotFound, "nothing to mark done on this purchase")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not mark the perk done")
		return
	}
	a.recordAudit(ctx, ac, "PERK_CUSTOM_DONE", playerTarget(p.RecipientPlayerID), p.OfferName, "success", nil, map[string]any{"purchaseId": p.ID})
	writeSaaSJSON(w, http.StatusOK, p)
}

func (a *App) handleRetryPerks(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.perkAdmin(w, r, permissions.CapPerksView, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer cancel()
	p, err := a.Perks.GetPurchase(ctx, ac.scope.GuildID, id)
	if errors.Is(err, repository.ErrPerkPurchaseNotFound) {
		writeSaaSError(w, codeNotFound, err.Error())
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the purchase")
		return
	}
	if p.Status == repository.PerkActive {
		a.applyPerks(ctx, p)
	} else {
		a.removePerks(ctx, p)
	}
	p, _ = a.Perks.GetPurchase(ctx, ac.scope.GuildID, id)
	writeSaaSJSON(w, http.StatusOK, p)
}

// --- Player Hub ------------------------------------------------------------------------------------

// perkPlayer runs the gate every Donate-tab route shares.
func (a *App) perkPlayer(w http.ResponseWriter, r *http.Request) (economyRequest, bool) {
	er, ok := a.scopedContext(w, r, "")
	if !ok {
		return er, false
	}
	if !a.requirePlanFeature(w, r, er.scope.OrganizationID, entitlements.PerkStore) {
		return er, false
	}
	if a.Perks == nil {
		writeSaaSError(w, codeInternalError, "the perk store is unavailable")
		return er, false
	}
	return er, true
}

type playerPerkOffer struct {
	perkstore.Offer
	// Unavailable is why it cannot be bought right now ("" = it can).
	Unavailable string `json:"unavailable,omitempty"`
	Remaining   *int   `json:"remaining"`
	// Owned is true while the player holds this offer.
	Owned bool `json:"owned"`
}

func (a *App) handlePerkStore(w http.ResponseWriter, r *http.Request) {
	er, ok := a.perkPlayer(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	now := time.Now().UTC()
	guildID := er.scope.GuildID
	settings, err := a.Perks.Settings(ctx, guildID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the perk store")
		return
	}
	out := map[string]any{"currency": economy.ChampionPoints, "enabled": settings.Enabled, "linked": false, "balance": int64(0),
		"offers": []playerPerkOffer{}, "mine": []repository.PerkPurchase{}, "topSupporters": a.perkBoards(ctx, guildID, now, 10)}
	var playerID int64
	acct, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	switch {
	case err == nil:
		playerID = acct.AccountID
		out["linked"], out["balance"], out["playerId"], out["playerName"] = true, acct.Balance, acct.AccountID, acct.Gamertag
	case errors.Is(err, economy.ErrIdentityRequired):
	default:
		economyFailed(w, "load perk store player", err)
		return
	}
	held := map[int64]bool{}
	if playerID > 0 {
		mine, err := a.Perks.ListForPlayer(ctx, guildID, playerID, 50)
		if err != nil {
			writeSaaSError(w, codeInternalError, "could not load your perks")
			return
		}
		out["mine"] = mine
		for _, p := range mine {
			if p.Status == repository.PerkActive && p.RecipientPlayerID == playerID {
				held[p.OfferID] = true
			}
		}
	}
	if settings.Enabled {
		offers, err := a.Perks.ListOffers(ctx, guildID)
		if err != nil {
			writeSaaSError(w, codeInternalError, "could not load the offers")
			return
		}
		shown := make([]playerPerkOffer, 0, len(offers))
		for _, o := range offers {
			reason := o.Unavailable(now)
			// A switched-off or finished offer is not shown; one that has not started or sold out is.
			if reason == perkstore.UnavailableDisabled || reason == perkstore.UnavailableEnded {
				continue
			}
			shown = append(shown, playerPerkOffer{Offer: o, Unavailable: reason, Remaining: o.Remaining(), Owned: held[o.ID]})
		}
		out["offers"] = shown
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

func (a *App) handlePerkRecipients(w http.ResponseWriter, r *http.Request) {
	er, ok := a.perkPlayer(w, r)
	if !ok {
		return
	}
	q := strings.Join(strings.Fields(r.URL.Query().Get("q")), " ")
	if n := len([]rune(q)); n < 2 || n > 40 {
		writeSaaSError(w, codeInvalidRequest, "search with 2 to 40 characters")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	// Only a linked player can gift, so only a linked player can search the player list.
	if _, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID); err != nil {
		economyFailed(w, "verify perk store player", err)
		return
	}
	rows, err := a.Perks.SearchRecipients(ctx, er.scope.GuildID, q, 10)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not search players")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": rows})
}

type perkPurchaseBody struct {
	OfferID           int64  `json:"offerId"`
	RecipientPlayerID int64  `json:"recipientPlayerId"` // 0 = for myself
	IdempotencyKey    string `json:"idempotencyKey"`
	// GiftMessage is an optional note for the recipient of a gift (up to 200 characters).
	GiftMessage string `json:"giftMessage,omitempty"`
}

// handlePerkPurchase is POST .../perks/purchases: a linked player buys an offer for themselves or
// as a gift. 201 for a new purchase, 200 for a replay of the same idempotency key.
func (a *App) handlePerkPurchase(w http.ResponseWriter, r *http.Request) {
	er, ok := a.perkPlayer(w, r)
	if !ok {
		return
	}
	if a.saasShopPurchaseLimiter != nil && !enforceRateLimit(w, a.saasShopPurchaseLimiter, rateLimitKey(r)) {
		return
	}
	var body perkPurchaseBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	if body.OfferID <= 0 || body.RecipientPlayerID < 0 {
		writeSaaSError(w, codeInvalidRequest, "pick an offer")
		return
	}
	if !perkKeyRe.MatchString(body.IdempotencyKey) {
		writeSaaSError(w, codeInvalidRequest, "idempotencyKey must be 8-64 characters of A-Z a-z 0-9 . _ : -")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer cancel()
	acct, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	if err != nil {
		economyFailed(w, "verify perk store player", err)
		return
	}
	recipient := body.RecipientPlayerID
	if recipient == 0 {
		recipient = acct.AccountID
	}
	scope := repository.PerkScope{GuildID: er.scope.GuildID, InstallationID: er.scope.InstallationID, ServerID: er.scope.ServerID}
	res, err := a.Perks.Purchase(ctx, scope, acct.AccountID, recipient, body.OfferID, body.IdempotencyKey, time.Now().UTC())
	var unavailable *repository.PerkUnavailableError
	switch {
	case errors.Is(err, repository.ErrInsufficientFunds):
		writeSaaSError(w, codeInsufficientFunds, "you don't have enough Champion Points")
		return
	case errors.Is(err, repository.ErrPerkOfferNotFound), errors.Is(err, repository.ErrPerkRecipientNotFound):
		writeSaaSError(w, codeNotFound, err.Error())
		return
	case errors.As(err, &unavailable), errors.Is(err, repository.ErrPerkStoreClosed), errors.Is(err, repository.ErrPerkNotGiftable),
		errors.Is(err, repository.ErrPerkAlreadyActive), errors.Is(err, repository.ErrPerkTierConflict):
		writeSaaSError(w, codeConflict, err.Error())
		return
	case err != nil:
		slog.Error("component=perk_store", "msg", "purchase failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "the purchase couldn't be completed; nothing was charged")
		return
	}
	status := http.StatusOK
	if !res.Duplicate {
		status = http.StatusCreated
		slog.Info("component=perk_store", "event", "purchase", "installation_id", scope.InstallationID, "purchase_id", res.Purchase.ID,
			"offer_id", res.Purchase.OfferID, "price_points", res.Purchase.PricePoints, "gift", res.Purchase.Gift)
		a.applyPerks(ctx, res.Purchase)
		a.announcePerkPurchase(ctx, res.Purchase)
		a.sendGiftNotice(ctx, res.Purchase, cleanGiftMessage(body.GiftMessage))
		if p, err := a.Perks.GetPurchase(ctx, scope.GuildID, res.Purchase.ID); err == nil {
			res.Purchase = p
		}
	}
	writeSaaSJSON(w, status, map[string]any{"currency": economy.ChampionPoints, "purchase": res.Purchase, "remainingBalance": res.BalanceAfter, "duplicate": res.Duplicate})
}

type perkAutoRenewBody struct {
	Enabled bool `json:"enabled"`
}

func (a *App) handlePerkAutoRenew(w http.ResponseWriter, r *http.Request) {
	er, ok := a.perkPlayer(w, r)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "purchaseID")
	if !ok {
		return
	}
	var body perkAutoRenewBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	acct, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	if err != nil {
		economyFailed(w, "verify perk store player", err)
		return
	}
	p, err := a.Perks.SetAutoRenew(ctx, er.scope.GuildID, id, acct.AccountID, body.Enabled)
	if errors.Is(err, repository.ErrPerkPurchaseNotFound) {
		writeSaaSError(w, codeNotFound, "that is not a monthly purchase you pay for")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not change the renewal")
		return
	}
	writeSaaSJSON(w, http.StatusOK, p)
}

// --- handing perks out and taking them back --------------------------------------------------------

// perkPriorityClient builds the Nitrado client and service id of a purchase's server.
func (a *App) perkPriorityClient(ctx context.Context, installationID int64) (*nitrado.Client, string, error) {
	if a.Servers == nil || a.SaaSCredentials == nil || a.CredentialCipher == nil {
		return nil, "", errors.New("Nitrado integration unavailable")
	}
	orgID, serverID, err := a.Perks.InstallationServer(ctx, installationID)
	if err != nil {
		return nil, "", err
	}
	if serverID <= 0 {
		return nil, "", errors.New("no DayZ server selected")
	}
	server, err := a.Servers.GetByID(ctx, serverID)
	if err != nil {
		return nil, "", err
	}
	envelope, err := a.SaaSCredentials.GetForOrganizationOnly(ctx, orgID)
	if err != nil || envelope == nil {
		return nil, "", errors.New("no Nitrado credential connected")
	}
	client, err := a.nitradoClientFromEnvelope(*envelope)
	if err != nil {
		return nil, "", err
	}
	return client, server.ProviderServiceID, nil
}

// changePerkPriority puts name on the server's Nitrado priority list or takes it off. changed
// reports whether the list was written (false when it already was as wanted).
func (a *App) changePerkPriority(ctx context.Context, installationID int64, name string, add bool) (changed bool, err error) {
	if !nitrado.ValidPriorityName(name) {
		return false, fmt.Errorf("player name cannot go on the priority list")
	}
	client, serviceID, err := a.perkPriorityClient(ctx, installationID)
	if err != nil {
		return false, err
	}
	list, err := client.PriorityList(ctx, serviceID)
	if err != nil {
		return false, err
	}
	at := priorityIndex(list, name)
	switch {
	case add && at >= 0, !add && at < 0:
		return false, nil
	case add:
		if len(list) >= nitrado.MaxPriorityEntries {
			return false, errors.New("the priority list is full")
		}
		list = append(append(make([]string, 0, len(list)+1), list...), name)
	default:
		list = append(append(make([]string, 0, len(list)), list[:at]...), list[at+1:]...)
	}
	return true, client.SetPriorityList(ctx, serviceID, list)
}

// applyPerks hands out everything an active purchase grants. It is safe to repeat: a tier the
// player already holds is extended, a name already on the priority list is left alone.
func (a *App) applyPerks(ctx context.Context, p repository.PerkPurchase) {
	var problems []string
	var memberID *int64
	priorityName := ""
	if p.VIPTierID != nil {
		switch {
		case a.VIP == nil:
			problems = append(problems, "Supporter tiers are unavailable.")
		default:
			m, created, err := a.VIP.GrantOrExtend(ctx, p.GuildID, *p.VIPTierID, p.RecipientPlayerID, p.ExpiresAt, "Perk store: "+p.OfferName, "PERK_STORE")
			switch {
			case errors.Is(err, repository.ErrVIPAlreadyMember):
				problems = append(problems, "The player holds a different supporter tier; end it under Supporters, then retry.")
			case errors.Is(err, repository.ErrVIPTierNotFound):
				problems = append(problems, "The supporter tier of this offer no longer exists.")
			case err != nil:
				slog.Warn("component=perk_store", "msg", "tier grant failed", "purchase_id", p.ID, "err", err.Error())
				problems = append(problems, "The supporter tier could not be granted.")
			default:
				a.syncVIPRole(ctx, m, true)
				if created {
					memberID = &m.ID
					// A new tier is worth a message; a renewal that only moves its end is not.
					a.notifyVIPGranted(ctx, p.GuildID, p.ServerID, m)
				}
			}
		}
	}
	if p.PriorityQueue {
		added, err := a.changePerkPriority(ctx, p.InstallationID, p.RecipientName, true)
		switch {
		case err != nil:
			slog.Warn("component=perk_store", "msg", "priority add failed", "purchase_id", p.ID, "err", err.Error())
			problems = append(problems, "Could not add the player to the priority queue; add them under Server, Priority.")
		case added:
			priorityName = p.RecipientName
		}
	}
	if err := a.Perks.MarkApplied(ctx, p.ID, memberID, priorityName, strings.Join(problems, " ")); err != nil {
		slog.Warn("component=perk_store", "msg", "apply stamp failed", "purchase_id", p.ID, "err", err.Error())
	}
}

// removePerks takes back what a closed purchase granted. A tier is ended only when the purchase
// was closed early (refund or staff): one that ran out has a membership that ran out with it. A
// priority entry is removed only when the store put it there and nothing else still pays for it.
func (a *App) removePerks(ctx context.Context, p repository.PerkPurchase) {
	var problems []string
	early := p.Status == repository.PerkRefunded || p.EndReason == perkReasonStaff
	if early && p.VIPMemberID != nil && a.VIP != nil {
		m, ended, err := a.VIP.RevokeIfActive(ctx, p.GuildID, *p.VIPMemberID, "PERK_ENDED")
		if err != nil {
			problems = append(problems, "The supporter tier could not be ended; end it under Supporters.")
		} else if ended {
			a.syncVIPRole(ctx, m, false)
		}
	}
	if p.PriorityQueue {
		if problem := a.removePerkPriority(ctx, p); problem != "" {
			problems = append(problems, problem)
		}
	}
	if err := a.Perks.MarkRemoved(ctx, p.ID, strings.Join(problems, " ")); err != nil {
		slog.Warn("component=perk_store", "msg", "remove stamp failed", "purchase_id", p.ID, "err", err.Error())
	}
}

func (a *App) removePerkPriority(ctx context.Context, p repository.PerkPurchase) string {
	const failed = "Could not take the player off the priority queue; remove them under Server, Priority."
	paid, err := a.Perks.PriorityStillPaid(ctx, p.GuildID, p.RecipientPlayerID, p.ID)
	if err != nil {
		return failed
	}
	if paid {
		return ""
	}
	name, err := a.Perks.PriorityNameFromStore(ctx, p.GuildID, p.RecipientPlayerID)
	if err != nil {
		return failed
	}
	if name == "" {
		return "" // staff put them on the list themselves; it is not the store's to remove
	}
	if _, err := a.changePerkPriority(ctx, p.InstallationID, name, false); err != nil {
		slog.Warn("component=perk_store", "msg", "priority remove failed", "purchase_id", p.ID, "err", err.Error())
		return failed
	}
	if err := a.Perks.ClearPriorityName(ctx, p.GuildID, p.RecipientPlayerID); err != nil {
		slog.Warn("component=perk_store", "msg", "priority name clear failed", "purchase_id", p.ID, "err", err.Error())
	}
	return ""
}

// runPerkStore is the scheduler pass: renew monthly purchases that came due, close the ones that
// ran out, and retry perks that could not be handed out or taken back.
func (a *App) runPerkStore(ctx context.Context, guildID int64, now time.Time) {
	if a.Perks == nil {
		return
	}
	due, err := a.Perks.DueRenewals(ctx, guildID, now, perkWorkBatch)
	if err != nil {
		slog.Warn("component=perk_store", "msg", "renewal sweep failed", "err", err.Error())
		return
	}
	for _, p := range due {
		renewed, err := a.Perks.Renew(ctx, guildID, p.ID, now)
		var unavailable *repository.PerkUnavailableError
		switch {
		case err == nil:
			slog.Info("component=perk_store", "event", "renewed", "purchase_id", p.ID, "renewals", renewed.Renewals)
			a.applyPerks(ctx, renewed)
		case errors.Is(err, repository.ErrInsufficientFunds):
			a.endPerk(ctx, guildID, p.ID, perkReasonNoCredits, now)
		case errors.As(err, &unavailable):
			a.endPerk(ctx, guildID, p.ID, perkReasonOfferRemoved, now)
		case errors.Is(err, repository.ErrPerkPurchaseNotFound):
		default:
			slog.Warn("component=perk_store", "msg", "renewal failed", "purchase_id", p.ID, "err", err.Error())
		}
	}
	expired, err := a.Perks.DueExpiries(ctx, guildID, now, perkWorkBatch)
	if err != nil {
		slog.Warn("component=perk_store", "msg", "expiry sweep failed", "err", err.Error())
		return
	}
	for _, p := range expired {
		a.endPerk(ctx, guildID, p.ID, perkReasonExpired, now)
	}
	if pending, err := a.Perks.PendingApply(ctx, guildID, perkWorkBatch); err == nil {
		for _, p := range pending {
			a.applyPerks(ctx, p)
		}
	}
	if pending, err := a.Perks.PendingRemove(ctx, guildID, perkWorkBatch); err == nil {
		for _, p := range pending {
			a.removePerks(ctx, p)
		}
	}
}

func (a *App) endPerk(ctx context.Context, guildID, purchaseID int64, reason string, now time.Time) {
	p, err := a.Perks.End(ctx, guildID, purchaseID, reason, now)
	if err != nil {
		if !errors.Is(err, repository.ErrPerkPurchaseNotFound) {
			slog.Warn("component=perk_store", "msg", "end failed", "purchase_id", purchaseID, "err", err.Error())
		}
		return
	}
	slog.Info("component=perk_store", "event", "ended", "purchase_id", purchaseID, "reason", reason)
	a.removePerks(ctx, p)
}

// --- Discord shout-out -----------------------------------------------------------------------------

const perkEmbedColor = 0xE7B94A

// buildPerkShoutout is the card posted in the donations channel when a player buys an offer.
func buildPerkShoutout(p repository.PerkPurchase, top []repository.PerkSupporter, storeURL string) *discordgo.MessageEmbed {
	title, line := "💎 New supporter", fmt.Sprintf("**%s** picked up **%s**. Thank you for supporting the server!", p.BuyerName, p.OfferName)
	if p.Gift {
		title, line = "🎁 Perk gifted", fmt.Sprintf("**%s** gifted **%s** to **%s**.", p.BuyerName, p.OfferName, p.RecipientName)
	}
	var perks []string
	if p.VIPTierName != "" {
		perks = append(perks, "Supporter tier: "+p.VIPTierName)
	}
	if p.PriorityQueue {
		perks = append(perks, "Priority queue")
	}
	if p.CustomPerk != "" {
		perks = append(perks, p.CustomPerk)
	}
	length := "for good"
	switch {
	case p.Billing == perkstore.BillingMonthly:
		length = "monthly"
	case p.DurationDays > 0:
		length = fmt.Sprintf("%d days", p.DurationDays)
	}
	embed := &discordgo.MessageEmbed{Title: title, Description: line, Color: perkEmbedColor, Timestamp: p.CreatedAt.UTC().Format(time.RFC3339)}
	if len(perks) > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Perks (" + length + ")", Value: "• " + strings.Join(perks, "\n• ")})
	}
	if len(top) > 0 {
		lines := make([]string, 0, 3)
		medals := []string{"🥇", "🥈", "🥉"}
		for i, s := range top {
			if i >= 3 {
				break
			}
			lines = append(lines, fmt.Sprintf("%s %s · %d pts", medals[i], s.PlayerName, s.Points))
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Top supporters this month", Value: strings.Join(lines, "\n")})
	}
	if storeURL != "" {
		embed.Footer = &discordgo.MessageEmbedFooter{Text: "Pick up your own perks in the Player Hub: " + storeURL}
	}
	return embed
}

// announcePerkPurchase posts the shout-out. It never fails a purchase: no channel, shout-outs
// switched off or a Discord error only means no card.
func (a *App) announcePerkPurchase(ctx context.Context, p repository.PerkPurchase) {
	settings, err := a.Perks.Settings(ctx, p.GuildID)
	if err != nil || !settings.Shoutouts {
		return
	}
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	top, _ := a.Perks.TopSupporters(ctx, p.GuildID, &month, 3)
	embed := buildPerkShoutout(p, top, a.siteURL()+"/dashboard/player/donate")
	if a.perkAnnouncer != nil {
		a.perkAnnouncer(p, embed)
		return
	}
	if a.ChannelRoutes == nil || a.Discord == nil {
		return
	}
	channelID, found, err := a.ChannelRoutes.Resolve(ctx, p.GuildID, p.ServerID, routing.RoutePerkStore)
	if err != nil || !found || channelID == "" {
		return
	}
	if _, err := a.feedSender(p.ServerID).ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}}); err != nil {
		slog.Warn("component=perk_store", "msg", "shout-out failed", "purchase_id", p.ID, "err", err.Error())
	}
}

// installationRouteProducers is channelRouteProducers as it applies to one installation: the
// donations channel belongs only to a server whose perk store is open, so every other server's
// layout skips it instead of getting an empty channel.
func (a *App) installationRouteProducers(ctx context.Context, installationID int64) map[string]routeProducer {
	out := a.channelRouteProducers()
	if out[routing.RoutePerkStore].Health != HealthActive {
		return out
	}
	open := false
	if a.Perks != nil {
		var err error
		if open, err = a.Perks.StoreOpenForInstallation(ctx, installationID); err != nil {
			return out // unknown: leave an existing channel alone rather than report it switched off
		}
	}
	if !open {
		out[routing.RoutePerkStore] = routeProducer{HealthDisabled, "the perk store is closed - open it under Growth, Donations"}
	}
	return out
}

// ensurePerkChannel makes the donations channel when the owner opens the store. Best effort: the
// store works without it, and Repair Champion Discord Layout makes it later.
func (a *App) ensurePerkChannel(ctx context.Context, organizationID, installationID int64) {
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil || a.SaaSChannelRoutes == nil || a.saasDiscordVerifier == nil {
		return
	}
	_, guildID, code, msg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
	if code != "" {
		slog.Warn("component=perk_store", "msg", "donations channel skipped", "reason", msg)
		return
	}
	if _, err := a.runChannelLayout(ctx, organizationID, installationID, guildID, true); err != nil {
		slog.Warn("component=perk_store", "msg", "donations channel not created", "installation_id", installationID, "err", err.Error())
	}
}
