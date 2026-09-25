package billing

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCASECanCancelAndReactivateOnlyItsOwnSubscription(t *testing.T) {
	s, provider, store := newCaseTestBilling(t)
	now := time.Now().UTC()
	end := now.Add(time.Hour)
	store.row = &repository.CaseAddonSubscription{
		ID: 8, OrganizationID: 10, InstallationID: 20, GameServerID: 30,
		Tier: string(casebilling.Pro), Status: "ACTIVE", Provider: "stripe",
		ProviderCustomerID: "cus_case", ProviderSubscriptionID: "sub_case",
		ProviderPriceID: "price_pro", CurrentPeriodEnd: &end, PaidThrough: &end,
	}
	meta := CaseMetadata(CaseCheckoutInput{AddonID: 8, OrganizationID: 10,
		InstallationID: 20, GameServerID: 30, Tier: casebilling.Pro})
	provider.Put(SubscriptionState{SubscriptionID: "sub_case", CustomerID: "cus_case",
		PriceID: "price_pro", StripeStatus: "active", CurrentPeriodEnd: end, Metadata: meta})
	// Sales being turned off must NOT strand an existing paying customer.
	s.caseEnabled = false
	row, err := s.CaseSetCancellation(context.Background(), 10, 20, true)
	if err != nil || row == nil || !row.CancelAtPeriodEnd {
		t.Fatalf("cancel failed: %+v %v", row, err)
	}
	if row.PaidThrough == nil || !row.PaidThrough.Equal(end) {
		t.Fatal("cancel should preserve paid coverage")
	}
	row, err = s.CaseSetCancellation(context.Background(), 10, 20, false)
	if err != nil || row == nil || row.CancelAtPeriodEnd {
		t.Fatalf("reactivate failed: %+v %v", row, err)
	}
	for _, scope := range [][2]int64{{11, 20}, {10, 21}} {
		if _, err := s.CaseSetCancellation(context.Background(), scope[0], scope[1], true); !errors.Is(err, ErrCaseNotManaged) {
			t.Fatalf("foreign scope %v may manage subscription: %v", scope, err)
		}
	}
	foreign := CaseMetadata(CaseCheckoutInput{AddonID: 8, OrganizationID: 11,
		InstallationID: 20, GameServerID: 30, Tier: casebilling.Pro})
	provider.Put(SubscriptionState{SubscriptionID: "sub_case", CustomerID: "cus_case",
		PriceID: "price_pro", StripeStatus: "active", CurrentPeriodEnd: end, Metadata: foreign})
	if _, err := s.CaseSetCancellation(context.Background(), 10, 20, true); !errors.Is(err, repository.ErrCaseWebhookMismatch) {
		t.Fatalf("mismatched Stripe metadata allowed cancel: %v", err)
	}
}

func TestCASEActiveRequiresSignedInvoicePaidCoverage(t *testing.T) {
	s, provider, store := newCaseTestBilling(t)
	now := time.Now().UTC().Truncate(time.Second)
	end := now.Add(30 * 24 * time.Hour)
	meta := CaseMetadata(CaseCheckoutInput{
		AddonID: 8, OrganizationID: 10, InstallationID: 20, GameServerID: 30, Tier: casebilling.Pro,
	})
	provider.Put(SubscriptionState{
		SubscriptionID: "sub_case", CustomerID: "cus_case", PriceID: "price_pro",
		StripeStatus: "active", CurrentPeriodStart: now, CurrentPeriodEnd: end,
		Metadata: meta,
	})
	checkout := ParsedEvent{ID: "evt_case_checkout", Type: EventCheckoutCompleted,
		Session: &webhookCheckoutSession{ID: "cs_case", Mode: "subscription",
			Subscription: "sub_case", Customer: "cus_case", Metadata: meta}}
	if err := s.applyCaseEvent(context.Background(), checkout); err != nil { t.Fatal(err) }
	if len(store.applied) != 1 || store.applied[0].PaidThrough != nil {
		t.Fatalf("checkout incorrectly claimed invoice was paid: %+v", store.applied)
	}
	// Current Stripe Invoice uses parent.subscription_details.subscription.
	raw := []byte(`{"id":"in_paid","status":"paid","customer":"cus_case",
	"parent":{"subscription_details":{"subscription":"sub_case"}},
	"lines":{"data":[{"period":{"start":` + formatUnix(now) + `,"end":` + formatUnix(end) + `}}]}}`)
	var invoice webhookInvoice
	if err := json.Unmarshal(raw, &invoice); err != nil { t.Fatal(err) }
	if invoice.Subscription == "" { invoice.Subscription = invoice.Parent.SubscriptionDetails.Subscription }
	paid := ParsedEvent{ID: "evt_case_paid", Type: EventInvoicePaid, Invoice: &invoice}
	if err := s.applyCaseEvent(context.Background(), paid); err != nil {t.Fatal(err)}
	if len(store.applied) != 2 || store.applied[1].PaidThrough == nil || !store.applied[1].PaidThrough.Equal(end) {
		t.Fatalf("invoice.paid coverage mismatch: %+v", store.applied)
	}
	if got := []string{store.applied[0].Status, store.applied[1].Status}; !reflect.DeepEqual(got, []string{"ACTIVE", "ACTIVE"}) {
		t.Fatalf("expected active statuses but separate payment proof: %v", got)
	}
	invoice.Lines.Data = nil
	paid.ID = "evt_no_period"
	if err := s.applyCaseEvent(context.Background(), paid); !errors.Is(err, repository.ErrCaseWebhookMismatch) {
		t.Fatalf("invoice without coverage period was accepted: %v", err)
	}
}

func formatUnix(t time.Time) string {
	return strconv.FormatInt(t.Unix(),10)
}
