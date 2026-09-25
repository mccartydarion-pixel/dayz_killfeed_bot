package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCASEFounderTrialOnlyUsesExplicitSevenDayOffer(t *testing.T) {
	s,provider,store:=newCaseTestBilling(t)
	start:=time.Date(2026,9,24,12,0,0,0,time.UTC)
	end:=start.Add(7*24*time.Hour)
	meta:=CaseMetadata(CaseCheckoutInput{AddonID:8,OrganizationID:10,InstallationID:20,GameServerID:30,Tier:casebilling.Pro})
	meta["champion_case_trial_offer"]="founder_pro_7d"
	provider.Put(SubscriptionState{SubscriptionID:"sub_case",CustomerID:"cus_case",PriceID:"price_pro",
		StripeStatus:"trialing",CurrentPeriodStart:start,CurrentPeriodEnd:end,
		TrialStart:&start,TrialEnd:&end,Metadata:meta})
	e:=ParsedEvent{ID:"evt_case_trial",Type:EventCheckoutCompleted,Session:&webhookCheckoutSession{
		ID:"cs_case",Mode:"subscription",Customer:"cus_case",Subscription:"sub_case",Metadata:meta,
	}}
	if err:=s.applyCaseEvent(context.Background(),e);err!=nil{t.Fatal(err)}
	if len(store.applied)!=1 || store.applied[0].Status!="TRIAL" ||
		!store.applied[0].FounderTrialOffer || store.applied[0].TrialStart==nil ||
		!store.applied[0].TrialStart.Equal(start) || store.applied[0].TrialEnd==nil ||
		!store.applied[0].TrialEnd.Equal(end) {
		t.Fatalf("explicit trial evidence lost: %+v",store.applied)
	}
	// Stripe retains the original trial marker after converting to paid.
	provider.Put(SubscriptionState{SubscriptionID:"sub_case",CustomerID:"cus_case",PriceID:"price_pro",
		StripeStatus:"active",CurrentPeriodStart:end,CurrentPeriodEnd:end.Add(30*24*time.Hour),
		TrialStart:&start,TrialEnd:&end,Metadata:meta})
	e.ID="evt_case_after_trial"
	if err:=s.applyCaseEvent(context.Background(),e);err!=nil{t.Fatal(err)}
	if len(store.applied)!=2 || store.applied[1].FounderTrialOffer {
		t.Fatalf("paid renewal must not grant another trial: %+v",store.applied)
	}
	meta["champion_case_trial_offer"]="unknown_offer"
	e.ID="evt_case_bad_offer"
	if err:=s.applyCaseEvent(context.Background(),e);!errors.Is(err,repository.ErrCaseWebhookMismatch) {
		t.Fatalf("unknown trial marker accepted: %v",err)
	}
}
