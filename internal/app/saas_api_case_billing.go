package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

const (
	codeCaseNotAvailable = "CASE_NOT_AVAILABLE"
	codeCaseCheckoutConflict = "CASE_CHECKOUT_CONFLICT"
	codeCaseBaseRequired = "CASE_BASE_REQUIRED"
	codeCasePreviewExpired = "CASE_PREVIEW_EXPIRED"
)

func init(){
	httpStatusForCode[codeCaseNotAvailable]=http.StatusServiceUnavailable
	httpStatusForCode[codeCaseCheckoutConflict]=http.StatusConflict
	httpStatusForCode[codeCaseBaseRequired]=http.StatusConflict
	httpStatusForCode[codeCasePreviewExpired]=http.StatusConflict
}

func (a *App) registerCaseBillingRoutes(){
	h:=a.HTTPServer.Handle
	h("GET /api/saas/billing/case/plans",a.handleCaseBillingPlans)
	const base="/api/saas/organizations/{organizationID}/billing/case"
	h("GET "+base+"/servers",a.handleCaseBillingServers)
	h("GET "+base+"/coverage",a.handleCaseBillingCoverage)
	h("POST "+base+"/checkout",a.handleCaseBillingCheckout)
	h("POST "+base+"/checkout/recover",a.handleCaseBillingRecover)
	h("POST "+base+"/cancel",a.handleCaseBillingCancel)
	h("POST "+base+"/reactivate",a.handleCaseBillingReactivate)
	h("POST "+base+"/plan/preview",a.handleCaseBillingPlanPreview)
	h("POST "+base+"/plan",a.handleCaseBillingPlanChange)
}

type casePlanDTO struct {
	Key string `json:"key"`
	Name string `json:"name"`
	AmountCents int64 `json:"amountCents"`
	Currency string `json:"currency"`
	Interval string `json:"interval"`
	Purchasable bool `json:"purchasable"`
}

func (a *App) handleCaseBillingPlans(w http.ResponseWriter,r *http.Request){
	if !a.requireSaaSServiceAuth(w,r) || a.resolveActingUser(w,r)==nil{return}
	if a.Billing==nil {writeSaaSError(w,codeBillingUnavailable,"billing unavailable");return}
	plans:=a.Billing.CasePlans()
	items:=make([]casePlanDTO,0,len(plans))
	for _,p:=range plans {
		items=append(items,casePlanDTO{Key:string(p.Tier),Name:p.Name,AmountCents:p.AmountCents,
			Currency:p.Currency,Interval:p.Interval,Purchasable:p.Purchasable})
	}
	writeSaaSJSON(w,http.StatusOK,map[string]any{"items":items})
}

type caseServerDTO struct {
	InstallationID int64 `json:"installationId"`
	GameServerID int64 `json:"gameServerId"`
	Tier string `json:"tier"`
	// PaidTier is the tier currently paid for; access follows it until
	// paidThrough. PendingDowngrade: tier (next renewal) is below paidTier.
	PaidTier string `json:"paidTier"`
	PendingDowngrade bool `json:"pendingDowngrade"`
	Status string `json:"status"`
	CurrentPeriodEnd *string `json:"currentPeriodEnd"`
	PaidThrough *string `json:"paidThrough"`
	TrialEndsAt *string `json:"trialEndsAt"`
	FounderTrialGranted bool `json:"founderTrialGranted"`
	CancelAtPeriodEnd bool `json:"cancelAtPeriodEnd"`
	BoundToSelectedServer bool `json:"boundToSelectedServer"`
	CanRetryCheckout bool `json:"canRetryCheckout"`
	// CoverageState reflects refunds/disputes on the latest paid period:
	// OK, PARTIALLY_REFUNDED, REFUNDED, DISPUTED or DISPUTE_LOST.
	CoverageState string `json:"coverageState"`
}

