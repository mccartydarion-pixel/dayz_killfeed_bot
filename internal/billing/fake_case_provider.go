package billing

import (
	"context"
	"fmt"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

func (f *FakeProvider) ValidateCasePrice(_ context.Context, priceID string, amount int64, tier casebilling.Tier) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls=append(f.Calls,FakeCall{"ValidateCasePrice",struct{PriceID string; Amount int64; Tier casebilling.Tier}{priceID,amount,tier}})
	return nil
}

func (f *FakeProvider) CreateCaseCheckoutSession(_ context.Context, in CaseCheckoutInput) (*CheckoutSession,error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls=append(f.Calls,FakeCall{"CreateCaseCheckoutSession",in})
	id:=f.next("cs_case")
	if f.caseSessions==nil{f.caseSessions=map[string]*CaseCheckoutSessionState{}}
	f.caseSessions[id]=&CaseCheckoutSessionState{ID:id,Status:"open",CustomerID:in.CustomerID,Metadata:CaseMetadata(in)}
	return &CheckoutSession{ID:id,URL:"https://checkout.stripe.example/test/"+id},nil
}

// PutCaseCheckout is test-only setup, not a production Stripe operation.
func (f *FakeProvider) PutCaseCheckout(s CaseCheckoutSessionState) {
	f.mu.Lock();defer f.mu.Unlock()
	if f.caseSessions==nil{f.caseSessions=map[string]*CaseCheckoutSessionState{}}
	cp:=s
	f.caseSessions[s.ID]=&cp
}

func (f *FakeProvider) GetCaseCheckoutSession(_ context.Context,id string)(*CaseCheckoutSessionState,error){
	f.mu.Lock();defer f.mu.Unlock()
	s,ok:=f.caseSessions[id]
	if !ok{return nil,fmt.Errorf("fake checkout not found")}
	cp:=*s
	return &cp,nil
}

var _ CaseProvider = (*FakeProvider)(nil)

// Test knobs for tier changes: the Stripe-calculated proration a preview
// returns, and whether the next charged upgrade's payment is declined
// (Stripe then leaves the change pending and the old price in place).
func (f *FakeProvider) SetCaseUpgradeProration(amountDue int64, declineNext bool) {
	f.mu.Lock(); defer f.mu.Unlock()
	f.caseProration = amountDue
	f.caseDeclineNext = declineNext
}

func (f *FakeProvider) PreviewCaseTierChange(_ context.Context, subID, fromPrice, newPrice string, prorationDate int64) (*CaseProrationPreview, error) {
	f.mu.Lock(); defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{"PreviewCaseTierChange", struct{ SubscriptionID, From, To string; ProrationDate int64 }{subID, fromPrice, newPrice, prorationDate}})
	s, ok := f.subs[subID]
	if !ok || s.PriceID != fromPrice {
		return nil, fmt.Errorf("fake case subscription not on expected price")
	}
	return &CaseProrationPreview{AmountDue: f.caseProration, Currency: "usd"}, nil
}

func (f *FakeProvider) ChangeCaseTier(_ context.Context, in CaseTierChangeInput) (*CaseTierChangeResult, error) {
	f.mu.Lock(); defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeCall{"ChangeCaseTier", in})
	s, ok := f.subs[in.SubscriptionID]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}
	if s.PriceID != in.FromPriceID {
		return nil, fmt.Errorf("fake case subscription not on expected price")
	}
	if in.Charge && f.caseDeclineNext {
		f.caseDeclineNext = false
		s.PendingUpdate = true
		cp := *s
		return &CaseTierChangeResult{State: &cp, Pending: true}, nil
	}
	s.PriceID = in.NewPriceID
	cp := *s
	return &CaseTierChangeResult{State: &cp}, nil
}
