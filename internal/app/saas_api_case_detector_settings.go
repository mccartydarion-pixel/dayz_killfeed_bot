package app

import (
 "context"
 "log/slog"
 "net/http"

 "github.com/yourname/dayz-killfeed/internal/caseintel"
 "github.com/yourname/dayz-killfeed/internal/permissions"
 "github.com/yourname/dayz-killfeed/internal/repository"
)

// These owner preferences are inert. A saved sensitivity is not a validated
// threshold, detector enablement, case admission, or notification permission.
func (a *App) registerCaseDetectorSettingRoutes(base string) {
 h:=a.HTTPServer.Handle
 h("GET "+base+"/case/detector-settings",a.handleCaseListDetectorSettings)
 h("PUT "+base+"/case/detector-settings/{moduleID}",a.handleCaseSetDetectorSetting)
 h("POST "+base+"/case/alerts/test",a.handleCaseSendTestAlert)
 h("GET "+base+"/case/alerts/settings",a.handleGetCaseStaffAlerts)
 h("PUT "+base+"/case/alerts/settings",a.handleSetCaseStaffAlerts)
 h("GET "+base+"/case/setup",a.handleGetCaseSetup)
 h("PUT "+base+"/case/setup/evidence",a.handleSetCaseEvidence)
}

func (a *App) caseDetectorSettingsActor(w http.ResponseWriter,r *http.Request)(adminActor,*repository.CaseDetectorSettingsRepository,bool){
 ac,ok:=a.requireCapability(w,r,permissions.CapUAVManage)
 if !ok{return adminActor{},nil,false}
 if ac.level!=permissions.LevelOwner {writeSaaSError(w,codeAdminForbidden,"server owner required");return adminActor{},nil,false}
 if ac.scope.ServerID==nil {writeSaaSError(w,codeInvalidRequest,"no DayZ server selected");return adminActor{},nil,false}
 if a.DB==nil||a.DB.Pool==nil {writeSaaSError(w,codeInternalError,"C.A.S.E. preferences unavailable");return adminActor{},nil,false}
 return ac,repository.NewCaseDetectorSettingsRepository(a.DB.Pool),true
}

func (a *App) handleCaseListDetectorSettings(w http.ResponseWriter,r *http.Request){
 ac,repo,ok:=a.caseDetectorSettingsActor(w,r);if !ok{return}
 if !enforceRateLimit(w,a.saasAdminReadLimiter,rateLimitKey(r)){return}
 ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout);defer cancel()
 items,err:=repo.List(ctx,ac.scope.InstallationID,ac.scope.GuildID,*ac.scope.ServerID)
 if err!=nil {slog.Warn("component=case","event","detector_settings_list_failed","err",err.Error());writeSaaSError(w,codeInternalError,"could not list detector preferences");return}
 a.recordAudit(ctx,ac,"CASE_DETECTOR_SETTINGS_VIEWED","","","success",nil,map[string]any{"count":len(items)})
 writeSaaSJSON(w,http.StatusOK,map[string]any{"items":items,"operational":false})
}

type caseSetDetectorSettingRequest struct {
 Sensitivity caseintel.Sensitivity `json:"sensitivity"`
}

func (a *App) handleCaseSetDetectorSetting(w http.ResponseWriter,r *http.Request){
 ac,repo,ok:=a.caseDetectorSettingsActor(w,r);if !ok{return}
 if !enforceRateLimit(w,a.saasAdminActionLimiter,rateLimitKey(r)){return}
 moduleID:=r.PathValue("moduleID")
 var req caseSetDetectorSettingRequest
 if err:=readCaseBaseJSON(w,r,&req);err!=nil{writeSaaSError(w,codeInvalidRequest,"invalid detector preference");return}
 ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout);defer cancel()
 setting,err:=repo.Set(ctx,ac.scope.InstallationID,ac.scope.GuildID,*ac.scope.ServerID,moduleID,req.Sensitivity)
 if err!=nil {writeSaaSError(w,codeInvalidRequest,"invalid or mismatched detector preference");return}
 a.recordAudit(ctx,ac,"CASE_DETECTOR_SENSITIVITY_SAVED","case-detector:"+moduleID,"","success",nil,
  map[string]any{"sensitivity":setting.Sensitivity,"revision":setting.Revision})
 writeSaaSJSON(w,http.StatusOK,map[string]any{"setting":setting,"operational":false})
}