func (a *App) handleCaseBillingServers(w http.ResponseWriter,r *http.Request){
	br,ok:=a.billingContext(w,r,false);if !ok{return}
	ctx,cancel:=context.WithTimeout(r.Context(),billingTimeout);defer cancel()
	subs,err:=a.Billing.CaseSubscriptions(ctx,br.orgID)
	if err!=nil{billingFailed(w,"list case add-ons",err);return}
	items:=make([]caseServerDTO,0,len(subs))
	for _,sub:=range subs {
		matches:=sub.SelectedGameServerID!=nil && *sub.SelectedGameServerID==sub.GameServerID
		items=append(items,caseServerDTO{
			InstallationID:sub.InstallationID,GameServerID:sub.GameServerID,
			Tier:sub.Tier,Status:sub.Status,PaidTier:sub.PaidTier,
			PendingDowngrade:sub.Status=="ACTIVE" && sub.PaidTier!="" && sub.PaidThrough!=nil && sub.PaidThrough.After(time.Now()) &&
				casebilling.Rank(casebilling.Tier(sub.Tier))<casebilling.Rank(casebilling.Tier(sub.PaidTier)),CurrentPeriodEnd:nullableTimeStr(sub.CurrentPeriodEnd),
			TrialEndsAt:nullableTimeStr(sub.TrialEndsAt),FounderTrialGranted:sub.FounderTrialGranted,
			PaidThrough:nullableTimeStr(sub.PaidThrough),CancelAtPeriodEnd:sub.CancelAtPeriodEnd,
			BoundToSelectedServer:matches,
			CanRetryCheckout:sub.Status=="PENDING" && sub.ProviderSubscriptionID=="" && sub.CheckoutSessionID=="",
			CoverageState:caseCoverageStateOrOK(sub.CoverageState),
		})
	}
	writeSaaSJSON(w,http.StatusOK,map[string]any{"items":items,
		"canManageBilling":br.role==repository.RoleOwner || br.role==repository.RoleAdmin})
}

func caseCoverageStateOrOK(s string) string {
	if s=="" {return "OK"}
	return s
}

type caseCoverageInvoiceDTO struct {
	Tier string `json:"tier"`
	PeriodStart string `json:"periodStart"`
	PeriodEnd string `json:"periodEnd"`
	AmountPaidCents int64 `json:"amountPaidCents"`
	AmountRefundedCents int64 `json:"amountRefundedCents"`
	Currency string `json:"currency"`
	Status string `json:"status"`
	DisputeStatus string `json:"disputeStatus,omitempty"`
	UpdatedAt string `json:"updatedAt"`
}

type caseCoverageEventDTO struct {
	EventType string `json:"eventType"`
	PreviousStatus string `json:"previousStatus,omitempty"`
	NewStatus string `json:"newStatus"`
	DisputeStatus string `json:"disputeStatus,omitempty"`
	AmountRefundedCents *int64 `json:"amountRefundedCents"`
	PaidThroughBefore *string `json:"paidThroughBefore"`
	PaidThroughAfter *string `json:"paidThroughAfter"`
	PaidTierBefore string `json:"paidTierBefore,omitempty"`
	PaidTierAfter string `json:"paidTierAfter,omitempty"`
	RecordedAt string `json:"recordedAt"`
}

// Organization OWNER/ADMIN only: payment amounts, refunds and disputes for one
// installation's add-on. Read-only, database only (no Stripe call), and no
// Stripe identifiers leave the server.
func (a *App) handleCaseBillingCoverage(w http.ResponseWriter,r *http.Request){
	br,ok:=a.billingContext(w,r,true);if !ok{return}
	installationID,err:=strconv.ParseInt(r.URL.Query().Get("installationId"),10,64)
	if err!=nil || installationID<=0 {
		writeSaaSError(w,codeInvalidRequest,"invalid installationId")
		return
	}
	ctx,cancel:=context.WithTimeout(r.Context(),billingTimeout);defer cancel()
	records,events,err:=a.Billing.CaseCoverage(ctx,br.orgID,installationID)
	if errors.Is(err,billing.ErrProviderNotConfigured) {
		writeSaaSError(w,codeCaseNotAvailable,"C.A.S.E. billing unavailable")
		return
	}
	if err!=nil{billingFailed(w,"list case coverage",err);return}
	invoices:=make([]caseCoverageInvoiceDTO,0,len(records))
	for _,c:=range records {
		invoices=append(invoices,caseCoverageInvoiceDTO{Tier:c.Tier,PeriodStart:c.PeriodStart.UTC().Format(time.RFC3339),
			PeriodEnd:c.PeriodEnd.UTC().Format(time.RFC3339),AmountPaidCents:c.AmountPaid,AmountRefundedCents:c.AmountRefunded,
			Currency:c.Currency,Status:c.Status,DisputeStatus:c.DisputeStatus,UpdatedAt:c.UpdatedAt.UTC().Format(time.RFC3339)})
	}
	history:=make([]caseCoverageEventDTO,0,len(events))
	for _,e:=range events {
		history=append(history,caseCoverageEventDTO{EventType:e.EventType,PreviousStatus:e.PreviousStatus,NewStatus:e.NewStatus,
			DisputeStatus:e.DisputeStatus,AmountRefundedCents:e.AmountRefunded,PaidThroughBefore:nullableTimeStr(e.PaidThroughBefore),
			PaidThroughAfter:nullableTimeStr(e.PaidThroughAfter),PaidTierBefore:e.PaidTierBefore,PaidTierAfter:e.PaidTierAfter,
			RecordedAt:e.RecordedAt.UTC().Format(time.RFC3339)})
	}
	writeSaaSJSON(w,http.StatusOK,map[string]any{"installationId":installationID,"invoices":invoices,"history":history})
}

