package main

import (
 "bytes"
 "context"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"
)

func specs() []expectedPrice {
 return []expectedPrice{
  {tier:"WATCH",priceID:"price_watch",productID:"prod_watch",productTier:"CASE_WATCH",cents:499},
  {tier:"PRO",priceID:"price_pro",productID:"prod_pro",productTier:"CASE_PRO",cents:999},
 }
}

func mockPrice(s expectedPrice) stripePrice {
 live:=false
 amount:=s.cents
 p:=stripePrice{ID:s.priceID,Active:true,Livemode:&live,Currency:"usd",
  UnitAmount:&amount,Type:"recurring",BillingScheme:"per_unit",
  Product:stripeProduct{ID:s.productID,Active:true,Livemode:&live,
   Metadata:map[string]string{"champion_product_kind":"CASE_ADDON","champion_case_tier":s.productTier}},
 }
 p.Recurring=&struct{
  Interval string `json:"interval"`
  IntervalCount int64 `json:"interval_count"`
  UsageType string `json:"usage_type"`
 }{Interval:"month",IntervalCount:1,UsageType:"licensed"}
 return p
}

func TestReadOnlySandboxPreflight(t *testing.T){
 checks:=specs()
 requests:=0
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  requests++
  if r.Method!=http.MethodGet{t.Errorf("mutating Stripe HTTP method %s",r.Method)}
  u,p,ok:=r.BasicAuth();if !ok||u!="sk_test_fake"||p!=""{t.Error("unexpected sandbox authorization")}
  if r.URL.Query().Get("expand[]")!="product"{t.Error("product expansion missing")}
  w.Header().Set("Content-Type","application/json")
  switch r.URL.Path {
  case "/v1/prices/price_watch": _=json.NewEncoder(w).Encode(mockPrice(checks[0]))
  case "/v1/prices/price_pro": _=json.NewEncoder(w).Encode(mockPrice(checks[1]))
  default: t.Errorf("unexpected endpoint %s",r.URL.Path);http.NotFound(w,r)
  }
 }))
 defer server.Close()
 var out bytes.Buffer
 if err:=verify(context.Background(),server.Client(),server.URL,"sk_test_fake",checks,&out);err!=nil{t.Fatal(err)}
 if requests!=2 || strings.Count(out.String(),"PASS ")!=2 || !strings.Contains(out.String(),"READ-ONLY"){
  t.Fatalf("expected two GETs and a safe receipt; requests=%d output=%s",requests,out.String())
 }
}

func TestPriceValidationRejectsLiveAndWrongCatalog(t *testing.T){
 s:=specs()[0]
 test:=func(label string,edit func(*stripePrice)){
  t.Run(label,func(t *testing.T){
   p:=mockPrice(s);edit(&p)
   if err:=validatePrice(p,s);err==nil{t.Fatal("mismatched Stripe object passed")}
  })
 }
 test("price live",func(p *stripePrice){b:=true;p.Livemode=&b})
 test("price mode missing",func(p *stripePrice){p.Livemode=nil})
 test("product live",func(p *stripePrice){b:=true;p.Product.Livemode=&b})
 test("product missing",func(p *stripePrice){p.Product.ID=""})
 test("product id wrong",func(p *stripePrice){p.Product.ID="prod_other"})
 test("wrong currency",func(p *stripePrice){p.Currency="eur"})
 test("amount wrong",func(p *stripePrice){v:=int64(49900);p.UnitAmount=&v})
 test("price inactive",func(p *stripePrice){p.Active=false})
 test("product inactive",func(p *stripePrice){p.Product.Active=false})
 test("usage metered",func(p *stripePrice){p.Recurring.UsageType="metered"})
 test("annual",func(p *stripePrice){p.Recurring.Interval="year"})
 test("quarterly",func(p *stripePrice){p.Recurring.IntervalCount=3})
 test("unexpanded",func(p *stripePrice){p.Product=stripeProduct{}})
 test("base product",func(p *stripePrice){p.Product.Metadata["champion_product_kind"]="BASE"})
 test("wrong tier",func(p *stripePrice){p.Product.Metadata["champion_case_tier"]="CASE_PRO"})
 test("tiered price",func(p *stripePrice){p.BillingScheme="tiered"})
}

func TestReadOnlyVerifierRejectsLiveKeyWithoutNetwork(t *testing.T) {
 requests:=0
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){requests++}))
 defer server.Close()
 var out bytes.Buffer
 if err:=verify(context.Background(),server.Client(),server.URL,"sk_live_example",specs(),&out);err==nil{
  t.Fatal("live key accepted")
 }
 if requests!=0||out.Len()!=0{t.Fatalf("live key made requests=%d or printed output",requests)}
}

func TestReadOnlyVerifierDoesNotReportSuccessAfterOneFailedPrice(t *testing.T) {
 checks:=specs()
 requests:=0
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  requests++
  if strings.HasSuffix(r.URL.Path,"price_watch"){
   _=json.NewEncoder(w).Encode(mockPrice(checks[0]))
  } else {
   http.Error(w,"secret diagnostics must not be reflected",http.StatusForbidden)
  }
 }))
 defer server.Close()
 var out bytes.Buffer
 err:=verify(context.Background(),server.Client(),server.URL,"sk_test_fake",checks,&out)
 if err==nil||requests!=2||out.Len()!=0||strings.Contains(err.Error(),"secret diagnostics"){
  t.Fatalf("failed second Price must not produce PASS or expose API body: requests=%d err=%v output=%s",requests,err,out.String())
 }
}
