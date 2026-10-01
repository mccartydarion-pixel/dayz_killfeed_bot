// Package app: Champion platform-owner write API (Owner Hub controls).
//
// Phase 2 of docs/ADMIN_API.md: the first routes under /api/admin that change something. Every
// one goes through adminRoute (service bearer + acting user on the CHAMPION_ADMIN_DISCORD_IDS
// allowlist), demands a reason, writes platform_audit_log, and touches exactly one target. The
// Stripe-owned subscription row is never overwritten here: a Stripe-managed organization is
// cancelled, reactivated or reconciled through billing.Service like a customer would, and the
// owner-only trial/grant/revoke mutations refuse it.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

const (
	ownerReasonMax     = 500
	ownerActionTimeout = 15 * time.Second
	// ownerTrialMaxDays bounds an owner-set trial so a typo cannot hand out years.
	ownerTrialMaxDays = 365
	ownerGrantMaxDays = 3650
)

// ownerRequest is the JSON body every owner mutation accepts. Fields not used by a route are
// ignored; reason is always required.
type ownerRequest struct {
	Reason string  `json:"reason"`
	Days   int     `json:"days,omitempty"`  // trial / grant length
	Until  string  `json:"until,omitempty"` // RFC3339 alternative to days
	Plan   string  `json:"plan,omitempty"`  // grant plan key
	UserID int64   `json:"userId,omitempty"`
	Note   *string `json:"note,omitempty"`
}

func (a *App) readOwnerRequest(w http.ResponseWriter, r *http.Request) (ownerRequest, bool) {
	var req ownerRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, "could not read request body")
		return req, false
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeSaaSError(w, codeInvalidRequest, "request body must be JSON")
			return req, false
		}
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		writeSaaSError(w, codeInvalidRequest, "a reason is required for every owner action")
		return req, false
	}
	if utf8.RuneCountInString(req.Reason) > ownerReasonMax {
		writeSaaSError(w, codeInvalidRequest, "reason is too long (max 500 characters)")
		return req, false
	}
	return req, true
}

// ownerUntil resolves days/until into a deadline; nil when neither was given.
func ownerUntil(req ownerRequest, maxDays int) (*time.Time, error) {
	now := time.Now().UTC()
	if u := strings.TrimSpace(req.Until); u != "" {
		t, err := time.Parse(time.RFC3339, u)
		if err != nil {
			return nil, errors.New("until must be an RFC3339 timestamp")
		}
		t = t.UTC()
		if !t.After(now) {
			return nil, errors.New("until must be in the future")
		}
		if t.After(now.AddDate(0, 0, maxDays)) {
			return nil, errors.New("until is too far in the future")
		}
		return &t, nil
	}
	if req.Days != 0 {
		if req.Days < 1 || req.Days > maxDays {
			return nil, errors.New("days is out of range")
		}
		t := now.AddDate(0, 0, req.Days)
		return &t, nil
	}
	return nil, nil
}

// ownerAudit writes the platform audit row for one owner action. It never blocks the action:
// a failed write is logged at ERROR (the action already happened).
func (a *App) ownerAudit(ctx context.Context, admin adminIdentity, action, targetType string, targetID int64, orgID *int64, reason, result string, before, after any) {
	if a.PlatformOwner == nil {
		return
	}
	e := repository.PlatformAuditEntry{ActorDiscordID: admin.DiscordID, Action: action, TargetType: targetType, Reason: reason, Result: result}
	if targetID > 0 {
		e.TargetID = &targetID
	}
	e.OrganizationID = orgID
	if before != nil {
		if b, err := json.Marshal(before); err == nil {
			e.BeforeState = b
		}
	}
	if after != nil {
		if b, err := json.Marshal(after); err == nil {
			e.AfterState = b
		}
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.PlatformOwner.RecordAudit(wctx, e); err != nil {
		slog.Error("component=admin_api", "event", "owner_audit_write_failed", "action", action, "target_type", targetType, "target_id", targetID, "err", err.Error())
	}
	slog.Info("component=admin_api", "event", "owner_action", "acting_admin_discord_id", admin.DiscordID, "action", action, "target_type", targetType, "target_id", targetID, "result", result)
}

func ownerFailed(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, repository.ErrOwnerStripeManaged):
		writeSaaSError(w, codeConflict, "this organization is billed through Stripe; cancel, reactivate or reconcile it instead")
	case errors.Is(err, repository.ErrOwnerNoChange):
		writeSaaSError(w, codeConflict, "already in the requested state")
	case errors.Is(err, billing.ErrNoActiveSubscription), errors.Is(err, billing.ErrProviderNotConfigured), errors.Is(err, billing.ErrUnknownPlan):
		billingFailed(w, what, err)
	default:
		slog.Warn("component=admin_api", "event", "owner_action_failed", "what", what, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not "+what)
	}
}

