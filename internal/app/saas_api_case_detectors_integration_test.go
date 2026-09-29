//go:build integration

package app

import (
 "fmt"
 "net/http"
 "testing"
 "time"
)

func TestCASEDetectorReadinessIsScopedReadOnlyAndBlocked(t *testing.T) {
 w:=newClientAdminWorld(t)
 path:=w.path("/anti-cheat/detector-readiness")
 rr:=w.call(w.a.handleAntiCheatDetectorReadiness,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
 if rr.Code!=http.StatusOK{t.Fatalf("authorized readiness: %d %s",rr.Code,rr.Body.String())}
 out:=decodeBody[caseDetectorReadiness](t,rr)
 if out.ServerID!=w.serverID||out.Mode!="READINESS_ONLY"||out.ExecutionEnabled||
  out.DetectorsEnabled||out.Enforcement!="DISABLED"||len(out.Findings)!=0||
  len(out.Cases)!=0||len(out.Registry)!=8||len(out.Evaluations)!=8||len(out.Health)!=8{
  t.Fatalf("unsafe readiness response: %+v",out)
 }
 for i,result:=range out.Evaluations {
  health:=out.Health[i]
  if health.ModuleID!=out.Registry[i].ID||health.State=="ACTIVE"||!health.ConclusionsSuspended||health.LastSuccessfulEvaluationAt!=nil {t.Fatalf("unsafe detector health: %+v",health)}
  if out.Registry[i].ID=="CASE-MOV-001"||result.DetectorID!=out.Registry[i].ID||
   result.Status!="BLOCKED"||result.RiskScore!=nil||len(result.Findings)!=0||
   len(result.EvidenceIDs)!=0||len(result.Blockers)==0{t.Fatalf("unsafe detector state: %+v",result)}
  for _,blocker:=range result.Blockers {if blocker.Reason=="" {t.Fatalf("missing reason: %+v",result)}}
 }
 stranger:=syncUser(t,w.a,fmt.Sprintf("case-readiness-stranger-%d",time.Now().UnixNano()),"Stranger")
 denied:=w.call(w.a.handleAntiCheatDetectorReadiness,http.MethodGet,path,stranger.DiscordUserID,nil,nil)
 if denied.Code!=http.StatusForbidden{t.Fatalf("unauthorized read: %d",denied.Code)}
}
