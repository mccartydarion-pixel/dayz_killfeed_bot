//go:build integration

package repository

import (
 "context"
 "errors"
 "fmt"
 "os"
 "strings"
 "sync"
 "testing"
 "time"

 "github.com/jackc/pgx/v5/pgconn"
 "github.com/yourname/dayz-killfeed/internal/database"
)

// All test writes are restricted to a fresh disposable PostgreSQL schema.
func TestCASEReviewFixtureTransaction(t *testing.T){
 url:=strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
 if url==""{
  if os.Getenv("REQUIRE_INTEGRATION_DB")=="1"{t.Fatal("disposable DB required")}
  t.Skip("no disposable DB")
 }
 if os.Getenv("ALLOW_INTEGRATION_DB_TESTS")!="true"{t.Fatal("disposable DB authorization required")}
 ctx,cancel:=context.WithTimeout(context.Background(),3*time.Minute);defer cancel()
 admin,err:=database.Connect(ctx,url);if err!=nil{t.Fatal(err)}
 schema:=fmt.Sprintf("case_tx_%d",time.Now().UnixNano())
 if _,err=admin.Pool.Exec(ctx,"CREATE SCHEMA "+schema);err!=nil{admin.Close();t.Fatal(err)}
 t.Cleanup(func(){_,_=admin.Pool.Exec(context.Background(),"DROP SCHEMA IF EXISTS "+schema+" CASCADE");admin.Close()})
 sep:="?";if strings.Contains(url,"?"){sep="&"}
 db,err:=database.Connect(ctx,url+sep+"search_path="+schema);if err!=nil{t.Fatal(err)}
 t.Cleanup(db.Close)
 if err=db.Migrate(ctx);err!=nil{t.Fatal(err)}
 one:=func(q string,args ...any)int64{
  t.Helper();var id int64
  if e:=db.Pool.QueryRow(ctx,q,args...).Scan(&id);e!=nil{t.Fatalf("%s: %v",q,e)}
  return id
 }
 owner:=one("INSERT INTO app_users(discord_user_id,discord_username) VALUES('review-fixture-owner','Fixture') RETURNING id")
 outsider:=one("INSERT INTO app_users(discord_user_id,discord_username) VALUES('review-fixture-outsider','Fixture') RETURNING id")
 org:=one("INSERT INTO organizations(name,slug,owner_user_id) VALUES('Review fixture','case-review-tx',$1) RETURNING id",owner)
 guild:=one("INSERT INTO guilds(discord_guild_id) VALUES('case-review-tx-guild') RETURNING id")
 conn:=one("INSERT INTO discord_guild_connections(organization_id,guild_id) VALUES($1,$2) RETURNING id",org,guild)
 srv:=one("INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id) VALUES($1,'fixture','review-one','dayz','PLAYSTATION','ACTIVE',$2) RETURNING id",guild,org)
 otherSrv:=one("INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id) VALUES($1,'fixture','review-two','dayz','PLAYSTATION','ACTIVE',$2) RETURNING id",guild,org)
 inst:=one("INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id) VALUES($1,$2,$3) RETURNING id",org,conn,srv)
 otherInst:=one("INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id) VALUES($1,$2,$3) RETURNING id",org,conn,otherSrv)
 if _,err=db.Pool.Exec(ctx,"INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,'OWNER')",org,owner);err!=nil{t.Fatal(err)}
 a,b,c:=strings.Repeat("a",64),strings.Repeat("b",64),strings.Repeat("c",64)
 caseID:=one("INSERT INTO case_review_cases(guild_id,server_id,installation_id,discord_guild_connection_id,detector_id,detector_version,evidence_fingerprint,source_quality_ref,status) VALUES($1,$2,$3,$4,'SYNTHETIC','0.0.0',$5,$6,'PENDING_REVIEW') RETURNING id",guild,srv,inst,conn,a,c)
 foreignCase:=one("INSERT INTO case_review_cases(guild_id,server_id,installation_id,discord_guild_connection_id,detector_id,detector_version,evidence_fingerprint,source_quality_ref,status) VALUES($1,$2,$3,$4,'SYNTHETIC','0.0.0',$5,$6,'PENDING_REVIEW') RETURNING id",guild,otherSrv,otherInst,conn,a,c)
 writer:=NewCaseReviewMutation(db.Pool)
 at:=time.Now().UTC().Truncate(time.Second).Add(2*time.Second)
 input:=SyntheticReviewInput{FixtureOnly:true,CallerCapabilityVerified:true,Scope:CaseReviewScope{GuildID:guild,ServerID:srv,InstallationID:inst},CaseID:caseID,ActorUserID:owner,ActionKey:a,ExpectedStatus:"PENDING_REVIEW",ToStatus:"REVIEWED",ReasonCode:"EVIDENCE_REVIEWED",Note:"Synthetic review",At:at}
 // Authorization is enforced inside the SAME bounded queue statement.
 // Merely asserting FixtureOnly cannot grant a foreign or revoked actor access.
 reader:=NewCaseReviewReader(db.Pool)
 read:=SyntheticAuthorizedCaseRead{FixtureOnly:true,Scope:input.Scope,ActorUserID:owner,Limit:1}
 visible,readErr:=reader.ListAuthorizedSynthetic(ctx,read)
 if readErr!=nil||len(visible)!=1||visible[0].ID!=caseID||visible[0].EvidenceCount!=0||visible[0].AuditCount!=0{
  t.Fatalf("authorized fixture read: %+v %v",visible,readErr)
 }
 read.ActorUserID=outsider
 visible,readErr=reader.ListAuthorizedSynthetic(ctx,read)
 if readErr!=nil||len(visible)!=0{t.Fatalf("outsider saw fixture case: %+v %v",visible,readErr)}
 read.ActorUserID=owner
 read.Scope.InstallationID=otherInst
 read.Scope.ServerID=otherSrv
 visible,readErr=reader.ListAuthorizedSynthetic(ctx,read)
 if readErr!=nil||len(visible)!=1||visible[0].ID!=foreignCase{
  t.Fatalf("selected foreign installation should return only its own fixture: %+v %v",visible,readErr)
 }
 read.Scope=input.Scope
 cursor:=caseID
 read.Before=&cursor
 visible,readErr=reader.ListAuthorizedSynthetic(ctx,read)
 if readErr!=nil||len(visible)!=0{t.Fatalf("cursor escaped descending scope: %+v %v",visible,readErr)}
 read.Before=nil
 read.FixtureOnly=false
 if _,readErr=reader.ListAuthorizedSynthetic(ctx,read);!errors.Is(readErr,ErrCASEReviewFixtureDisabled){
  t.Fatalf("fixture read gate bypass: %v",readErr)
 }
 read.FixtureOnly=true
 read.Limit=51
 if _,readErr=reader.ListAuthorizedSynthetic(ctx,read);readErr==nil{t.Fatal("unbounded case page accepted")}
 read.Limit=1
 if _,err=db.Pool.Exec(ctx,"UPDATE organization_members SET role='MEMBER' WHERE organization_id=$1 AND user_id=$2",org,owner);err!=nil{t.Fatal(err)}
 visible,readErr=reader.ListAuthorizedSynthetic(ctx,read)
 if readErr!=nil||len(visible)!=0{t.Fatalf("revoked role saw case queue: %+v %v",visible,readErr)}
 if _,err=db.Pool.Exec(ctx,"UPDATE organization_members SET role='OWNER' WHERE organization_id=$1 AND user_id=$2",org,owner);err!=nil{t.Fatal(err)}
 state:=func()(string,int){
  t.Helper();var s string;var n int
  if e:=db.Pool.QueryRow(ctx,"SELECT status FROM case_review_cases WHERE id=$1",caseID).Scan(&s);e!=nil{t.Fatal(e)}
  if e:=db.Pool.QueryRow(ctx,"SELECT COUNT(*) FROM case_review_audit WHERE case_id=$1",caseID).Scan(&n);e!=nil{t.Fatal(e)}
  return s,n
 }
 reject:=func(in SyntheticReviewInput){
  t.Helper();before,count:=state()
  ok,e:=writer.ApplySynthetic(ctx,in)
  if ok||e==nil{t.Fatalf("unsafe action admitted: %+v",in)}
  after,next:=state();if before!=after||count!=next{t.Fatal("rejected action changed status or audit")}
 }
 bad:=input;bad.ActorUserID=outsider;reject(bad)
 bad=input;bad.Scope.InstallationID=otherInst;reject(bad)
 bad=input;bad.CaseID=foreignCase;reject(bad)
 bad=input;bad.ActionKey="invalid";reject(bad)
 bad=input;bad.Note="@everyone";reject(bad)
 bad=input;bad.At=time.Date(2000,1,1,0,0,0,0,time.UTC);reject(bad)
 bad=input;bad.ToStatus="RESOLVED";reject(bad)
 bad=input;bad.FixtureOnly=false;reject(bad)
 bad=input;bad.CallerCapabilityVerified=false;reject(bad)
 // Inject a failing status write after the audit INSERT, and require rollback.
 _,err=db.Pool.Exec(ctx,"CREATE FUNCTION block_case_review_fixture() RETURNS TRIGGER AS $$ BEGIN RAISE EXCEPTION 'fixture block'; END; $$ LANGUAGE plpgsql; CREATE TRIGGER block_case_review_fixture BEFORE UPDATE ON case_review_cases FOR EACH ROW EXECUTE FUNCTION block_case_review_fixture()")
 if err!=nil{t.Fatal(err)}
 reject(input)
 if _,err=db.Pool.Exec(ctx,"DROP TRIGGER block_case_review_fixture ON case_review_cases; DROP FUNCTION block_case_review_fixture()");err!=nil{t.Fatal(err)}
 ok,err:=writer.ApplySynthetic(ctx,input);if err!=nil||!ok{t.Fatalf("review: %v %v",ok,err)}
 s,n:=state();if s!="REVIEWED"||n!=1{t.Fatalf("missing atomic pair: %s %d",s,n)}
 ok,err=writer.ApplySynthetic(ctx,input);if err!=nil||ok{t.Fatalf("identical action replay: %v %v",ok,err)}
 bad=input;bad.Note="Changed note";reject(bad)
 bad=input;bad.ActionKey=b;reject(bad)
 end:=input;end.ActionKey=b;end.ExpectedStatus="REVIEWED";end.ToStatus="RESOLVED";end.ReasonCode="STAFF_CLOSED";end.At=at.Add(time.Second)
 ok,err=writer.ApplySynthetic(ctx,end);if err!=nil||!ok{t.Fatalf("resolve: %v %v",ok,err)}
 s,n=state();if s!="RESOLVED"||n!=2{t.Fatalf("missing resolution audit: %s %d",s,n)}
 // Two competing reviews must serialize on one scoped case. Only one
 // transition and one audit row may survive; no duplicated successful review.
 contested:=one("INSERT INTO case_review_cases(guild_id,server_id,installation_id,discord_guild_connection_id,detector_id,detector_version,evidence_fingerprint,source_quality_ref,status) VALUES($1,$2,$3,$4,'SYNTHETIC','0.0.0',$5,$6,'PENDING_REVIEW') RETURNING id",guild,srv,inst,conn,b,c)
 type result struct{ok bool;err error}
 results:=make(chan result,2)
 start:=make(chan struct{})
 var wg sync.WaitGroup
 for _,key:=range []string{b,c}{
  wg.Add(1)
  go func(actionKey string){
   defer wg.Done()
   <-start
   attempt:=input
   attempt.CaseID=contested
   attempt.ActionKey=actionKey
   attempt.At=at.Add(3*time.Second)
   success,e:=writer.ApplySynthetic(ctx,attempt)
   results<-result{success,e}
  }(key)
 }
 close(start);wg.Wait();close(results)
 successes,failures:=0,0
 for item:=range results{
  if item.ok && item.err==nil{successes++}else if !item.ok && item.err!=nil{failures++}else{
   t.Fatalf("ambiguous concurrent review outcome: %+v",item)
  }
 }
 var contestedState string
 var contestedAudit int
 if err=db.Pool.QueryRow(ctx,"SELECT status FROM case_review_cases WHERE id=$1",contested).Scan(&contestedState);err!=nil{t.Fatal(err)}
 if err=db.Pool.QueryRow(ctx,"SELECT COUNT(*) FROM case_review_audit WHERE case_id=$1",contested).Scan(&contestedAudit);err!=nil{t.Fatal(err)}
 if successes!=1||failures!=1||contestedState!="REVIEWED"||contestedAudit!=1{
  t.Fatalf("concurrent review broke atomicity: success=%d failed=%d status=%s audit=%d",successes,failures,contestedState,contestedAudit)
 }
 // Hold a different case row to make the review wait AFTER acquiring its
 // membership lock. A concurrent role revocation must not pass that lock.
 revocationCase:=one("INSERT INTO case_review_cases(guild_id,server_id,installation_id,discord_guild_connection_id,detector_id,detector_version,evidence_fingerprint,source_quality_ref,status) VALUES($1,$2,$3,$4,'SYNTHETIC','0.0.0',$5,$6,'PENDING_REVIEW') RETURNING id",guild,srv,inst,conn,c,c)
 blocking,err:=db.Pool.Begin(ctx);if err!=nil{t.Fatal(err)}
 if _,err=blocking.Exec(ctx,"SELECT id FROM case_review_cases WHERE id=$1 FOR UPDATE",revocationCase);err!=nil{_ = blocking.Rollback(ctx);t.Fatal(err)}
 attempt:=input;attempt.CaseID=revocationCase;attempt.ActionKey=c;attempt.At=at.Add(4*time.Second)
 reviewResult:=make(chan result,1)
 go func(){success,e:=writer.ApplySynthetic(ctx,attempt);reviewResult<-result{success,e}}()
 // NOWAIT detects the held membership lock without relying on a sleep to
 // infer that the reviewing transaction reached its authorization check.
 membershipLocked:=false
 deadline:=time.Now().Add(5*time.Second)
 for time.Now().Before(deadline){
  var ignored int64
  e:=db.Pool.QueryRow(ctx,"SELECT user_id FROM organization_members WHERE organization_id=$1 AND user_id=$2 FOR UPDATE NOWAIT",org,owner).Scan(&ignored)
  var pgErr *pgconn.PgError
  if errors.As(e,&pgErr)&&pgErr.Code=="55P03"{membershipLocked=true;break}
  if e!=nil{_ = blocking.Rollback(ctx);t.Fatalf("probe member lock: %v",e)}
  time.Sleep(20*time.Millisecond)
 }
 if !membershipLocked{_ = blocking.Rollback(ctx);t.Fatal("review did not retain membership lock while waiting for case")}
 revokeCtx,cancelRevoke:=context.WithTimeout(ctx,150*time.Millisecond)
 _,revocationErr:=db.Pool.Exec(revokeCtx,"UPDATE organization_members SET role='MEMBER' WHERE organization_id=$1 AND user_id=$2",org,owner)
 cancelRevoke()
 if revocationErr==nil{_ = blocking.Rollback(ctx);t.Fatal("role revocation committed through an active review lock")}
 if err=blocking.Commit(ctx);err!=nil{t.Fatal(err)}
 outcome:=<-reviewResult
 if !outcome.ok||outcome.err!=nil{t.Fatalf("review should commit before serialized revocation: %+v",outcome)}
 var reviewCount int
 if err=db.Pool.QueryRow(ctx,"SELECT COUNT(*) FROM case_review_audit WHERE case_id=$1",revocationCase).Scan(&reviewCount);err!=nil||reviewCount!=1{t.Fatalf("revocation race audit count=%d err=%v",reviewCount,err)}
 // Once revocation actually commits, a new action and an exact replay must
 // both fail closed. This also proves we did not merely trust fixture flags.
 if _,err=db.Pool.Exec(ctx,"UPDATE organization_members SET role='MEMBER' WHERE organization_id=$1 AND user_id=$2",org,owner);err!=nil{t.Fatal(err)}
 reject(attempt)
 if _,err=db.Pool.Exec(ctx,"UPDATE organization_members SET role='OWNER' WHERE organization_id=$1 AND user_id=$2",org,owner);err!=nil{t.Fatal(err)}
 if _,err=db.Pool.Exec(ctx,"DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2",org,owner);err!=nil{t.Fatal(err)}
 reject(input) // Even replay must recheck current membership.
}