// ownerSubscriptionDTO is the sanitized subscription an owner action returns (no Stripe ids).
type ownerSubscriptionDTO struct {
	Plan              string  `json:"plan"`
	Status            string  `json:"status"`
	TrialEndsAt       *string `json:"trialEndsAt"`
	CurrentPeriodEnd  *string `json:"currentPeriodEnd"`
	CancelAtPeriodEnd bool    `json:"cancelAtPeriodEnd"`
	ExternallyBilled  bool    `json:"externallyBilled"`
	OwnerGrantUntil   *string `json:"ownerGrantUntil"`
	OwnerGrantReason  *string `json:"ownerGrantReason"`
	BillingRequired   bool    `json:"billingRequired"`
	TrialStatus       string  `json:"trialStatus"`
}

func ownerSubscription(sub *repository.Subscription) ownerSubscriptionDTO {
	if sub == nil {
		return ownerSubscriptionDTO{Plan: repository.PlanNone, Status: repository.SubscriptionInactive, BillingRequired: true, TrialStatus: billing.TrialNotStarted}
	}
	st := billing.StateOf(sub, time.Now())
	return ownerSubscriptionDTO{
		Plan: sub.Plan, Status: sub.Status, TrialEndsAt: nullableTimeStr(sub.TrialEndsAt), CurrentPeriodEnd: nullableTimeStr(sub.CurrentPeriodEnd),
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd, ExternallyBilled: sub.ProviderSubscriptionID != "",
		OwnerGrantUntil: nullableTimeStr(sub.OwnerGrantUntil), OwnerGrantReason: optStr(sub.OwnerGrantReason),
		BillingRequired: st.BillingRequired, TrialStatus: st.TrialStatus,
	}
}

func ownerSummaryDTO(sum *billing.Summary) ownerSubscriptionDTO {
	return ownerSubscriptionDTO{Plan: sum.Plan, Status: sum.Status, TrialEndsAt: nullableTimeStr(sum.TrialEndsAt), CurrentPeriodEnd: nullableTimeStr(sum.CurrentPeriodEnd),
		CancelAtPeriodEnd: sum.CancelAtPeriodEnd, ExternallyBilled: sum.HasActiveSubscription, BillingRequired: sum.BillingRequired, TrialStatus: sum.TrialStatus}
}

// --- organization / billing -------------------------------------------------------------------------

func (a *App) ownerOrgContext(w http.ResponseWriter, r *http.Request) (orgID int64, before *repository.Subscription, ok bool) {
	orgID, ok = pathInt64(w, r, "organizationID")
	if !ok {
		return 0, nil, false
	}
	if a.PlatformOwner == nil || a.SaaSSubscriptions == nil || a.SaaSOrganizations == nil {
		writeSaaSError(w, codeInternalError, "owner controls unavailable")
		return 0, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	org, err := a.SaaSOrganizations.GetByID(ctx, orgID)
	if err != nil {
		ownerFailed(w, "load organization", err)
		return 0, nil, false
	}
	if org == nil {
		writeSaaSError(w, codeNotFound, "organization not found")
		return 0, nil, false
	}
	before, err = a.SaaSSubscriptions.GetForOrganization(ctx, orgID)
	if err != nil {
		ownerFailed(w, "load subscription", err)
		return 0, nil, false
	}
	return orgID, before, true
}

// handleOwnerSetTrial is POST /api/admin/organizations/{id}/trial: start or extend a trial
// (`days` from now, or `until`). Refused for a Stripe-managed subscription.
func (a *App) handleOwnerSetTrial(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	until, err := ownerUntil(req, ownerTrialMaxDays)
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	}
	if until == nil {
		writeSaaSError(w, codeInvalidRequest, "days or until is required")
		return
	}
	orgID, before, ok := a.ownerOrgContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	after, err := a.PlatformOwner.SetTrial(ctx, orgID, *until, req.Reason)
	if err != nil {
		ownerFailed(w, "set trial", err)
		return
	}
	a.ownerAudit(ctx, admin, "organization.trial_set", "organization", orgID, &orgID, req.Reason, "OK", ownerSubscription(before), ownerSubscription(after))
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"subscription": ownerSubscription(after)})
}

