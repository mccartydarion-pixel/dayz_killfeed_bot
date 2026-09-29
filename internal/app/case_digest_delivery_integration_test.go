//go:build integration

package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func setupCaseDigestWorld(t *testing.T) (*clientAdminWorld,*repository.CaseDigestOutbox,repository.CaseDigestInput){
	t.Helper()
	w:=newClientAdminWorld(t)
	seedCasePremiumAccess(t,w)
	store:=repository.NewCaseDigestOutbox(w.a.DB.Pool)
	w.a.CaseDigestOutbox=store
	t.Cleanup(func(){_,err:=w.a.DB.Pool.Exec(context.Background(),"DELETE FROM case_watch_digest_outbox WHERE organization_id=$1",w.f.OrgID);if err!=nil{t.Errorf("cleanup case digest: %v",err)}})
	w.a.caseWatchRequesterCheck=func(_ context.Context,scope repository.AdminScope,userID int64)(bool,error){
		return scope.InstallationID==w.f.InstallationID &&
			userID==mustAppUserID(t,w.a,w.f.OwnerDiscordID),nil
	}
	w.a.caseWatchDigestLimiter=newSaaSRateLimiter(time.Hour,1)
	err:=w.a.SaaSChannelRoutes.UpsertRoute(context.Background(),w.f.OrgID,w.f.InstallationID,
		repository.CaseWatchDigestRouteKey,"private-staff-channel",false)
	if err!=nil{t.Fatal(err)}
	w.a.caseWatchPrivacyCheck=func(_ context.Context,guild,channel string)error{
		if guild==""||channel!="private-staff-channel"{return fmt.Errorf("destination privacy failed")}
		return nil
	}
	now:=time.Now().UTC()
	return w,store,repository.CaseDigestInput{
		OrganizationID:w.f.OrgID,InstallationID:w.f.InstallationID,
		GuildID:w.guildID,GameServerID:w.serverID,
		RequesterUserID:mustAppUserID(t,w.a,w.f.OwnerDiscordID),
		WindowStart:now.Add(-24*time.Hour),WindowEnd:now,
		SourceLines:11,HitLines:5,KillLines:3,CollectorEnabled:true,
	}
}

func TestCASEWatchOutboxReceiptAndExactlyOneDispatch(t *testing.T){
	w,store,input:=setupCaseDigestWorld(t)
	id,err:=store.Enqueue(context.Background(),input)
	if err!=nil{t.Fatal(err)}
	var sends atomic.Int64
	w.a.caseWatchSender=func(_ context.Context,channel string,embed *discordgo.MessageEmbed)(string,error){
		sends.Add(1)
		if channel!="private-staff-channel" || embed==nil ||
			embed.Title!="C.A.S.E. WATCH • OBSERVATION DIGEST"{t.Fatalf("invalid send: %s %+v",channel,embed)}
		return "discord-message-123",nil
	}
	worked,err:=w.a.processOneCaseDigest(context.Background())
	if err!=nil||!worked{t.Fatalf("worker: %v %v",worked,err)}
	receipt,err:=store.GetScoped(context.Background(),w.f.OrgID,w.f.InstallationID,id)
	if err!=nil || receipt==nil || receipt.Status!="SENT" ||
		receipt.DiscordMessageID==nil || *receipt.DiscordMessageID!="discord-message-123" ||
		receipt.SentAt==nil || sends.Load()!=1 {t.Fatalf("invalid receipt: %+v %v sends=%d",receipt,err,sends.Load())}
	worked,err=w.a.processOneCaseDigest(context.Background())
	if err!=nil||worked||sends.Load()!=1{t.Fatalf("duplicate send: %v %v sends=%d",worked,err,sends.Load())}
	path:=w.path("/anti-cheat/premium/watch-digest/"+strconv.FormatInt(id,10))
	rr:=w.call(w.a.handleAntiCheatWatchDigestReceipt,http.MethodGet,path,w.f.OwnerDiscordID,nil,
		map[string]string{"deliveryID":strconv.FormatInt(id,10)})
	if rr.Code!=http.StatusOK {t.Fatalf("receipt endpoint: %d %s",rr.Code,rr.Body.String())}
	body:=decodeBody[map[string]any](t,rr)
	if body["status"]!="SENT"||body["messageId"]!="discord-message-123"{t.Fatalf("incorrect status body: %v",body)}
	foreign:=syncUser(t,w.a,fmt.Sprintf("case-receipt-foreign-%d",time.Now().UnixNano()),"Other")
	rr=w.call(w.a.handleAntiCheatWatchDigestReceipt,http.MethodGet,path,foreign.DiscordUserID,nil,
		map[string]string{"deliveryID":strconv.FormatInt(id,10)})
	if rr.Code!=http.StatusForbidden{t.Fatalf("foreign receipt exposure: %d",rr.Code)}
}

