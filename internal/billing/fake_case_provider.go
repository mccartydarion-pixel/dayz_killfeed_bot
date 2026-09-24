package billing

import (
	"context"

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
	return &CheckoutSession{ID:id,URL:"https://checkout.stripe.example/test/"+id},nil
}

var _ CaseProvider = (*FakeProvider)(nil)
