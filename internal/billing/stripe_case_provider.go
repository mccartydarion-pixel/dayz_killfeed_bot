package billing

import (
	"context"
	"fmt"
	"strconv"

	stripe "github.com/stripe/stripe-go/v82"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	stripeprice "github.com/stripe/stripe-go/v82/price"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

// ValidateCasePrice reads Stripe's actual price/product before initiating a
// charge. An operator typo in the configured price ID must not bill a user an
// arbitrary amount, non-recurring price, or another Champion base product.
func (p *StripeProvider) ValidateCasePrice(ctx context.Context, priceID string, expectedAmount int64, tier casebilling.Tier) error {
	params := &stripe.PriceParams{Params:*withCtx(ctx)}
	params.AddExpand("product")
	price,err:=stripeprice.Get(priceID,params)
	if err!=nil{return fmt.Errorf("retrieve case price: %w",err)}
	if price==nil || !price.Active || price.Currency!="usd" || price.UnitAmount!=expectedAmount ||
		price.Recurring==nil || string(price.Recurring.Interval)!="month" ||
		price.Recurring.IntervalCount!=1 || price.Product==nil || !price.Product.Active ||
		price.Product.Metadata["champion_product_kind"]!=caseProductKind ||
		price.Product.Metadata["champion_case_tier"]!=string(tier) {
		return fmt.Errorf("configured case price/product does not match approved recurring package")
	}
	return nil
}

func (p *StripeProvider) CreateCaseCheckoutSession(ctx context.Context, in CaseCheckoutInput) (*CheckoutSession,error) {
	meta:=CaseMetadata(in)
	params:=&stripe.CheckoutSessionParams{
		Params:*withCtx(ctx),
		Mode:stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:stripe.String(in.CustomerID),
		ClientReferenceID:stripe.String(strconv.FormatInt(in.OrganizationID,10)),
		SuccessURL:stripe.String(in.SuccessURL),
		CancelURL:stripe.String(in.CancelURL),
		LineItems:[]*stripe.CheckoutSessionLineItemParams{{
			Price:stripe.String(in.PriceID),Quantity:stripe.Int64(1),
		}},
		SubscriptionData:&stripe.CheckoutSessionSubscriptionDataParams{Metadata:meta},
	}
	for k,v:=range meta {params.AddMetadata(k,v)}
	if in.Attempt<=0{return nil,fmt.Errorf("invalid case checkout attempt")}
	params.SetIdempotencyKey(fmt.Sprintf("champion-case-checkout-%d-%d",in.AddonID,in.Attempt))
	session,err:=checkoutsession.New(params)
	if err!=nil{return nil,fmt.Errorf("create case checkout: %w",err)}
	if session.ID=="" || session.URL=="" {return nil,fmt.Errorf("case checkout returned no hosted session")}
	return &CheckoutSession{ID:session.ID,URL:session.URL},nil
}

// Read-only status check; no local reset can occur on an OPEN or COMPLETE
// Checkout Session, or one that already created a subscription.
func (p *StripeProvider) GetCaseCheckoutSession(ctx context.Context, id string) (*CaseCheckoutSessionState,error) {
	if id=="" {return nil,ErrCaseCheckoutNotExpired}
	s,err:=checkoutsession.Get(id,&stripe.CheckoutSessionParams{Params:*withCtx(ctx)})
	if err!=nil{return nil,fmt.Errorf("retrieve case checkout: %w",err)}
	if s==nil{return nil,ErrCaseCheckoutNotExpired}
	out:=&CaseCheckoutSessionState{ID:s.ID,Status:string(s.Status),Metadata:s.Metadata}
	if s.Customer!=nil{out.CustomerID=s.Customer.ID}
	if s.Subscription!=nil{out.SubscriptionID=s.Subscription.ID}
	return out,nil
}

var _ CaseProvider = (*StripeProvider)(nil)
