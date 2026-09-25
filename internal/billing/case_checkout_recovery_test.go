package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCASECheckoutRecoveryRequiresStripeConfirmedExpiration(t *testing.T) {
	type tc struct{ name,status,subscription,customer string; mismatch bool; allowed bool }
	cases:=[]tc{
		{name:"expired with no subscription",status:"expired",customer:"cus_case",allowed:true},
		{name:"still open",status:"open",customer:"cus_case"},
		{name:"already completed",status:"complete",customer:"cus_case"},
		{name:"expired with subscription",status:"expired",subscription:"sub_case",customer:"cus_case"},
		{name:"other customer",status:"expired",customer:"cus_foreign"},
		{name:"other server metadata",status:"expired",customer:"cus_case",mismatch:true},
	}
	for _,tt:=range cases {t.Run(tt.name,func(t *testing.T){
		s,p,store:=newCaseTestBilling(t)
		meta:=CaseMetadata(CaseCheckoutInput{
			AddonID:8,OrganizationID:10,InstallationID:20,GameServerID:30,Tier:casebilling.Pro,
		})
		if tt.mismatch{meta["champion_game_server_id"]="999"}
		p.PutCaseCheckout(CaseCheckoutSessionState{
			ID:"cs_case",Status:tt.status,CustomerID:tt.customer,
			SubscriptionID:tt.subscription,Metadata:meta,
		})
		err:=s.RecoverCaseCheckout(context.Background(),10,20)
		if tt.allowed {
			if err!=nil{t.Fatal(err)}
			if store.reservation.SessionID!="" || store.reservation.Attempt!=2 {
				t.Fatalf("retry did not rotate checkout identity: %+v",store.reservation)
			}
			if err=s.RecoverCaseCheckout(context.Background(),10,20);!errors.Is(err,repository.ErrCaseCheckoutConflict){
				t.Fatalf("recovered session was reset twice: %v",err)
			}
			return
		}
		if err==nil{t.Fatal("unsafe session was recoverable")}
		if store.reservation.SessionID!="cs_case" || store.reservation.Attempt!=1 {
			t.Fatalf("unsafe reset modified reservation: %+v",store.reservation)
		}
	})}
}

func TestCASECheckoutRecoveryIsTenantScoped(t *testing.T){
	s,p,store:=newCaseTestBilling(t)
	p.PutCaseCheckout(CaseCheckoutSessionState{
		ID:"cs_case",Status:"expired",CustomerID:"cus_case",
		Metadata:CaseMetadata(CaseCheckoutInput{AddonID:8,OrganizationID:10,InstallationID:20,
			GameServerID:30,Tier:casebilling.Pro}),
	})
	for _,scope:=range [][2]int64{{11,20},{10,21}}{
		if err:=s.RecoverCaseCheckout(context.Background(),scope[0],scope[1]);!errors.Is(err,repository.ErrCaseCheckoutConflict){
			t.Fatalf("foreign tenant %v recovered checkout: %v",scope,err)
		}
	}
	if store.reservation.Attempt!=1{t.Fatal("foreign lookup advanced attempt")}
}
