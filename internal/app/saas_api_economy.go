package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Points economy web API (docs/ECONOMY.md). Player routes read the acting user's own
// account through their VERIFIED DayZ link; admin routes need an OWNER/ADMIN role in the
// organization (a faction role grants nothing here). Every route is scoped organization +
// installation; the installation fixes the Discord guild (the balance scope) and the DayZ
// server (write attribution). The handlers contain no balance logic: everything goes through
// internal/economy, which uses the one ledger.

const (
	codeInsufficientFunds      = "INSUFFICIENT_FUNDS"
	codeInvalidAmount          = "INVALID_AMOUNT"
	codePlayerIdentityRequired = "PLAYER_IDENTITY_REQUIRED"
	codeEconomyAccountNotFound = "ECONOMY_ACCOUNT_NOT_FOUND"
	codeDuplicateTransaction   = "DUPLICATE_TRANSACTION"
	codeEconomyForbidden       = "ECONOMY_FORBIDDEN"

	economyTimeout = 10 * time.Second
)

func init() {
	httpStatusForCode[codeInsufficientFunds] = http.StatusConflict
	httpStatusForCode[codeInvalidAmount] = http.StatusBadRequest
	httpStatusForCode[codePlayerIdentityRequired] = http.StatusConflict
	httpStatusForCode[codeEconomyAccountNotFound] = http.StatusNotFound
	httpStatusForCode[codeDuplicateTransaction] = http.StatusConflict
	httpStatusForCode[codeEconomyForbidden] = http.StatusForbidden
}

func (a *App) registerEconomyRoutes() {
	if a.saasEconomyAdjustLimiter == nil {
		a.saasEconomyAdjustLimiter = newSaaSRateLimiter(time.Minute, 30)
	}
	if a.saasEconomyHistoryLimiter == nil {
		a.saasEconomyHistoryLimiter = newSaaSRateLimiter(time.Minute, 120)
	}
	const base = "/api/saas/organizations/{organizationID}/installations/{installationID}/economy"
	h := a.HTTPServer.Handle
	h("GET "+base+"/me", a.handleEconomyMe)
	h("GET "+base+"/me/transactions", a.handleEconomyMyTransactions)
	h("GET "+base+"/accounts", a.handleEconomyAccountSearch)
	h("GET "+base+"/accounts/{accountID}", a.handleEconomyAccount)
	h("GET "+base+"/accounts/{accountID}/transactions", a.handleEconomyAccountTransactions)
	h("POST "+base+"/accounts/{accountID}/grant", a.handleEconomyGrant)
	h("POST "+base+"/accounts/{accountID}/debit", a.handleEconomyDebit)
}

type economyRequest struct {
	user  *repository.AppUser
	scope repository.EconomyScope
}

// economyContext runs the standard chain (service auth, acting user, path ids) and resolves the
// installation to its scope. admin additionally requires an OWNER/ADMIN role in the organization.
func (a *App) economyContext(w http.ResponseWriter, r *http.Request, admin bool) (er economyRequest, ok bool) {
	code := ""
	if admin {
		code = codeEconomyForbidden
	}
	return a.scopedContext(w, r, code)
}

// scopedContext is economyContext with the admin gate's error code chosen by the caller (the shop uses
// SHOP_FORBIDDEN): adminCode == "" means a player route (no organization role needed).
func (a *App) scopedContext(w http.ResponseWriter, r *http.Request, adminCode string) (er economyRequest, ok bool) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	orgID, good := pathInt64(w, r, "organizationID")
	if !good {
		return
	}
	instID, good := pathInt64(w, r, "installationID")
	if !good {
		return
	}
	if a.EconomyAccounts == nil {
		writeSaaSError(w, codeInternalError, "economy unavailable")
		return
	}
	if adminCode != "" && !a.economyAdminAllowed(w, r, orgID, user.ID, adminCode) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	scope, err := a.EconomyAccounts.Scope(ctx, orgID, instID)
	if err != nil {
		economyFailed(w, "resolve economy scope", err)
		return
	}
	return economyRequest{user: user, scope: scope}, true
}

// economyAdminAllowed is the economy admin gate: an OWNER or ADMIN of the organization. Faction
// roles and MEMBER grant nothing.
func (a *App) economyAdminAllowed(w http.ResponseWriter, r *http.Request, orgID, userID int64, code string) bool {
	if a.SaaSOrganizations == nil {
		writeSaaSError(w, codeInternalError, "organization directory unavailable")
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	role, member, err := a.SaaSOrganizations.VerifyMembership(ctx, orgID, userID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "economy admin membership check failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not verify membership")
		return false
	}
	if !member || (role != repository.RoleOwner && role != repository.RoleAdmin) {
		writeSaaSError(w, code, "OWNER or ADMIN role required")
		return false
	}
	return true
}