// handleOwnerGrantPlan is POST /api/admin/organizations/{id}/grant: activate `plan` without
// Stripe, open-ended or until `days`/`until`. Refused for a Stripe-managed subscription.
func (a *App) handleOwnerGrantPlan(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	plan := strings.TrimSpace(req.Plan)
	if plan == "" {
		writeSaaSError(w, codeInvalidRequest, "plan is required")
		return
	}
	if catalog := a.billingCatalog(); catalog != nil {
		if _, known := catalog.Get(plan); !known {
			writeSaaSError(w, codeInvalidPlan, "unknown plan")
			return
		}
	} else if _, unknown := adminPlanParamValue(plan); unknown {
		writeSaaSError(w, codeInvalidPlan, "unknown plan")
		return
	}
	until, err := ownerUntil(req, ownerGrantMaxDays)
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	}
	orgID, before, ok := a.ownerOrgContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	after, err := a.PlatformOwner.GrantPlan(ctx, orgID, plan, until, req.Reason)
	if err != nil {
		ownerFailed(w, "grant plan", err)
		return
	}
	a.ownerAudit(ctx, admin, "organization.plan_granted", "organization", orgID, &orgID, req.Reason, "OK", ownerSubscription(before), ownerSubscription(after))
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"subscription": ownerSubscription(after)})
}

// handleOwnerRevokeAccess is POST /api/admin/organizations/{id}/revoke: end a grant or trial
// (INACTIVE / NONE: billing required). Refused for a Stripe-managed subscription.
func (a *App) handleOwnerRevokeAccess(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	orgID, before, ok := a.ownerOrgContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	after, err := a.PlatformOwner.RevokeAccess(ctx, orgID, req.Reason)
	if err != nil {
		ownerFailed(w, "revoke access", err)
		return
	}
	a.ownerAudit(ctx, admin, "organization.access_revoked", "organization", orgID, &orgID, req.Reason, "OK", ownerSubscription(before), ownerSubscription(after))
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"subscription": ownerSubscription(after)})
}

// ownerBillingAction runs a Stripe-backed billing.Service action on behalf of the owner.
func (a *App) ownerBillingAction(w http.ResponseWriter, r *http.Request, admin adminIdentity, action, what string, run func(ctx context.Context, orgID int64) (*billing.Summary, error)) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	if a.Billing == nil {
		writeSaaSError(w, codeBillingUnavailable, "billing is not configured on this environment")
		return
	}
	orgID, before, ok := a.ownerOrgContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	sum, err := run(ctx, orgID)
	if err != nil {
		ownerFailed(w, what, err)
		return
	}
	a.ownerAudit(ctx, admin, action, "organization", orgID, &orgID, req.Reason, "OK", ownerSubscription(before), ownerSummaryDTO(sum))
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"subscription": ownerSummaryDTO(sum)})
}

func (a *App) handleOwnerBillingCancel(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	a.ownerBillingAction(w, r, admin, "organization.billing_cancelled", "cancel subscription", a.Billing.Cancel)
}

func (a *App) handleOwnerBillingReactivate(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	a.ownerBillingAction(w, r, admin, "organization.billing_reactivated", "reactivate subscription", a.Billing.Reactivate)
}

func (a *App) handleOwnerBillingReconcile(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	a.ownerBillingAction(w, r, admin, "organization.billing_reconciled", "reconcile subscription", a.Billing.Reconcile)
}

// --- installations ----------------------------------------------------------------------------------

type ownerInstallationDTO struct {
	ID                  int64   `json:"id"`
	OrganizationID      int64   `json:"organizationId"`
	Status              string  `json:"status"`
	SuspendedAt         *string `json:"suspendedAt"`
	SuspendedReason     *string `json:"suspendedReason"`
	StatusBeforeSuspend *string `json:"statusBeforeSuspend"`
	WorkerRunning       bool    `json:"workerRunning"`
}