type caseCheckoutRequestBody struct {
	InstallationID int64 `json:"installationId"`
	Tier string `json:"tier"`
	ReturnPath string `json:"returnPath"`
}

func (a *App) handleCaseBillingCheckout(w http.ResponseWriter,r *http.Request){
	br,ok:=a.billingContext(w,r,true)
	if !ok || !enforceRateLimit(w,a.saasBillingActionLimiter,rateLimitKey(r)){return}
	var req caseCheckoutRequestBody
	if !decodeFactionBody(w,r,&req){return}
	ctx,cancel:=context.WithTimeout(r.Context(),billingTimeout);defer cancel()
	out,err:=a.Billing.CaseCheckout(ctx,r,br.orgID,billing.CaseCheckoutRequest{
		InstallationID:req.InstallationID,
		Tier:casebilling.Tier(strings.ToUpper(strings.TrimSpace(req.Tier))),
		ReturnPath:req.ReturnPath,
	},a.SaaSInstallations)
	if err!=nil {
		switch {
		case errors.Is(err,billing.ErrCaseDisabled),errors.Is(err,billing.ErrProviderNotConfigured):
			writeSaaSError(w,codeCaseNotAvailable,"C.A.S.E. checkout is not yet available")
		case errors.Is(err,billing.ErrCaseNotPurchasable):
			writeSaaSError(w,codeInvalidPlan,"this C.A.S.E. package is not available")
		case errors.Is(err,billing.ErrCaseBaseRequired):
			writeSaaSError(w,codeCaseBaseRequired,"an active paid Champion subscription is required")
		case errors.Is(err,repository.ErrCaseCheckoutConflict):
			writeSaaSError(w,codeCaseCheckoutConflict,"this server has a pending or existing C.A.S.E. purchase")
		case errors.Is(err,billing.ErrInvalidReturnPath):
			writeSaaSError(w,codeInvalidRequest,"invalid returnPath")
		default:
			slog.Error("component=case_billing","event","checkout_failed","err",err.Error())
			writeSaaSError(w,codeInternalError,"could not create C.A.S.E. checkout")
		}
		return
	}
	billingAudit("case_checkout_created",br,"installation_id",req.InstallationID,"tier",req.Tier)
	writeSaaSJSON(w,http.StatusOK,checkoutResponseDTO{CheckoutURL:out.URL})
}

// Recover only an expired, unsubscribed Stripe Checkout Session for the
// authenticated organization's installation. This route creates no charge.
func (a *App) handleCaseBillingRecover(w http.ResponseWriter,r *http.Request) {
	br,ok:=a.billingContext(w,r,true)
	if !ok || !enforceRateLimit(w,a.saasBillingActionLimiter,rateLimitKey(r)){return}
	var body caseManageRequestBody
	if !decodeFactionBody(w,r,&body){return}
	if body.InstallationID<=0 {
		writeSaaSError(w,codeInvalidRequest,"invalid installationId")
		return
	}
	ctx,done:=context.WithTimeout(r.Context(),billingTimeout);defer done()
	if err:=a.Billing.RecoverCaseCheckout(ctx,br.orgID,body.InstallationID);err!=nil {
		switch {
		case errors.Is(err,repository.ErrCaseCheckoutConflict),errors.Is(err,billing.ErrCaseCheckoutNotExpired):
			writeSaaSError(w,codeCaseCheckoutConflict,"checkout is active, completed, or no longer recoverable")
		case errors.Is(err,repository.ErrCaseWebhookMismatch):
			writeSaaSError(w,codeCaseCheckoutConflict,"checkout identity requires reconciliation")
		case errors.Is(err,billing.ErrProviderNotConfigured):
			writeSaaSError(w,codeCaseNotAvailable,"billing provider unavailable")
		default:
			slog.Error("component=case_billing","event","checkout_recovery_failed","err",err.Error())
			writeSaaSError(w,codeInternalError,"could not recover C.A.S.E. checkout")
		}
		return
	}
	billingAudit("case_checkout_expired_recovered",br,"installation_id",body.InstallationID)
	writeSaaSJSON(w,http.StatusOK,map[string]any{"recovered":true})
}

