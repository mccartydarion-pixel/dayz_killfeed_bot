package app

import (
 "context"
 "log/slog"
 "net/http"
 "strconv"

 "github.com/yourname/dayz-killfeed/internal/permissions"
 "github.com/yourname/dayz-killfeed/internal/repository"
)

type caseEvidenceRefsPage struct {
 Mode string `json:"mode"`
 ServerID int64 `json:"serverId"`
 CaseID int64 `json:"caseId"`
 EvidenceIDs []int64 `json:"evidenceIds"`
 NextCursor *string `json:"nextCursor"`
 DetectorsEnabled bool `json:"detectorsEnabled"`
 AlertsEnabled bool `json:"alertsEnabled"`
 Enforcement string `json:"enforcement"`
}

// The selected installation and actor are resolved server-side. Every link
// still goes through the separately protected exact-evidence read endpoint.
func (a *App) handleAntiCheatCaseEvidenceRefs(w http.ResponseWriter,r *http.Request){
 ac,ok:=a.requireCapability(w,r,permissions.CapPlayerLocationView)
 if !ok{return}
 if ac.scope.ServerID==nil||*ac.scope.ServerID<=0{
  writeSaaSError(w,codeInvalidRequest,"no DayZ server selected");return
 }
 if a.DB==nil||a.DB.Pool==nil{
  writeSaaSError(w,codeInternalError,"C.A.S.E. evidence links unavailable");return
 }
 if !enforceRateLimit(w,a.saasAdminReadLimiter,rateLimitKey(r)){return}
 caseID,err:=strconv.ParseInt(r.PathValue("caseID"),10,64)
 if err!=nil||caseID<=0{writeSaaSError(w,codeInvalidRequest,"invalid case ID");return}
 var before *int64
 if raw:=r.URL.Query().Get("before");raw!=""{
  value,e:=strconv.ParseInt(raw,10,64)
  if e!=nil||value<=0{writeSaaSError(w,codeInvalidRequest,"invalid evidence cursor");return}
  before=&value
 }
 limit:=20
 if raw:=r.URL.Query().Get("limit");raw!=""{
  v,e:=strconv.Atoi(raw)
  if e!=nil||v<1||v>50{writeSaaSError(w,codeInvalidRequest,"limit must be 1-50");return}
  limit=v
 }
 ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
 defer cancel()
 ids,err:=repository.NewCaseEvidenceLinkReader(a.DB.Pool).ListAuthorized(ctx,
  repository.CaseReviewScope{GuildID:ac.scope.GuildID,ServerID:*ac.scope.ServerID,
   InstallationID:ac.scope.InstallationID},caseID,ac.user.ID,before,limit)
 if err!=nil{
  slog.Warn("component=case","event","case_evidence_refs_read_failed","err",err.Error())
  writeSaaSError(w,codeInternalError,"could not load case evidence links");return
 }
 out:=caseEvidenceRefsPage{Mode:"AUTHORIZED_EVIDENCE_REFERENCES_ONLY",
  ServerID:*ac.scope.ServerID,CaseID:caseID,EvidenceIDs:ids,
  DetectorsEnabled:false,AlertsEnabled:false,Enforcement:"DISABLED"}
 if len(ids)==limit{cursor:=strconv.FormatInt(ids[len(ids)-1],10);out.NextCursor=&cursor}
 a.recordAudit(ctx,ac,"CASE_EVIDENCE_REFS_VIEWED","","","success",nil,
  map[string]any{"caseId":caseID,"count":len(ids)})
 writeSaaSJSON(w,http.StatusOK,out)
}
