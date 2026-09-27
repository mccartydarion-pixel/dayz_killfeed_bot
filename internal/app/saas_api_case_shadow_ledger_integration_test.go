//go:build integration

package app

import (
 "context"
 "errors"
 "fmt"
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCASEBlockedShadowLedgerIdempotentAndTenantScoped(t *testing.T){
 w:=newClientAdminWorld(t)
 ctx:=context.Background()
 evidenceRepo:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
 if err:=evidenceRepo.RecordCaseEvidence(ctx,caseHitInput(w.guildID,w.serverID,101,
  "dayzps/config/shadow.ADM",fmt.Sprintf("%064x",101)));err!=nil{t.Fatal(err)}
 if err:=evidenceRepo.RecordCaseEvidence(ctx,caseHitInput(w.guildID,w.serverID,202,
  "dayzps/config/shadow.ADM",fmt.Sprintf("%064x",202)));err!=nil{t.Fatal(err)}
 events,err:=evidenceRepo.ListCaseEvidence(ctx,w.guildID,w.serverID,nil,nil,nil,20)
 if err!=nil||len(events)!=2{t.Fatalf("setup evidence: %v %+v",err,events)}
 ids:=[]int64{events[0].ID,events[1].ID}
 ledger:=repository.NewShadowLedger(w.a.DB.Pool)
 in:=repository.BlockedShadowInput{GuildID:w.guildID,ServerID:w.serverID,
  DetectorID:"CASE-MOV-001",Version:"0.1.0",EvidenceIDs:ids}
 if _,err:=ledger.RecordBlocked(ctx,in);!errors.Is(err,repository.ErrShadowDisabled){
  t.Fatalf("default gate failed: %v",err)
 }
 var count int
 if err:=w.a.DB.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations
 WHERE guild_id=$1 AND server_id=$2`,w.guildID,w.serverID).Scan(&count);err!=nil||count!=0{
  t.Fatalf("disabled gate wrote data: %d %v",count,err)
 }
 in.Enabled=true
 id,err:=ledger.RecordBlocked(ctx,in);if err!=nil{t.Fatal(err)}
 in.EvidenceIDs=[]int64{ids[1],ids[0]}
 again,err:=ledger.RecordBlocked(ctx,in)
 if err!=nil||id!=again{t.Fatalf("replay changed identity: %d %d %v",id,again,err)}
 var status string;var reasons []string
 err=w.a.DB.Pool.QueryRow(ctx,`SELECT status,reason_codes FROM case_shadow_evaluations WHERE id=$1`,id).Scan(&status,&reasons)
 if err!=nil||status!="BLOCKED"||len(reasons)!=4{t.Fatalf("unsafe persisted status: %s %v %v",status,reasons,err)}
 var links int
 err=w.a.DB.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluation_evidence WHERE evaluation_id=$1`,id).Scan(&links)
 if err!=nil||links!=2{t.Fatalf("duplicate or missing link: %d %v",links,err)}
 var other int64
 err=w.a.DB.Pool.QueryRow(ctx,`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
 w.guildID,fmt.Sprintf("shadow-other-%d",time.Now().UnixNano()),w.f.OrgID).Scan(&other)
 if err!=nil{t.Fatal(err)}
 in.ServerID=other
 if _,err=ledger.RecordBlocked(ctx,in);err==nil{t.Fatal("foreign evidence accepted")}
 in.ServerID=w.serverID
 in.EvidenceIDs=[]int64{ids[0],ids[0]}
 if _,err=ledger.RecordBlocked(ctx,in);err==nil{t.Fatal("duplicate evidence IDs accepted")}
 if err=w.a.DB.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations
 WHERE guild_id=$1 AND server_id=$2`,w.guildID,w.serverID).Scan(&count);err!=nil||count!=1{
  t.Fatalf("unexpected shadow evaluations: %d %v",count,err)
 }
}
