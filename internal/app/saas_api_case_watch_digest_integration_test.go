//go:build integration

package app

import (
 "context"
 "fmt"
 "net/http"
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/routing"
 "github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCASEWatchDigestIsPaidScopedOptInAndRequiresStaffRoute(t *testing.T){
 w:=newClientAdminWorld(t)
 // Direct handler tests bypass registerSaaSAPI, which normally builds this limiter.
 w.a.caseWatchDigestLimiter=newSaaSRateLimiter(time.Hour,1)
 w.a.CaseDigestOutbox=repository.NewCaseDigestOutbox(w.a.DB.Pool)
 t.Cleanup(func(){_,err:=w.a.DB.Pool.Exec(context.Background(),"DELETE FROM case_watch_digest_outbox WHERE organization_id=$1",w.f.OrgID);if err!=nil{t.Errorf("cleanup case digest: %v",err)}})
 routePath:=w.path("/anti-cheat/premium/watch-digest")
 // None of the old observation/evidence routes requires a paid add-on.
 read:=w.call(w.a.handleAntiCheatOverview,http.MethodGet,w.path("/anti-cheat/overview"),w.f.OwnerDiscordID,nil,nil)
 if read.Code!=http.StatusOK{t.Fatalf("free observation changed: %d %s",read.Code,read.Body.String())}
 rr:=w.call(w.a.handleAntiCheatWatchDigest,http.MethodPost,routePath,w.f.OwnerDiscordID,nil,nil)
 if rr.Code!=http.StatusServiceUnavailable{t.Fatalf("missing billing must deny: %d %s",rr.Code,rr.Body.String())}
 seedCasePremiumAccess(t,w)
 w.a.caseWatchPrivacyCheck=func(_ context.Context,guild,channel string)error{
  if guild=="" || channel!="staff-test-channel"{return fmt.Errorf("channel not private")}
  return nil
 }
 rr=w.call(w.a.handleAntiCheatWatchDigest,http.MethodPost,routePath,w.f.OwnerDiscordID,nil,nil)
 if rr.Code!=http.StatusConflict {t.Fatalf("missing staff route accepted: %d %s",rr.Code,rr.Body.String())}
 if err:=w.a.SaaSChannelRoutes.UpsertRoute(context.Background(),w.f.OrgID,w.f.InstallationID,
  routing.RouteAdminAlerts,"staff-test-channel",false);err!=nil{t.Fatal(err)}
 // Seed source evidence from this selected server; no additional Nitrado polling.
 evidence:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
 in:=caseHitInput(w.guildID,w.serverID,100,"dayzps/config/digest.ADM",fmt.Sprintf("%064x",100))
 if err:=evidence.RecordCaseEvidence(context.Background(),in);err!=nil{t.Fatal(err)}
 rr=w.call(w.a.handleAntiCheatWatchDigest,http.MethodPost,routePath,w.f.OwnerDiscordID,nil,nil)
 if rr.Code!=http.StatusAccepted {t.Fatalf("paid request not queued: %d %s",rr.Code,rr.Body.String())}
 body:=decodeBody[map[string]any](t,rr)
 if body["queued"]!=true || body["mode"]!="OBSERVATION_ONLY"||body["delivery"]!="DURABLE_PENDING"||body["deliveryId"]==nil{
  t.Fatalf("unsafe delivery claim: %v",body)
 }
 rr=w.call(w.a.handleAntiCheatWatchDigest,http.MethodPost,routePath,w.f.OwnerDiscordID,nil,nil)
 if rr.Code!=http.StatusTooManyRequests{t.Fatalf("digest cooldown bypassed: %d %s",rr.Code,rr.Body.String())}
 // A payment failure revokes both API and queued-worker eligibility.
 if _,err:=w.a.DB.Pool.Exec(context.Background(),
   `UPDATE case_addon_subscriptions SET status='PAST_DUE' WHERE organization_id=$1 AND installation_id=$2`,
   w.f.OrgID,w.f.InstallationID);err!=nil{t.Fatal(err)}
 rr=w.call(w.a.handleAntiCheatWatchDigest,http.MethodPost,routePath,w.f.OwnerDiscordID,nil,nil)
 if rr.Code!=http.StatusForbidden{t.Fatalf("unpaid share accepted: %d",rr.Code)}
 allowed,err:=w.a.caseWorkerAllowed(context.Background(),w.f.OrgID,w.f.InstallationID,w.serverID,"case.watch")
 if err!=nil||allowed{t.Fatalf("queued worker retained paid access: %v %v",allowed,err)}
 // A user outside the installation never receives the billing state or sends.
 stranger:=syncUser(t,w.a,fmt.Sprintf("case-digest-foreign-%d",time.Now().UnixNano()),"Stranger")
 rr=w.call(w.a.handleAntiCheatWatchDigest,http.MethodPost,routePath,stranger.DiscordUserID,nil,nil)
 if rr.Code!=http.StatusForbidden{t.Fatalf("foreign staff actor sent digest: %d",rr.Code)}
}
