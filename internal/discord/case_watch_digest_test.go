package discord

import (
 "context"
 "errors"
 "strings"
 "testing"
)

func watchDigestFixture(serverID int64) (AdminAlert,CaseWatchScope) {
 scope:=CaseWatchScope{OrganizationID:10,InstallationID:20,GameServerID:serverID}
 return AdminAlert{GuildRowID:7,ServerID:serverID,Kind:AlertKindCaseWatchDigest,
 Fields:[][2]string{{"Persisted source lines","5"},{"Coverage","Source observations only"}}},scope
}

// The existing ADMIN_ALERTS dispatcher must never trust the state at enqueue:
// payment or installation server can change while the message sits in queue.
func TestCaseWatchDiscordDispatcherRechecksPremiumAtSend(t *testing.T){
 p,sender,resolver,_:=newAlertFixture()
 resolver.set(7,30,routeKeyAdminAlerts,"staff-30")
 resolver.set(7,31,routeKeyAdminAlerts,"staff-31")
 msg,scope:=watchDigestFixture(30)
 if !p.QueueCaseWatchDigest(msg,scope){t.Fatal("valid digest not queued")}
 drain(p)
 if sender.total()!=0{t.Fatal("missing premium authorizer sent digest")}
 allowed:=false
 calls:=0
 p.SetCaseWatchAuthorizer(func(_ context.Context,got CaseWatchScope)(bool,error){
  calls++
  if got!=scope{t.Fatalf("wrong server scope: %+v",got)}
  return allowed,nil
 })
 if !p.QueueCaseWatchDigest(msg,scope){t.Fatal("queue failed")}
 drain(p)
 if sender.total()!=0||calls!=1{t.Fatalf("unpaid digest sent: %d %d",sender.total(),calls)}
 allowed=true
 if !p.QueueCaseWatchDigest(msg,scope){t.Fatal("queue failed")}
 drain(p)
 if sender.total()!=1||calls!=2{t.Fatalf("paid digest not sent exactly once: %d %d",sender.total(),calls)}
 embed:=sender.messages("staff-30")[0].embeds[0]
 if !strings.Contains(embed.Title,"OBSERVATION DIGEST")||
    !strings.Contains(embed.Description,"not cheat alerts")||
    strings.Contains(embed.Description,"confirmed cheat"){
  t.Fatalf("digest mislabeled: %+v",embed)
 }
 if len(embed.Fields)<2{t.Fatalf("missing source-count fields: %+v",embed)}
 allowed=false
 if !p.QueueCaseWatchDigest(msg,scope){t.Fatal("queue failed")}
 drain(p)
 if sender.total()!=1 {t.Fatal("revoked premium still published")}
 // A stolen or altered scope cannot be used for a different server.
 other,otherScope:=watchDigestFixture(31)
 if !p.QueueCaseWatchDigest(other,otherScope){t.Fatal("queue failed")}
 drain(p)
 if sender.total()!=1{t.Fatal("different server inherited premium")}
}

func TestCaseWatchDispatchFailClosedOnErrorMissingRouteAndForgedKind(t *testing.T){
 p,sender,resolver,_:=newAlertFixture()
 msg,scope:=watchDigestFixture(30)
 if p.QueueCaseWatchDigest(msg,CaseWatchScope{OrganizationID:10,InstallationID:20,GameServerID:31}){
   t.Fatal("mismatched queue scope accepted")
 }
 if p.QueueCaseWatchDigest(AdminAlert{GuildRowID:7,ServerID:30,Kind:AlertKindADMStale},scope){
   t.Fatal("ordinary alert accepted as paid digest")
 }
 p.SetCaseWatchAuthorizer(func(context.Context,CaseWatchScope)(bool,error){return true,nil})
 p.Publish(msg) // no server-authored paid scope
 drain(p)
 if sender.total()!=0{t.Fatal("generic Publish bypassed paid scope")}
 if !p.QueueCaseWatchDigest(msg,scope){t.Fatal("queue failed")}
 drain(p)
 if sender.total()!=0{t.Fatal("missing route must fail closed")}
 resolver.set(7,30,routeKeyAdminAlerts,"staff-30")
 p.SetCaseWatchAuthorizer(func(context.Context,CaseWatchScope)(bool,error){return false,errors.New("database unavailable")})
 if !p.QueueCaseWatchDigest(msg,scope){t.Fatal("queue failed")}
 drain(p)
 if sender.total()!=0{t.Fatal("billing error must fail closed")}
 p.Publish(AdminAlert{GuildRowID:7,ServerID:30,Kind:AlertKindADMStale,
  Headline:"ADM STALE",CaseWatch:&scope})
 drain(p)
 if sender.total()!=0{t.Fatal("paid scope on free alert bypassed dispatcher")}
 p.Publish(AdminAlert{GuildRowID:7,ServerID:30,Kind:AlertKindADMStale,Headline:"ADM STALE"})
 drain(p)
 if sender.total()!=1{t.Fatal("free ADM alerts must remain unaffected")}
}
