package billing

import (
	"context"
	"fmt"
	"strconv"

	stripe "github.com/stripe/stripe-go/v82"
	checkoutsession "github.com/stripe/stripe-go/v82/checkout/session"
	stripecharge "github.com/stripe/stripe-go/v82/charge"
	stripedispute "github.com/stripe/stripe-go/v82/dispute"
	stripeinvoice "github.com/stripe/stripe-go/v82/invoice"
	stripeinvoicepayment "github.com/stripe/stripe-go/v82/invoicepayment"
	stripeprice "github.com/stripe/stripe-go/v82/price"
	"github.com/stripe/stripe-go/v82/subscription"

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

// caseSingleItem returns the add-on subscription's only item, which must
// currently bill fromPriceID. An add-on never carries a second item, so a
// tier change can never leave Watch and Pro billing side by side.
func caseSingleItem(ctx context.Context, subscriptionID, fromPriceID string) (*stripe.Subscription, string, error) {
	cur, err := subscription.Get(subscriptionID, &stripe.SubscriptionParams{Params: *withCtx(ctx)})
	if err != nil {
		if stripeNotFound(err) {
			return nil, "", ErrSubscriptionNotFound
		}
		return nil, "", fmt.Errorf("get case subscription: %w", err)
	}
	if cur.Items == nil || len(cur.Items.Data) != 1 || cur.Items.Data[0].Price == nil ||
		cur.Items.Data[0].Price.ID != fromPriceID || cur.Customer == nil {
		return nil, "", fmt.Errorf("case subscription does not have exactly one expected price item")
	}
	return cur, cur.Items.Data[0].ID, nil
}

// PreviewCaseTierChange is read-only: Stripe's invoice preview of the
// immediate proration invoice an upgrade with always_invoice would create.
func (p *StripeProvider) PreviewCaseTierChange(ctx context.Context, subscriptionID, fromPriceID, newPriceID string, prorationDate int64) (*CaseProrationPreview, error) {
	cur, itemID, err := caseSingleItem(ctx, subscriptionID, fromPriceID)
	if err != nil {
		return nil, err
	}
	inv, err := stripeinvoice.CreatePreview(&stripe.InvoiceCreatePreviewParams{
		Params:       *withCtx(ctx),
		Customer:     stripe.String(cur.Customer.ID),
		Subscription: stripe.String(subscriptionID),
		SubscriptionDetails: &stripe.InvoiceCreatePreviewSubscriptionDetailsParams{
			Items:             []*stripe.InvoiceCreatePreviewSubscriptionDetailsItemParams{{ID: stripe.String(itemID), Price: stripe.String(newPriceID)}},
			ProrationBehavior: stripe.String("always_invoice"),
			ProrationDate:     stripe.Int64(prorationDate),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("preview case proration: %w", err)
	}
	return &CaseProrationPreview{AmountDue: inv.AmountDue, Currency: string(inv.Currency)}, nil
}

// ChangeCaseTier swaps the single price. An upgrade (Charge) invoices the
// proration now with pending_if_incomplete: if that payment fails Stripe
// keeps the old price, so an unpaid upgrade can never take effect. A
// downgrade/restore uses proration_behavior=none: no charge and no credit.
func (p *StripeProvider) ChangeCaseTier(ctx context.Context, in CaseTierChangeInput) (*CaseTierChangeResult, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("case tier change requires an idempotency key")
	}
	_, itemID, err := caseSingleItem(ctx, in.SubscriptionID, in.FromPriceID)
	if err != nil {
		return nil, err
	}
	params := &stripe.SubscriptionParams{
		Params: *withCtx(ctx),
		Items:  []*stripe.SubscriptionItemsParams{{ID: stripe.String(itemID), Price: stripe.String(in.NewPriceID)}},
	}
	if in.Charge {
		params.ProrationBehavior = stripe.String("always_invoice")
		params.PaymentBehavior = stripe.String("pending_if_incomplete")
		params.ProrationDate = stripe.Int64(in.ProrationDate)
	} else {
		params.ProrationBehavior = stripe.String("none")
	}
	params.SetIdempotencyKey(in.IdempotencyKey)
	s, err := subscription.Update(in.SubscriptionID, params)
	if err != nil {
		return nil, fmt.Errorf("update case subscription tier: %w", err)
	}
	return &CaseTierChangeResult{State: normalizeSubscription(s), Pending: s.PendingUpdate != nil}, nil
}

// InvoiceForPaymentIntent resolves the invoice a PaymentIntent paid, through InvoicePayments (the only
// link in API 2026-08-26: charges, PaymentIntents and invoices carry no direct reference). Read-only.
func (p *StripeProvider) InvoiceForPaymentIntent(ctx context.Context, paymentIntentID string) (string, error) {
	if paymentIntentID == "" {
		return "", nil
	}
	params := &stripe.InvoicePaymentListParams{Payment: &stripe.InvoicePaymentListPaymentParams{
		Type: stripe.String("payment_intent"), PaymentIntent: stripe.String(paymentIntentID)}}
	params.Context = ctx
	found := ""
	it := stripeinvoicepayment.List(params)
	for it.Next() {
		ip := it.InvoicePayment()
		if ip.Invoice == nil || ip.Invoice.ID == "" {
			continue
		}
		if found != "" && found != ip.Invoice.ID {
			return "", fmt.Errorf("payment intent is linked to more than one invoice")
		}
		found = ip.Invoice.ID
	}
	if err := it.Err(); err != nil {
		return "", fmt.Errorf("list invoice payments: %w", err)
	}
	return found, nil
}

func normalizeCaseInvoice(inv *stripe.Invoice) *CaseInvoice {
	out := &CaseInvoice{ID: inv.ID, Status: string(inv.Status), Currency: string(inv.Currency), AmountPaid: inv.AmountPaid}
	if inv.Customer != nil {
		out.CustomerID = inv.Customer.ID
	}
	if inv.Parent != nil && inv.Parent.SubscriptionDetails != nil && inv.Parent.SubscriptionDetails.Subscription != nil {
		out.SubscriptionID = inv.Parent.SubscriptionDetails.Subscription.ID
	}
	if inv.Lines != nil {
		for _, l := range inv.Lines.Data {
			line := CaseInvoiceLine{Amount: l.Amount}
			if l.Pricing != nil && l.Pricing.PriceDetails != nil {
				line.PriceID = l.Pricing.PriceDetails.Price
			}
			if l.Parent != nil {
				line.SubscriptionItem = string(l.Parent.Type) == "subscription_item_details"
				if l.Parent.SubscriptionItemDetails != nil {
					line.SubscriptionID = l.Parent.SubscriptionItemDetails.Subscription
				}
			}
			if l.Period != nil {
				line.PeriodStart, line.PeriodEnd = l.Period.Start, l.Period.End
			}
			out.Lines = append(out.Lines, line)
		}
	}
	return out
}

// GetCaseInvoice reads one invoice (read-only). A single-item C.A.S.E. invoice fits the embedded lines.
func (p *StripeProvider) GetCaseInvoice(ctx context.Context, invoiceID string) (*CaseInvoice, error) {
	inv, err := stripeinvoice.Get(invoiceID, &stripe.InvoiceParams{Params: *withCtx(ctx)})
	if err != nil {
		return nil, fmt.Errorf("get invoice: %w", err)
	}
	return normalizeCaseInvoice(inv), nil
}

// ListCasePaidInvoices lists every paid invoice of one subscription (read-only), used once to
// reconstruct the coverage ledger of an add-on paid before migration 0064.
func (p *StripeProvider) ListCasePaidInvoices(ctx context.Context, subscriptionID string) ([]CaseInvoice, error) {
	params := &stripe.InvoiceListParams{Subscription: stripe.String(subscriptionID), Status: stripe.String("paid")}
	params.Context = ctx
	var out []CaseInvoice
	it := stripeinvoice.List(params)
	for it.Next() {
		out = append(out, *normalizeCaseInvoice(it.Invoice()))
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("list paid invoices: %w", err)
	}
	return out, nil
}

// CasePaymentState reads the live refund and dispute state of one PaymentIntent (read-only).
func (p *StripeProvider) CasePaymentState(ctx context.Context, paymentIntentID string) (*CasePaymentState, error) {
	st := &CasePaymentState{}
	cp := &stripe.ChargeListParams{PaymentIntent: stripe.String(paymentIntentID)}
	cp.Context = ctx
	ci := stripecharge.List(cp)
	for ci.Next() {
		c := ci.Charge()
		st.AmountCaptured += c.AmountCaptured
		st.AmountRefunded += c.AmountRefunded
	}
	if err := ci.Err(); err != nil {
		return nil, fmt.Errorf("list charges: %w", err)
	}
	dp := &stripe.DisputeListParams{PaymentIntent: stripe.String(paymentIntentID)}
	dp.Context = ctx
	di := stripedispute.List(dp)
	for di.Next() {
		st.DisputeStatuses = append(st.DisputeStatuses, string(di.Dispute().Status))
	}
	if err := di.Err(); err != nil {
		return nil, fmt.Errorf("list disputes: %w", err)
	}
	return st, nil
}
