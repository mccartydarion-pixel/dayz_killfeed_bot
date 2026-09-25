//go:build integration

package repository_test

import (
 "context"
 "errors"
 "fmt"
 "os"
 "strings"
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/database"
 "github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCASECheckoutAndWebhookTransaction(t *testing.T){
 url:=strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
 if url=="" {
  if os.Getenv("REQUIRE_INTEGRATION_DB")=="1"{t.Fatal("TEST_DATABASE_URL required")}
  t.Skip("disposable test database not available")
 }
 if os.Getenv("ALLOW_INTEGRATION_DB_TESTS")!="true"{t.Fatal("requires explicitly allowed integration database")}
 ctx,cancel:=context.WithTimeout(context.Background(),30*time.Second);defer cancel()
 db,err:=database.Connect(ctx,url);if err!=nil{t.Fatal(err)}
 defer db.Close()
 if err:=db.Migrate(ctx);err!=nil{t.Fatal(err)}
 marker:=time.Now().UnixNano()
 var user,org,other,guild,server,connection,installation int64
 q:=func(query string,args ...any)int64{
  t.Helper();var id int64
  if err:=db.Pool.QueryRow(ctx,query,args...).Scan(&id);err!=nil{t.Fatal(err)}
  return id
 }
 user=q(`INSERT INTO app_users(discord_user_id,discord_username) VALUES($1,'case-test') RETURNING id`,fmt.Sprintf("case-webhook-user-%d",marker))
 org=q(`INSERT INTO organizations(name,slug,owner_user_id) VALUES('Case Owner',$1,$2) RETURNING id`,fmt.Sprintf("case-webhook-org-%d",marker),user)
 other=q(`INSERT INTO organizations(name,slug,owner_user_id) VALUES('Other',$1,$2) RETURNING id`,fmt.Sprintf("case-webhook-other-%d",marker),user)
 guild=q(`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`,fmt.Sprintf("case-webhook-guild-%d",marker))
 server=q(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,guild,fmt.Sprintf("case-webhook-server-%d",marker),org)
 connection=q(`INSERT INTO discord_guild_connections(organization_id,guild_id) VALUES($1,$2) RETURNING id`,org,guild)
 installation=q(`INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id,status)
 VALUES($1,$2,$3,'READY') RETURNING id`,org,connection,server)
 if _,err:=db.Pool.Exec(ctx,`INSERT INTO subscriptions(organization_id,plan,status)
 VALUES($1,'LOW','ACTIVE')`,org);err!=nil{t.Fatal(err)}
 defer db.Pool.Exec(context.Background(),`DELETE FROM organizations WHERE id=$1`,other)
 // The add-on binds installation and game server with RESTRICT foreign keys;
 // clean it before fixture cleanup.
 repo:=repository.NewCaseAddonSubscriptionRepository(db.Pool)
 reservation,err:=repo.ReserveCaseCheckout(ctx,org,installation,server,"CASE_PRO","cus_case_test")
 if err!=nil{t.Fatal(err)}
 defer func(){
  db.Pool.Exec(context.Background(),`DELETE FROM case_addon_webhook_events WHERE addon_id=$1`,reservation.ID)
  db.Pool.Exec(context.Background(),`DELETE FROM case_addon_trial_grants WHERE addon_id=$1`,reservation.ID)
  db.Pool.Exec(context.Background(),`DELETE FROM case_addon_subscriptions WHERE id=$1`,reservation.ID)
  db.Pool.Exec(context.Background(),`DELETE FROM organizations WHERE id=$1`,org)
  db.Pool.Exec(context.Background(),`DELETE FROM app_users WHERE id=$1`,user)
  db.Pool.Exec(context.Background(),`DELETE FROM guilds WHERE id=$1`,guild)
 }()
 if _,err:=repo.ReserveCaseCheckout(ctx,other,installation,server,"CASE_PRO","cus_case_test");!errors.Is(err,repository.ErrCaseCheckoutConflict){
  t.Fatalf("cross-tenant reservation: %v",err)
 }
 retry,err:=repo.ReserveCaseCheckout(ctx,org,installation,server,"CASE_PRO","cus_case_test")
 if err!=nil || retry.ID!=reservation.ID {t.Fatalf("checkout retry created another reservation: %+v %v",retry,err)}
 if err:=repo.StoreCaseCheckout(ctx,reservation.ID,"cs_case_test","https://checkout.stripe.example/test");err!=nil{t.Fatal(err)}
 pending,err:=repo.GetPendingCaseCheckout(ctx,org,installation)
 if err!=nil || pending.Attempt!=1 || pending.SessionID!="cs_case_test"{
  t.Fatalf("pending checkout not correctly retrieved: %+v %v",pending,err)
 }
 if err:=repo.ResetExpiredCaseCheckout(ctx,other,installation,pending.ID,pending.Attempt,pending.SessionID);!errors.Is(err,repository.ErrCaseCheckoutConflict){
  t.Fatalf("other organization reset checkout: %v",err)
 }
 if err:=repo.ResetExpiredCaseCheckout(ctx,org,installation,pending.ID,pending.Attempt,pending.SessionID);err!=nil{t.Fatal(err)}
 if err:=repo.ResetExpiredCaseCheckout(ctx,org,installation,pending.ID,pending.Attempt,pending.SessionID);!errors.Is(err,repository.ErrCaseCheckoutConflict){
  t.Fatalf("stale Checkout attempt reset twice: %v",err)
 }
 pending,err=repo.ReserveCaseCheckout(ctx,org,installation,server,"CASE_PRO","cus_case_test")
 if err!=nil || pending.Attempt!=2 || pending.SessionID!=""{t.Fatalf("attempt not rotated: %+v %v",pending,err)}
 if err:=repo.StoreCaseCheckout(ctx,pending.ID,"cs_case_retry","https://checkout.stripe.example/test/retry");err!=nil{t.Fatal(err)}
 end:=time.Now().Add(time.Hour)
 in:=repository.CaseWebhookState{
  EventID:fmt.Sprintf("evt-case-%d",marker),EventType:"checkout.session.completed",
  AddonID:reservation.ID,OrganizationID:org,InstallationID:installation,GameServerID:server,
  Tier:"CASE_PRO",CustomerID:"cus_case_test",SubscriptionID:fmt.Sprintf("sub_case_%d",marker),
  PriceID:"price_case_pro",Status:"ACTIVE",CheckoutSessionID:"cs_case_retry",
  CurrentPeriodEnd:&end,
 }
 forged:=in;forged.EventID+="-forged";forged.OrganizationID=other
 if err:=repo.ApplyCaseWebhook(ctx,forged);!errors.Is(err,repository.ErrCaseWebhookMismatch) {
  t.Fatalf("foreign organization was allowed to reconcile: %v",err)
 }
 bad:=in;bad.EventID+="-bad";bad.CheckoutSessionID="cs_other"
 if err:=repo.ApplyCaseWebhook(ctx,bad);!errors.Is(err,repository.ErrCaseWebhookMismatch){
  t.Fatalf("different checkout session accepted: %v",err)
 }
 stale:=in;stale.EventID+="-old-session";stale.CheckoutSessionID="cs_case_test"
 if err:=repo.ApplyCaseWebhook(ctx,stale);!errors.Is(err,repository.ErrCaseWebhookMismatch){
  t.Fatalf("expired checkout callback accepted after recovery: %v",err)
 }
 if err:=repo.ApplyCaseWebhook(ctx,in);err!=nil{t.Fatal(err)}
 if err:=repo.ApplyCaseWebhook(ctx,in);err!=nil{t.Fatalf("duplicate event not idempotent: %v",err)}
 scoped,err:=repo.GetScoped(ctx,org,installation)
 if err!=nil || scoped==nil || scoped.Status!="ACTIVE" || scoped.ProviderSubscriptionID!=in.SubscriptionID {
  t.Fatalf("incorrect add-on reconciliation: %+v %v",scoped,err)
 }
 foreign,err:=repo.GetScoped(ctx,other,installation)
 if err!=nil || foreign!=nil {t.Fatalf("another tenant read add-on: %+v %v",foreign,err)}
 var n int
 if err:=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_addon_webhook_events WHERE addon_id=$1`,reservation.ID).Scan(&n);err!=nil{t.Fatal(err)}
 if n!=1{t.Fatalf("expected one committed initial event, got %d",n)}
 // ACTIVE after checkout is not proof of payment. The confirmed paid period
 // is granted only by a separate invoice.paid event and can never shrink.
 if scoped.PaidThrough!=nil{t.Fatal("checkout improperly marked invoice paid")}
 paid:=in
 paid.EventID=fmt.Sprintf("evt-case-invoice-%d",marker)
 paid.EventType="invoice.paid"
 paid.CheckoutSessionID=""
 paid.PaidThrough=&end
 if err:=repo.ApplyCaseWebhook(ctx,paid);err!=nil{t.Fatal(err)}
 paidRow,err:=repo.GetScoped(ctx,org,installation)
 if err!=nil || paidRow==nil || paidRow.PaidThrough==nil || !paidRow.PaidThrough.Equal(end.Truncate(time.Microsecond)){
  t.Fatalf("confirmed paid coverage not persisted: %+v %v",paidRow,err)
 }
 // Webhook delivery can be out of order: a failed invoice for the same
 // already-paid period must not overwrite a newer invoice.paid confirmation.
 staleFailure:=in
 staleFailure.EventID=fmt.Sprintf("evt-case-old-failure-%d",marker)
 staleFailure.EventType="invoice.payment_failed"
 staleFailure.CheckoutSessionID=""
 staleFailure.Status="PAST_DUE"
 staleFailure.FailedPeriodEnd=&end
 if err:=repo.ApplyCaseWebhook(ctx,staleFailure);err!=nil{t.Fatal(err)}
 afterFailure,err:=repo.GetScoped(ctx,org,installation)
 if err!=nil || afterFailure==nil || afterFailure.Status!="ACTIVE" ||
 afterFailure.PaidThrough==nil || !afterFailure.PaidThrough.Equal(end.Truncate(time.Microsecond)){
  t.Fatalf("out-of-order failed invoice revoked confirmed access: %+v %v",afterFailure,err)
 }
 // A genuinely newer unpaid period must still fail closed.
 newFailure:=staleFailure
 newFailure.EventID=fmt.Sprintf("evt-case-new-failure-%d",marker)
 newerEnd:=end.Add(30*24*time.Hour)
 newFailure.FailedPeriodEnd=&newerEnd
 if err:=repo.ApplyCaseWebhook(ctx,newFailure);err!=nil{t.Fatal(err)}
 afterFailure,err=repo.GetScoped(ctx,org,installation)
 if err!=nil || afterFailure==nil || afterFailure.Status!="PAST_DUE" {
  t.Fatalf("new unpaid cycle failed to revoke premium access: %+v %v",afterFailure,err)
 }
 // A later paid event recovers access without shrinking confirmed coverage.
 recovered:=paid
 recovered.EventID=fmt.Sprintf("evt-case-repaid-%d",marker)
 recovered.CurrentPeriodEnd=&newerEnd
 recovered.PaidThrough=&newerEnd
 if err:=repo.ApplyCaseWebhook(ctx,recovered);err!=nil{t.Fatal(err)}
 afterRecovery,err:=repo.GetScoped(ctx,org,installation)
 if err!=nil || afterRecovery==nil || afterRecovery.Status!="ACTIVE" ||
 afterRecovery.PaidThrough==nil || !afterRecovery.PaidThrough.Equal(newerEnd.Truncate(time.Microsecond)){
  t.Fatalf("repaid period did not restore premium access: %+v %v",afterRecovery,err)
 }
 listed,err:=repo.ListByOrganization(ctx,org)
 if err!=nil || len(listed)!=1 || listed[0].ID!=reservation.ID ||
 listed[0].PaidThrough==nil || !listed[0].PaidThrough.Equal(newerEnd.Truncate(time.Microsecond)) {
  t.Fatalf("scoped add-on listing lost paid coverage: %+v %v",listed,err)
 }
 if err:=repo.SaveCaseCancelFlag(ctx,other,installation,in.SubscriptionID,true);!errors.Is(err,repository.ErrCaseCheckoutConflict){
  t.Fatalf("cross-org cancellation accepted: %v",err)
 }
 if err:=repo.SaveCaseCancelFlag(ctx,org,installation,in.SubscriptionID,true);err!=nil{t.Fatal(err)}
 paidRow,err=repo.GetScoped(ctx,org,installation)
 if err!=nil || paidRow==nil || !paidRow.CancelAtPeriodEnd || paidRow.PaidThrough==nil{
  t.Fatalf("cancel state lost paid access: %+v %v",paidRow,err)
 }


 // Only a server-bound, explicit Pro trial claim grants trial access.
 trialStart:=time.Now().UTC().Truncate(time.Second)
 trialEnd:=trialStart.Add(7*24*time.Hour)
 trial:=in
 trial.EventID=fmt.Sprintf("evt-case-founder-%d",marker)
 trial.EventType="customer.subscription.updated"
 trial.CheckoutSessionID=""
 trial.Status="TRIAL"
 trial.TrialStart=&trialStart
 trial.TrialEnd=&trialEnd
 trial.CurrentPeriodEnd=&trialEnd
 trial.FounderTrialOffer=true
 if err:=repo.ApplyCaseWebhook(ctx,trial);err!=nil{t.Fatalf("first founder trial failed: %v",err)}
 if err:=repo.ApplyCaseWebhook(ctx,trial);err!=nil{t.Fatalf("replayed founder trial failed: %v",err)}
 trialRow,err:=repo.GetScoped(ctx,org,installation)
 if err!=nil || trialRow==nil || !trialRow.FounderTrialGranted || trialRow.TrialStartedAt==nil {
  t.Fatalf("one-time verified trial grant not exposed: %+v %v",trialRow,err)
 }
 // A different subscription must never consume a second grant on the same
 // game server, even if its installer/customer attempts to reuse that server.
 _,err=db.Pool.Exec(ctx,`INSERT INTO case_addon_trial_grants(
  organization_id,game_server_id,installation_id,addon_id,provider_subscription_id,
  tier,trial_started_at,trial_ends_at)
  VALUES($1,$2,$3,$4,$5,'CASE_PRO',$6,$7)`,
  org,server,installation,reservation.ID,trial.SubscriptionID+"-another",trialStart,trialEnd)
 if err==nil {t.Fatal("duplicate founder trial on same server was accepted")}
 // A trial longer than seven days cannot be stored, even with a new server.
 _,err=db.Pool.Exec(ctx,`UPDATE case_addon_trial_grants SET trial_ends_at=$2
  WHERE addon_id=$1`,reservation.ID,trialStart.Add(8*24*time.Hour))
 if err==nil{t.Fatal("trial duration greater than seven days was accepted")}
 var plan,status string
 if err:=db.Pool.QueryRow(ctx,`SELECT plan,status FROM subscriptions WHERE organization_id=$1`,org).Scan(&plan,&status);err!=nil{t.Fatal(err)}
 if plan!="LOW" || status!="ACTIVE"{t.Fatalf("case webhook changed base billing %s/%s",plan,status)}
}
