package billing

import (
 "context"
 "errors"
 "net/http"
 "testing"
 "time"

 "github.com/yourname/dayz-killfeed/internal/casebilling"
 "github.com/yourname/dayz-killfeed/internal/repository"
)

type caseTestStore struct {
 row *repository.CaseAddonSubscription
 reservation *repository.CaseCheckoutReservation
 applied []repository.CaseWebhookState
 errorOnApply error
}
func (f *caseTestStore) ReserveCaseCheckout(_ context.Context, org,installation,server int64,tier,customer string)(*repository.CaseCheckoutReservation,error){
 if f.reservation==nil{return nil,repository.ErrCaseCheckoutConflict}
 if f.reservation.OrganizationID!=org || f.reservation.InstallationID!=installation ||
 f.reservation.GameServerID!=server || f.reservation.Tier!=tier || f.reservation.ProviderCustomerID!=customer {
 return nil,repository.ErrCaseCheckoutConflict}
 return f.reservation,nil
}
func (f *caseTestStore) StoreCaseCheckout(_ context.Context,id int64,session,url string)error{
 if f.reservation==nil || f.reservation.ID!=id{return repository.ErrCaseCheckoutConflict}
 f.reservation.SessionID=session;f.reservation.CheckoutURL=url;return nil
}
func (f *caseTestStore) GetByCaseSubscriptionID(_ context.Context,id string)(*repository.CaseAddonSubscription,error){
 if f.row!=nil && f.row.ProviderSubscriptionID==id{return f.row,nil}
 return nil,nil
}
func (f *caseTestStore) ApplyCaseWebhook(_ context.Context,in repository.CaseWebhookState)error{
 if f.errorOnApply!=nil{return f.errorOnApply}
 if f.reservation==nil || f.reservation.ID!=in.AddonID ||
 f.reservation.OrganizationID!=in.OrganizationID || f.reservation.InstallationID!=in.InstallationID ||
 f.reservation.GameServerID!=in.GameServerID || f.reservation.Tier!=in.Tier ||
 f.reservation.ProviderCustomerID!=in.CustomerID ||
 (in.CheckoutSessionID!="" && in.CheckoutSessionID!=f.reservation.SessionID){
 return repository.ErrCaseWebhookMismatch}
 f.applied=append(f.applied,in);return nil
}
func (f *caseTestStore) GetScoped(_ context.Context,org,installation int64)(*repository.CaseAddonSubscription,error){
 if f.row!=nil && f.row.OrganizationID==org && f.row.InstallationID==installation {return f.row,nil}
 return nil,nil
}
func (f *caseTestStore) SaveCaseCancelFlag(_ context.Context,org,installation int64,sub string,cancel bool)error{
 if f.row==nil || f.row.OrganizationID!=org || f.row.InstallationID!=installation ||
 f.row.ProviderSubscriptionID!=sub {return repository.ErrCaseCheckoutConflict}
 f.row.CancelAtPeriodEnd=cancel;return nil
}
func (f *caseTestStore) GetPendingCaseCheckout(_ context.Context,org,installation int64)(*repository.CaseCheckoutReservation,error){
	if f.reservation==nil || f.reservation.OrganizationID!=org || f.reservation.InstallationID!=installation ||
		f.reservation.SessionID=="" {return nil,repository.ErrCaseCheckoutConflict}
	return f.reservation,nil
}
func (f *caseTestStore) ResetExpiredCaseCheckout(_ context.Context,org,installation,addonID,attempt int64,sessionID string)error{
	row:=f.reservation
	if row==nil || row.ID!=addonID || row.OrganizationID!=org || row.InstallationID!=installation ||
		row.Attempt!=attempt || row.SessionID!=sessionID {return repository.ErrCaseCheckoutConflict}
	row.SessionID="";row.CheckoutURL="";row.Attempt++
	return nil
}
func (f *caseTestStore) ListByOrganization(_ context.Context,_ int64)([]repository.CaseAddonSubscription,error){
 return []repository.CaseAddonSubscription{},nil
}

func newCaseTestBilling(t *testing.T)(*Service,*FakeProvider,*caseTestStore){
 t.Helper()
 catalog,err:=LoadCatalog(`[{"key":"LOW","monthly":{"amountCents":599,"currency":"usd","stripePriceId":"price_base"}}]`)
 if err!=nil {t.Fatal(err)}
 provider:=NewFakeProvider()
 store:=&caseTestStore{reservation:&repository.CaseCheckoutReservation{
 ID:8,Attempt:1,OrganizationID:10,InstallationID:20,GameServerID:30,Tier:string(casebilling.Pro),ProviderCustomerID:"cus_case",
 SessionID:"cs_case",
 }}
 s:=NewService(nil,catalog,provider,Options{WebhookSecret:"whsec_unit_case"})
 if err:=s.ConfigureCaseAddons(store,CaseOptions{
 Enabled:true,VerifiedThrough:casebilling.Pro,
 PriceIDs:map[casebilling.Tier]string{casebilling.Watch:"price_watch",casebilling.Pro:"price_pro"},
 });err!=nil{t.Fatal(err)}
 return s,provider,store
}

