//go:build integration

package app

import (
 "context"
 "fmt"
 "os"
 "os/exec"
 "regexp"
 "strconv"
 "strings"
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/repository"
)

// Exercise the actual offline CLI against the disposable integration database,
// not merely the repository runner. Never pass a production DSN to this test.
func TestCASE2G4OneShotCLIEndToEnd(t *testing.T) {
 if os.Getenv("ALLOW_INTEGRATION_DB_TESTS")!="true" || os.Getenv("TEST_DATABASE_URL")=="" {
  t.Fatal("one-shot CLI integration requires the explicit disposable database gate")
 }
 w:=newClientAdminWorld(t)
 ctx,cancel:=context.WithTimeout(context.Background(),180*time.Second);defer cancel()
 evidence:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
 source:="case/phase2g4-cli-fixture.ADM"
 for _,offset:=range []int64{110,220} {
  if err:=evidence.RecordCaseEvidence(ctx,caseHitInput(w.guildID,w.serverID,offset,source,fmt.Sprintf("%064x",offset)));err!=nil{t.Fatal(err)}
 }
 args:=[]string{"run","../../cmd/case-shadow-once","-guild",strconv.FormatInt(w.guildID,10),"-server",strconv.FormatInt(w.serverID,10),"-limit","2"}
 invoke:=func(gate string,extra ...string)(string,error){
  t.Helper()
  cmd:=exec.CommandContext(ctx,"go",append(append([]string{},args...),extra...)...)
  // Remove any inherited production connection or process-local execution gate.
  env:=make([]string,0,len(os.Environ())+4)
  for _,v:=range os.Environ(){
   if strings.HasPrefix(v,"DATABASE_URL=")||strings.HasPrefix(v,"DATABASE_PUBLIC_URL=")||strings.HasPrefix(v,"CASE_SHADOW_ONESHOT_ALLOWED="){continue}
   env=append(env,v)
  }
  env=append(env,"DATABASE_URL="+os.Getenv("TEST_DATABASE_URL"),"CASE_SHADOW_ONESHOT_ALLOWED="+gate)
  cmd.Env=env
  out,err:=cmd.CombinedOutput()
  return string(out),err
 }
 count:=func()int{
  t.Helper()
  var n int
  if err:=w.a.DB.Pool.QueryRow(ctx,"SELECT COUNT(*) FROM case_shadow_evaluations WHERE guild_id=$1 AND server_id=$2",w.guildID,w.serverID).Scan(&n);err!=nil{t.Fatal(err)}
  return n
 }
 before:=count()
 preview,err:=invoke("","-mode","preview")
 if err!=nil{t.Fatalf("preview failed: %s: %v",preview,err)}
 if !strings.Contains(preview,"READ_ONLY: no evaluation recorded")||count()!=before{t.Fatal("preview was not read-only")}
 fp:=regexp.MustCompile(`FINGERPRINT=([0-9a-f]{64})`).FindStringSubmatch(preview)
 plan:=regexp.MustCompile(`PLAN_HASH=([0-9a-f]{64})`).FindStringSubmatch(preview)
 if len(fp)!=2||len(plan)!=2||!strings.Contains(preview,"EVIDENCE_COUNT=2"){t.Fatalf("invalid preview contract: %q",preview)}
 execute:=[]string{"-mode","execute","-expected-fingerprint",fp[1],"-expected-plan",plan[1],"-ack","BLOCKED_DIAGNOSTICS_ONLY"}
 if out,err:=invoke("",execute...);err==nil||count()!=before{t.Fatalf("missing gate wrote evaluation: %q %v",out,err)}
 wrong:=append([]string{},execute...)
 wrong[5]=strings.Repeat("0",64) // -expected-plan's value
 if out,err:=invoke("1",wrong...);err==nil||count()!=before{t.Fatalf("wrong plan wrote evaluation: %q %v",out,err)}
 first,err:=invoke("1",execute...)
 if err!=nil||!strings.Contains(first,"READBACK=PASS ENFORCEMENT=DISABLED")||count()!=before+1{t.Fatalf("first execution: %q %v",first,err)}
 evaluation:=regexp.MustCompile(`BLOCKED_EVALUATION_ID=([0-9]+)`).FindStringSubmatch(first)
 if len(evaluation)!=2{t.Fatalf("evaluation ID absent: %q",first)}
 second,err:=invoke("1",execute...)
 if err!=nil||!strings.Contains(second,"BLOCKED_EVALUATION_ID="+evaluation[1]+" ")||count()!=before+1{t.Fatalf("replay not idempotent: %q %v",second,err)}
 id,err:=strconv.ParseInt(evaluation[1],10,64);if err!=nil{t.Fatal(err)}
 history,err:=repository.NewShadowLedger(w.a.DB.Pool).ListShadowHistory(ctx,w.guildID,w.serverID,nil,50)
 if err!=nil{t.Fatal(err)}
 found:=false
 for _,h:=range history{if h.ID==id&&h.Status=="BLOCKED"&&len(h.ReasonCodes)==4&&len(h.EvidenceIDs)==2{found=true}}
 if !found{t.Fatal("independent scoped history does not match CLI output")}
 // A newly persisted source line invalidates the approved evidence window.
 // Reusing the old preview must fail without creating a second record.
 if err:=evidence.RecordCaseEvidence(ctx,caseHitInput(w.guildID,w.serverID,330,source,fmt.Sprintf("%064x",330)));err!=nil{t.Fatal(err)}
 stale,err:=invoke("1",execute...)
 if err==nil||count()!=before+1{t.Fatalf("stale evidence plan was accepted: %q %v",stale,err)}
 refreshed,err:=invoke("","-mode","preview")
 if err!=nil||count()!=before+1{t.Fatalf("fresh preview failed or wrote: %q %v",refreshed,err)}
 nextFP:=regexp.MustCompile(`FINGERPRINT=([0-9a-f]{64})`).FindStringSubmatch(refreshed)
 nextPlan:=regexp.MustCompile(`PLAN_HASH=([0-9a-f]{64})`).FindStringSubmatch(refreshed)
 if len(nextFP)!=2||len(nextPlan)!=2||nextFP[1]==fp[1]||nextPlan[1]==plan[1]{
  t.Fatalf("source growth did not invalidate both preview identities: %q",refreshed)
 }
 freshExecute:=[]string{"-mode","execute","-expected-fingerprint",nextFP[1],"-expected-plan",nextPlan[1],"-ack","BLOCKED_DIAGNOSTICS_ONLY"}
 next,err:=invoke("1",freshExecute...)
 if err!=nil||!strings.Contains(next,"READBACK=PASS ENFORCEMENT=DISABLED")||count()!=before+2{
  t.Fatalf("fresh approved snapshot failed: %q %v",next,err)
 }
 if strings.Contains(next,"BLOCKED_EVALUATION_ID="+evaluation[1]+" "){t.Fatal("changed evidence reused old evaluation ID")}
 // Even if newer diagnostics push the first record beyond the newest 50,
 // its exact scoped readback must still be available without a false failure.
 for i:=1;i<=51;i++{
  fingerprint:=fmt.Sprintf("%064x",i)
  _,err:=w.a.DB.Pool.Exec(ctx,`INSERT INTO case_shadow_evaluations
   (guild_id,server_id,detector_id,detector_version,fingerprint,status,reason_codes)
   VALUES($1,$2,'CASE-MOV-001','0.1.0',$3,'BLOCKED',ARRAY['VERIFIED_EVENT_ELAPSED_TIME'])`,
   w.guildID,w.serverID,fingerprint)
  if err!=nil{t.Fatalf("insert newer synthetic diagnostic %d: %v",i,err)}
 }
 ledger:=repository.NewShadowLedger(w.a.DB.Pool)
 recent,err:=ledger.ListShadowHistory(ctx,w.guildID,w.serverID,nil,50)
 if err!=nil{t.Fatal(err)}
 for _,h:=range recent{if h.ID==id{t.Fatal("older evaluation unexpectedly remained in newest history page")}}
 exact,err:=ledger.GetShadowHistoryByID(ctx,w.guildID,w.serverID,id)
 if err!=nil||exact.ID!=id||exact.Status!="BLOCKED"||len(exact.EvidenceIDs)!=2{
  t.Fatalf("exact scoped older readback failed: %+v %v",exact,err)
 }
 if _,err=ledger.GetShadowHistoryByID(ctx,w.guildID,w.serverID+1,id);err==nil{
  t.Fatal("cross-server exact history lookup succeeded")
 }

}
