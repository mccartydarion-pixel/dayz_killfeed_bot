//go:build integration

package main

import (
 "context"
 "crypto/sha256"
 "fmt"
 "os"
 "os/exec"
 "regexp"
 "strconv"
 "strings"
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/database"
)

// Only the disposable CI database is used; Railway staging and production
// variables are never read or inherited into the connection.
func TestQASyntheticFixtureOnDisposablePostgres(t *testing.T){
 if os.Getenv("ALLOW_INTEGRATION_DB_TESTS")!="true"||os.Getenv("TEST_DATABASE_URL")==""{
  t.Fatal("explicit disposable database gate required")
 }
 ctx,cancel:=context.WithTimeout(context.Background(),240*time.Second);defer cancel()
 db,err:=database.Connect(ctx,os.Getenv("TEST_DATABASE_URL"))
 if err!=nil{t.Fatal(err)}
 defer db.Close()
 if err:=db.Migrate(ctx);err!=nil{t.Fatal(err)}
 g,s,err:=seedSynthetic(ctx,db.Pool)
 if err!=nil{t.Fatalf("fixture first seed: %v",err)}
 g2,s2,err:=seedSynthetic(ctx,db.Pool)
 if err!=nil||g2!=g||s2!=s{t.Fatalf("fixture not idempotent: %d %d %v",g2,s2,err)}
 var n int
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_evidence_events WHERE guild_id=$1 AND server_id=$2`,g,s).Scan(&n);err!=nil||n!=2{
  t.Fatalf("unexpected synthetic evidence count %d: %v",n,err)
 }
 var evaluations int
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations WHERE guild_id=$1 AND server_id=$2`,g,s).Scan(&evaluations);err!=nil||evaluations!=0{
  t.Fatalf("setup created a diagnostic %d: %v",evaluations,err)
 }
 // Run the actual verifier against this fixture using a child process with
 // production database URLs and the write gate stripped from its environment.
 cmd:=exec.CommandContext(ctx,"go","run","../case-shadow-once","-mode","preview",
  "-guild",strconv.FormatInt(g,10),"-server",strconv.FormatInt(s,10),"-limit","2")
 env:=make([]string,0,len(os.Environ())+2)
 for _,v:=range os.Environ(){
  if strings.HasPrefix(v,"DATABASE_URL=")||strings.HasPrefix(v,"DATABASE_PUBLIC_URL=")||
   strings.HasPrefix(v,"CASE_SHADOW_ONESHOT_ALLOWED="){continue}
  env=append(env,v)
 }
 cmd.Env=append(env,"DATABASE_URL="+os.Getenv("TEST_DATABASE_URL"),
  "DATABASE_PUBLIC_URL=","CASE_SHADOW_ONESHOT_ALLOWED=")
 out,err:=cmd.CombinedOutput()
 if err!=nil||!strings.Contains(string(out),"MODE=preview")||
  !strings.Contains(string(out),"EVIDENCE_COUNT=2")||
  !strings.Contains(string(out),"READ_ONLY: no evaluation recorded")||
  len(regexp.MustCompile(`PLAN_HASH=([0-9a-f]{64})`).FindStringSubmatch(string(out)))!=2{
  t.Fatalf("actual CLI preview of synthetic fixture failed: %q %v",string(out),err)
 }
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations
 WHERE guild_id=$1 AND server_id=$2`,g,s).Scan(&evaluations);err!=nil||evaluations!=0{
  t.Fatalf("actual CLI preview wrote a diagnostic: %d %v",evaluations,err)
 }
 // A prior fixture with altered source content must not be accepted as valid.
 _,err=db.Pool.Exec(ctx,`UPDATE case_evidence_events SET line_sha256=$1
 WHERE guild_id=$2 AND server_id=$3 AND source_id=$4 AND source_end_offset=110`,
 fmt.Sprintf("%064x",9),g,s,qaSource)
 if err!=nil{t.Fatal(err)}
 if _,_,err:=seedSynthetic(ctx,db.Pool);err==nil{t.Fatal("altered synthetic source content accepted")}
 fp:=regexp.MustCompile(`FINGERPRINT=([0-9a-f]{64})`).FindStringSubmatch(string(out))
 plan:=regexp.MustCompile(`PLAN_HASH=([0-9a-f]{64})`).FindStringSubmatch(string(out))
 if len(fp)!=2||len(plan)!=2{t.Fatal("preview omitted exact execution identities")}
 executeArgs:=[]string{"-mode","execute","-guild",strconv.FormatInt(g,10),
  "-server",strconv.FormatInt(s,10),"-limit","2",
  "-expected-fingerprint",fp[1],"-expected-plan",plan[1],
  "-ack","BLOCKED_DIAGNOSTICS_ONLY"}
 invoke:=func(gate string,args ...string)(string,error){
  t.Helper()
  cli:=exec.CommandContext(ctx,"go",append([]string{"run","../case-shadow-once"},args...)...)
  cli.Env=append(append([]string{},env[:len(env)-3]...),
   "DATABASE_URL="+os.Getenv("TEST_DATABASE_URL"),"DATABASE_PUBLIC_URL=",
   "CASE_SHADOW_ONESHOT_ALLOWED="+gate)
  output,runErr:=cli.CombinedOutput()
  return string(output),runErr
 }
 // The approved preview must go stale when its source content is altered.
 if result,runErr:=invoke("1",executeArgs...);runErr==nil{
  t.Fatalf("stale evidence plan was accepted: %q",result)
 }
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations
 WHERE guild_id=$1 AND server_id=$2`,g,s).Scan(&evaluations);err!=nil||evaluations!=0{
  t.Fatalf("stale execution wrote a diagnostic: %d %v",evaluations,err)
 }
 // Restore the synthetic input and ensure fixture setup accepts the exact data.
 hash:=sha256.Sum256([]byte(fmt.Sprintf("%s:%d",qaSource,110)))
 _,err=db.Pool.Exec(ctx,`UPDATE case_evidence_events SET line_sha256=$1
 WHERE guild_id=$2 AND server_id=$3 AND source_id=$4 AND source_end_offset=110`,
 fmt.Sprintf("%x",hash),g,s,qaSource)
 if err!=nil{t.Fatal(err)}
 if g3,s3,setupErr:=seedSynthetic(ctx,db.Pool);setupErr!=nil||g3!=g||s3!=s{
  t.Fatalf("restored fixture failed: %d %d %v",g3,s3,setupErr)
 }
 if result,runErr:=invoke("",executeArgs...);runErr==nil{
  t.Fatalf("missing gate allowed diagnostic: %q",result)
 }
 first,runErr:=invoke("1",executeArgs...)
 if runErr!=nil||!strings.Contains(first,"READBACK=PASS ENFORCEMENT=DISABLED"){
  t.Fatalf("gated fixture execution failed: %q %v",first,runErr)
 }
 idMatch:=regexp.MustCompile(`BLOCKED_EVALUATION_ID=([0-9]+)`).FindStringSubmatch(first)
 if len(idMatch)!=2{t.Fatalf("missing evaluation ID in %q",first)}
 evaluationID,parseErr:=strconv.ParseInt(idMatch[1],10,64)
 if parseErr!=nil{t.Fatal(parseErr)}
 // Independent SQL readback, not just the CLI's own success message.
 var status string
 var reasons []string
 err=db.Pool.QueryRow(ctx,`SELECT status,reason_codes FROM case_shadow_evaluations
 WHERE id=$1 AND guild_id=$2 AND server_id=$3`,evaluationID,g,s).Scan(&status,&reasons)
 if err!=nil||status!="BLOCKED"||len(reasons)!=4{
  t.Fatalf("independent scoped evaluation mismatch: status=%s reasons=%v err=%v",status,reasons,err)
 }
 var linked int
 err=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluation_evidence l
 JOIN case_evidence_events e ON e.id=l.evidence_id AND e.guild_id=l.guild_id AND e.server_id=l.server_id
 WHERE l.evaluation_id=$1 AND l.guild_id=$2 AND l.server_id=$3 AND e.source_id=$4`,
 evaluationID,g,s,qaSource).Scan(&linked)
 if err!=nil||linked!=2{t.Fatalf("independent source linkage mismatch: %d %v",linked,err)}
 second,runErr:=invoke("1",executeArgs...)
 if runErr!=nil||!strings.Contains(second,"BLOCKED_EVALUATION_ID="+idMatch[1]+" ")||
  !strings.Contains(second,"READBACK=PASS ENFORCEMENT=DISABLED"){
  t.Fatalf("fixture replay failed: %q %v",second,runErr)
 }
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations
 WHERE guild_id=$1 AND server_id=$2`,g,s).Scan(&evaluations);err!=nil||evaluations!=1{
  t.Fatalf("replay was not idempotent: %d %v",evaluations,err)
 }
}
