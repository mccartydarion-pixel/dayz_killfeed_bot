//go:build integration

package repository

import (
 "context"
 "fmt"
 "os"
 "strings"
 "testing"
 "time"

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
 at:=time.Date(2026,9,27,17,0,0,0,time.UTC)
 input:=SyntheticReviewInput{FixtureOnly:true,CallerCapabilityVerified:true,Scope:CaseReviewScope{GuildID:guild,ServerID:srv,InstallationID:inst},CaseID:caseID,ActorUserID:owner,ActionKey:a,ExpectedStatus:"PENDING_REVIEW",ToStatus:"REVIEWED",ReasonCode:"EVIDENCE_REVIEWED",Note:"Synthetic review",At:at}
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
 if _,err=db.Pool.Exec(ctx,"DELETE FROM organization_members WHERE organization_id=$1 AND user_id=$2",org,owner);err!=nil{t.Fatal(err)}
 reject(input) // Even replay must recheck current membership.
}
