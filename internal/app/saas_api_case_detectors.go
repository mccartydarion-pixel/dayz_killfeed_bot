package app

import (
 "context"
 "net/http"

 "github.com/yourname/dayz-killfeed/internal/caseintel"
 "github.com/yourname/dayz-killfeed/internal/permissions"
)

// This endpoint exposes the installed, disabled detector registry. It is not
// an execution endpoint and cannot create a finding, case, alert or sanction.
type caseDetectorReadiness struct {
 Mode string `json:"mode"`
 ServerID int64 `json:"serverId"`
 Registry []caseintel.DetectorDefinition `json:"registry"`
 Evaluations []caseintel.DetectorEvaluation `json:"evaluations"`
 InputCoverage string `json:"inputCoverage"`
 ExecutionEnabled bool `json:"executionEnabled"`
 Findings []any `json:"findings"`
 Cases []any `json:"cases"`
 DetectorsEnabled bool `json:"detectorsEnabled"`
 Enforcement string `json:"enforcement"`
}

func (a *App) handleAntiCheatDetectorReadiness(w http.ResponseWriter,r *http.Request) {
 ac,ok:=a.requireCapability(w,r,permissions.CapPlayerLocationView)
 if !ok{return}
 if ac.scope.ServerID==nil{writeSaaSError(w,codeInvalidRequest,"no DayZ server selected");return}
 if a.DB==nil||a.DB.Pool==nil{writeSaaSError(w,codeInternalError,"C.A.S.E. unavailable");return}
 if !enforceRateLimit(w,a.saasAdminReadLimiter,rateLimitKey(r)){return}
 defs:=caseintel.Registry()
 // Global source limitations are not eliminated by healthy polling.
 quality:=caseintel.QualityReport{CoverageStatus:"FILTERED_SOURCE_EVENTS_ONLY",
  TimeStatus:"CLOCK_ONLY_NO_TRUSTED_ELAPSED_TIME",MovementDetectorStatus:"BLOCKED",
  SafeSpeedPairs:0}
 evals:=make([]caseintel.DetectorEvaluation,0,len(defs))
 for _,def:=range defs {evals=append(evals,caseintel.EvaluatePrerequisites(def,quality))}
 out:=caseDetectorReadiness{
  Mode:"READINESS_ONLY",ServerID:*ac.scope.ServerID,Registry:defs,Evaluations:evals,
  InputCoverage:"PERSISTED_SELECTED_ADM_SOURCE_LINES",ExecutionEnabled:false,
  Findings:make([]any,0),Cases:make([]any,0),DetectorsEnabled:false,Enforcement:"DISABLED",
 }
 ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
 defer cancel()
 a.recordAudit(ctx,ac,"CASE_DETECTOR_READINESS_VIEWED","","","success",nil,
  map[string]any{"detectors":len(defs),"executionEnabled":false})
 writeSaaSJSON(w,http.StatusOK,out)
}