func (a *App) ownerInstallation(s *repository.InstallationSuspension) ownerInstallationDTO {
	out := ownerInstallationDTO{ID: s.InstallationID, OrganizationID: s.OrganizationID, Status: s.Status, SuspendedAt: nullableTimeStr(s.SuspendedAt), SuspendedReason: optStr(s.SuspendedReason), StatusBeforeSuspend: optStr(s.StatusBeforeSuspend)}
	if s.GameServerID != nil && a.WorkerManager != nil {
		out.WorkerRunning = a.WorkerManager.Running(*s.GameServerID)
	}
	return out
}

func (a *App) ownerInstallationContext(w http.ResponseWriter, r *http.Request) (*repository.InstallationSuspension, bool) {
	id, ok := pathInt64(w, r, "installationID")
	if !ok {
		return nil, false
	}
	if a.PlatformOwner == nil {
		writeSaaSError(w, codeInternalError, "owner controls unavailable")
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	inst, err := a.PlatformOwner.GetInstallation(ctx, id)
	if err != nil {
		ownerFailed(w, "load installation", err)
		return nil, false
	}
	if inst == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return nil, false
	}
	return inst, true
}

// handleOwnerSuspendInstallation is POST /api/admin/installations/{id}/suspend: the installation
// goes SUSPENDED and its game server's worker stops and is deactivated (no more ADM ingestion,
// feeds or counters) until reinstated. Data is kept.
func (a *App) handleOwnerSuspendInstallation(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	before, ok := a.ownerInstallationContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	after, err := a.PlatformOwner.SuspendInstallation(ctx, before.InstallationID, req.Reason)
	if err != nil {
		ownerFailed(w, "suspend installation", err)
		return
	}
	if after.GameServerID != nil {
		a.DisconnectServer(*after.GameServerID)
		if a.Servers != nil {
			if err := a.Servers.Deactivate(ctx, *after.GameServerID); err != nil {
				slog.Warn("component=admin_api", "event", "owner_suspend_deactivate_failed", "installation_id", after.InstallationID, "err", err.Error())
			}
		}
	}
	a.ownerAudit(ctx, admin, "installation.suspended", "installation", after.InstallationID, &after.OrganizationID, req.Reason, "OK", a.ownerInstallation(before), a.ownerInstallation(after))
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"installation": a.ownerInstallation(after)})
}

// handleOwnerReinstateInstallation is POST /api/admin/installations/{id}/reinstate: restores the
// pre-suspension status, reactivates the game server and restarts its worker.
func (a *App) handleOwnerReinstateInstallation(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	before, ok := a.ownerInstallationContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	after, err := a.PlatformOwner.ReinstateInstallation(ctx, before.InstallationID)
	if err != nil {
		ownerFailed(w, "reinstate installation", err)
		return
	}
	if after.GameServerID != nil {
		if a.Servers != nil {
			if err := a.Servers.Reactivate(ctx, *after.GameServerID); err != nil {
				slog.Warn("component=admin_api", "event", "owner_reinstate_reactivate_failed", "installation_id", after.InstallationID, "err", err.Error())
			}
		}
		if a.WorkerManager != nil {
			if err := a.RepairServer(context.WithoutCancel(ctx), *after.GameServerID); err != nil {
				slog.Warn("component=admin_api", "event", "owner_reinstate_worker_failed", "installation_id", after.InstallationID, "err", err.Error())
			}
		}
	}
	a.ownerAudit(ctx, admin, "installation.reinstated", "installation", after.InstallationID, &after.OrganizationID, req.Reason, "OK", a.ownerInstallation(before), a.ownerInstallation(after))
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"installation": a.ownerInstallation(after)})
}