// economyFailed maps a service error to the stable error contract. Raw errors are logged, never returned.
func economyFailed(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, economy.ErrInstallationNotFound):
		writeSaaSError(w, codeNotFound, "installation not found")
	case errors.Is(err, economy.ErrAccountNotFound):
		writeSaaSError(w, codeEconomyAccountNotFound, "economy account not found")
	case errors.Is(err, economy.ErrIdentityRequired):
		writeSaaSError(w, codePlayerIdentityRequired, "link your DayZ account to use the economy")
	case errors.Is(err, economy.ErrInvalidAmount):
		writeSaaSError(w, codeInvalidAmount, "amount must be a whole number from 1 to 1,000,000,000")
	case errors.Is(err, economy.ErrInsufficientFunds):
		writeSaaSError(w, codeInsufficientFunds, "the account balance is too low for this debit")
	case errors.Is(err, economy.ErrIdempotencyMismatch):
		writeSaaSError(w, codeDuplicateTransaction, "this idempotency key was already used with a different amount")
	case errors.Is(err, economy.ErrReasonRequired):
		writeSaaSError(w, codeInvalidRequest, "a reason is required")
	case errors.Is(err, economy.ErrInvalidQuery):
		writeSaaSError(w, codeInvalidRequest, "q must be 2 to 50 characters")
	case errors.Is(err, economy.ErrInvalidCursor):
		writeSaaSError(w, codeInvalidRequest, "invalid cursor")
	case errors.Is(err, economy.ErrInvalidFilter):
		writeSaaSError(w, codeInvalidRequest, "unknown transaction type")
	case errors.Is(err, economy.ErrInvalidKey):
		writeSaaSError(w, codeInvalidRequest, "idempotencyKey must be 8-64 characters of A-Z a-z 0-9 . _ : -")
	case errors.Is(err, economy.ErrNoServer):
		writeSaaSError(w, codeConflict, "the installation has no DayZ server selected")
	case errors.Is(err, economy.ErrSuspended):
		writeSaaSError(w, codeConflict, "the installation is suspended")
	case errors.Is(err, economy.ErrForbiddenTenant), errors.Is(err, economy.ErrServerNotInGuild):
		writeSaaSError(w, codeEconomyForbidden, "not allowed for this installation")
	default:
		slog.Error("component=saas_api", "msg", what+" failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "economy request failed")
	}
}

// --- DTOs (docs/ECONOMY.md "Website handoff") ---------------------------------------------------------

type economyAccountDTO struct {
	AccountID int64   `json:"accountId"`
	Gamertag  string  `json:"gamertag"`
	Balance   int64   `json:"balance"`
	UpdatedAt *string `json:"updatedAt"`
}

type economyBalanceResponse struct {
	Currency       economy.Currency  `json:"currency"`
	Account        economyAccountDTO `json:"account"`
	InstallationID int64             `json:"installationId"`
	GameServerID   *int64            `json:"gameServerId"`
}

type adminEconomyAccountDTO struct {
	economyAccountDTO
	Linked        bool    `json:"linked"`
	DiscordUserID *string `json:"discordUserId"`
	DisplayName   *string `json:"displayName"`
	Avatar        *string `json:"avatar"`
}

type economyTransactionDTO struct {
	ID            int64   `json:"id"`
	Type          string  `json:"type"`
	Direction     string  `json:"direction"`
	Amount        int64   `json:"amount"`
	BalanceAfter  int64   `json:"balanceAfter"`
	Description   string  `json:"description"`
	ReferenceType *string `json:"referenceType"`
	CreatedAt     string  `json:"createdAt"`
}

type adminEconomyTransactionDTO struct {
	economyTransactionDTO
	Reason             *string `json:"reason"`
	ActorDiscordUserID *string `json:"actorDiscordUserId"`
	ReferenceID        *string `json:"referenceId"`
	GameServerID       *int64  `json:"gameServerId"`
	IsSystem           bool    `json:"isSystem"`
}

type economyTransactionList[T any] struct {
	Currency   economy.Currency `json:"currency"`
	Items      []T              `json:"items"`
	NextCursor *string          `json:"nextCursor"`
	Limit      int              `json:"limit"`
}

type adminEconomyAccountList struct {
	Currency economy.Currency         `json:"currency"`
	Items    []adminEconomyAccountDTO `json:"items"`
	Limit    int                      `json:"limit"`
}

