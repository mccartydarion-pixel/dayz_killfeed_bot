//go:build integration

package app

import (
 "context"
 "fmt"
 "net/http"
 "strings"
 "testing"
 "time"
)

func TestCASEEvidenceRefsExactScopeAndRevocation(t *testing.T){
 w:=newClientAdminWorld(t)
 ctx:=context.Background()
 ownerID:=mustAppUserID(t,w.a,w.f.OwnerDiscordID)
 addCase:=func(serverID,installationID int64,fp string)int64{
  t.Helper();var id int64
  err:=w.a.DB.Pool.QueryRow(ctx,`INSERT INTO case_review_cases
  (guild_id,server_id,installation_id,discord_guild_connection_id,
   detector_id,detector_version,evidence_fingerprint,source_quality_ref,status)
  VALUES($1,$2,$3,$4,'FIXTURE','0.0.0',$5,$6,'PENDING_REVIEW') RETURNING id`,
  w.guildID,serverID,installationID,w.f.ConnectionID,fp,strings.Repeat("f",64)).Scan(&id)
  if err!=nil{t.Fatal(err)};return id
 }
 addEvidence:=func(serverID,offset int64)int64{
  t.Helper();var id int64
  err:=w.a.DB.Pool.QueryRow(ctx,`INSERT INTO case_evidence_events
  (guild_id,server_id,source_id,source_end_offset,line_sha256,event_type)
  VALUES($1,$2,'source-fixture',$3,$4,'PLAYER_HIT') RETURNING id`,
  w.guildID,serverID,offset,strings.Repeat("a",64)).Scan(&id)
  if err!=nil{t.Fatal(err)};return id
 }
 link:=func(serverID,inst,caseID,evidenceID int64){
  t.Helper()
  _,err:=w.a.DB.Pool.Exec(ctx,`INSERT INTO case_review_evidence
  (guild_id,server_id,installation_id,case_id,evidence_id)
  VALUES($1,$2,$3,$4,$5)`,w.guildID,serverID,inst,caseID,evidenceID)
  if err!=nil{t.Fatal(err)}
 }
 caseID:=addCase(w.serverID,w.f.InstallationID,strings.Repeat("b",64))
 first:=addEvidence(w.serverID,100);second:=addEvidence(w.serverID,200)
 link(w.serverID,w.f.InstallationID,caseID,first)
 link(w.serverID,w.f.InstallationID,caseID,second)
 var otherServer,otherInstallation int64
 if err:=w.a.DB.Pool.QueryRow(ctx,`INSERT INTO game_servers
  (guild_id,provider,provider_service_id,game,platform,status,organization_id)
  VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
  w.guildID,fmt.Sprintf("case-evidence-other-%d",time.Now().UnixNano()),w.f.OrgID).Scan(&otherServer);err!=nil{t.Fatal(err)}
 if err:=w.a.DB.Pool.QueryRow(ctx,`INSERT INTO installations
  (organization_id,discord_guild_connection_id,game_server_id)
  VALUES($1,$2,$3) RETURNING id`,w.f.OrgID,w.f.ConnectionID,otherServer).Scan(&otherInstallation);err!=nil{t.Fatal(err)}
 foreignCase:=addCase(otherServer,otherInstallation,strings.Repeat("c",64))
 foreignEvidence:=addEvidence(otherServer,300)
 link(otherServer,otherInstallation,foreignCase,foreignEvidence)
 path:=w.path(fmt.Sprintf("/anti-cheat/cases/%d/evidence-refs",caseID))
 call:=func(url,actor,id string)(int,caseEvidenceRefsPage,string){
  t.Helper()
  rr:=w.call(w.a.handleAntiCheatCaseEvidenceRefs,http.MethodGet,url,actor,nil,map[string]string{"caseID":id})
  if rr.Code!=http.StatusOK{return rr.Code,caseEvidenceRefsPage{},rr.Body.String()}
  return rr.Code,decodeBody[caseEvidenceRefsPage](t,rr),rr.Body.String()
 }
 code,page,body:=call(path+"?limit=1",w.f.OwnerDiscordID,fmt.Sprint(caseID))
 if code!=http.StatusOK||page.Mode!="AUTHORIZED_EVIDENCE_REFERENCES_ONLY"||
  page.CaseID!=caseID||page.ServerID!=w.serverID||len(page.EvidenceIDs)!=1||
  page.EvidenceIDs[0]!=second||page.NextCursor==nil||page.DetectorsEnabled||
  page.AlertsEnabled||page.Enforcement!="DISABLED"||strings.Contains(body,"source-fixture"){
  t.Fatalf("unsafe evidence page: %d %+v %s",code,page,body)
 }
 code,next,_:=call(path+"?limit=1&before="+*page.NextCursor,w.f.OwnerDiscordID,fmt.Sprint(caseID))
 if code!=http.StatusOK||len(next.EvidenceIDs)!=1||next.EvidenceIDs[0]!=first{
  t.Fatalf("bad evidence cursor: %d %+v",code,next)
 }
 code,foreign,_:=call(w.path(fmt.Sprintf("/anti-cheat/cases/%d/evidence-refs",foreignCase)),
  w.f.OwnerDiscordID,fmt.Sprint(foreignCase))
 if code!=http.StatusOK||len(foreign.EvidenceIDs)!=0{t.Fatalf("foreign case exposed: %d %+v",code,foreign)}
 for _,bad:=range []struct{url,id string}{
  {path,"0"},{path,"invalid"},{path+"?before=0",fmt.Sprint(caseID)},
  {path+"?limit=51",fmt.Sprint(caseID)},
 }{
  c,_,_:=call(bad.url,w.f.OwnerDiscordID,bad.id)
  if c!=http.StatusBadRequest{t.Fatalf("invalid request accepted: %+v %d",bad,c)}
 }
 stranger:=syncUser(t,w.a,fmt.Sprintf("case-evidence-outsider-%d",time.Now().UnixNano()),"Outsider")
 c,_,_:=call(path,stranger.DiscordUserID,fmt.Sprint(caseID))
 if c!=http.StatusForbidden{t.Fatalf("outsider read: %d",c)}
 if _,err:=w.a.DB.Pool.Exec(ctx,`UPDATE organization_members SET role='MEMBER'
 WHERE organization_id=$1 AND user_id=$2`,w.f.OrgID,ownerID);err!=nil{t.Fatal(err)}
 c,revoked,_:=call(path,w.f.OwnerDiscordID,fmt.Sprint(caseID))
 if c==http.StatusOK&&len(revoked.EvidenceIDs)!=0{t.Fatalf("revoked actor saw evidence refs: %+v",revoked)}
 if c!=http.StatusOK&&c!=http.StatusForbidden{t.Fatalf("unexpected revoked status: %d",c)}
}