// handleOwnerRestartWorker is POST /api/admin/installations/{id}/restart-worker: stop and start
// the installation's ADM worker (no tail-start, nothing is skipped). A suspended installation is
// refused; reinstate it instead.
func (a *App) handleOwnerRestartWorker(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	inst, ok := a.ownerInstallationContext(w, r)
	if !ok {
		return
	}
	if inst.Status == repository.InstallationSuspended {
		writeSaaSError(w, codeConflict, "installation is suspended; reinstate it to run its worker")
		return
	}
	if inst.GameServerID == nil {
		writeSaaSError(w, codeConflict, "installation has no game server yet")
		return
	}
	if a.WorkerManager == nil {
		writeSaaSError(w, codeInternalError, "killfeed runtime is not initialized")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	a.DisconnectServer(*inst.GameServerID)
	result := "OK"
	if err := a.RepairServer(ctx, *inst.GameServerID); err != nil {
		result = "FAILED"
		a.ownerAudit(ctx, admin, "installation.worker_restarted", "installation", inst.InstallationID, &inst.OrganizationID, req.Reason, result, nil, map[string]string{"error": err.Error()})
		ownerFailed(w, "restart worker", err)
		return
	}
	a.ownerAudit(ctx, admin, "installation.worker_restarted", "installation", inst.InstallationID, &inst.OrganizationID, req.Reason, result, nil, nil)
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"installation": a.ownerInstallation(inst), "workerRunning": a.WorkerManager.Running(*inst.GameServerID)})
}

// --- users ------------------------------------------------------------------------------------------

type ownerUserDTO struct {
	ID          int64   `json:"id"`
	DiscordID   string  `json:"discordUserId"`
	Username    string  `json:"username"`
	GlobalName  *string `json:"globalName"`
	Avatar      *string `json:"avatar"`
	CreatedAt   string  `json:"createdAt"`
	LastLoginAt *string `json:"lastLoginAt"`
	BannedAt    *string `json:"bannedAt"`
	BanReason   *string `json:"banReason"`
}

func ownerUser(u *repository.AppUser) ownerUserDTO {
	return ownerUserDTO{ID: u.ID, DiscordID: u.DiscordUserID, Username: u.DiscordUsername, GlobalName: optStr(u.DiscordGlobalName), Avatar: optStr(u.Avatar),
		CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339), LastLoginAt: nullableTimeStr(u.LastLoginAt), BannedAt: nullableTimeStr(u.BannedAt), BanReason: optStr(u.BanReason)}
}

func (a *App) ownerUserContext(w http.ResponseWriter, r *http.Request) (*repository.AppUser, bool) {
	id, ok := pathInt64(w, r, "userID")
	if !ok {
		return nil, false
	}
	if a.PlatformOwner == nil {
		writeSaaSError(w, codeInternalError, "owner controls unavailable")
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	u, err := a.PlatformOwner.GetUser(ctx, id)
	if err != nil {
		ownerFailed(w, "load user", err)
		return nil, false
	}
	if u == nil {
		writeSaaSError(w, codeNotFound, "user not found")
		return nil, false
	}
	return u, true
}

// handleOwnerBanUser is POST /api/admin/users/{id}/ban. The platform owner cannot ban an
// allowlisted admin (including themself).
func (a *App) handleOwnerBanUser(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	before, ok := a.ownerUserContext(w, r)
	if !ok {
		return
	}
	if a.Config != nil && a.Config.IsPlatformAdmin(before.DiscordUserID) {
		writeSaaSError(w, codeConflict, "platform admins cannot be banned; remove them from CHAMPION_ADMIN_DISCORD_IDS first")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	after, err := a.PlatformOwner.BanUser(ctx, before.ID, req.Reason)
	if err != nil {
		ownerFailed(w, "ban user", err)
		return
	}
	a.ownerAudit(ctx, admin, "user.banned", "user", after.ID, nil, req.Reason, "OK", ownerUser(before), ownerUser(after))
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"user": ownerUser(after)})
}

func (a *App) handleOwnerUnbanUser(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	before, ok := a.ownerUserContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	after, err := a.PlatformOwner.UnbanUser(ctx, before.ID)
	if err != nil {
		ownerFailed(w, "unban user", err)
		return
	}
	a.ownerAudit(ctx, admin, "user.unbanned", "user", after.ID, nil, req.Reason, "OK", ownerUser(before), ownerUser(after))
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"user": ownerUser(after)})
}

// --- audit log --------------------------------------------------------------------------------------

type ownerAuditDTO struct {
	ID             int64           `json:"id"`
	ActorDiscordID string          `json:"actorDiscordId"`
	Action         string          `json:"action"`
	TargetType     string          `json:"targetType"`
	TargetID       *int64          `json:"targetId"`
	OrganizationID *int64          `json:"organizationId"`
	Reason         string          `json:"reason"`
	Before         json.RawMessage `json:"before,omitempty"`
	After          json.RawMessage `json:"after,omitempty"`
	Result         string          `json:"result"`
	CreatedAt      string          `json:"createdAt"`
}

