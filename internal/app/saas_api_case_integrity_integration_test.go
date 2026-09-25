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
func TestCASEIntegrityServerIsolationAndAuthorization(t *testing.T) {
 w:=newClientAdminWorld(t)
 repo:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
 in:=caseHitInput(w.guildID,w.serverID,500,"dayzps/config/integrity.ADM",fmt.Sprintf("%064x",500))
 if err:=repo.RecordCaseEvidence(context.Background(),in);err!=nil{t.Fatal(err)}
 path:=w.path("/anti-cheat/integrity")
 rr:=w.call(w.a.handleAntiCheatIntegrity,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
 if rr.Code!=http.StatusOK{t.Fatalf("owner read: %d %s",rr.Code,rr.Body.String())}
 out:=decodeBody[caseSourceIntegrity](t,rr)
 if out.ServerID!=w.serverID||out.EvidenceLines24h!=1||out.LatestEvidenceOffset==nil||*out.LatestEvidenceOffset!=500||
  out.LatestEvidenceSourceRef==nil||*out.LatestEvidenceSourceRef==in.SourceID||
  out.Continuity.CurrentSourceEvidenceStatus!="UNKNOWN"||len(out.Continuity.RecentSources)!=1||out.Continuity.RecentSources[0].RecordedLines!=1||
  out.DetectorsEnabled||out.ElapsedTimeTrusted||out.MovementDetectorStatus!="BLOCKED"{
  t.Fatalf("integrity scope/contract: %+v",out)
 }
 // One guild may contain more than one installation. Never aggregate both.
 var otherServer int64
 if err:=w.a.DB.Pool.QueryRow(context.Background(),`
 INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
 w.guildID,fmt.Sprintf("case-integrity-other-%d",time.Now().UnixNano()),w.f.OrgID).Scan(&otherServer);err!=nil{t.Fatal(err)}
 other:=caseHitInput(w.guildID,otherServer,600,"dayzps/config/other.ADM",fmt.Sprintf("%064x",600))
 if err:=repo.RecordCaseEvidence(context.Background(),other);err!=nil{t.Fatal(err)}
 rr=w.call(w.a.handleAntiCheatIntegrity,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
 out=decodeBody[caseSourceIntegrity](t,rr)
 if out.EvidenceLines24h!=1||*out.LatestEvidenceOffset!=500||len(out.Continuity.RecentSources)!=1||out.Continuity.RecentSources[0].RecordedLines!=1{t.Fatal("foreign server evidence leaked")}
 stranger:=syncUser(t,w.a,fmt.Sprintf("case-integrity-stranger-%d",time.Now().UnixNano()),"Stranger")
 denied:=w.call(w.a.handleAntiCheatIntegrity,http.MethodGet,path,stranger.DiscordUserID,nil,nil)
 if denied.Code!=http.StatusForbidden{t.Fatalf("unauthorized integrity read: %d",denied.Code)}
}
