//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Seed the real isolated repository with an observed, paid C.A.S.E. add-on.
// No live Stripe, Nitrado, Discord or server restart is involved.
func seedCasePremiumAccess(t *testing.T,w *clientAdminWorld) (*repository.CaseAddonSubscriptionRepository,time.Time) {
	t.Helper()
	ctx:=context.Background()
	end:=time.Now().UTC().Add(48*time.Hour).Truncate(time.Second)
	_,err:=w.a.DB.Pool.Exec(ctx,`UPDATE subscriptions
	SET plan='LOW', status='ACTIVE', provider='stripe',
	provider_customer_id=$2, provider_subscription_id=$3, provider_price_id='price_base',
	current_period_end=$4 WHERE organization_id=$1`,
	w.f.OrgID,fmt.Sprintf("cus-case-%d",w.f.OrgID),fmt.Sprintf("sub-base-%d",w.f.OrgID),end)
	if err!=nil{t.Fatal(err)}
	repo:=repository.NewCaseAddonSubscriptionRepository(w.a.DB.Pool)
	customer:=fmt.Sprintf("cus-case-%d",w.f.OrgID)
	res,err:=repo.ReserveCaseCheckout(ctx,w.f.OrgID,w.f.InstallationID,w.serverID,"CASE_PRO",customer)
	if err!=nil{t.Fatal(err)}
	if err=repo.StoreCaseCheckout(ctx,res.ID,fmt.Sprintf("cs-case-%d",res.ID),"https://checkout.stripe.example/test");err!=nil{t.Fatal(err)}
	ev:=repository.CaseWebhookState{
		EventID:fmt.Sprintf("evt-case-premium-%d",res.ID),EventType:"invoice.paid",
		AddonID:res.ID,OrganizationID:w.f.OrgID,InstallationID:w.f.InstallationID,
		GameServerID:w.serverID,Tier:"CASE_PRO",CustomerID:customer,
		SubscriptionID:fmt.Sprintf("sub-case-%d",res.ID),
		PriceID:"price_case_pro",Status:"ACTIVE",CurrentPeriodEnd:&end,PaidThrough:&end,
	}
	if err=repo.ApplyCaseWebhook(ctx,ev);err!=nil{t.Fatal(err)}
	service:=billing.NewService(w.a.SaaSSubscriptions,nil,nil,billing.Options{})
	if err=service.ConfigureCaseAddons(repo,billing.CaseOptions{
		Enabled:false,AccessEnabled:true,VerifiedThrough:casebilling.Pro,
	});err!=nil{t.Fatal(err)}
	w.a.Billing=service
	return repo,end
}