// handleOwnerAudit is GET /api/admin/audit: platform-owner actions, newest first. Filters:
// organizationId, targetType, targetId, action; cursor/limit as every admin list.
func (a *App) handleOwnerAudit(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	p, ok := parseAdminListParams(w, r)
	if !ok {
		return
	}
	if a.PlatformOwner == nil {
		writeSaaSError(w, codeInternalError, "owner controls unavailable")
		return
	}
	f := repository.PlatformAuditFilter{BeforeID: p.Cursor, Limit: p.Limit + 1}
	q := p.Query
	if v := strings.TrimSpace(q.Get("organizationId")); v != "" {
		f.OrganizationID, _ = parseOptionalInt64(v)
	}
	if v := strings.TrimSpace(q.Get("targetId")); v != "" {
		f.TargetID, _ = parseOptionalInt64(v)
	}
	if v := strings.ToLower(strings.TrimSpace(q.Get("targetType"))); v != "" && len(v) <= 32 {
		f.TargetType = v
	}
	if v := strings.ToLower(strings.TrimSpace(q.Get("action"))); v != "" && len(v) <= 64 {
		f.Action = v
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, err := a.PlatformOwner.ListAudit(ctx, f)
	if err != nil {
		a.adminReadFailed(w, "audit log", err)
		return
	}
	var next int64
	if len(rows) > p.Limit {
		rows = rows[:p.Limit]
		next = rows[len(rows)-1].ID
	}
	items := make([]ownerAuditDTO, 0, len(rows))
	for _, e := range rows {
		items = append(items, ownerAuditDTO{ID: e.ID, ActorDiscordID: e.ActorDiscordID, Action: e.Action, TargetType: e.TargetType, TargetID: e.TargetID, OrganizationID: e.OrganizationID,
			Reason: e.Reason, Before: e.BeforeState, After: e.AfterState, Result: e.Result, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339)})
	}
	a.writeAdminJSON(w, http.StatusOK, adminList(items, next, p.Limit))
}

func parseOptionalInt64(v string) (int64, bool) {
	var n int64
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
		if n > 1<<53 {
			return 0, false
		}
	}
	return n, n > 0
}

// adminPlanParamValue validates a plan key the way the list filter does (used when no billing
// catalog is configured, so a grant can still name a plan on a non-billing environment).
func adminPlanParamValue(v string) (string, bool) {
	if len(v) > 40 {
		return "", true
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return "", true
		}
	}
	return v, false
}

// registerOwnerAPI wires the Owner Hub write routes under /api/admin.
func (a *App) registerOwnerAPI() {
	if a.HTTPServer == nil {
		return
	}
	h := a.HTTPServer.Handle
	h("POST /api/admin/organizations/{organizationID}/trial", a.adminRoute(a.handleOwnerSetTrial))
	h("POST /api/admin/organizations/{organizationID}/grant", a.adminRoute(a.handleOwnerGrantPlan))
	h("POST /api/admin/organizations/{organizationID}/revoke", a.adminRoute(a.handleOwnerRevokeAccess))
	h("POST /api/admin/organizations/{organizationID}/billing/cancel", a.adminRoute(a.handleOwnerBillingCancel))
	h("POST /api/admin/organizations/{organizationID}/billing/reactivate", a.adminRoute(a.handleOwnerBillingReactivate))
	h("POST /api/admin/organizations/{organizationID}/billing/reconcile", a.adminRoute(a.handleOwnerBillingReconcile))
	h("POST /api/admin/installations/{installationID}/suspend", a.adminRoute(a.handleOwnerSuspendInstallation))
	h("POST /api/admin/installations/{installationID}/reinstate", a.adminRoute(a.handleOwnerReinstateInstallation))
	h("POST /api/admin/installations/{installationID}/restart-worker", a.adminRoute(a.handleOwnerRestartWorker))
	h("POST /api/admin/users/{userID}/ban", a.adminRoute(a.handleOwnerBanUser))
	h("POST /api/admin/users/{userID}/unban", a.adminRoute(a.handleOwnerUnbanUser))
	h("GET /api/admin/audit", a.adminRoute(a.handleOwnerAudit))
}
