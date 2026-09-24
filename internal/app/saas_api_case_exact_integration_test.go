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

func TestCASEExactEvidenceReadScopedAndAudited(t *testing.T) {
 w:=newClientAdminWorld(t)
 repo:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
 ctx:=context.Background()
 source:="dayzps/config/exact-evidence.ADM"
 for _,offset:=range []int64{100,200,300} {
  x:=caseHitInput(w.guildID,w.serverID,offset,source,fmt.Sprintf("%064x",offset))
  if err:=repo.RecordCaseEvidence(ctx,x);err!=nil {t.Fatal(err)}
 }
 list,err:=repo.ListCaseEvidence(ctx,w.guildID,w.serverID,nil,nil,nil,50)
 if err!=nil||len(list)!=3{t.Fatalf("seed failed: %+v / %v",list,err)}
 wanted:=list[1].ID
 path:=fmt.Sprintf("%s?evidenceId=%d",w.path("/anti-cheat/evidence"),wanted)
 rr:=w.call(w.a.handleAntiCheatEvidence,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
 if rr.Code!=http.StatusOK{t.Fatalf("exact read failed %d: %s",rr.Code,rr.Body.String())}
 out:=decodeBody[caseEvidencePage](t,rr)
 if len(out.Items)!=1||out.Items[0].ID!=wanted||out.NextCursor!=nil||out.DetectorsEnabled||out.Enforcement!="DISABLED"{
  t.Fatalf("exact source read must return singleton and no paging/verdict: %+v",out)
 }
 var otherServer int64
 if err:=w.a.DB.Pool.QueryRow(ctx,`
 INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
 w.guildID,fmt.Sprintf("case-exact-other-%d",time.Now().UnixNano()),w.f.OrgID).Scan(&otherServer);err!=nil {t.Fatal(err)}
 foreign:=caseHitInput(w.guildID,otherServer,900,source,fmt.Sprintf("%064x",900))
 if err:=repo.RecordCaseEvidence(ctx,foreign);err!=nil{t.Fatal(err)}
 otherRows,err:=repo.ListCaseEvidence(ctx,w.guildID,otherServer,nil,nil,nil,20)
 if err!=nil||len(otherRows)!=1{t.Fatalf("foreign fixture failed %+v / %v",otherRows,err)}
 foreignPath:=fmt.Sprintf("%s?evidenceId=%d",w.path("/anti-cheat/evidence"),otherRows[0].ID)
 forbiddenRecord:=w.call(w.a.handleAntiCheatEvidence,http.MethodGet,foreignPath,w.f.OwnerDiscordID,nil,nil)
 if forbiddenRecord.Code!=http.StatusOK {t.Fatalf("scoped missing response: %d",forbiddenRecord.Code)}
 empty:=decodeBody[caseEvidencePage](t,forbiddenRecord)
 if len(empty.Items)!=0 {t.Fatal("foreign server evidence leaked")}
 for _,suffix:=range []string{"?evidenceId=0","?evidenceId=bad","?evidenceId=1&before=2"}{
  invalid:=w.call(w.a.handleAntiCheatEvidence,http.MethodGet,w.path("/anti-cheat/evidence")+suffix,w.f.OwnerDiscordID,nil,nil)
  if invalid.Code!=http.StatusBadRequest{t.Fatalf("invalid exact selector accepted %s: %d",suffix,invalid.Code)}
 }
 stranger:=syncUser(t,w.a,fmt.Sprintf("case-exact-stranger-%d",time.Now().UnixNano()),"Stranger")
 denied:=w.call(w.a.handleAntiCheatEvidence,http.MethodGet,path,stranger.DiscordUserID,nil,nil)
 if denied.Code!=http.StatusForbidden{t.Fatalf("unprivileged direct read: %d",denied.Code)}
}
