package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

const (
	codeCasePremiumRequired = "CASE_PREMIUM_REQUIRED"
	codeCaseAccessUnavailable = "CASE_ACCESS_UNAVAILABLE"
)

func init() {
	httpStatusForCode[codeCasePremiumRequired] = http.StatusForbidden
	httpStatusForCode[codeCaseAccessUnavailable] = http.StatusServiceUnavailable
}

// requireCasePremium first authorizes the actor using existing Champion
// installation permissions; only then may the server-side billing resolver
// check the requested paid capability. Never trust a website plan label or a
// query-supplied server ID. All C.A.S.E. paid handlers must pass this check.
func (a *App) requireCasePremium(w http.ResponseWriter, r *http.Request, cap casebilling.Capability) (adminActor, bool) {
	ac,ok:=a.requireCapability(w,r,permissions.CapPlayerLocationView)
	if !ok{return adminActor{},false}
	if ac.scope.ServerID==nil || *ac.scope.ServerID<=0 {
		writeSaaSError(w,codeInvalidRequest,"no DayZ server selected")
		return adminActor{},false
	}
	if a.Billing==nil {
		writeSaaSError(w,codeCaseAccessUnavailable,"C.A.S.E. billing unavailable")
		return adminActor{},false
	}
	ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
	defer cancel()
	allowed,err:=a.Billing.CaseAllows(ctx,ac.scope.OrganizationID,ac.scope.InstallationID,
		*ac.scope.ServerID,cap,time.Now().UTC())
	if err!=nil {
		slog.Warn("component=case","event","premium_access_check_failed","err",err.Error())
		writeSaaSError(w,codeCaseAccessUnavailable,"could not verify C.A.S.E. access")
		return adminActor{},false
	}
	if !allowed {
		writeSaaSError(w,codeCasePremiumRequired,casePremiumRequiredMessage(cap))
		return adminActor{},false
	}
	return ac,true
}

// casePremiumRequiredMessage names the capability this route needs, so a
// server with active Watch that calls a Pro-only route is told Pro is
// required rather than that it has no C.A.S.E. access at all.
func casePremiumRequiredMessage(cap casebilling.Capability) string {
	switch cap {
	case casebilling.CapWatch:
		return "C.A.S.E. Watch access is required for the selected server"
	case casebilling.CapPro:
		return "C.A.S.E. Pro access is required for the selected server"
	case casebilling.CapCommand:
		return "C.A.S.E. Command access is required for the selected server"
	default:
		return "the required C.A.S.E. access is not active for the selected server"
	}
}

// This read is a presentation contract, not an authorization token. Downstream
// premium APIs/workers MUST reload the authoritative subscriptions themselves.
func (a *App) handleAntiCheatEntitlements(w http.ResponseWriter,r *http.Request){
	ac,ok:=a.requireCapability(w,r,permissions.CapPlayerLocationView)
	if !ok{return}
	if ac.scope.ServerID==nil || *ac.scope.ServerID<=0 {
		writeSaaSError(w,codeInvalidRequest,"no DayZ server selected");return
	}
	if a.Billing==nil {
		writeSaaSError(w,codeCaseAccessUnavailable,"C.A.S.E. billing unavailable");return
	}
	if !enforceRateLimit(w,a.saasAdminReadLimiter,rateLimitKey(r)){return}
	ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
	defer cancel()
	caps,err:=a.Billing.CaseAccess(ctx,ac.scope.OrganizationID,ac.scope.InstallationID,*ac.scope.ServerID,time.Now().UTC())
	if err!=nil{
		slog.Warn("component=case","event","entitlement_read_failed","err",err.Error())
		writeSaaSError(w,codeCaseAccessUnavailable,"could not verify C.A.S.E. access");return
	}
	writeSaaSJSON(w,http.StatusOK,map[string]any{
		"serverId":*ac.scope.ServerID,
		"mode":"OBSERVATION_ONLY",
		"capabilities":caps,
		"detectorsEnabled":false,
		"enforcement":"DISABLED",
	})
}

// Premium bounded evidence export (no detector, verdict, ban or extra Nitrado
// poll). Normal observation/evidence/sessions endpoints remain unchanged.
// Reads are scoped to the SAME guild and selected server on every page.
// The underlying repository enforces 100 rows per SQL query, and this handler
// permits at most 250 source-provenanced rows in one premium request.
func (a *App) handleAntiCheatPremiumExport(w http.ResponseWriter,r *http.Request){
	ac,ok:=a.requireCasePremium(w,r,casebilling.CapPro)
	if !ok{return}
	if a.DB==nil || a.DB.Pool==nil {
		writeSaaSError(w,codeCaseAccessUnavailable,"C.A.S.E. evidence unavailable");return
	}
	if !enforceRateLimit(w,a.saasAdminReadLimiter,rateLimitKey(r)){return}
	limit:=100
	if raw:=r.URL.Query().Get("limit");raw!=""{
		n,err:=strconv.Atoi(raw)
		if err!=nil || n<1 || n>250 {writeSaaSError(w,codeInvalidRequest,"limit must be 1-250");return}
		limit=n
	}
	var before *int64
	if raw:=r.URL.Query().Get("before");raw!=""{
		id,err:=strconv.ParseInt(raw,10,64)
		if err!=nil || id<=0 {writeSaaSError(w,codeInvalidRequest,"invalid cursor");return}
		before=&id
	}
	ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
	defer cancel()
	items:=make([]repository.CaseEvidenceRow,0,limit)
	repo:=repository.NewCaseEvidenceRepository(a.DB.Pool)
	// Seek pagination uses the last persisted ID; it never crosses another
	// tenant, game server or raw ADM source path.
	for len(items)<limit {
		pageLimit:=limit-len(items)
		if pageLimit>100 {pageLimit=100}
		page,err:=repo.ListCaseEvidence(ctx,ac.scope.GuildID,*ac.scope.ServerID,nil,before,nil,pageLimit)
		if err!=nil{
			slog.Warn("component=case","event","premium_export_failed","err",err.Error())
			writeSaaSError(w,codeCaseAccessUnavailable,"could not load evidence export");return
		}
		if len(page)==0 {break}
		items=append(items,page...)
		last:=page[len(page)-1].ID
		before=&last
		if len(page)<pageLimit {break}
	}
	var cursor *string
	if len(items)==limit && before!=nil {
		value:=strconv.FormatInt(*before,10);cursor=&value
	}
	a.recordAudit(ctx,ac,"CASE_PREMIUM_EVIDENCE_EXPORT","","","success",nil,
		map[string]any{"count":len(items),"hasMore":cursor!=nil})
	writeSaaSJSON(w,http.StatusOK,map[string]any{
		"serverId":*ac.scope.ServerID,
		"mode":"OBSERVATION_ONLY","items":items,"nextCursor":cursor,
		"detectorsEnabled":false,"enforcement":"DISABLED",
	})
}

// Protect workers exactly as API handlers: query the same billing source
// before premium processing, never use the website's snapshot as authority.
func (a *App) caseWorkerAllowed(ctx context.Context,orgID,installationID,serverID int64,cap casebilling.Capability)(bool,error){
	if a.Billing==nil{return false,nil}
	return a.Billing.CaseAllows(ctx,orgID,installationID,serverID,cap,time.Now().UTC())
}