func TestCaseCatalogCannotReuseBasePrice(t *testing.T){
 c,err:=LoadCatalog(`[{"key":"LOW","monthly":{"amountCents":599,"currency":"usd","stripePriceId":"price_base"}}]`)
 if err!=nil {t.Fatal(err)}
 s:=NewService(nil,c,NewFakeProvider(),Options{})
 store:=&caseTestStore{}
 if err:=s.ConfigureCaseAddons(store,CaseOptions{PriceIDs:map[casebilling.Tier]string{casebilling.Watch:"price_base"}});err==nil {
 t.Fatal("base and case price collision accepted")
 }
 if err:=s.ConfigureCaseAddons(store,CaseOptions{});err!=nil{t.Fatal(err)}
 for _,p:=range s.CasePlans(){if p.Purchasable{t.Fatalf("disabled catalog sold %s",p.Tier)}}
}

func TestCaseCheckoutMetadataIsBoundToExactServer(t *testing.T){
 in:=CaseCheckoutInput{CustomerID:"cus_case",PriceID:"price_pro",AddonID:8,
 OrganizationID:10,InstallationID:20,GameServerID:30,Tier:casebilling.Pro}
 meta:=CaseMetadata(in)
 want:=map[string]string{
 "champion_product_kind":"CASE_ADDON","champion_case_addon_id":"8",
 "champion_organization_id":"10","champion_installation_id":"20",
 "champion_game_server_id":"30","champion_case_tier":"CASE_PRO",
 }
 for k,v:=range want{if meta[k]!=v{t.Errorf("%s = %q, want %q",k,meta[k],v)}}
}

func TestCaseEventClassificationSeparatesBaseAndAddons(t *testing.T){
 s,provider,store:=newCaseTestBilling(t)
 ctx:=context.Background()
 expired:=time.Now().Add(time.Hour)
 store.row=&repository.CaseAddonSubscription{ProviderSubscriptionID:"sub_case"}
 tests:=[]struct{name string;e ParsedEvent;want bool}{
 {"checkout tagged",ParsedEvent{Type:EventCheckoutCompleted,Session:&webhookCheckoutSession{Metadata:map[string]string{"champion_product_kind":"CASE_ADDON"}}},true},
 {"subscription tagged",ParsedEvent{Type:EventSubscriptionUpdated,Sub:&webhookSubscription{Metadata:map[string]string{"champion_product_kind":"CASE_ADDON"}}},true},
 {"known subscription",ParsedEvent{Type:EventSubscriptionUpdated,Sub:&webhookSubscription{ID:"sub_case"}},true},
 {"known invoice",ParsedEvent{Type:EventInvoicePaid,Invoice:&webhookInvoice{Subscription:"sub_case"}},true},
 {"base subscription",ParsedEvent{Type:EventSubscriptionUpdated,Sub:&webhookSubscription{ID:"sub_base",Metadata:map[string]string{"champion_plan_key":"LOW"}}},false},
 }
 provider.Put(SubscriptionState{SubscriptionID:"sub_case",CustomerID:"cus_case",PriceID:"price_pro",
 StripeStatus:"active",CurrentPeriodEnd:expired,Metadata:map[string]string{"champion_product_kind":"CASE_ADDON"}})
 for _,tc:=range tests{t.Run(tc.name,func(t *testing.T){
 got,err:=s.classifyCaseEvent(ctx,tc.e);if err!=nil{t.Fatal(err)}
 if got!=tc.want{t.Fatalf("got case=%v, want %v",got,tc.want)}
 })}
}

func TestCaseWebhookDoesNotCallBaseApply(t *testing.T){
 s,provider,store:=newCaseTestBilling(t)
 now:=time.Now().UTC()
 provider.Put(SubscriptionState{
 SubscriptionID:"sub_case",CustomerID:"cus_case",PriceID:"price_pro",StripeStatus:"active",
 CurrentPeriodStart:now,CurrentPeriodEnd:now.Add(time.Hour),
 Metadata:CaseMetadata(CaseCheckoutInput{AddonID:8,OrganizationID:10,InstallationID:20,
 GameServerID:30,Tier:casebilling.Pro}),
 })
 e:=ParsedEvent{ID:"evt_case",Type:EventCheckoutCompleted,
 Session:&webhookCheckoutSession{ID:"cs_case",Mode:"subscription",
 Customer:"cus_case",Subscription:"sub_case",
 Metadata:CaseMetadata(CaseCheckoutInput{AddonID:8,OrganizationID:10,InstallationID:20,
 GameServerID:30,Tier:casebilling.Pro})}}
 if err:=s.applyCaseEvent(context.Background(),e);err!=nil{t.Fatal(err)}
 if len(store.applied)!=1 || store.applied[0].Status!="ACTIVE"{t.Fatalf("unexpected event application: %+v",store.applied)}
 e.ID="evt_foreign"
 e.Session.Metadata["champion_game_server_id"]="999"
 if err:=s.applyCaseEvent(context.Background(),e);!errors.Is(err,repository.ErrCaseWebhookMismatch){
 t.Fatalf("forged server metadata accepted: %v",err)
 }
 store.errorOnApply=errors.New("transient db failure")
 e.ID="evt_retry";e.Session.Metadata["champion_game_server_id"]="30"
 if err:=s.applyCaseEvent(context.Background(),e);err==nil{t.Fatal("transient storage error swallowed")}
 if len(store.applied)!=1{t.Fatal("failure unexpectedly committed an event")}
}

func TestCaseUnconfiguredCannotCheckout(t *testing.T){
 s:=NewService(nil,nil,nil,Options{})
 req:=CaseCheckoutRequest{InstallationID:2,Tier:casebilling.Watch}
 _,err:=s.CaseCheckout(context.Background(),&http.Request{},1,req,nil)
 if !errors.Is(err,ErrCaseDisabled){t.Fatalf("disabled checkout: %v",err)}
}
