package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stripewebhook "github.com/stripe/stripe-go/v82/webhook"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// replayCaseStore is an in-memory billing.CaseStore that mirrors the real
// case_addon_webhook_events primary key and counts every write path.
type replayCaseStore struct {
	seen    map[string]bool
	applied []repository.CaseWebhookState
	writes  []string // any mutation other than a first-time webhook application
}

func (s *replayCaseStore) ApplyCaseWebhookResult(_ context.Context, in repository.CaseWebhookState) (bool, error) {
	if s.seen[in.EventID] {
		return false, nil
	}
	s.seen[in.EventID] = true
	s.applied = append(s.applied, in)
	return true, nil
}
func (s *replayCaseStore) ReserveCaseCheckout(context.Context, int64, int64, int64, string, string) (*repository.CaseCheckoutReservation, error) {
	s.writes = append(s.writes, "ReserveCaseCheckout")
	return nil, repository.ErrCaseCheckoutConflict
}
func (s *replayCaseStore) StoreCaseCheckout(context.Context, int64, string, string) error {
	s.writes = append(s.writes, "StoreCaseCheckout")
	return nil
}
func (s *replayCaseStore) GetByCaseSubscriptionID(context.Context, string) (*repository.CaseAddonSubscription, error) {
	return nil, nil
}
func (s *replayCaseStore) ListByOrganization(context.Context, int64) ([]repository.CaseAddonSubscription, error) {
	return nil, nil
}
func (s *replayCaseStore) GetScoped(context.Context, int64, int64) (*repository.CaseAddonSubscription, error) {
	return nil, nil
}
func (s *replayCaseStore) SaveCaseCancelFlag(context.Context, int64, int64, string, bool) error {
	s.writes = append(s.writes, "SaveCaseCancelFlag")
	return nil
}
func (s *replayCaseStore) GetPendingCaseCheckout(context.Context, int64, int64) (*repository.CaseCheckoutReservation, error) {
	return nil, repository.ErrCaseCheckoutConflict
}
func (s *replayCaseStore) ResetExpiredCaseCheckout(context.Context, int64, int64, int64, int64, string) error {
	s.writes = append(s.writes, "ResetExpiredCaseCheckout")
	return nil
}
func (s *replayCaseStore) SaveCaseTierChange(context.Context, int64, int64, string, string, string, string) error {
	s.writes = append(s.writes, "SaveCaseTierChange")
	return nil
}

// Phase 6.22: an already-processed Stripe event redelivered to the real webhook route (as the
// Phase 6.21 staging replay did) is acknowledged with HTTP 200, takes the duplicate path, logs
// case_webhook_duplicate with its id and type, and never logs a fresh reconciliation or writes.
func TestCaseWebhookReplayThroughHTTPIsAcknowledgedDuplicateNoOp(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	const secret = "whsec_unit_replay"
	catalog, err := billing.LoadCatalog(`[{"key":"LOW","monthly":{"amountCents":599,"currency":"usd","stripePriceId":"price_base"}}]`)
	if err != nil {
		t.Fatal(err)
	}
	provider := billing.NewFakeProvider()
	meta := billing.CaseMetadata(billing.CaseCheckoutInput{AddonID: 1, OrganizationID: 1, InstallationID: 1, GameServerID: 1, Tier: casebilling.Watch})
	now := time.Now().UTC().Truncate(time.Second)
	provider.Put(billing.SubscriptionState{SubscriptionID: "sub_watch", CustomerID: "cus_case", PriceID: "price_watch",
		StripeStatus: "active", CurrentPeriodStart: now, CurrentPeriodEnd: now.Add(30 * 24 * time.Hour), Metadata: meta})
	svc := billing.NewService(nil, catalog, provider, billing.Options{WebhookSecret: secret})
	store := &replayCaseStore{seen: map[string]bool{}}
	if err := svc.ConfigureCaseAddons(store, billing.CaseOptions{
		PriceIDs: map[casebilling.Tier]string{casebilling.Watch: "price_watch", casebilling.Pro: "price_pro"},
	}); err != nil {
		t.Fatal(err)
	}
	a := &App{Billing: svc}

	metaJSON, _ := json.Marshal(meta)
	payload := []byte(fmt.Sprintf(`{"id":"evt_watch_replay","type":"checkout.session.completed","data":{"object":{
		"id":"cs_watch","mode":"subscription","customer":"cus_case","subscription":"sub_watch","metadata":%s}}}`, metaJSON))
	deliver := func() int {
		ts := time.Now()
		sig := fmt.Sprintf("t=%d,v1=%x", ts.Unix(), stripewebhook.ComputeSignature(ts, payload, secret))
		req := httptest.NewRequest(http.MethodPost, "/api/saas/billing/webhook", bytes.NewReader(payload))
		req.Header.Set("Stripe-Signature", sig)
		rr := httptest.NewRecorder()
		a.handleStripeWebhook(rr, req)
		return rr.Code
	}
	if code := deliver(); code != http.StatusOK {
		t.Fatalf("first delivery: HTTP %d", code)
	}
	if code := deliver(); code != http.StatusOK {
		t.Fatalf("replay must be acknowledged with 200 (never retried), got HTTP %d", code)
	}
	if len(store.applied) != 1 || len(store.writes) != 0 {
		t.Fatalf("replay must not mutate: applied=%d writes=%v", len(store.applied), store.writes)
	}
	var reconciled, duplicate []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m["stripe_event_id"] != "evt_watch_replay" {
			continue
		}
		switch m["event"] {
		case "case_subscription_reconciled":
			reconciled = append(reconciled, m)
		case "case_webhook_duplicate":
			duplicate = append(duplicate, m)
		}
	}
	if len(reconciled) != 1 {
		t.Fatalf("want exactly one fresh-reconciliation log (the first delivery), got %d:\n%s", len(reconciled), logs.String())
	}
	if len(duplicate) != 1 || duplicate[0]["type"] != "checkout.session.completed" || duplicate[0]["outcome"] != "no_op" {
		t.Fatalf("replay must log case_webhook_duplicate with id, type and outcome=no_op, got %v", duplicate)
	}
}
