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

// Base registration requests. A verified player asks for their base to be
// registered at the position the server log last reported for them (never a
// position they type in); the server owner approves (registering the base) or
// declines, and the player gets a DM either way.

// A position older than this is too stale to register a base from.
const baseRequestPositionMaxAge = 30 * time.Minute

type baseRequestPosition struct {
	X          float64   `json:"x"`
	Z          float64   `json:"z"`
	ObservedAt time.Time `json:"observedAt"`
	Fresh      bool      `json:"fresh"`
}

type playerBaseRequestsResponse struct {
	Position *baseRequestPosition         `json:"position"`
	Bases    []repository.BlackBoxBase    `json:"bases"`
	Requests []repository.CaseBaseRequest `json:"requests"`
	MaxBases int                          `json:"maxBases"`
}

type playerBaseRequestBody struct {
	Name   string  `json:"name"`
	Radius float64 `json:"radius"`
	Note   string  `json:"note"`
}

// playerBaseRequestContext verifies the signed-in player. ok is false when a
// response has already been written.
func (a *App) playerBaseRequestContext(w http.ResponseWriter, r *http.Request) (repository.BaseRequestScope, int64, context.Context, context.CancelFunc, bool) {
	er, ok := a.scopedContext(w, r, "")
	if !ok {
		return repository.BaseRequestScope{}, 0, nil, nil, false
	}
	if er.scope.ServerID <= 0 || a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeConflict, "the installation has no DayZ server selected")
		return repository.BaseRequestScope{}, 0, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), economyTimeout)
	acct, err := a.EconomyAccounts.Me(ctx, er.scope, er.user.DiscordUserID)
	if err != nil {
		cancel()
		economyFailed(w, "verify base request player", err)
		return repository.BaseRequestScope{}, 0, nil, nil, false
	}
	return repository.BaseRequestScope{InstallationID: er.scope.InstallationID, GuildID: er.scope.GuildID, ServerID: er.scope.ServerID},
		acct.AccountID, ctx, cancel, true
}

func (a *App) latestBaseRequestPosition(ctx context.Context, s repository.BaseRequestScope, playerID int64) (*baseRequestPosition, error) {
	if a.Locations == nil {
		return nil, nil
	}
	loc, err := a.Locations.LatestLocation(ctx, s.GuildID, s.ServerID, playerID)
	if err != nil || loc == nil {
		return nil, err
	}
	return &baseRequestPosition{X: loc.X, Z: loc.Z, ObservedAt: loc.ObservedAt,
		Fresh: time.Since(loc.ObservedAt) <= baseRequestPositionMaxAge}, nil
}