func TestCASEPremiumAPIRequiresExactServerAndConfirmedPayment(t *testing.T){
	w:=newClientAdminWorld(t)
	path:=w.path("/anti-cheat/premium/evidence-export")
	// The permission/auth check precedes billing and the query.
	anonymous:=syncUser(t,w.a,fmt.Sprintf("case-premium-stranger-%d",time.Now().UnixNano()),"Stranger")
	rr:=w.call(w.a.handleAntiCheatPremiumExport,http.MethodGet,path,anonymous.DiscordUserID,nil,nil)
	if rr.Code!=http.StatusForbidden{t.Fatalf("foreign actor bypassed permission: %d",rr.Code)}
	rr=w.call(w.a.handleAntiCheatPremiumExport,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusServiceUnavailable{t.Fatalf("missing billing service: %d %s",rr.Code,rr.Body.String())}
	_,end:=seedCasePremiumAccess(t,w)
	workerOK,workerErr:=w.a.caseWorkerAllowed(context.Background(),w.f.OrgID,w.f.InstallationID,w.serverID,casebilling.CapPro)
	if workerErr!=nil || !workerOK{t.Fatalf("worker gate denied paid server: %v %v",workerOK,workerErr)}
	workerOK,workerErr=w.a.caseWorkerAllowed(context.Background(),w.f.OrgID,w.f.InstallationID,w.serverID+1,casebilling.CapPro)
	if workerErr!=nil || workerOK{t.Fatalf("worker inherited another server\u0027s purchase: %v %v",workerOK,workerErr)}
	rr=w.call(w.a.handleAntiCheatPremiumExport,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusOK{t.Fatalf("paid Pro denied: %d %s",rr.Code,rr.Body.String())}
	view:=decodeBody[map[string]any](t,rr)
	if view["serverId"].(float64)!=float64(w.serverID)||view["enforcement"]!="DISABLED"{t.Fatalf("unsafe export: %v",view)}
	// Entitlement status is informative, never a reusable bearer token.
	rr=w.call(w.a.handleAntiCheatEntitlements,http.MethodGet,w.path("/anti-cheat/entitlements"),w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusOK{t.Fatalf("status: %d %s",rr.Code,rr.Body.String())}
	caps:=decodeBody[map[string]any](t,rr)["capabilities"].([]any)
	if len(caps)!=2||caps[0]!="case.watch"||caps[1]!="case.pro" {t.Fatalf("unexpected caps: %v",caps)}
	// Keep sales off; a purchased Pro add-on retains verified coverage.
	if w.a.Billing.CasePlans()[0].Purchasable {t.Fatal("sales flag was accidentally enabled")}
	_,err:=w.a.DB.Pool.Exec(context.Background(),`UPDATE case_addon_subscriptions
	SET status='PAST_DUE' WHERE organization_id=$1 AND installation_id=$2`,w.f.OrgID,w.f.InstallationID)
	if err!=nil{t.Fatal(err)}
	rr=w.call(w.a.handleAntiCheatPremiumExport,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusForbidden{t.Fatalf("payment failure bypassed: %d %s",rr.Code,rr.Body.String())}
	workerOK,workerErr=w.a.caseWorkerAllowed(context.Background(),w.f.OrgID,w.f.InstallationID,w.serverID,casebilling.CapPro)
	if workerErr!=nil || workerOK{t.Fatalf("worker retained premium after failed payment: %v %v",workerOK,workerErr)}
	// A canceled add-on cannot be reactivated by website/Discord state.
	_,err=w.a.DB.Pool.Exec(context.Background(),`UPDATE case_addon_subscriptions
	SET status='ACTIVE',paid_through=$3 WHERE organization_id=$1 AND installation_id=$2`,
	w.f.OrgID,w.f.InstallationID,end)
	if err!=nil{t.Fatal(err)}
	_,err=w.a.DB.Pool.Exec(context.Background(),`UPDATE subscriptions SET status='PAST_DUE'
	WHERE organization_id=$1`,w.f.OrgID)
	if err!=nil{t.Fatal(err)}
	rr=w.call(w.a.handleAntiCheatPremiumExport,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusForbidden{t.Fatalf("unpaid base bypassed: %d",rr.Code)}
}

func TestCASEPremiumExportBoundedAndNoCrossServerEvidence(t *testing.T){
	w:=newClientAdminWorld(t)
	seedCasePremiumAccess(t,w)
	repo:=repository.NewCaseEvidenceRepository(w.a.DB.Pool)
	for i:=int64(1);i<=105;i++{
		in:=caseHitInput(w.guildID,w.serverID,i*100,"dayzps/config/case-premium.ADM",fmt.Sprintf("%064x",i))
		if err:=repo.RecordCaseEvidence(context.Background(),in);err!=nil{t.Fatal(err)}
	}
	// A full export fetches two bounded database pages, not an unlimited
	// query, and retains the exact server scope on every iteration.
	path:=w.path("/anti-cheat/premium/evidence-export?limit=120")
	rr:=w.call(w.a.handleAntiCheatPremiumExport,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusOK{t.Fatalf("export: %d %s",rr.Code,rr.Body.String())}
	body:=decodeBody[map[string]any](t,rr)
	if len(body["items"].([]any))!=105{t.Fatalf("export count: %d",len(body["items"].([]any)))}
	for _,bad:=range []string{"?limit=251","?limit=0","?limit=abc","?before=-1"}{
		rr=w.call(w.a.handleAntiCheatPremiumExport,http.MethodGet,w.path("/anti-cheat/premium/evidence-export")+bad,w.f.OwnerDiscordID,nil,nil)
		if rr.Code!=http.StatusBadRequest{t.Fatalf("bad request %s accepted: %d",bad,rr.Code)}
	}
	// Paid entitlement is strictly bound to the installation's current server.
	var other int64
	if err:=w.a.DB.Pool.QueryRow(context.Background(),`
	INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
	VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
	w.guildID,fmt.Sprintf("case-repoint-%d",time.Now().UnixNano()),w.f.OrgID).Scan(&other);err!=nil{t.Fatal(err)}
	_,err:=w.a.DB.Pool.Exec(context.Background(),`UPDATE installations SET game_server_id=$1 WHERE id=$2`,other,w.f.InstallationID)
	if err!=nil{t.Fatal(err)}
	rr=w.call(w.a.handleAntiCheatPremiumExport,http.MethodGet,path,w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusForbidden{t.Fatalf("repointed server inherited purchase: %d %s",rr.Code,rr.Body.String())}
	rr=w.call(w.a.handleAntiCheatEntitlements,http.MethodGet,w.path("/anti-cheat/entitlements"),w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusOK{t.Fatalf("entitlement status: %d",rr.Code)}
	caps:=decodeBody[map[string]any](t,rr)["capabilities"].([]any)
	if len(caps)!=0{t.Fatalf("repointed server gained capabilities: %v",caps)}
}
