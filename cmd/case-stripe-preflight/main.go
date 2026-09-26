// Command case-stripe-preflight checks operator-supplied C.A.S.E. prices in
// Stripe test mode using GET only. It never creates a payment or starts the app.
package main

import (
 "context"
 "encoding/json"
 "errors"
 "flag"
 "fmt"
 "io"
 "net/http"
 "net/url"
 "os"
 "strings"
 "time"
)

type expectedPrice struct {
 tier, priceID, productID, productTier string
 cents int64
}

type stripeProduct struct {
 ID string `json:"id"`
 Active bool `json:"active"`
 Livemode *bool `json:"livemode"`
 Metadata map[string]string `json:"metadata"`
}

type stripePrice struct {
 ID string `json:"id"`
 Active bool `json:"active"`
 Livemode *bool `json:"livemode"`
 Currency string `json:"currency"`
 UnitAmount *int64 `json:"unit_amount"`
 Type string `json:"type"`
 BillingScheme string `json:"billing_scheme"`
 Recurring *struct {
  Interval string `json:"interval"`
  IntervalCount int64 `json:"interval_count"`
  UsageType string `json:"usage_type"`
 } `json:"recurring"`
 Product stripeProduct `json:"product"`
}

func validatePrice(p stripePrice, expected expectedPrice) error {
 if p.ID!=expected.priceID || !p.Active || p.Livemode==nil || *p.Livemode {
  return errors.New("Price ID, active status or test-mode flag does not match")
 }
 if p.Currency!="usd" || p.UnitAmount==nil || *p.UnitAmount!=expected.cents ||
  p.Type!="recurring" || p.BillingScheme!="per_unit" || p.Recurring==nil ||
  p.Recurring.Interval!="month" || p.Recurring.IntervalCount!=1 ||
  p.Recurring.UsageType!="licensed" {
  return errors.New("Price must be a fixed USD, monthly, licensed recurring amount")
 }
 product:=p.Product
 if product.ID!=expected.productID || !product.Active ||
  product.Livemode==nil || *product.Livemode ||
  product.Metadata["champion_product_kind"]!="CASE_ADDON" ||
  product.Metadata["champion_case_tier"]!=expected.productTier {
  return errors.New("expanded Product ID, active/test mode or C.A.S.E. metadata does not match")
 }
 return nil
}

func verify(ctx context.Context, client *http.Client, baseURL,key string, specs []expectedPrice, output io.Writer) error {
 if !strings.HasPrefix(key,"sk_test_") && !strings.HasPrefix(key,"rk_test_") {
  return errors.New("requires an independent Stripe test/sandbox key (sk_test_ or rk_test_); never use a live key")
 }
 if len(specs)!=2 || specs[0].priceID==specs[1].priceID || specs[0].productID==specs[1].productID {
  return errors.New("Watch and Pro must have distinct Price and Product IDs")
 }
 for _,spec:=range specs {
  if !strings.HasPrefix(spec.priceID,"price_") || !strings.HasPrefix(spec.productID,"prod_") {
   return errors.New("expected test Price and Product IDs are missing")
  }
 }
 for _,spec:=range specs {
  endpoint:=baseURL+"/v1/prices/"+url.PathEscape(spec.priceID)+"?expand%5B%5D=product"
  req,err:=http.NewRequestWithContext(ctx,http.MethodGet,endpoint,nil)
  if err!=nil{return errors.New("could not construct read-only Stripe request")}
  req.SetBasicAuth(key,"")
  resp,err:=client.Do(req)
  if err!=nil{return fmt.Errorf("%s read-only Stripe GET failed; verify test key and network access",spec.tier)}
  body,readErr:=io.ReadAll(io.LimitReader(resp.Body,1<<20))
  _=resp.Body.Close()
  if resp.StatusCode!=http.StatusOK || readErr!=nil {
   return fmt.Errorf("%s test Price lookup did not succeed (HTTP %d); check same sandbox/account and read-only key permissions",spec.tier,resp.StatusCode)
  }
  var price stripePrice
  if json.Unmarshal(body,&price)!=nil{return fmt.Errorf("%s Stripe response could not be decoded",spec.tier)}
  if err:=validatePrice(price,spec);err!=nil{return fmt.Errorf("%s failed validation: %w",spec.tier,err)}
 }
 for _,spec:=range specs {
  fmt.Fprintf(output,"PASS %s: %s -> %s, USD %d cents/month, active, livemode=false, metadata correct\n",
   spec.tier,spec.priceID,spec.productID,spec.cents)
 }
 fmt.Fprintln(output,"READ-ONLY: no Stripe objects, customers or subscriptions were created or modified.")
 return nil
}

func main(){
 watchPrice:=flag.String("watch-price","price_1UJVqQ9sqOgctIAtK8UiKgJP","expected Watch test Price ID")
 watchProduct:=flag.String("watch-product","prod_VKAAcsjmLTvLGs","expected Watch test Product ID")
 proPrice:=flag.String("pro-price","price_1UJVqp9sqOgctIAtFWedx67E","expected Pro test Price ID")
 proProduct:=flag.String("pro-product","prod_VKABdveP3lpSWd","expected Pro test Product ID")
 webhookURL:=flag.String("webhook-url","","optional: staging https://<host>/api/saas/billing/webhook to verify in the sandbox")
 flag.Parse()
 key:=strings.TrimSpace(os.Getenv("CASE_STRIPE_TEST_SECRET_KEY"))
 if key==""{
  fmt.Fprintln(os.Stderr,"C.A.S.E. Stripe preflight blocked: CASE_STRIPE_TEST_SECRET_KEY not set; load the sandbox key privately in your terminal")
  os.Exit(2)
 }
 specs:=[]expectedPrice{
  {tier:"WATCH",priceID:*watchPrice,productID:*watchProduct,productTier:"CASE_WATCH",cents:499},
  {tier:"PRO",priceID:*proPrice,productID:*proProduct,productTier:"CASE_PRO",cents:999},
 }
 ctx,cancel:=context.WithTimeout(context.Background(),35*time.Second);defer cancel()
 client:=&http.Client{Timeout:15*time.Second}
 if err:=verify(ctx,client,"https://api.stripe.com",key,specs,os.Stdout);err!=nil{
  fmt.Fprintln(os.Stderr,"C.A.S.E. Stripe preflight blocked:",err)
  os.Exit(2)
 }
 // Optional staging checks, still GET-only: the base catalog must live in
 // the same sandbox, and the staging webhook endpoint must be subscribed.
 if raw:=strings.TrimSpace(os.Getenv("CHAMPION_BILLING_PLANS_JSON"));raw!=""{
  if err:=verifyBaseCatalog(ctx,client,"https://api.stripe.com",key,raw,specs,os.Stdout);err!=nil{
   fmt.Fprintln(os.Stderr,"C.A.S.E. Stripe preflight blocked:",err)
   os.Exit(2)
  }
 } else {
  fmt.Println("SKIP BASE: CHAMPION_BILLING_PLANS_JSON not set (required before staging checkout)")
 }
 if *webhookURL!=""{
  if err:=verifyWebhookEndpoint(ctx,client,"https://api.stripe.com",key,*webhookURL,os.Stdout);err!=nil{
   fmt.Fprintln(os.Stderr,"C.A.S.E. Stripe preflight blocked:",err)
   os.Exit(2)
  }
 } else {
  fmt.Println("SKIP WEBHOOK: -webhook-url not given (required before staging checkout)")
 }
}
