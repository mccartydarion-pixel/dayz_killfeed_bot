package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Base registration is owner-only draft management. It cannot activate a
// detector, mark ownership verified, or send a C.A.S.E. Discord notification.
// The Base Raid Alarm (saas_api_base_raid_alarm.go) reads these bases to DM
// the base owner, only after the server owner turns it on.
func (a *App) registerCaseBaseRoutes(base string) {
	h := a.HTTPServer.Handle
	h("GET "+base+"/case/bases", a.handleCaseListBases)
	h("POST "+base+"/case/bases", a.handleCaseCreateBaseDraft)
	h("GET "+base+"/case/bases/{baseID}/grants", a.handleCaseListBaseGrants)
	h("POST "+base+"/case/bases/{baseID}/grants", a.handleCaseAddBaseGrant)
	h("POST "+base+"/case/bases/{baseID}/withdraw", a.handleCaseWithdrawBaseDraft)
	h("POST "+base+"/case/bases/{baseID}/grants/{grantID}/end", a.handleCaseEndBaseGrant)
	h("GET "+base+"/case/bases/shadow", a.handleCaseBaseShadow)
	h("GET "+base+"/case/shadow-verdicts", a.handleGetCaseShadowVerdicts)
	h("PUT "+base+"/case/shadow-verdicts", a.handleSaveCaseShadowVerdict)
	h("GET "+base+"/case/raid-alarm", a.handleGetBaseRaidAlarm)
	h("PUT "+base+"/case/raid-alarm", a.handleSetBaseRaidAlarm)
	h("PUT "+base+"/case/raid-alarm/offer", a.handleSetBaseRaidAlarmOffer)
	h("GET "+base+"/case/perimeter-watch", a.handleGetPerimeterWatch)
	h("PUT "+base+"/case/perimeter-watch", a.handleSetPerimeterWatch)
	h("PUT "+base+"/case/perimeter-watch/offer", a.handleSetPerimeterWatchOffer)
	h("GET "+base+"/case/black-box", a.handleGetBaseBlackBox)
	h("PUT "+base+"/case/black-box", a.handleSetBaseBlackBox)
	h("PUT "+base+"/case/black-box/offer", a.handleSetBaseBlackBoxOffer)
	h("GET "+base+"/case/faction-security", a.handleGetFactionSecurity)
	h("PUT "+base+"/case/faction-security", a.handleSetFactionSecurity)
	h("PUT "+base+"/case/faction-security/offer", a.handleSetFactionSecurityOffer)
	h("GET "+base+"/case/base-requests", a.handleGetBaseRequests)
	h("POST "+base+"/case/base-requests/{requestID}/approve", a.handleApproveBaseRequest)
	h("POST "+base+"/case/base-requests/{requestID}/decline", a.handleDeclineBaseRequest)
	h("GET "+base+"/case/sentinel-pro", a.handleGetSentinelPro)
	h("PUT "+base+"/case/sentinel-pro/offer", a.handleSetSentinelProOffer)
	h("GET "+base+"/case/security-sales", a.handleGetSecuritySales)
	h("GET "+base+"/case/security-panel", a.handleGetSecurityPanel)
	h("PUT "+base+"/case/security-panel", a.handleSetSecurityPanel)
	h("GET "+base+"/case/security-gifts", a.handleListSecurityGifts)
	h("POST "+base+"/case/security-gifts", a.handleGiveSecurityGift)
	h("GET "+base+"/case/base-rent", a.handleGetBaseRent)
	h("PUT "+base+"/case/base-rent", a.handleSetBaseRent)
	h("POST "+base+"/case/base-rent/gift", a.handleGiftBaseRent)
	h("PUT "+base+"/case/base-rent/rent-free", a.handleSetBaseRentFree)
	h("GET "+base+"/case/base-transfers", a.handleGetBaseTransfers)
	h("POST "+base+"/case/base-transfers/{transferID}/approve", a.handleApproveBaseTransfer)
	h("POST "+base+"/case/base-transfers/{transferID}/decline", a.handleDeclineBaseTransfer)
}

func (a *App) caseBaseActor(w http.ResponseWriter, r *http.Request) (adminActor, *repository.CaseBaseRegistrationRepository, bool) {
	ac, ok := a.requireCapability(w, r, permissions.CapUAVManage)
	if !ok {
		return adminActor{}, nil, false
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return adminActor{}, nil, false
	}
	if a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeInternalError, "C.A.S.E. base drafts unavailable")
		return adminActor{}, nil, false
	}
	return ac, repository.NewCaseBaseRegistrationRepository(a.DB.Pool), true
}

