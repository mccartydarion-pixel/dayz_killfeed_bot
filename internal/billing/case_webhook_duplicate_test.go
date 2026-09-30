package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

// Phase 6.21 staging replay: Stripe redelivered an already-processed Watch event, state stayed
// unchanged, but the log said case_subscription_reconciled - indistinguishable from a first
// delivery. A replay must log case_webhook_duplicate (outcome=no_op) and apply nothing.
func TestCaseWebhookReplayLogsDuplicateNoOp(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	s, provider, store := newCaseTestBilling(t)
	store.reservation.Tier = string(casebilling.Watch)
	now := time.Now().UTC().Truncate(time.Second)
	meta := CaseMetadata(CaseCheckoutInput{AddonID: 8, OrganizationID: 10, InstallationID: 20, GameServerID: 30, Tier: casebilling.Watch})
	provider.Put(SubscriptionState{SubscriptionID: "sub_case", CustomerID: "cus_case", PriceID: "price_watch",
		StripeStatus: "active", CurrentPeriodStart: now, CurrentPeriodEnd: now.Add(30 * 24 * time.Hour), Metadata: meta})
	event := ParsedEvent{ID: "evt_watch_checkout", Type: EventCheckoutCompleted,
		Session: &webhookCheckoutSession{ID: "cs_case", Mode: "subscription", Subscription: "sub_case", Customer: "cus_case", Metadata: meta}}

	for i := 0; i < 2; i++ { // original delivery, then the replay
		if err := s.applyCaseEvent(context.Background(), event); err != nil {
			t.Fatalf("delivery %d must be acknowledged: %v", i+1, err)
		}
	}
	if len(store.applied) != 1 {
		t.Fatalf("replay applied state again: %d applications", len(store.applied))
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["stripe_event_id"] == "evt_watch_checkout" {
			events = append(events, m)
		}
	}
	if len(events) != 2 {
		t.Fatalf("want exactly 2 log lines for the event, got %d: %s", len(events), buf.String())
	}
	if events[0]["event"] != "case_subscription_reconciled" {
		t.Fatalf("first delivery log: %v", events[0])
	}
	if events[1]["event"] != "case_webhook_duplicate" || events[1]["outcome"] != "no_op" {
		t.Fatalf("replay must log case_webhook_duplicate/no_op, got %v", events[1])
	}
	if _, claimsState := events[1]["status"]; claimsState {
		t.Fatalf("duplicate log must not report a reconciled status: %v", events[1])
	}
}
