package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

const (
	codeCaseNotAvailable = "CASE_NOT_AVAILABLE"
	codeCaseCheckoutConflict = "CASE_CHECKOUT_CONFLICT"
	codeCaseBaseRequired = "CASE_BASE_REQUIRED"
)

func init(){
	httpStatusForCode[codeCaseNotAvailable]=http.StatusServiceUnavailable
	httpStatusForCode[codeCaseCheckoutConflict]=http.StatusConflict
	httpStatusForCode[codeCaseBaseRequired]=http.StatusConflict
}

func (a *App) registerCaseBillingRoutes(){
	h:=a.HTTPServer.Handle
	h("GET /api/saas/billing/case/plans",a.handleCaseBillingPlans)
	const base="/api/saas/organizations/{organizationID}/billing/case"
	h("GET "+base+"/servers",a.handleCaseBillingServers)
	h("POST "+base+"/checkout",a.handleCaseBillingCheckout)
	h("POST "+base+"/checkout/recover",a.handleCaseBillingRecover)
	h("POST "+base+"/cancel",a.handleCaseBillingCancel)
	h("POST "+base+"/reactivate",a.handleCaseBillingReactivate)
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
	Status string `json:"status"`
	CurrentPeriodEnd *string `json:"currentPeriodEnd"`
	PaidThrough *string `json:"paidThrough"`
	TrialEndsAt *string `json:"trialEndsAt"`
	FounderTrialGranted bool `json:"founderTrialGranted"`
	CancelAtPeriodEnd bool `json:"cancelAtPeriodEnd"`
	BoundToSelectedServer bool `json:"boundToSelectedServer"`
	CanRetryCheckout bool `json:"canRetryCheckout"`
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
			Tier:sub.Tier,Status:sub.Status,CurrentPeriodEnd:nullableTimeStr(sub.CurrentPeriodEnd),
			TrialEndsAt:nullableTimeStr(sub.TrialEndsAt),FounderTrialGranted:sub.FounderTrialGranted,
			PaidThrough:nullableTimeStr(sub.PaidThrough),CancelAtPeriodEnd:sub.CancelAtPeriodEnd,
			BoundToSelectedServer:matches,
			CanRetryCheckout:sub.Status=="PENDING" && sub.ProviderSubscriptionID=="" && sub.CheckoutSessionID=="",
		})
	}
	writeSaaSJSON(w,http.StatusOK,map[string]any{"items":items,
		"canManageBilling":br.role==repository.RoleOwner || br.role==repository.RoleAdmin})
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

