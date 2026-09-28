package app

import (
 "context"
 "encoding/json"
 "errors"
 "io"
 "net/http"
 "os"
 "strconv"
 "time"

 "github.com/yourname/dayz-killfeed/internal/caseintel"
 "github.com/yourname/dayz-killfeed/internal/permissions"
 "github.com/yourname/dayz-killfeed/internal/repository"
)

type caseReviewWriteBody struct {
 ActionKey string `json:"actionKey"`
 ExpectedStatus string `json:"expectedStatus"`
 ToStatus string `json:"toStatus"`
 ReasonCode string `json:"reasonCode"`
 Note string `json:"note"`
}
type caseReviewWriteResult struct {
 CaseID int64 `json:"caseId"`
 Changed bool `json:"changed"`
 Mode string `json:"mode"`
 AlertsEnabled bool `json:"alertsEnabled"`
 Enforcement string `json:"enforcement"`
}

// The feature switch defaults OFF. Setting it is a separate operator release
// action, not implied by source merge. The independently validated registry
// must also contain a detector before the route is exposed. Per-case version
// and linked-evidence eligibility are rechecked inside ApplyReviewed.
func caseReviewWritesAvailable() bool {
 if os.Getenv("CHAMPION_CASE_REVIEW_WRITES_ENABLED")!="true"{return false}
 for _,d:=range caseintel.Registry(){
  if d.Mode=="VALIDATED_SHADOW"{return true}
 }
 return false
}

// handleAntiCheatCaseReview is a guarded staff transition, not case admission
// or a finding/verdict API. Scope and actor come ONLY from existing auth, never
// from JSON. It cannot enqueue or send a Discord message.
func (a *App) handleAntiCheatCaseReview(w http.ResponseWriter,r *http.Request){
 if !caseReviewWritesAvailable(){http.NotFound(w,r);return}
 ac,ok:=a.requireCapability(w,r,permissions.CapPlayerLocationView)
 if !ok{return}
 if ac.scope.ServerID==nil||*ac.scope.ServerID<=0{
  writeSaaSError(w,codeInvalidRequest,"no DayZ server selected");return
 }
 if a.DB==nil||a.DB.Pool==nil{
  writeSaaSError(w,codeInternalError,"C.A.S.E. review unavailable");return
 }
 if !enforceRateLimit(w,a.saasAdminReadLimiter,rateLimitKey(r)){return}
 caseID,err:=strconv.ParseInt(r.PathValue("caseID"),10,64)
 if err!=nil||caseID<=0{
  writeSaaSError(w,codeInvalidRequest,"invalid case ID");return
 }
 dec:=json.NewDecoder(http.MaxBytesReader(w,r.Body,2048))
 dec.DisallowUnknownFields()
 var body caseReviewWriteBody
 if err:=dec.Decode(&body);err!=nil{
  writeSaaSError(w,codeInvalidRequest,"invalid review request");return
 }
 if _,err=dec.Token();!errors.Is(err,io.EOF){
  writeSaaSError(w,codeInvalidRequest,"invalid review request");return
 }
 ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
 defer cancel()
 changed,err:=repository.NewCaseReviewMutation(a.DB.Pool).ApplyReviewed(ctx,
  repository.CaseReviewAction{
   Scope:repository.CaseReviewScope{GuildID:ac.scope.GuildID,
    ServerID:*ac.scope.ServerID,InstallationID:ac.scope.InstallationID},
   CaseID:caseID,ActorUserID:ac.user.ID,ActionKey:body.ActionKey,
   ExpectedStatus:body.ExpectedStatus,ToStatus:body.ToStatus,
   ReasonCode:body.ReasonCode,Note:body.Note,At:time.Now().UTC(),
  })
 if err!=nil{
  // Deliberately do not disclose existence, staff membership or evidence.
  writeSaaSError(w,codeInvalidRequest,"review unavailable or no longer eligible");return
 }
 a.recordAudit(ctx,ac,"CASE_REVIEW_ACTION","","","success",nil,
  map[string]any{"caseId":caseID,"changed":changed,"toStatus":body.ToStatus})
 writeSaaSJSON(w,http.StatusOK,caseReviewWriteResult{
  CaseID:caseID,Changed:changed,Mode:"NEUTRAL_REVIEW_ONLY",
  AlertsEnabled:false,Enforcement:"DISABLED",
 })
}
