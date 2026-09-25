package billing

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCaseRuntimeAccessIsServerScopedPaidAndIndependentOfCheckoutFlag(t *testing.T) {
	now := time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)
	end := now.Add(24*time.Hour)
	serverID := int64(30)
	baseStore := newFakeStore()
	baseStore.byOrg[10] = &repository.Subscription{
		OrganizationID:10,Plan:"LOW",Status:repository.SubscriptionActive,
		Provider:repository.ProviderStripe,ProviderCustomerID:"cus_case",
		ProviderSubscriptionID:"sub_base",ProviderPriceID:"price_base",
		CurrentPeriodEnd:&end,
	}
	addon := &repository.CaseAddonSubscription{
		OrganizationID:10,InstallationID:20,GameServerID:serverID,
		SelectedGameServerID:&serverID,Tier:string(casebilling.Pro),
		Status:"ACTIVE",Provider:repository.ProviderStripe,
		ProviderCustomerID:"cus_case",ProviderSubscriptionID:"sub_addon",
		ProviderPriceID:"price_pro",CurrentPeriodEnd:&end,PaidThrough:&end,
	}
	caseStore := &caseTestStore{row:addon}
	s := NewService(baseStore,nil,nil,Options{})
	check := func(t *testing.T,org,inst,server int64,want []casebilling.Capability){
		t.Helper()
		got,err:=s.CaseAccess(context.Background(),org,inst,server,now)
		if err!=nil{t.Fatal(err)}
		if !reflect.DeepEqual(got,want){t.Fatalf("scope %d/%d/%d: got %v, want %v",org,inst,server,got,want)}
	}
	empty:=[]casebilling.Capability{}
	check(t,10,20,30,empty)
	if err:=s.ConfigureCaseAddons(caseStore,CaseOptions{AccessEnabled:true,VerifiedThrough:casebilling.Pro});err!=nil{t.Fatal(err)}
	want:=[]casebilling.Capability{casebilling.CapWatch,casebilling.CapPro}
	check(t,10,20,30,want) // checkout remains disabled
	check(t,10,20,31,empty)
	check(t,11,20,30,empty)
	check(t,10,21,30,empty)
	ok,err:=s.CaseAllows(context.Background(),10,20,30,casebilling.CapPro,now)
	if err!=nil||!ok{t.Fatalf("valid Pro access denied: %v %v",ok,err)}
	ok,err=s.CaseAllows(context.Background(),10,20,30,casebilling.CapCommand,now)
	if err!=nil||ok{t.Fatalf("Command must never inherit Pro: %v %v",ok,err)}
	old:=*addon
	testCases:=[]struct{name string; mutate func()}{
		{"addon different customer",func(){addon.ProviderCustomerID="cus_other"}},
		{"addon different linked server",func(){v:=int64(31);addon.SelectedGameServerID=&v}},
		{"payment failure",func(){addon.Status="PAST_DUE"}},
		{"canceled subscription",func(){addon.Status="CANCELED"}},
		{"missing confirmed invoice",func(){addon.PaidThrough=nil}},
		{"expired paid coverage",func(){addon.PaidThrough=&now}},
		{"expired addon period",func(){addon.CurrentPeriodEnd=&now}},
		{"missing stripe id",func(){addon.ProviderSubscriptionID=""}},
		{"unverified tier",func(){addon.Tier=string(casebilling.Command)}},
	}
	for _,tc:=range testCases{
		t.Run(tc.name,func(t *testing.T){*addon=old;tc.mutate();check(t,10,20,30,empty)})
	}
	*addon=old
	base:=baseStore.byOrg[10]
	original:=*base
	baseTests:=[]struct{name string; mutate func()}{
		{"base trial",func(){base.Status=repository.SubscriptionTrial}},
		{"base unpaid",func(){base.Status=repository.SubscriptionPastDue}},
		{"base other customer",func(){base.ProviderCustomerID="cus_other"}},
		{"base missing subscription",func(){base.ProviderSubscriptionID=""}},
		{"base missing price",func(){base.ProviderPriceID=""}},
		{"base expired",func(){base.CurrentPeriodEnd=&now}},
		{"base non-Stripe",func(){base.Provider="manual"}},
		{"base plan none",func(){base.Plan=repository.PlanNone}},
	}
	for _,tc:=range baseTests{
		t.Run(tc.name,func(t *testing.T){*base=original;tc.mutate();check(t,10,20,30,empty)})
	}
	*base=original
	s.caseVerifiedThrough=casebilling.Watch
	check(t,10,20,30,empty) // no partial grant of a Pro product before verification
	s.caseVerifiedThrough=casebilling.Pro
	s.caseAccessEnabled=false
	check(t,10,20,30,empty)
}

func TestCaseRuntimeAccessRequiresConfirmedFounderTrialAndFailsClosedOnStoreError(t *testing.T){
	now:=time.Now().UTC().Truncate(time.Second)
	end:=now.Add(7*24*time.Hour)
	id:=int64(3)
	store:=newFakeStore()
	store.byOrg[1]=&repository.Subscription{OrganizationID:1,Plan:"LOW",Status:"ACTIVE",
		Provider:"stripe",ProviderCustomerID:"cus",ProviderSubscriptionID:"sub_base",
		ProviderPriceID:"price_base",CurrentPeriodEnd:&end}
	row:=&repository.CaseAddonSubscription{
		OrganizationID:1,InstallationID:2,GameServerID:3,SelectedGameServerID:&id,
		Tier:string(casebilling.Pro),Status:"TRIAL",Provider:"stripe",
		ProviderCustomerID:"cus",ProviderSubscriptionID:"sub_pro",ProviderPriceID:"price_pro",
		CurrentPeriodEnd:&end,TrialEndsAt:&end,
	}
	cs:=&caseTestStore{row:row}
	s:=NewService(store,nil,nil,Options{})
	if err:=s.ConfigureCaseAddons(cs,CaseOptions{AccessEnabled:true,VerifiedThrough:casebilling.Pro});err!=nil{t.Fatal(err)}
	got,err:=s.CaseAccess(context.Background(),1,2,3,now)
	if err!=nil || len(got)!=0{t.Fatalf("unverified trial granted access: %v %v",got,err)}
	row.FounderTrialGranted=true
	got,err=s.CaseAccess(context.Background(),1,2,3,now)
	if err!=nil || !reflect.DeepEqual(got,[]casebilling.Capability{casebilling.CapWatch,casebilling.CapPro}){
		t.Fatalf("verified founder trial unavailable: %v %v",got,err)
	}
	got,err=s.CaseAccess(context.Background(),1,2,3,end)
	if err!=nil || len(got)!=0{t.Fatalf("expired founder trial granted: %v %v",got,err)}
	s.caseStore=nil
	got,err=s.CaseAccess(context.Background(),1,2,3,now)
	if !errors.Is(err,ErrProviderNotConfigured)||len(got)!=0 {
		t.Fatalf("missing store must fail closed: %v %v",got,err)
	}
}