// handleGetPlayerBaseRequests is GET .../security-marketplace/base-requests.
func (a *App) handleGetPlayerBaseRequests(w http.ResponseWriter, r *http.Request) {
	s, playerID, ctx, cancel, ok := a.playerBaseRequestContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	repo := repository.NewCaseBaseRequestRepository(a.DB.Pool)
	out := playerBaseRequestsResponse{MaxBases: repository.BaseRequestMaxPerPlayer}
	var err error
	if out.Position, err = a.latestBaseRequestPosition(ctx, s, playerID); err != nil {
		writeSaaSError(w, codeInternalError, "could not load your base requests")
		return
	}
	if out.Bases, err = repo.PlayerBases(ctx, s, playerID); err != nil {
		writeSaaSError(w, codeInternalError, "could not load your base requests")
		return
	}
	if out.Requests, err = repo.Mine(ctx, s, playerID, 10); err != nil {
		writeSaaSError(w, codeInternalError, "could not load your base requests")
		return
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

// handleCreatePlayerBaseRequest is POST .../security-marketplace/base-requests.
// The position always comes from the server log, never from the request.
func (a *App) handleCreatePlayerBaseRequest(w http.ResponseWriter, r *http.Request) {
	var body playerBaseRequestBody
	if !decodeFactionBody(w, r, &body) {
		return
	}
	if body.Radius < repository.BaseRequestMinRadius || body.Radius > repository.BaseRequestMaxRadius {
		writeSaaSError(w, codeInvalidRequest, "the base size must be 10 to 150 m")
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
	req, code, msg := a.submitBaseRequest(ctx, s, playerID, body.Name, body.Radius, body.Note)
	if code != "" {
		writeSaaSError(w, code, msg)
		return
	}
	writeSaaSJSON(w, http.StatusCreated, map[string]any{"request": req})
}

// submitBaseRequest creates a request at the player's last logged position
// (the website and /registerbase share it). On failure it returns an error
// code and a player-facing message.
func (a *App) submitBaseRequest(ctx context.Context, s repository.BaseRequestScope, playerID int64, name string, radius float64, note string) (repository.CaseBaseRequest, string, string) {
	if radius < repository.BaseRequestMinRadius || radius > repository.BaseRequestMaxRadius {
		return repository.CaseBaseRequest{}, codeInvalidRequest, "the base size must be 10 to 150 m"
	}
	pos, err := a.latestBaseRequestPosition(ctx, s, playerID)
	if err != nil {
		return repository.CaseBaseRequest{}, codeInternalError, "could not read your position"
	}
	if pos == nil || !pos.Fresh {
		return repository.CaseBaseRequest{}, codeConflict, "the server hasn't logged your position in the last 30 minutes; go to your base in game, wait a minute and try again"
	}
	req, err := repository.NewCaseBaseRequestRepository(a.DB.Pool).Create(ctx, s, repository.BaseRequestInput{
		PlayerID: playerID, Name: name, Note: note, CenterX: pos.X, CenterZ: pos.Z, Radius: radius, PositionSeenAt: pos.ObservedAt})
	switch {
	case errors.Is(err, repository.ErrBaseRequestOpen):
		return req, codeConflict, "you already have a base request waiting for the server owner"
	case errors.Is(err, repository.ErrBaseRequestLimit):
		return req, codeConflict, "you already have the most bases allowed on this server"
	case errors.Is(err, repository.ErrInvalidBaseRequest):
		return req, codeInvalidRequest, "give your base a name (up to 64 characters); the note can be up to 300"
	case err != nil:
		slog.Warn("component=base_requests", "event", "create_failed", "err", err.Error())
		return req, codeInternalError, "your request couldn't be sent"
	}
	slog.Info("component=base_requests", "event", "requested", "installation_id", s.InstallationID, "request_id", req.ID)
	a.notifyNewBaseRequest(s, req)
	return req, "", ""
}

// handleCancelPlayerBaseRequest is POST .../security-marketplace/base-requests/{requestID}/cancel.
func (a *App) handleCancelPlayerBaseRequest(w http.ResponseWriter, r *http.Request) {
	requestID, good := pathInt64(w, r, "requestID")
	if !good {
		return
	}
	s, playerID, ctx, cancel, ok := a.playerBaseRequestContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	err := repository.NewCaseBaseRequestRepository(a.DB.Pool).Cancel(ctx, s, playerID, requestID)
	if errors.Is(err, repository.ErrBaseRequestNotFound) {
		writeSaaSError(w, codeNotFound, "no waiting request to cancel")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "the request couldn't be cancelled")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// Owner side (server owner only, like base registration).

type baseRequestApproveBody struct {
	MapKey string  `json:"mapKey"`
	Name   string  `json:"name"`
	Radius float64 `json:"radius"`
}

type baseRequestDeclineBody struct {
	Reason string `json:"reason"`
}

func (a *App) ownerBaseRequestScope(w http.ResponseWriter, r *http.Request, write bool) (adminActor, repository.BaseRequestScope, bool) {
	ac, _, ok := a.caseBaseActor(w, r)
	if !ok {
		return ac, repository.BaseRequestScope{}, false
	}
	limiter := a.saasAdminReadLimiter
	if write {
		limiter = a.saasAdminActionLimiter
	}
	if !enforceRateLimit(w, limiter, rateLimitKey(r)) {
		return ac, repository.BaseRequestScope{}, false
	}
	return ac, repository.BaseRequestScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID}, true
}

// handleGetBaseRequests is GET .../admin/case/base-requests.
func (a *App) handleGetBaseRequests(w http.ResponseWriter, r *http.Request) {
	_, s, ok := a.ownerBaseRequestScope(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	repo := repository.NewCaseBaseRequestRepository(a.DB.Pool)
	reqs, err := repo.ForOwner(ctx, s, 50)
	if err != nil {
		slog.Warn("component=base_requests", "event", "list_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load base requests")
		return
	}
	hint, err := repo.MapKeyHint(ctx, s)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load base requests")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"requests": reqs, "mapKeyHint": hint})
}

// handleApproveBaseRequest is POST .../admin/case/base-requests/{requestID}/approve.
func (a *App) handleApproveBaseRequest(w http.ResponseWriter, r *http.Request) {
	requestID, good := pathInt64(w, r, "requestID")
	if !good {
		return
	}
	ac, s, ok := a.ownerBaseRequestScope(w, r, true)
	if !ok {
		return
	}
	var body baseRequestApproveBody
	if err := readCaseBaseJSON(w, r, &body); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid approval")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	d, err := repository.NewCaseBaseRequestRepository(a.DB.Pool).Approve(ctx, s, requestID, body.MapKey, body.Name, body.Radius, actor)
	if !a.baseRequestDecisionFailed(w, err) {
		a.recordAudit(ctx, ac, "CASE_BASE_REQUEST_APPROVED", "base-request", "", "success", nil,
			map[string]any{"requestId": requestID, "baseId": *d.Request.BaseID})
		a.notifyBaseRequestDecision(s.ServerID, d)
		writeSaaSJSON(w, http.StatusOK, map[string]any{"request": d.Request})
	}
}

// handleDeclineBaseRequest is POST .../admin/case/base-requests/{requestID}/decline.
func (a *App) handleDeclineBaseRequest(w http.ResponseWriter, r *http.Request) {
	requestID, good := pathInt64(w, r, "requestID")
	if !good {
		return
	}
	ac, s, ok := a.ownerBaseRequestScope(w, r, true)
	if !ok {
		return
	}
	var body baseRequestDeclineBody
	if err := readCaseBaseJSON(w, r, &body); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid decline")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	d, err := repository.NewCaseBaseRequestRepository(a.DB.Pool).Decline(ctx, s, requestID, body.Reason, actor)
	if !a.baseRequestDecisionFailed(w, err) {
		a.recordAudit(ctx, ac, "CASE_BASE_REQUEST_DECLINED", "base-request", "", "success", nil, map[string]any{"requestId": requestID})
		a.notifyBaseRequestDecision(s.ServerID, d)
		writeSaaSJSON(w, http.StatusOK, map[string]any{"request": d.Request})
	}
}

// baseRequestDecisionFailed writes the error response, if any, and reports whether it did.
func (a *App) baseRequestDecisionFailed(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, repository.ErrBaseRequestNotFound):
		writeSaaSError(w, codeNotFound, "base request not found")
	case errors.Is(err, repository.ErrBaseRequestDecided):
		writeSaaSError(w, codeConflict, "this request was already answered or cancelled")
	case errors.Is(err, repository.ErrInvalidBaseRequest):
		writeSaaSError(w, codeInvalidRequest, "choose a map, and a size of 10 to 150 m")
	default:
		slog.Warn("component=base_requests", "event", "decision_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "the request couldn't be answered right now")
	}
	return true
}

// notifyBaseRequestDecision DMs the player in the background; a closed DM is
// only logged.
func (a *App) notifyBaseRequestDecision(serverID int64, d repository.BaseRequestDecision) {
	if d.DiscordUserID == "" || a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	session := a.Discord.Session()
	server := a.serverName(serverID)
	msg := discord.BaseRequestDecisionMessage(d.Request.Status == repository.BaseRequestApproved, d.Request.Name, server, d.Request.DeclineReason)
	if d.Request.Status == repository.BaseRequestApproved && a.DB != nil && a.DB.Pool != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		rent, err := repository.NewBaseRentRepository(a.DB.Pool).GetSettings(ctx, repository.SecurityScope{
			InstallationID: d.InstallationID, GuildID: d.GuildID, ServerID: serverID})
		cancel()
		if err == nil && rent.Enabled {
			msg = discord.WithRentNotice(msg, rent.PricePoints, rent.PeriodDays, repository.BaseRentGraceDays)
		}
	}
	go func(userID string, msg *discordgo.MessageSend) {
		ch, err := session.UserChannelCreate(userID)
		if err == nil {
			_, err = session.ChannelMessageSendComplex(ch.ID, msg)
		}
		if err != nil {
			slog.Warn("component=base_requests", "event", "dm_failed", "request_id", d.Request.ID, "err", err.Error())
		}
	}(d.DiscordUserID, msg)
}

// notifyNewBaseRequest posts a staff notice to the ADMIN_ALERTS channel (when
// routed) and DMs the organization owner. Best effort and in the background:
// a failure never affects the request.
func (a *App) notifyNewBaseRequest(s repository.BaseRequestScope, req repository.CaseBaseRequest) {
	if a.DB == nil || a.DB.Pool == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ownerID, playerName, err := repository.NewCaseBaseRequestRepository(a.DB.Pool).RequestNotice(ctx, s, req.PlayerID)
		if err != nil {
			slog.Warn("component=base_requests", "event", "notice_lookup_failed", "request_id", req.ID, "err", err.Error())
			return
		}
		if playerName == "" {
			playerName = "A player"
		}
		if a.AdminAlerts != nil {
			a.AdminAlerts.Publish(discord.AdminAlert{GuildRowID: s.GuildID, ServerID: s.ServerID, Kind: discord.AlertKindBaseRequest,
				Severity: discord.AlertInfo, Headline: "NEW BASE REQUEST",
				Detail: presentation.SafeName(playerName, 64) + " wants **" + presentation.SafeName(req.Name, 64) + "** registered. Approve or decline it on the anti-cheat Bases tab.",
				Fields: [][2]string{{"Size", fmt.Sprintf("%.0f m", req.Radius)}}})
		}
		if ownerID == "" || a.Discord == nil || a.Discord.Session() == nil {
			return
		}
		session := a.Discord.Session()
		msg := discord.NewBaseRequestMessage(playerName, req.Name, a.serverName(s.ServerID), req.Radius, a.baseRequestsReviewURL())
		ch, err := session.UserChannelCreate(ownerID)
		if err == nil {
			_, err = session.ChannelMessageSendComplex(ch.ID, msg)
		}
		if err != nil {
			slog.Warn("component=base_requests", "event", "owner_dm_failed", "request_id", req.ID, "err", err.Error())
		}
	}()
}

// baseRequestsReviewURL is the dashboard link for reviewing requests, or ""
// when the site address isn't configured as https.
func (a *App) baseRequestsReviewURL() string {
	if a.Config == nil {
		return ""
	}
	base := strings.TrimRight(a.Config.SiteBaseURL, "/")
	if !strings.HasPrefix(base, "https://") {
		return ""
	}
	return base + "/dashboard/anti-cheat?tab=bases"
}