func TestCASEWatchOutboxRevocationAndPrivacyFailBeforeDiscord(t *testing.T){
	for _,tc:=range []struct{name string; revoke func(*testing.T,*clientAdminWorld)}{
		{"past due",func(t *testing.T,w *clientAdminWorld){
			if _,err:=w.a.DB.Pool.Exec(context.Background(),`
				UPDATE case_addon_subscriptions SET status='PAST_DUE'
				WHERE organization_id=$1 AND installation_id=$2`,
				w.f.OrgID,w.f.InstallationID);err!=nil{t.Fatal(err)}
		}},
		{"requester role revoked",func(_ *testing.T,w *clientAdminWorld){
			w.a.caseWatchRequesterCheck=func(context.Context,repository.AdminScope,int64)(bool,error){return false,nil}
		}},
		{"public staff destination",func(_ *testing.T,w *clientAdminWorld){
			w.a.caseWatchPrivacyCheck=func(context.Context,string,string)error{return errors.New("everyone can view")}
		}},
		{"route removed",func(t *testing.T,w *clientAdminWorld){
			if err:=w.a.SaaSChannelRoutes.DeleteRoute(context.Background(),w.f.OrgID,w.f.InstallationID,
				repository.CaseWatchDigestRouteKey);err!=nil{t.Fatal(err)}
		}},
	}{
		t.Run(tc.name,func(t *testing.T){
			w,store,input:=setupCaseDigestWorld(t)
			id,err:=store.Enqueue(context.Background(),input)
			if err!=nil{t.Fatal(err)}
			var calls atomic.Int64
			w.a.caseWatchSender=func(context.Context,string,*discordgo.MessageEmbed)(string,error){
				calls.Add(1);return "forbidden",nil
			}
			tc.revoke(t,w)
			worked,err:=w.a.processOneCaseDigest(context.Background())
			if err!=nil||!worked{t.Fatalf("worker: %v %v",worked,err)}
			d,err:=store.GetScoped(context.Background(),w.f.OrgID,w.f.InstallationID,id)
			if err!=nil||d.Status!="BLOCKED"||calls.Load()!=0{t.Fatalf("unsafe delivery: %+v %v sends=%d",d,err,calls.Load())}
		})
	}
}

func TestCASEWatchOutboxAmbiguousSendNeverAutomaticallyRetries(t *testing.T){
	w,store,input:=setupCaseDigestWorld(t)
	id,err:=store.Enqueue(context.Background(),input)
	if err!=nil{t.Fatal(err)}
	var calls atomic.Int64
	w.a.caseWatchSender=func(context.Context,string,*discordgo.MessageEmbed)(string,error){
		calls.Add(1);return "",errors.New("network timeout after remote acceptance")
	}
	worked,err:=w.a.processOneCaseDigest(context.Background())
	if err!=nil||!worked{t.Fatalf("worker: %v %v",worked,err)}
	d,err:=store.GetScoped(context.Background(),w.f.OrgID,w.f.InstallationID,id)
	if err!=nil||d.Status!="UNKNOWN"{t.Fatalf("network uncertainty was not retained: %+v %v",d,err)}
	for i:=0;i<3;i++{
		worked,err=w.a.processOneCaseDigest(context.Background())
		if err!=nil||worked{t.Fatalf("ambiguous send requeued: %v %v",worked,err)}
	}
	if calls.Load()!=1{t.Fatalf("ambiguous delivery retried %d times",calls.Load())}
}

