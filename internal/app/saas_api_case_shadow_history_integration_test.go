//go:build integration

package app

import (
 "context"
 "fmt"
 "net/http"
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCASEShadowRunReadOnlyHistoryAndIsolation(t *testing.T) {
 w:=newClientAdminWorld(t)
 ctx:=context.Background()
 evidence:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
 source:="dayzps/config/case-2g3.ADM"
 for _,offset:=range []int64{101,202,303} {
  if err:=evidence.RecordCaseEvidence(ctx,caseHitInput(w.guildID,w.serverID,offset,source,fmt.Sprintf("%064x",offset)));err!=nil{t.Fatal(err)}
 }
 ledger:=repository.NewShadowLedger(w.a.DB.Pool)
 in:=repository.ShadowRunInput{GuildID:w.guildID,ServerID:w.serverID,DetectorID:"CASE-MOV-001",Version:"0.1.0",Limit:2}
 if _,err:=ledger.RunBlockedOnLatestSource(ctx,in);err!=repository.ErrShadowDisabled{t.Fatalf("run without gate: %v",err)}
 in.Enabled=true
 first,err:=ledger.RunBlockedOnLatestSource(ctx,in);if err!=nil{t.Fatal(err)}
 again,err:=ledger.RunBlockedOnLatestSource(ctx,in)
 if err!=nil||first.EvaluationID!=again.EvaluationID||first.EvidenceCount!=2||first.Status!="BLOCKED"{
  t.Fatalf("replay mismatch: %+v %+v %v",first,again,err)
 }
 hist,err:=ledger.ListShadowHistory(ctx,w.guildID,w.serverID,nil,20)
 if err!=nil||len(hist)!=1||hist[0].ID!=first.EvaluationID||len(hist[0].EvidenceIDs)!=2||hist[0].Status!="BLOCKED"{
  t.Fatalf("history mismatch: %+v %v",hist,err)
 }
 var other int64
 err=w.a.DB.Pool.QueryRow(ctx,`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
 w.guildID,fmt.Sprintf("shadow-review-other-%d",time.Now().UnixNano()),w.f.OrgID).Scan(&other)
 if err!=nil{t.Fatal(err)}
 alien,err:=ledger.ListShadowHistory(ctx,w.guildID,other,nil,20)
 if err!=nil||len(alien)!=0{t.Fatalf("cross-server history leaked: %+v %v",alien,err)}
 path:=w.path("/anti-cheat/shadow-history")
 rr:=w.call(w.a.handleAntiCheatShadowHistory,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
 if rr.Code!=http.StatusOK{t.Fatalf("staff read failed: %d %s",rr.Code,rr.Body.String())}
 page:=decodeBody[caseShadowHistoryPage](t,rr)
 if page.Mode!="BLOCKED_DIAGNOSTICS_ONLY"||page.ExecutionEnabled||page.DetectorsEnabled||
 page.Enforcement!="DISABLED"||len(page.Items)!=1{t.Fatalf("unsafe history response: %+v",page)}
 stranger:=syncUser(t,w.a,fmt.Sprintf("shadow-read-stranger-%d",time.Now().UnixNano()),"Stranger")
 denied:=w.call(w.a.handleAntiCheatShadowHistory,http.MethodGet,path,stranger.DiscordUserID,nil,nil)
 if denied.Code!=http.StatusForbidden{t.Fatalf("unprivileged read: %d",denied.Code)}
}

func TestCASEShadowRunRejectsEmptySource(t *testing.T){
 w:=newClientAdminWorld(t)
 _,err:=repository.NewShadowLedger(w.a.DB.Pool).RunBlockedOnLatestSource(context.Background(),
 repository.ShadowRunInput{Enabled:true,GuildID:w.guildID,ServerID:w.serverID,DetectorID:"CASE-MOV-001",Version:"0.1.0",Limit:5})
 if err==nil{t.Fatal("empty source should not create a diagnostic")}
}
