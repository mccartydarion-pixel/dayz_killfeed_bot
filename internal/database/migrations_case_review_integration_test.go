//go:build integration

package database

import (
 "context"
 "fmt"
 "strings"
 "testing"
 "time"
)

func TestCASEReviewSchemaIsInertAndScoped(t *testing.T) {
 ctx,cancel:=context.WithTimeout(context.Background(),3*time.Minute)
 defer cancel()
 db:=isolatedDB(t,ctx,fmt.Sprintf("mig_case_review_%d",time.Now().UnixNano()))
 if indexOfMigration("0063_case_review_outbox_skeleton")<0 {t.Fatal("review schema not registered")}
 if err:=db.Migrate(ctx);err!=nil {t.Fatal(err)}
 // A second migration must be a no-op, not a new write or duplicate index.
 if err:=db.Migrate(ctx);err!=nil {t.Fatalf("migration not idempotent: %v",err)}
 one:=func(sql string,args ...any)int64 {
  t.Helper();var id int64
  if err:=db.Pool.QueryRow(ctx,sql,args...).Scan(&id);err!=nil{t.Fatalf("%s: %v",sql,err)}
  return id
 }
 user:=one(`INSERT INTO app_users(discord_user_id,discord_username)
 VALUES('case-schema-user','fixture') RETURNING id`)
 org:=one(`INSERT INTO organizations(name,slug,owner_user_id)
 VALUES('C.A.S.E. schema fixture','case-schema',$1) RETURNING id`,user)
 guild:=one(`INSERT INTO guilds(discord_guild_id)
 VALUES('case-schema-guild') RETURNING id`)
 conn:=one(`INSERT INTO discord_guild_connections(organization_id,guild_id)
 VALUES($1,$2) RETURNING id`,org,guild)
 server:=one(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'qa-fixture','case-one','dayz','PLAYSTATION','ACTIVE',$2) RETURNING id`,guild,org)
 other:=one(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'qa-fixture','case-two','dayz','PLAYSTATION','ACTIVE',$2) RETURNING id`,guild,org)
 installation:=one(`INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id)
 VALUES($1,$2,$3) RETURNING id`,org,conn,server)
 foreignInstallation:=one(`INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id)
 VALUES($1,$2,$3) RETURNING id`,org,conn,other)
 hexA:=strings.Repeat("a",64)
 hexB:=strings.Repeat("b",64)
 evidence:=one(`INSERT INTO case_evidence_events(guild_id,server_id,source_id,source_end_offset,line_sha256,event_type)
 VALUES($1,$2,'fixture-adm',100,$3,'PLAYER_HIT') RETURNING id`,guild,server,hexA)
 otherEvidence:=one(`INSERT INTO case_evidence_events(guild_id,server_id,source_id,source_end_offset,line_sha256,event_type)
 VALUES($1,$2,'other-adm',100,$3,'PLAYER_HIT') RETURNING id`,guild,other,hexA)
 caseID:=one(`INSERT INTO case_review_cases(guild_id,server_id,installation_id,discord_guild_connection_id,
 detector_id,detector_version,evidence_fingerprint,source_quality_ref,status)
 VALUES($1,$2,$3,$4,'TEST-FIXTURE','0.0.0',$5,$6,'PENDING_REVIEW') RETURNING id`,
 guild,server,installation,conn,hexA,hexB)
 if _,err:=db.Pool.Exec(ctx,`INSERT INTO case_review_evidence(guild_id,server_id,installation_id,case_id,evidence_id)
 VALUES($1,$2,$3,$4,$5)`,guild,server,installation,caseID,evidence);err!=nil{t.Fatal(err)}
 shouldFail:=func(sql string,args ...any) {
  t.Helper()
  if _,err:=db.Pool.Exec(ctx,sql,args...);err==nil{t.Fatalf("unsafe scope/state was accepted: %s",sql)}
 }
 // A forged installation cannot bind to a foreign game server.
 shouldFail(`INSERT INTO case_review_cases(guild_id,server_id,installation_id,discord_guild_connection_id,
 detector_id,detector_version,evidence_fingerprint,source_quality_ref,status)
 VALUES($1,$2,$3,$4,'TEST-FIXTURE','0.0.0',$5,$6,'PENDING_REVIEW')`,
 guild,server,foreignInstallation,conn,hexB,hexA)
 // Evidence must match the case's guild AND game server.
 shouldFail(`INSERT INTO case_review_evidence(guild_id,server_id,installation_id,case_id,evidence_id)
 VALUES($1,$2,$3,$4,$5)`,guild,server,installation,caseID,otherEvidence)
 // A case's audit/outbox cannot be relabeled as belonging to a second installation.
 shouldFail(`INSERT INTO case_review_audit(guild_id,server_id,installation_id,case_id,
 action_key,actor_user_id,from_status,to_status,reason_code)
 VALUES($1,$2,$3,$4,$5,$6,'PENDING_REVIEW','DISMISSED','STAFF_REVIEW')`,
 guild,server,foreignInstallation,caseID,hexB,user)
 shouldFail(`INSERT INTO case_staff_outbox(guild_id,server_id,installation_id,case_id,event_version,delivery_key,status)
 VALUES($1,$2,$3,$4,3,$5,'PENDING')`,
 guild,server,foreignInstallation,caseID,hexB)
 // A second case under the same installation cannot reuse fingerprint identity.
 shouldFail(`INSERT INTO case_review_cases(guild_id,server_id,installation_id,discord_guild_connection_id,
 detector_id,detector_version,evidence_fingerprint,source_quality_ref,status)
 VALUES($1,$2,$3,$4,'TEST-FIXTURE','0.0.0',$5,$6,'PENDING_REVIEW')`,
 guild,server,installation,conn,hexA,hexB)
 if _,err:=db.Pool.Exec(ctx,`INSERT INTO case_review_audit(guild_id,server_id,installation_id,case_id,
 action_key,actor_user_id,from_status,to_status,reason_code,note)
 VALUES($1,$2,$3,$4,$5,$6,'PENDING_REVIEW','DISMISSED','STAFF_REVIEW','Fixture only')`,
 guild,server,installation,caseID,hexA,user);err!=nil{t.Fatal(err)}
 shouldFail(`INSERT INTO case_review_audit(guild_id,server_id,installation_id,case_id,
 action_key,actor_user_id,from_status,to_status,reason_code)
 VALUES($1,$2,$3,$4,$5,$6,'PENDING_REVIEW','DISMISSED','STAFF_REVIEW')`,
 guild,server,installation,caseID,hexA,user)
 if _,err:=db.Pool.Exec(ctx,`INSERT INTO case_staff_outbox(guild_id,server_id,installation_id,case_id,
 event_version,delivery_key,status)
 VALUES($1,$2,$3,$4,1,$5,'PENDING')`,guild,server,installation,caseID,hexA);err!=nil{t.Fatal(err)}
 shouldFail(`INSERT INTO case_staff_outbox(guild_id,server_id,installation_id,case_id,event_version,delivery_key,status)
 VALUES($1,$2,$3,$4,1,$5,'PENDING')`,guild,server,installation,caseID,hexA)
 shouldFail(`INSERT INTO case_staff_outbox(guild_id,server_id,installation_id,case_id,event_version,delivery_key,status)
 VALUES($1,$2,$3,$4,2,$5,'SENT')`,guild,server,installation,caseID,hexB)
 shouldFail(`INSERT INTO case_staff_outbox(guild_id,server_id,installation_id,case_id,event_version,delivery_key,status)
 VALUES($1,$2,$3,$4,2,$5,'LEASED')`,guild,server,installation,caseID,hexB)
 // Review history must not be silently edited, deleted, or removed by deleting its parent case.
 shouldFail(`UPDATE case_review_audit SET note='overwritten' WHERE case_id=$1`,caseID)
 shouldFail(`DELETE FROM case_review_audit WHERE case_id=$1`,caseID)
 shouldFail(`DELETE FROM case_review_cases WHERE id=$1`,caseID)
 // The evidence source itself cannot be removed while an active case links it.
 shouldFail(`DELETE FROM case_evidence_events WHERE id=$1`,evidence)
 var auditCount int
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_review_audit WHERE case_id=$1`,caseID).Scan(&auditCount);err!=nil{t.Fatal(err)}
 if auditCount!=1 {t.Fatalf("audit history changed: %d rows",auditCount)}
 // Migration itself created no live reviews or deliveries before fixtures.
 var cases,entries int
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_review_cases`).Scan(&cases);err!=nil{t.Fatal(err)}
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_staff_outbox`).Scan(&entries);err!=nil{t.Fatal(err)}
 if cases!=1||entries!=1{t.Fatalf("unexpected fixture rows: cases=%d outbox=%d",cases,entries)}
}