type caseManageRequestBody struct {
	InstallationID int64 `json:"installationId"`
}

func (a *App) handleCaseBillingCancel(w http.ResponseWriter,r *http.Request) {
	a.handleCaseBillingManage(w,r,true)
}
func (a *App) handleCaseBillingReactivate(w http.ResponseWriter,r *http.Request) {
	a.handleCaseBillingManage(w,r,false)
}

// Organization OWNER/ADMIN only. This manages the selected server's add-on,
// not the organization's base subscription. No purchase-release flag is
// required for cancellation: operators must never strand paid subscribers.
func (a *App) handleCaseBillingManage(w http.ResponseWriter,r *http.Request,cancel bool) {
	br,ok:=a.billingContext(w,r,true)
	if !ok || !enforceRateLimit(w,a.saasBillingActionLimiter,rateLimitKey(r)){return}
	var body caseManageRequestBody
	if !decodeFactionBody(w,r,&body){return}
	if body.InstallationID<=0 {
		writeSaaSError(w,codeInvalidRequest,"invalid installationId")
		return
	}
	ctx,done:=context.WithTimeout(r.Context(),billingTimeout);defer done()
	sub,err:=a.Billing.CaseSetCancellation(ctx,br.orgID,body.InstallationID,cancel)
	if err!=nil {
		switch {
		case errors.Is(err,billing.ErrCaseNotManaged),errors.Is(err,repository.ErrCaseCheckoutConflict):
			writeSaaSError(w,codeCaseCheckoutConflict,"this server has no manageable C.A.S.E. subscription")
		case errors.Is(err,billing.ErrProviderNotConfigured):
			writeSaaSError(w,codeCaseNotAvailable,"billing provider unavailable")
		case errors.Is(err,repository.ErrCaseWebhookMismatch):
			writeSaaSError(w,codeCaseCheckoutConflict,"C.A.S.E. subscription needs reconciliation before changes")
		default:
			slog.Error("component=case_billing","event","manage_failed","err",err.Error())
			writeSaaSError(w,codeInternalError,"could not update C.A.S.E. subscription")
		}
		return
	}
	event:="case_cancellation_scheduled"
	if !cancel {event="case_cancellation_reversed"}
	billingAudit(event,br,"installation_id",body.InstallationID,"tier",sub.Tier)
	writeSaaSJSON(w,http.StatusOK,map[string]any{
		"installationId":sub.InstallationID,
		"tier":sub.Tier,"status":sub.Status,
		"cancelAtPeriodEnd":sub.CancelAtPeriodEnd,
		"currentPeriodEnd":nullableTimeStr(sub.CurrentPeriodEnd),
		"paidThrough":nullableTimeStr(sub.PaidThrough),
	})
}


type caseTierRequestBody struct {
	InstallationID int64 `json:"installationId"`
	Tier string `json:"tier"`
	// ProrationDate echoes the preview's value; it is not a price or amount.
	ProrationDate int64 `json:"prorationDate"`
}

