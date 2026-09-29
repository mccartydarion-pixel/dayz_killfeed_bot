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
 // Audit history is read only through the exact installation and a
 // currently authorized OWNER/ADMIN in the same bounded query.
 auditRead:=SyntheticAuthorizedAuditRead{FixtureOnly:true,Scope:input.Scope,CaseID:caseID,ActorUserID:owner,Limit:1}
 history,historyErr:=reader.ListAuthorizedAuditSynthetic(ctx,auditRead)
 if historyErr!=nil||len(history)!=1||history[0].FromStatus!="PENDING_REVIEW"||
  history[0].ToStatus!="REVIEWED"||history[0].ReasonCode!="EVIDENCE_REVIEWED"{
  t.Fatalf("authorized audit history: %+v %v",history,historyErr)
 }
 auditCursor:=history[0].ID
 auditRead.Before=&auditCursor
 history,historyErr=reader.ListAuthorizedAuditSynthetic(ctx,auditRead)
 if historyErr!=nil||len(history)!=0{t.Fatalf("audit cursor escaped scope: %+v %v",history,historyErr)}
 auditRead.Before=nil
 auditRead.ActorUserID=outsider
 history,historyErr=reader.ListAuthorizedAuditSynthetic(ctx,auditRead)
 if historyErr!=nil||len(history)!=0{t.Fatalf("outsider saw audit history: %+v %v",history,historyErr)}
 auditRead.ActorUserID=owner
 auditRead.Scope.InstallationID=otherInst
 auditRead.Scope.ServerID=otherSrv
 history,historyErr=reader.ListAuthorizedAuditSynthetic(ctx,auditRead)
 if historyErr!=nil||len(history)!=0{t.Fatalf("foreign installation saw audit history: %+v %v",history,historyErr)}
 auditRead.Scope=input.Scope
 auditRead.FixtureOnly=false
 if _,historyErr=reader.ListAuthorizedAuditSynthetic(ctx,auditRead);!errors.Is(historyErr,ErrCASEReviewFixtureDisabled){t.Fatalf("audit fixture gate bypass: %v",historyErr)}
 auditRead.FixtureOnly=true
 auditRead.Limit=51
 if _,historyErr=reader.ListAuthorizedAuditSynthetic(ctx,auditRead);historyErr==nil{t.Fatal("unbounded audit history accepted")}
 auditRead.Limit=1
 if _,err=db.Pool.Exec(ctx,"UPDATE organization_members SET role='MEMBER' WHERE organization_id=$1 AND user_id=$2",org,owner);err!=nil{t.Fatal(err)}
 history,historyErr=reader.ListAuthorizedAuditSynthetic(ctx,auditRead)
 if historyErr!=nil||len(history)!=0{t.Fatalf("revoked role saw audit history: %+v %v",history,historyErr)}
 if _,err=db.Pool.Exec(ctx,"UPDATE organization_members SET role='OWNER' WHERE organization_id=$1 AND user_id=$2",org,owner);err!=nil{t.Fatal(err)}
 bad=input;bad.Note="Changed note";reject(bad)
 bad=input;bad.ActionKey=b;reject(bad)
 // Due inspection is a diagnostic only: no evidence link, no due entry.
 inspector:=NewCASEOutboxInspector(db.Pool)
 pendingKey:=strings.Repeat("d",64)
 outboxID:=one("INSERT INTO case_staff_outbox(guild_id,server_id,installation_id,case_id,event_version,delivery_key,status,next_attempt_at) VALUES($1,$2,$3,$4,1,$5,'PENDING',$6) RETURNING id",guild,srv,inst,caseID,pendingKey,at)
 inspect:=SyntheticDueInspection{FixtureOnly:true,Scope:input.Scope,At:at.Add(10*time.Second),Limit:10}
 due,inspectErr:=inspector.InspectDueSynthetic(ctx,inspect)
 if inspectErr!=nil||len(due)!=0{t.Fatalf("no-evidence item exposed: %+v %v",due,inspectErr)}
 evidenceID:=one("INSERT INTO case_evidence_events(guild_id,server_id,source_id,source_end_offset,line_sha256,event_type) VALUES($1,$2,'synthetic-outbox-fixture',123,$3,'PLAYER_HIT') RETURNING id",guild,srv,a)
 if _,err=db.Pool.Exec(ctx,"INSERT INTO case_review_evidence(guild_id,server_id,installation_id,case_id,evidence_id) VALUES($1,$2,$3,$4,$5)",guild,srv,inst,caseID,evidenceID);err!=nil{t.Fatal(err)}
 due,inspectErr=inspector.InspectDueSynthetic(ctx,inspect)
 if inspectErr!=nil||len(due)!=1||due[0].ID!=outboxID||due[0].DeliveryKey!=pendingKey||due[0].CaseID!=caseID{
  t.Fatalf("exact scoped fixture due: %+v %v",due,inspectErr)
 }
 // A direct status-only write is not an audited staff review. Even with
 // linked evidence and a pending outbox row it must stay out of diagnostics.
 statusOnly:=one("INSERT INTO case_review_cases(guild_id,server_id,installation_id,discord_guild_connection_id,detector_id,detector_version,evidence_fingerprint,source_quality_ref,status) VALUES($1,$2,$3,$4,'SYNTHETIC','0.0.0',$5,$6,'REVIEWED') RETURNING id",guild,srv,inst,conn,strings.Repeat("d",64),c)
 if _,err=db.Pool.Exec(ctx,"INSERT INTO case_review_evidence(guild_id,server_id,installation_id,case_id,evidence_id) VALUES($1,$2,$3,$4,$5)",guild,srv,inst,statusOnly,evidenceID);err!=nil{t.Fatal(err)}
 one("INSERT INTO case_staff_outbox(guild_id,server_id,installation_id,case_id,event_version,delivery_key,status,next_attempt_at) VALUES($1,$2,$3,$4,1,$5,'PENDING',$6) RETURNING id",guild,srv,inst,statusOnly,strings.Repeat("e",64),at)
 due,inspectErr=inspector.InspectDueSynthetic(ctx,inspect)
 if inspectErr!=nil||len(due)!=1||due[0].ID!=outboxID{
  t.Fatalf("status-only review entered due diagnostic: %+v %v",due,inspectErr)
 }
 inspect.Scope.InstallationID=otherInst
 due,inspectErr=inspector.InspectDueSynthetic(ctx,inspect)
 if inspectErr!=nil||len(due)!=0{t.Fatalf("foreign outbox scope leaked: %+v %v",due,inspectErr)}
 inspect.Scope=input.Scope
 inspect.At=at.Add(-time.Second)
 due,inspectErr=inspector.InspectDueSynthetic(ctx,inspect)
 if inspectErr!=nil||len(due)!=0{t.Fatalf("early outbox item claimed due: %+v %v",due,inspectErr)}
 inspect.At=at.Add(10*time.Second)
 inspect.FixtureOnly=false
 if _,inspectErr=inspector.InspectDueSynthetic(ctx,inspect);!errors.Is(inspectErr,ErrCASEReviewFixtureDisabled){t.Fatalf("outbox fixture gate bypass: %v",inspectErr)}
 inspect.FixtureOnly=true
 inspect.Limit=51
 if _,inspectErr=inspector.InspectDueSynthetic(ctx,inspect);inspectErr==nil{t.Fatal("unbounded outbox inspection accepted")}
 inspect.Limit=10
 end:=input;end.ActionKey=b;end.ExpectedStatus="REVIEWED";end.ToStatus="RESOLVED";end.ReasonCode="STAFF_CLOSED";end.At=at.Add(time.Second)
 ok,err=writer.ApplySynthetic(ctx,end);if err!=nil||!ok{t.Fatalf("resolve: %v %v",ok,err)}
 s,n=state();if s!="RESOLVED"||n!=2{t.Fatalf("missing resolution audit: %s %d",s,n)}
 due,inspectErr=inspector.InspectDueSynthetic(ctx,inspect)
 if inspectErr!=nil||len(due)!=0{t.Fatalf("resolved case still appears due: %+v %v",due,inspectErr)}
 // The lease repository is fixture-only and never sends. A status-only
 // REVIEWED row above must not qualify, even with evidence and a due outbox.
 leaseCase:=one("INSERT INTO case_review_cases(guild_id,server_id,installation_id,discord_guild_connection_id,detector_id,detector_version,evidence_fingerprint,source_quality_ref,status) VALUES($1,$2,$3,$4,'SYNTHETIC','0.0.0',$5,$6,'PENDING_REVIEW') RETURNING id",guild,srv,inst,conn,strings.Repeat("f",64),c)
 if _,err=db.Pool.Exec(ctx,"INSERT INTO case_review_evidence(guild_id,server_id,installation_id,case_id,evidence_id) VALUES($1,$2,$3,$4,$5)",guild,srv,inst,leaseCase,evidenceID);err!=nil{t.Fatal(err)}
 leaseReview:=input;leaseReview.CaseID=leaseCase;leaseReview.ActionKey=strings.Repeat("d",64);leaseReview.At=at.Add(2*time.Second)
 ok,err=writer.ApplySynthetic(ctx,leaseReview)
 if err!=nil||!ok{t.Fatalf("lease fixture review: %v %v",ok,err)}
 leaseID:=one("INSERT INTO case_staff_outbox(guild_id,server_id,installation_id,case_id,event_version,delivery_key,status,next_attempt_at) VALUES($1,$2,$3,$4,1,$5,'PENDING',$6) RETURNING id",guild,srv,inst,leaseCase,strings.Repeat("f",64),at)
 claimer:=NewCASEOutboxLeaseRepository(db.Pool)
 claim:=SyntheticOutboxClaim{FixtureOnly:true,Scope:input.Scope,At:at.Add(10*time.Second),LeaseFor:30*time.Second}
 if _,taken,e:=claimer.ClaimDueSynthetic(ctx,SyntheticOutboxClaim{Scope:input.Scope,At:claim.At,LeaseFor:claim.LeaseFor});taken||!errors.Is(e,ErrCASEReviewFixtureDisabled){t.Fatalf("fixture claim gate bypass: %v",e)}
 early:=claim;early.At=at.Add(-time.Second)
 if _,taken,e:=claimer.ClaimDueSynthetic(ctx,early);e!=nil||taken{t.Fatalf("early fixture claimed: %v %v",taken,e)}
 foreignClaim:=claim;foreignClaim.Scope.InstallationID=otherInst
 if _,taken,e:=claimer.ClaimDueSynthetic(ctx,foreignClaim);e!=nil||taken{t.Fatalf("foreign installation claimed: %v %v",taken,e)}
 // A concurrent staff transition owns the case row. Claim must skip it
 // instead of leasing a row whose REVIEWED state may be changing.
 lockedCase,lockErr:=db.Pool.Begin(ctx)
 if lockErr!=nil{t.Fatal(lockErr)}
 if _,lockErr=lockedCase.Exec(ctx,"SELECT id FROM case_review_cases WHERE id=$1 FOR UPDATE",leaseCase);lockErr!=nil{
  _=lockedCase.Rollback(ctx);t.Fatal(lockErr)
 }
 if _,taken,e:=claimer.ClaimDueSynthetic(ctx,claim);e!=nil||taken{
  _=lockedCase.Rollback(ctx);t.Fatalf("claim raced case transition: %v %v",taken,e)
 }
 if lockErr=lockedCase.Rollback(ctx);lockErr!=nil{t.Fatal(lockErr)}
 type leaseResult struct{item SyntheticOutboxLease;taken bool;err error}
 leases:=make(chan leaseResult,2)
 var claimWG sync.WaitGroup
 startClaims:=make(chan struct{})
 for i:=0;i<2;i++{
  claimWG.Add(1)
  go func(){defer claimWG.Done();<-startClaims;item,taken,e:=claimer.ClaimDueSynthetic(ctx,claim);leases<-leaseResult{item,taken,e}}()
 }
 close(startClaims);claimWG.Wait();close(leases)
 claimed:=0
 var winning SyntheticOutboxLease
 for result:=range leases{
  if result.err!=nil{t.Fatalf("concurrent fixture claim error: %v",result.err)}
  if result.taken{claimed++;winning=result.item}
 }
 if claimed!=1||winning.ID!=leaseID||winning.CaseID!=leaseCase||
  winning.Attempts!=1||len(winning.LeaseToken)!=64||!winning.LeaseUntil.Equal(claim.At.Add(claim.LeaseFor)){
  t.Fatalf("atomic fixture claim failed: count=%d lease=%+v",claimed,winning)
 }
 var leaseStatus,storedToken string
 var attempts int
 if err=db.Pool.QueryRow(ctx,"SELECT status,attempts,lease_token FROM case_staff_outbox WHERE id=$1",leaseID).Scan(&leaseStatus,&attempts,&storedToken);err!=nil||leaseStatus!="LEASED"||attempts!=1||storedToken!=winning.LeaseToken{
  t.Fatalf("lease persistence mismatch: %s %d %v",leaseStatus,attempts,err)
 }
 if _,taken,e:=claimer.ClaimDueSynthetic(ctx,claim);e!=nil||taken{t.Fatalf("leased/status-only row claimed again: %v %v",taken,e)}
 // A lease is not permission to send after staff resolves the case. The
 // exact scoped token can be suppressed only after an audited resolution.
 suppression:=SyntheticResolvedLeaseSuppression{FixtureOnly:true,Scope:input.Scope,
  OutboxID:leaseID,LeaseToken:winning.LeaseToken,At:at.Add(12*time.Second)}
 if changed,e:=claimer.SuppressResolvedSynthetic(ctx,suppression);e!=nil||changed{
  t.Fatalf("unresolved fixture lease suppressed: %v %v",changed,e)
 }
 wrong:=suppression;wrong.LeaseToken=strings.Repeat("a",64)
 if changed,e:=claimer.SuppressResolvedSynthetic(ctx,wrong);e!=nil||changed{
  t.Fatalf("stale token suppressed fixture lease: %v %v",changed,e)
 }
 wrong=suppression;wrong.Scope.InstallationID=otherInst
 if changed,e:=claimer.SuppressResolvedSynthetic(ctx,wrong);e!=nil||changed{
  t.Fatalf("foreign installation suppressed fixture lease: %v %v",changed,e)
 }
 wrong=suppression;wrong.FixtureOnly=false
 if changed,e:=claimer.SuppressResolvedSynthetic(ctx,wrong);changed||!errors.Is(e,ErrCASEReviewFixtureDisabled){
  t.Fatalf("suppression fixture gate bypass: %v %v",changed,e)
 }
 resolvedLease:=leaseReview
 resolvedLease.ActionKey=strings.Repeat("e",64)
 resolvedLease.ExpectedStatus="REVIEWED"
 resolvedLease.ToStatus="RESOLVED"
 resolvedLease.ReasonCode="STAFF_CLOSED"
 resolvedLease.At=at.Add(11*time.Second)
 if changed,e:=writer.ApplySynthetic(ctx,resolvedLease);e!=nil||!changed{
  t.Fatalf("resolve leased fixture case: %v %v",changed,e)
 }
 if changed,e:=claimer.SuppressResolvedSynthetic(ctx,suppression);e!=nil||!changed{
  t.Fatalf("audited fixture suppression: %v %v",changed,e)
 }
 if changed,e:=claimer.SuppressResolvedSynthetic(ctx,suppression);e!=nil||changed{
  t.Fatalf("fixture suppression replay: %v %v",changed,e)
 }
 var finalStatus,finalToken string
 var leaseCleared bool
 if e:=db.Pool.QueryRow(ctx,`SELECT status,COALESCE(lease_token,''),lease_until IS NULL
  FROM case_staff_outbox WHERE id=$1`,leaseID).Scan(&finalStatus,&finalToken,&leaseCleared);e!=nil||
  finalStatus!="SUPPRESSED"||finalToken!=""||!leaseCleared{
  t.Fatalf("fixture lease not cleared: %s %q %v %v",finalStatus,finalToken,leaseCleared,e)
 }
 if _,taken,e:=claimer.ClaimDueSynthetic(ctx,claim);e!=nil||taken{
  t.Fatalf("suppressed fixture reclaimed: %v %v",taken,e)
 }
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
