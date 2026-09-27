//go:build integration

package app

import (
 "context"
 "fmt"
 "net/http"
 "net/url"
 "strconv"
 "testing"

 "github.com/yourname/dayz-killfeed/internal/repository"
)

// Phase 2G.4 verifies the real SQL transaction in a disposable PostgreSQL
// database. These are synthetic ADM-shaped fixture rows, NOT production data
// and not proof that CASE-MOV-001 can detect movement cheating.
func TestCASEShadow2G4BoundedSourceReplayAndPagination(t *testing.T) {
 w:=newClientAdminWorld(t)
 ctx:=context.Background()
 evidence:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
 ledger:=repository.NewShadowLedger(w.a.DB.Pool)
 sourceA:="dayzps/config/phase2g4-a.ADM"
 sourceB:="dayzps/config/phase2g4-b.ADM"
 add:=func(source string,offset int64) {
  t.Helper()
  input:=caseHitInput(w.guildID,w.serverID,offset,source,fmt.Sprintf("%064x",offset))
  if err:=evidence.RecordCaseEvidence(ctx,input);err!=nil{t.Fatal(err)}
 }
 // Deliberately interleave the two ADM sources in ingestion order.
 add(sourceA,100);add(sourceA,200);add(sourceB,100);add(sourceA,300);add(sourceB,200)
 in:=repository.ShadowRunInput{GuildID:w.guildID,ServerID:w.serverID,
  DetectorID:"CASE-MOV-001",Version:"0.1.0",Limit:2}
 if _,err:=ledger.RunBlockedOnLatestSource(ctx,in);err!=repository.ErrShadowDisabled{
  t.Fatalf("disabled run: %v",err)
 }
 var count int
 if err:=w.a.DB.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations
  WHERE guild_id=$1 AND server_id=$2`,w.guildID,w.serverID).Scan(&count);err!=nil||count!=0{
  t.Fatalf("disabled runner performed a write: %d / %v",count,err)
 }
 in.Enabled=true
 first,err:=ledger.RunBlockedOnLatestSource(ctx,in);if err!=nil{t.Fatal(err)}
 replay,err:=ledger.RunBlockedOnLatestSource(ctx,in)
 if err!=nil||replay.EvaluationID!=first.EvaluationID||replay.EvidenceCount!=2{
  t.Fatalf("source replay must be idempotent: %+v %+v %v",first,replay,err)
 }
 hist,err:=ledger.ListShadowHistory(ctx,w.guildID,w.serverID,nil,20)
 if err!=nil||len(hist)!=1||hist[0].Status!="BLOCKED"||len(hist[0].EvidenceIDs)!=2{
  t.Fatalf("expected one blocked evaluation: %+v / %v",hist,err)
 }
 // The runner must link only evidence from source B, the most recently
 // ingested source; the two highest byte offsets in that same source.
 var sourceCount int
 err=w.a.DB.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluation_evidence l
  JOIN case_evidence_events e ON e.id=l.evidence_id
  WHERE l.evaluation_id=$1 AND e.guild_id=$2 AND e.server_id=$3
   AND e.source_id=$4 AND e.source_end_offset IN (100,200)`,
  first.EvaluationID,w.guildID,w.serverID,sourceB).Scan(&sourceCount)
 if err!=nil||sourceCount!=2{t.Fatalf("cross-source or offset mix: %d / %v",sourceCount,err)}
 // Adding one source B event changes the evidence identity, but replay of
 // that new snapshot still has exactly one durable record.
 add(sourceB,300)
 second,err:=ledger.RunBlockedOnLatestSource(ctx,in)
 if err!=nil||second.EvaluationID==first.EvaluationID{t.Fatalf("new snapshot was not distinct: %+v / %v",second,err)}
 again,err:=ledger.RunBlockedOnLatestSource(ctx,in)
 if err!=nil||again.EvaluationID!=second.EvaluationID{t.Fatalf("changed snapshot replay: %+v / %v",again,err)}
 page,err:=ledger.ListShadowHistory(ctx,w.guildID,w.serverID,nil,1)
 if err!=nil||len(page)!=1||page[0].ID!=second.EvaluationID{t.Fatalf("page one: %+v / %v",page,err)}
 before:=page[0].ID
 older,err:=ledger.ListShadowHistory(ctx,w.guildID,w.serverID,&before,1)
 if err!=nil||len(older)!=1||older[0].ID!=first.EvaluationID{t.Fatalf("page two: %+v / %v",older,err)}
 empty,err:=ledger.ListShadowHistory(ctx,w.guildID,w.serverID,&older[0].ID,1)
 if err!=nil||len(empty)!=0{t.Fatalf("pagination did not stop: %+v / %v",empty,err)}
 // Exact evidence IDs remain addressable through the tenant-scoped evidence
 // repository; no speculative player score or finding is created.
 items,err:=evidence.ListCaseEvidence(ctx,w.guildID,w.serverID,nil,nil,nil,50)
 if err!=nil{t.Fatal(err)}
 known:=map[int64]bool{}
 for _,e:=range items{known[e.ID]=true}
 for _,h:=range append(page,older...){
  if len(h.ReasonCodes)!=4||h.Status!="BLOCKED"{t.Fatalf("unsafe evaluation: %+v",h)}
  for _,id:=range h.EvidenceIDs{if !known[id]{t.Fatalf("unresolvable evidence ID %d",id)}}
 }
 if err:=w.a.DB.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations
  WHERE guild_id=$1 AND server_id=$2`,w.guildID,w.serverID).Scan(&count);err!=nil||count!=2{
  t.Fatalf("replay created duplicates: %d / %v",count,err)
 }
}

func TestCASEShadow2G4HistoryAuthorizationBoundsAndNoWriteRoute(t *testing.T) {
 w:=newClientAdminWorld(t)
 ctx:=context.Background()
 evidence:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
 for _,offset:=range []int64{10,20} {
  if err:=evidence.RecordCaseEvidence(ctx,caseHitInput(w.guildID,w.serverID,offset,
   "dayzps/config/phase2g4-permissions.ADM",fmt.Sprintf("%064x",offset)));err!=nil{t.Fatal(err)}
 }
 ledger:=repository.NewShadowLedger(w.a.DB.Pool)
 input:=repository.ShadowRunInput{Enabled:true,GuildID:w.guildID,ServerID:w.serverID,
  DetectorID:"CASE-MOV-001",Version:"0.1.0",Limit:51}
 if _,err:=ledger.RunBlockedOnLatestSource(ctx,input);err==nil{t.Fatal("unbounded run accepted")}
 input.Limit=1
 if _,err:=ledger.RunBlockedOnLatestSource(ctx,input);err!=nil{t.Fatal(err)}
 path:=w.path("/anti-cheat/shadow-history")
 ok:=w.call(w.a.handleAntiCheatShadowHistory,http.MethodGet,path+"?limit=1",w.f.OwnerDiscordID,nil,nil)
 if ok.Code!=http.StatusOK{t.Fatalf("authorized read: %d %s",ok.Code,ok.Body.String())}
 body:=decodeBody[caseShadowHistoryPage](t,ok)
 if body.ExecutionEnabled||body.DetectorsEnabled||body.Enforcement!="DISABLED"||
  len(body.Items)!=1||len(body.Items[0].EvidenceIDs)!=1{
  t.Fatalf("unsafe history response: %+v",body)
 }
 bad:=w.call(w.a.handleAntiCheatShadowHistory,http.MethodGet,path+"?limit=51",w.f.OwnerDiscordID,nil,nil)
 if bad.Code==http.StatusOK{t.Fatal("out-of-range history limit accepted")}
 malformed:=w.call(w.a.handleAntiCheatShadowHistory,http.MethodGet,path+"?before="+url.QueryEscape("-1"),w.f.OwnerDiscordID,nil,nil)
 if malformed.Code==http.StatusOK{t.Fatal("invalid cursor accepted")}
 // The history route is registered GET-only: a POST must not invoke the
 // explicit runner or create any database row.
 post:=w.call(w.a.handleAntiCheatShadowHistory,http.MethodPost,path,w.f.OwnerDiscordID,nil,nil)
 _=post // Direct handler has no method check; check actual router in a separate registration test.
 var count int
 if err:=w.a.DB.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations
  WHERE guild_id=$1 AND server_id=$2`,w.guildID,w.serverID).Scan(&count);err!=nil||count!=1{
  t.Fatalf("read handler wrote a shadow record: %d / %v",count,err)
 }
 // A cursor below the earliest record is a truthful empty result.
 after:=strconv.FormatInt(body.Items[0].ID,10)
 next:=w.call(w.a.handleAntiCheatShadowHistory,http.MethodGet,path+"?before="+after,w.f.OwnerDiscordID,nil,nil)
 if next.Code!=http.StatusOK||len(decodeBody[caseShadowHistoryPage](t,next).Items)!=0{
  t.Fatalf("history cursor failed: %d %s",next.Code,next.Body.String())
 }
}