func (a *App) handleCaseListBases(w http.ResponseWriter, r *http.Request) {
	ac, repo, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	before := int64(0)
	if raw := r.URL.Query().Get("beforeId"); raw != "" {
		var err error
		before, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || before <= 0 {
			writeSaaSError(w, codeInvalidRequest, "invalid beforeId")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := repo.ListBases(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, before, 50)
	if err != nil {
		slog.Warn("component=case", "event", "list_base_drafts_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list base drafts")
		return
	}
	a.recordAudit(ctx, ac, "CASE_BASE_DRAFTS_VIEWED", "", "", "success", nil, map[string]any{"count": len(rows)})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": rows, "operational": false})
}

type caseCreateBaseDraftRequest struct {
	OwnerPlayerID int64   `json:"ownerPlayerId"`
	MapKey        string  `json:"mapKey"`
	Name          string  `json:"name"`
	CenterX       float64 `json:"centerX"`
	CenterZ       float64 `json:"centerZ"`
	Radius        float64 `json:"radius"`
}

// Decode one bounded object. A second JSON value, an unknown field, or a
// truncated/oversized body must fail before any draft or grant is persisted.
func readCaseBaseJSON(w http.ResponseWriter, r *http.Request, out any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values")
	}
	return nil
}

func (a *App) handleCaseCreateBaseDraft(w http.ResponseWriter, r *http.Request) {
	ac, repo, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req caseCreateBaseDraftRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid base draft")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	b, err := repo.CreateDraft(ctx, repository.CaseBaseDraftInput{
		InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: *ac.scope.ServerID,
		OwnerPlayerID: req.OwnerPlayerID, MapKey: strings.TrimSpace(req.MapKey), Name: strings.TrimSpace(req.Name),
		CenterX: req.CenterX, CenterZ: req.CenterZ, Radius: req.Radius,
	})
	if errors.Is(err, repository.ErrCaseBaseNotFound) {
		writeSaaSError(w, codeInvalidRequest, "owner player is not on this installation's guild")
		return
	}
	if err != nil {
		slog.Warn("component=case", "event", "create_base_draft_failed", "err", err.Error())
		writeSaaSError(w, codeInvalidRequest, "invalid or mismatched base draft")
		return
	}
	a.recordAudit(ctx, ac, "CASE_BASE_DRAFT_CREATED", "case-base:"+strconv.FormatInt(b.ID, 10), "", "success", nil, map[string]any{"state": b.State})
	writeSaaSJSON(w, http.StatusCreated, map[string]any{"base": b, "operational": false})
}

type caseAddGrantRequest struct {
	PlayerID   *int64     `json:"playerId"`
	FactionID  *int64     `json:"factionId"`
	ValidUntil *time.Time `json:"validUntil"`
}

func (a *App) handleCaseListBaseGrants(w http.ResponseWriter, r *http.Request) {
	ac, repo, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	id, good := pathInt64(w, r, "baseID")
	if !good {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := repo.ListAuthorizations(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, id)
	if err != nil {
		slog.Warn("component=case", "event", "list_base_grants_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list base grants")
		return
	}
	a.recordAudit(ctx, ac, "CASE_BASE_GRANTS_VIEWED", "case-base:"+strconv.FormatInt(id, 10), "", "success", nil, map[string]any{"count": len(rows)})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": rows, "operational": false})
}

func (a *App) handleCaseAddBaseGrant(w http.ResponseWriter, r *http.Request) {
	ac, repo, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	id, good := pathInt64(w, r, "baseID")
	if !good {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req caseAddGrantRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid base grant")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	grant, err := repo.AddDraftGrant(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, id, req.PlayerID, req.FactionID, req.ValidUntil)
	if errors.Is(err, repository.ErrCaseBaseNotFound) {
		writeSaaSError(w, codeInvalidRequest, "base draft or grant subject unavailable")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid base grant")
		return
	}
	a.recordAudit(ctx, ac, "CASE_BASE_DRAFT_GRANT_CREATED", "case-base:"+strconv.FormatInt(id, 10), "", "success", nil, map[string]any{"grantId": grant.ID})
	writeSaaSJSON(w, http.StatusCreated, map[string]any{"grant": grant, "operational": false})
}

func (a *App) handleCaseWithdrawBaseDraft(w http.ResponseWriter, r *http.Request) {
	ac, repo, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	id, good := pathInt64(w, r, "baseID")
	if !good {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	b, err := repo.RevokeDraft(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, id)
	if errors.Is(err, repository.ErrCaseBaseNotFound) {
		writeSaaSError(w, codeInvalidRequest, "base draft unavailable")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not withdraw base draft")
		return
	}
	a.recordAudit(ctx, ac, "CASE_BASE_DRAFT_WITHDRAWN", "case-base:"+strconv.FormatInt(id, 10), "", "success", nil, map[string]any{"state": b.State})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"base": b, "operational": false})
}

func (a *App) handleCaseEndBaseGrant(w http.ResponseWriter, r *http.Request) {
	ac, repo, ok := a.caseBaseActor(w, r)
	if !ok {
		return
	}
	baseID, good := pathInt64(w, r, "baseID")
	if !good {
		return
	}
	grantID, good := pathInt64(w, r, "grantID")
	if !good {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	grant, err := repo.EndDraftGrant(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, baseID, grantID)
	if errors.Is(err, repository.ErrCaseBaseNotFound) {
		writeSaaSError(w, codeInvalidRequest, "active base grant unavailable")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not end base grant")
		return
	}
	a.recordAudit(ctx, ac, "CASE_BASE_DRAFT_GRANT_ENDED", "case-base:"+strconv.FormatInt(baseID, 10), "", "success", nil, map[string]any{"grantId": grant.ID})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"grant": grant, "operational": false})
}