func TestCASEWatchOutboxLeaseExpiryIsRecoverableBeforeSendOnly(t *testing.T){
	w,store,input:=setupCaseDigestWorld(t)
	id,err:=store.Enqueue(context.Background(),input)
	if err!=nil{t.Fatal(err)}
	first,err:=store.ClaimNext(context.Background())
	if err!=nil||first==nil{t.Fatalf("initial claim: %v %v",first,err)}
	_,err=w.a.DB.Pool.Exec(context.Background(),`
		UPDATE case_watch_digest_outbox SET claim_expires_at=NOW()-INTERVAL '1 second'
		WHERE id=$1`,id)
	if err!=nil{t.Fatal(err)}
	second,err:=store.ClaimNext(context.Background())
	if err!=nil||second==nil||second.ID!=first.ID||second.ClaimVersion<=first.ClaimVersion{
		t.Fatalf("lease not rotated: %+v %+v %v",first,second,err)}
	if err=store.BeginSend(context.Background(),*first,"private-staff-channel");!errors.Is(err,repository.ErrCaseDigestClaimLost){
		t.Fatalf("stale claimant crossed send boundary: %v",err)
	}
	if err=store.BeginSend(context.Background(),*second,"private-staff-channel");err!=nil{t.Fatal(err)}
	_,err=w.a.DB.Pool.Exec(context.Background(),`
		UPDATE case_watch_digest_outbox SET updated_at=NOW()-INTERVAL '4 minutes'
		WHERE id=$1`,id)
	if err!=nil{t.Fatal(err)}
	if err=store.Sweep(context.Background());err!=nil{t.Fatal(err)}
	d,err:=store.GetScoped(context.Background(),w.f.OrgID,w.f.InstallationID,id)
	if err!=nil||d.Status!="UNKNOWN"{t.Fatalf("SENDING restarted as retry: %+v %v",d,err)}
	next,err:=store.ClaimNext(context.Background())
	if err!=nil||next!=nil{t.Fatalf("ambiguous send reclaimed: %+v %v",next,err)}
}

func TestCASEWatchOutboxConcurrentAdmissionAcrossReplicas(t *testing.T){
	w,_,input:=setupCaseDigestWorld(t)
	const workers=8
	var wg sync.WaitGroup
	var accepted atomic.Int64
	var cooled atomic.Int64
	for i:=0;i<workers;i++{
		wg.Add(1)
		go func(){
			defer wg.Done()
			repo:=repository.NewCaseDigestOutbox(w.a.DB.Pool) // different replica, same PostgreSQL
			_,err:=repo.Enqueue(context.Background(),input)
			switch {
			case err==nil: accepted.Add(1)
			case errors.Is(err,repository.ErrCaseDigestCooldown):cooled.Add(1)
			default:t.Errorf("unexpected enqueue error: %v",err)
			}
		}()
	}
	wg.Wait()
	if accepted.Load()!=1||cooled.Load()!=workers-1 {
		t.Fatalf("replicas admitted %d, cooled %d; expected 1/%d",accepted.Load(),cooled.Load(),workers-1)
	}
}

