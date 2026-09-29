package app

import (
 "context"
 "log/slog"
 "net/http"
 "strconv"

 "github.com/yourname/dayz-killfeed/internal/permissions"
 "github.com/yourname/dayz-killfeed/internal/repository"
)

type caseShadowHistoryPage struct {
 Mode string `json:"mode"`
 ServerID int64 `json:"serverId"`
 Items []repository.ShadowHistoryRow `json:"items"`
 NextCursor *string `json:"nextCursor"`
 ExecutionEnabled bool `json:"executionEnabled"`
 DetectorsEnabled bool `json:"detectorsEnabled"`
 Enforcement string `json:"enforcement"`
}

// Read-only, location-capability protected inspection of actual persisted
// BLOCKED diagnostics. No HTTP endpoint is allowed to trigger evaluation.
func (a *App) handleAntiCheatShadowHistory(w http.ResponseWriter,r *http.Request){
 ac,ok:=a.requireCapability(w,r,permissions.CapPlayerLocationView)
 if !ok{return}
 if ac.scope.ServerID==nil{writeSaaSError(w,codeInvalidRequest,"no DayZ server selected");return}
 if a.DB==nil||a.DB.Pool==nil{writeSaaSError(w,codeInternalError,"C.A.S.E. history unavailable");return}
 if !enforceRateLimit(w,a.saasAdminReadLimiter,rateLimitKey(r)){return}
 var before *int64
 if raw:=r.URL.Query().Get("before");raw!=""{
  value,err:=strconv.ParseInt(raw,10,64)
  if err!=nil||value<=0{writeSaaSError(w,codeInvalidRequest,"invalid history cursor");return}
  before=&value
 }
 limit:=20
 if raw:=r.URL.Query().Get("limit");raw!=""{
  value,err:=strconv.Atoi(raw)
  if err!=nil||value<1||value>50{writeSaaSError(w,codeInvalidRequest,"limit must be 1-50");return}
  limit=value
 }
 ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout);defer cancel()
 items,err:=repository.NewShadowLedger(a.DB.Pool).ListShadowHistory(ctx,ac.scope.GuildID,*ac.scope.ServerID,before,limit)
 if err!=nil{
  slog.Warn("component=case","event","shadow_history_read_failed","err",err.Error())
  writeSaaSError(w,codeInternalError,"could not load C.A.S.E. shadow history");return
 }
 out:=caseShadowHistoryPage{Mode:"BLOCKED_DIAGNOSTICS_ONLY",ServerID:*ac.scope.ServerID,Items:items,
  ExecutionEnabled:false,DetectorsEnabled:false,Enforcement:"DISABLED"}
 if len(items)==limit{v:=strconv.FormatInt(items[len(items)-1].ID,10);out.NextCursor=&v}
 a.recordAudit(ctx,ac,"CASE_SHADOW_HISTORY_VIEWED","","","success",nil,map[string]any{"count":len(items)})
 writeSaaSJSON(w,http.StatusOK,out)
}