type economyAdjustmentResponse struct {
	Currency    economy.Currency           `json:"currency"`
	Account     adminEconomyAccountDTO     `json:"account"`
	Transaction adminEconomyTransactionDTO `json:"transaction"`
	Duplicate   bool                       `json:"duplicate"`
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func accountDTO(acc economy.Account) economyAccountDTO {
	return economyAccountDTO{AccountID: acc.AccountID, Gamertag: acc.Gamertag, Balance: acc.Balance, UpdatedAt: nullableTimeStr(acc.UpdatedAt)}
}

func adminAccountDTO(acc economy.Account) adminEconomyAccountDTO {
	return adminEconomyAccountDTO{economyAccountDTO: accountDTO(acc), Linked: acc.Linked, DiscordUserID: optStr(acc.DiscordUserID), DisplayName: optStr(acc.DisplayName), Avatar: optStr(acc.Avatar)}
}

func transactionDTO(t economy.Transaction) economyTransactionDTO {
	return economyTransactionDTO{ID: t.ID, Type: t.Type, Direction: t.Direction, Amount: t.Amount, BalanceAfter: t.BalanceAfter,
		Description: t.Description, ReferenceType: optStr(t.ReferenceType), CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339)}
}

func adminTransactionDTO(t economy.Transaction) adminEconomyTransactionDTO {
	d := adminEconomyTransactionDTO{economyTransactionDTO: transactionDTO(t), Reason: optStr(t.Reason), ActorDiscordUserID: optStr(t.ActorID),
		ReferenceID: optStr(t.ReferenceID), IsSystem: t.ActorID == "SYSTEM"}
	if t.ServerID != 0 {
		id := t.ServerID
		d.GameServerID = &id
	}
	if d.IsSystem {
		d.ActorDiscordUserID = nil
	}
	return d
}

func serverPtr(s repository.EconomyScope) *int64 {
	if s.ServerID == 0 {
		return nil
	}
	id := s.ServerID
	return &id
}

// --- player routes -----------------------------------------------------------------------------------

// handleEconomyMe is GET .../economy/me: the acting user's own balance through their VERIFIED
// DayZ link. There is no way to name another player: the account comes from the acting identity.
func (a *App) handleEconomyMe(w http.ResponseWriter, r *http.Request) {
	er, ok := a.economyContext(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	acc, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	if err != nil {
		economyFailed(w, "load economy balance", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, economyBalanceResponse{Currency: economy.ChampionPoints, Account: accountDTO(acc), InstallationID: er.scope.InstallationID, GameServerID: serverPtr(er.scope)})
}

// economyPageParams reads limit, cursor and type (with the shared 400 behaviour).
func economyPageParams(w http.ResponseWriter, r *http.Request) (limit int, cursor string, filter repository.TransactionFilter, ok bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, "malformed query string")
		return
	}
	limit, ok = factionLimit(w, r, economy.DefaultTransactionLimit, economy.MaxTransactionLimit)
	if !ok {
		return
	}
	filter, err = economy.ParseTypeFilter(q.Get("type"))
	if err != nil {
		economyFailed(w, "parse type filter", err)
		return limit, "", filter, false
	}
	return limit, strings.TrimSpace(q.Get("cursor")), filter, true
}