// writeCaseTierError maps tier-change failures; no Stripe detail is exposed.
func writeCaseTierError(w http.ResponseWriter,err error,event string) {
	switch {
	case errors.Is(err,billing.ErrCaseDisabled),errors.Is(err,billing.ErrProviderNotConfigured):
		writeSaaSError(w,codeCaseNotAvailable,"C.A.S.E. upgrades are not yet available")
	case errors.Is(err,billing.ErrCaseNotPurchasable):
		writeSaaSError(w,codeInvalidPlan,"this C.A.S.E. package is not available")
	case errors.Is(err,billing.ErrCaseBaseRequired):
		writeSaaSError(w,codeCaseBaseRequired,"an active paid Champion subscription is required")
	case errors.Is(err,billing.ErrCasePreviewExpired):
		writeSaaSError(w,codeCasePreviewExpired,"preview expired; review the change again")
	case errors.Is(err,billing.ErrCaseTierUnchanged):
		writeSaaSError(w,codeCaseCheckoutConflict,"this server is already on that C.A.S.E. tier")
	case errors.Is(err,billing.ErrCaseCancelScheduled):
		writeSaaSError(w,codeCaseCheckoutConflict,"cancellation is scheduled; reactivate before changing tier")
	case errors.Is(err,billing.ErrCaseChangePending):
		writeSaaSError(w,codeCaseCheckoutConflict,"a previous tier change is awaiting payment")
	case errors.Is(err,billing.ErrCaseNotManaged),errors.Is(err,repository.ErrCaseCheckoutConflict):
		writeSaaSError(w,codeCaseCheckoutConflict,"this server has no active paid C.A.S.E. subscription to change")
	case errors.Is(err,repository.ErrCaseWebhookMismatch):
		writeSaaSError(w,codeCaseCheckoutConflict,"C.A.S.E. subscription needs reconciliation before changes")
	default:
		slog.Error("component=case_billing","event",event,"err",err.Error())
		writeSaaSError(w,codeInternalError,"could not change C.A.S.E. tier")
	}
}

func (a *App) decodeCaseTierRequest(w http.ResponseWriter,r *http.Request)(billingRequest,*caseTierRequestBody,bool){
	br,ok:=a.billingContext(w,r,true)
	if !ok || !enforceRateLimit(w,a.saasBillingActionLimiter,rateLimitKey(r)){return billingRequest{},nil,false}
	var body caseTierRequestBody
	if !decodeFactionBody(w,r,&body){return billingRequest{},nil,false}
	if body.InstallationID<=0 || strings.TrimSpace(body.Tier)=="" {
		writeSaaSError(w,codeInvalidRequest,"installationId and tier are required")
		return billingRequest{},nil,false
	}
	return br,&body,true
}

// Organization OWNER/ADMIN only. Read-only: returns what the change will do
// and cost (Stripe-calculated) before anything is mutated.
func (a *App) handleCaseBillingPlanPreview(w http.ResponseWriter,r *http.Request){
	br,body,ok:=a.decodeCaseTierRequest(w,r);if !ok{return}
	ctx,done:=context.WithTimeout(r.Context(),billingTimeout);defer done()
	p,err:=a.Billing.CasePreviewTierChange(ctx,br.orgID,body.InstallationID,body.Tier)
	if err!=nil{writeCaseTierError(w,err,"tier_preview_failed");return}
	writeSaaSJSON(w,http.StatusOK,map[string]any{
		"installationId":body.InstallationID,"kind":p.Kind,
		"currentTier":p.CurrentTier,"targetTier":p.TargetTier,
		"amountDueNowCents":p.AmountDueNowCents,"currency":p.Currency,
		"prorationDate":p.ProrationDate,"effectiveAt":nullableTimeStr(p.EffectiveAt),
		"nextRenewalAmountCents":p.NextRenewalAmountCents,
		"currentPeriodEnd":nullableTimeStr(p.CurrentPeriodEnd),
	})
}

// Organization OWNER/ADMIN only. Applies a previewed Watch <-> Pro change on
// the server's existing add-on subscription. 200 with status PAYMENT_PENDING
// means Stripe is holding an upgrade until its proration is paid; access is
// unchanged until the signed invoice.paid webhook arrives.
func (a *App) handleCaseBillingPlanChange(w http.ResponseWriter,r *http.Request){
	br,body,ok:=a.decodeCaseTierRequest(w,r);if !ok{return}
	ctx,done:=context.WithTimeout(r.Context(),billingTimeout);defer done()
	out,err:=a.Billing.CaseChangeTier(ctx,br.orgID,body.InstallationID,body.Tier,body.ProrationDate)
	if err!=nil{writeCaseTierError(w,err,"tier_change_failed");return}
	status:="APPLIED"
	if out.Pending{status="PAYMENT_PENDING"}
	billingAudit("case_tier_change_"+strings.ToLower(string(out.Kind)),br,
		"installation_id",body.InstallationID,"tier",out.Row.Tier,"status",status)
	writeSaaSJSON(w,http.StatusOK,map[string]any{
		"installationId":out.Row.InstallationID,"kind":out.Kind,"status":status,
		"tier":out.Row.Tier,"paidTier":out.Row.PaidTier,
		"paidThrough":nullableTimeStr(out.Row.PaidThrough),
		"currentPeriodEnd":nullableTimeStr(out.Row.CurrentPeriodEnd),
	})
}