// Phase 6.26F.2: after merging main's operational-alert isolation, an authorized Watch digest is
// still delivered (by the durable worker, to the private C.A.S.E. status channel), and the general
// ADMIN_ALERTS route is never used as a fallback - not even when it is configured and private.
func TestCASEWatchDigestUsesOnlyThePrivateCaseStatusChannel(t *testing.T){
	ctx:=context.Background()
	reason:=func(w *clientAdminWorld,id int64)string{
		var r *string
		if err:=w.a.DB.Pool.QueryRow(ctx,`SELECT reason_code FROM case_watch_digest_outbox WHERE id=$1`,id).Scan(&r);err!=nil{t.Fatal(err)}
		if r==nil{return ""}
		return *r
	}
	// 1. CASE_STATUS missing, ADMIN_ALERTS configured (and passing the privacy check): blocked.
	w,store,input:=setupCaseDigestWorld(t)
	if err:=w.a.SaaSChannelRoutes.DeleteRoute(ctx,w.f.OrgID,w.f.InstallationID,repository.CaseWatchDigestRouteKey);err!=nil{t.Fatal(err)}
	if err:=w.a.SaaSChannelRoutes.UpsertRoute(ctx,w.f.OrgID,w.f.InstallationID,"ADMIN_ALERTS","private-staff-channel",false);err!=nil{t.Fatal(err)}
	var sends atomic.Int64
	var sentTo []string
	w.a.caseWatchSender=func(_ context.Context,channel string,_ *discordgo.MessageEmbed)(string,error){
		sends.Add(1);sentTo=append(sentTo,channel);return "discord-message-1",nil
	}
	id,err:=store.Enqueue(ctx,input)
	if err!=nil{t.Fatal(err)}
	if worked,err:=w.a.processOneCaseDigest(ctx);err!=nil||!worked{t.Fatalf("worker: %v %v",worked,err)}
	d,_:=store.GetScoped(ctx,w.f.OrgID,w.f.InstallationID,id)
	if d.Status!="BLOCKED"||reason(w,id)!="STAFF_ROUTE_MISSING"||sends.Load()!=0{
		t.Fatalf("ADMIN_ALERTS used as a paid digest fallback: %s %s sends=%d",d.Status,reason(w,id),sends.Load())
	}
	rr:=w.call(w.a.handleAntiCheatWatchDigest,http.MethodPost,w.path("/anti-cheat/premium/watch-digest"),w.f.OwnerDiscordID,nil,nil)
	if rr.Code!=http.StatusConflict{t.Fatalf("API accepted a digest without the C.A.S.E. status channel: %d %s",rr.Code,rr.Body.String())}

	// 2. The private CASE_STATUS channel configured: the authorized digest is delivered exactly once, there.
	w,store,input=setupCaseDigestWorld(t)
	if err:=w.a.SaaSChannelRoutes.UpsertRoute(ctx,w.f.OrgID,w.f.InstallationID,"ADMIN_ALERTS","general-admin-alerts",false);err!=nil{t.Fatal(err)}
	sends.Store(0);sentTo=nil
	w.a.caseWatchSender=func(_ context.Context,channel string,_ *discordgo.MessageEmbed)(string,error){
		sends.Add(1);sentTo=append(sentTo,channel);return "discord-message-2",nil
	}
	id,err=store.Enqueue(ctx,input)
	if err!=nil{t.Fatal(err)}
	if worked,err:=w.a.processOneCaseDigest(ctx);err!=nil||!worked{t.Fatalf("worker: %v %v",worked,err)}
	d,_=store.GetScoped(ctx,w.f.OrgID,w.f.InstallationID,id)
	if d.Status!="SENT"||sends.Load()!=1||len(sentTo)!=1||sentTo[0]!="private-staff-channel"{
		t.Fatalf("authorized digest: %s sends=%d to=%v",d.Status,sends.Load(),sentTo)
	}
}

// C.A.S.E. access disabled, or no paid add-on for the installation: nothing is sent.
func TestCASEWatchDigestDisabledOrUnconfiguredCannotSend(t *testing.T){
	ctx:=context.Background()
	for _,tc:=range []struct{name string; mut func(*testing.T,*clientAdminWorld)}{
		{"access flag off",func(t *testing.T,w *clientAdminWorld){
			svc:=billing.NewService(w.a.SaaSSubscriptions,nil,nil,billing.Options{})
			if err:=svc.ConfigureCaseAddons(repository.NewCaseAddonSubscriptionRepository(w.a.DB.Pool),billing.CaseOptions{
				Enabled:false,AccessEnabled:false,PriceIDs:map[casebilling.Tier]string{casebilling.Watch:"price_case_watch",casebilling.Pro:"price_case_pro"},
			});err!=nil{t.Fatal(err)}
			w.a.Billing=svc
		}},
		{"billing not configured",func(_ *testing.T,w *clientAdminWorld){w.a.Billing=billing.NewService(w.a.SaaSSubscriptions,nil,nil,billing.Options{})}},
		{"add-on removed",func(t *testing.T,w *clientAdminWorld){
			if _,err:=w.a.DB.Pool.Exec(ctx,`UPDATE case_addon_subscriptions SET paid_through=NULL,paid_tier=NULL,coverage_state='REFUNDED'
				WHERE organization_id=$1 AND installation_id=$2`,w.f.OrgID,w.f.InstallationID);err!=nil{t.Fatal(err)}
		}},
	}{
		t.Run(tc.name,func(t *testing.T){
			w,store,input:=setupCaseDigestWorld(t)
			id,err:=store.Enqueue(ctx,input)
			if err!=nil{t.Fatal(err)}
			var sends atomic.Int64
			w.a.caseWatchSender=func(context.Context,string,*discordgo.MessageEmbed)(string,error){sends.Add(1);return "x",nil}
			tc.mut(t,w)
			if worked,err:=w.a.processOneCaseDigest(ctx);err!=nil||!worked{t.Fatalf("worker: %v %v",worked,err)}
			d,_:=store.GetScoped(ctx,w.f.OrgID,w.f.InstallationID,id)
			if d.Status!="BLOCKED"||sends.Load()!=0{t.Fatalf("unpaid digest: %s sends=%d",d.Status,sends.Load())}
		})
	}
}