// handleEconomyMyTransactions is GET .../economy/me/transactions?limit=&cursor=&type=: the acting
// user's own history, newest first. Descriptions are generated from the type; the admin's
// reason and the actor are never included.
func (a *App) handleEconomyMyTransactions(w http.ResponseWriter, r *http.Request) {
	er, ok := a.economyContext(w, r, false)
	if !ok || !enforceRateLimit(w, a.saasEconomyHistoryLimiter, rateLimitKey(r)) {
		return
	}
	limit, cursor, filter, ok := economyPageParams(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	acc, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	if err != nil {
		economyFailed(w, "resolve economy identity", err)
		return
	}
	page, err := a.EconomyAccounts.Transactions(ctx, er.scope, acc.AccountID, limit, cursor, filter, false)
	if err != nil {
		economyFailed(w, "load economy transactions", err)
		return
	}
	out := economyTransactionList[economyTransactionDTO]{Currency: economy.ChampionPoints, Items: make([]economyTransactionDTO, 0, len(page.Items)), Limit: page.Limit, NextCursor: optStr(page.NextCursor)}
	for _, t := range page.Items {
		out.Items = append(out.Items, transactionDTO(t))
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

// --- admin routes ------------------------------------------------------------------------------------

// handleEconomyAccountSearch is GET .../economy/accounts?q=&limit=: OWNER/ADMIN lookup of players
// of the installation's guild by gamertag or verified Discord name (2-50 characters, at most 25 results).
func (a *App) handleEconomyAccountSearch(w http.ResponseWriter, r *http.Request) {
	er, ok := a.economyContext(w, r, true)
	if !ok {
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, "malformed query string")
		return
	}
	limit, ok := factionLimit(w, r, repository.MaxAccountSearchResults, repository.MaxAccountSearchResults)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	accounts, err := a.EconomyAccounts.Search(ctx, er.scope, query.Get("q"), limit)
	if err != nil {
		economyFailed(w, "search economy accounts", err)
		return
	}
	out := adminEconomyAccountList{Currency: economy.ChampionPoints, Items: make([]adminEconomyAccountDTO, 0, len(accounts)), Limit: limit}
	for _, acc := range accounts {
		out.Items = append(out.Items, adminAccountDTO(acc))
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

func (a *App) handleEconomyAccount(w http.ResponseWriter, r *http.Request) {
	er, ok := a.economyContext(w, r, true)
	if !ok {
		return
	}
	accountID, ok := pathInt64(w, r, "accountID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	acc, err := a.EconomyAccounts.Account(ctx, er.scope, accountID)
	if err != nil {
		economyFailed(w, "load economy account", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, struct {
		Currency economy.Currency       `json:"currency"`
		Account  adminEconomyAccountDTO `json:"account"`
	}{economy.ChampionPoints, adminAccountDTO(acc)})
}

// handleEconomyAccountTransactions is the admin history of one account: the player view plus the
// reason, the acting admin and the reference.
func (a *App) handleEconomyAccountTransactions(w http.ResponseWriter, r *http.Request) {
	er, ok := a.economyContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasEconomyHistoryLimiter, rateLimitKey(r)) {
		return
	}
	accountID, ok := pathInt64(w, r, "accountID")
	if !ok {
		return
	}
	limit, cursor, filter, ok := economyPageParams(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	page, err := a.EconomyAccounts.Transactions(ctx, er.scope, accountID, limit, cursor, filter, true)
	if err != nil {
		economyFailed(w, "load economy transactions", err)
		return
	}
	out := economyTransactionList[adminEconomyTransactionDTO]{Currency: economy.ChampionPoints, Items: make([]adminEconomyTransactionDTO, 0, len(page.Items)), Limit: page.Limit, NextCursor: optStr(page.NextCursor)}
	for _, t := range page.Items {
		out.Items = append(out.Items, adminTransactionDTO(t))
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

type economyAdjustBody struct {
	Amount         json.Number `json:"amount"`
	Reason         string      `json:"reason"`
	IdempotencyKey string      `json:"idempotencyKey"`
}

func (a *App) handleEconomyGrant(w http.ResponseWriter, r *http.Request) {
	a.economyAdjust(w, r, false)
}
func (a *App) handleEconomyDebit(w http.ResponseWriter, r *http.Request) { a.economyAdjust(w, r, true) }

// economyAdjust is the shared admin grant/debit handler: OWNER/ADMIN only, rate limited, audited.
// The amount is a positive whole number (1..1,000,000,000); the reason is required (max 200).
func (a *App) economyAdjust(w http.ResponseWriter, r *http.Request, debit bool) {
	er, ok := a.economyContext(w, r, true)
	if !ok || !enforceRateLimit(w, a.saasEconomyAdjustLimiter, rateLimitKey(r)) {
		return
	}
	accountID, ok := pathInt64(w, r, "accountID")
	if !ok {
		return
	}
	var body economyAdjustBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	amount, err := strconv.ParseInt(body.Amount.String(), 10, 64)
	if err != nil {
		writeSaaSError(w, codeInvalidAmount, "amount must be a whole number from 1 to 1,000,000,000")
		return
	}
	req := economy.AdjustRequest{Scope: er.scope, ActorDiscordID: er.user.DiscordUserID, AccountID: accountID, Amount: amount, Reason: body.Reason, IdempotencyKey: body.IdempotencyKey}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	defer cancel()
	var adj economy.Adjustment
	event := "economy_admin_grant"
	if debit {
		event = "economy_admin_debit"
		adj, err = a.EconomyAccounts.Debit(ctx, req)
	} else {
		adj, err = a.EconomyAccounts.Grant(ctx, req)
	}
	if err != nil {
		economyFailed(w, event, err)
		return
	}
	// Audit: ids and the amount only - never the reason text or any credential.
	slog.Info("component=saas_api", "event", event, "organization_id", er.scope.OrganizationID, "installation_id", er.scope.InstallationID,
		"acting_user_id", er.user.ID, "account_id", accountID, "amount", amount, "transaction_id", adj.Transaction.ID, "duplicate", adj.Duplicate)
	acc, err := a.EconomyAccounts.Account(ctx, er.scope, accountID)
	if err != nil {
		economyFailed(w, "reload economy account", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, economyAdjustmentResponse{Currency: economy.ChampionPoints, Account: adminAccountDTO(acc), Transaction: adminTransactionDTO(adj.Transaction), Duplicate: adj.Duplicate})
}
