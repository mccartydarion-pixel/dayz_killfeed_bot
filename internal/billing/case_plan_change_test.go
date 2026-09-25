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

type caseTierHarness struct {
	s        *Service
	provider *FakeProvider
	store    *caseTestStore
	base     *fakeStore
	now, end time.Time
}

// newCaseTierHarness: org 10 has an ACTIVE paid LOW base plan and one paid
// ACTIVE add-on (addon 8, installation 20, server 30) on the same customer.
func newCaseTierHarness(t *testing.T, tier casebilling.Tier, salesEnabled bool) *caseTierHarness {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	end := now.Add(20 * 24 * time.Hour)
	serverID := int64(30)
	base := newFakeStore()
	base.byOrg[10] = &repository.Subscription{
		OrganizationID: 10, Plan: "LOW", Status: repository.SubscriptionActive,
		Provider: repository.ProviderStripe, ProviderCustomerID: "cus_case",
		ProviderSubscriptionID: "sub_base", ProviderPriceID: "price_base", CurrentPeriodEnd: &end,
	}
	prices := map[casebilling.Tier]string{casebilling.Watch: "price_watch", casebilling.Pro: "price_pro"}
	row := &repository.CaseAddonSubscription{
		ID: 8, OrganizationID: 10, InstallationID: 20, GameServerID: serverID, SelectedGameServerID: &serverID,
		Tier: string(tier), PaidTier: string(tier), Status: "ACTIVE", Provider: "stripe",
		ProviderCustomerID: "cus_case", ProviderSubscriptionID: "sub_case", ProviderPriceID: prices[tier],
		CurrentPeriodEnd: &end, PaidThrough: &end,
	}
	store := &caseTestStore{row: row, reservation: &repository.CaseCheckoutReservation{
		ID: 8, Attempt: 1, OrganizationID: 10, InstallationID: 20, GameServerID: serverID,
		Tier: string(tier), ProviderCustomerID: "cus_case", SessionID: "cs_case"}}
	provider := NewFakeProvider()
	provider.Put(SubscriptionState{
		SubscriptionID: "sub_case", CustomerID: "cus_case", PriceID: prices[tier], StripeStatus: "active",
		CurrentPeriodStart: now.Add(-10 * 24 * time.Hour), CurrentPeriodEnd: end,
		Metadata: CaseMetadata(CaseCheckoutInput{AddonID: 8, OrganizationID: 10, InstallationID: 20, GameServerID: serverID, Tier: tier}),
	})
	s := NewService(base, nil, provider, Options{WebhookSecret: "whsec_unit_case"})
	if err := s.ConfigureCaseAddons(store, CaseOptions{Enabled: salesEnabled, AccessEnabled: true,
		VerifiedThrough: casebilling.Pro, PriceIDs: prices}); err != nil {
		t.Fatal(err)
	}
	return &caseTierHarness{s: s, provider: provider, store: store, base: base, now: now, end: end}
}

func (h *caseTierHarness) access(t *testing.T) []casebilling.Capability {
	t.Helper()
	caps, err := h.s.CaseAccess(context.Background(), 10, 20, 30, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return caps
}

func (h *caseTierHarness) calls(method string) []FakeCall {
	var out []FakeCall
	for _, c := range h.provider.Calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// applyToRow mirrors the repository's paid_tier rule for the in-memory row.
func (h *caseTierHarness) applyToRow(in repository.CaseWebhookState) {
	r := h.store.row
	r.Tier, r.ProviderPriceID, r.Status = in.Tier, in.PriceID, in.Status
	if in.PaidThrough != nil && (r.PaidThrough == nil || in.PaidThrough.After(*r.PaidThrough) ||
		(in.PaidThrough.Equal(*r.PaidThrough) && casebilling.Rank(casebilling.Tier(in.PaidTier)) > casebilling.Rank(casebilling.Tier(r.PaidTier)))) {
		r.PaidTier = in.PaidTier
		p := *in.PaidThrough
		r.PaidThrough = &p
	}
}

func caseInvoice(t *testing.T, lines string) *webhookInvoice {
	t.Helper()
	raw := []byte(`{"id":"in_case","status":"paid","customer":"cus_case","subscription":"sub_case","lines":{"data":[` + lines + `]}}`)
	var inv webhookInvoice
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	return &inv
}

func caseLineJSON(amount int64, price string, start, end time.Time) string {
	return `{"amount":` + strconv.FormatInt(amount, 10) + `,"parent":{"type":"subscription_item_details","subscription_item_details":{"subscription":"sub_case"}},"pricing":{"price_details":{"price":"` + price + `"}},"period":{"start":` + formatUnix(start) + `,"end":` + formatUnix(end) + `}}`
}

var watchOnly = []casebilling.Capability{casebilling.CapWatch}
var watchPro = []casebilling.Capability{casebilling.CapWatch, casebilling.CapPro}

func TestCaseUpgradeChargesProrationAndUnlocksOnlyOnPaidInvoice(t *testing.T) {
	h := newCaseTierHarness(t, casebilling.Watch, true)
	ctx := context.Background()
	h.provider.SetCaseUpgradeProration(333, false)
	if got := h.access(t); !reflect.DeepEqual(got, watchOnly) {
		t.Fatalf("paid Watch access = %v", got)
	}
	p, err := h.s.CasePreviewTierChange(ctx, 10, 20, "case_pro")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != CaseTierUpgrade || p.AmountDueNowCents != 333 || p.TargetTier != casebilling.Pro ||
		p.NextRenewalAmountCents != 999 || p.EffectiveAt != nil {
		t.Fatalf("upgrade preview: %+v", p)
	}
	if len(h.calls("ChangeCaseTier")) != 0 {
		t.Fatal("preview mutated Stripe")
	}
	out, err := h.s.CaseChangeTier(ctx, 10, 20, "CASE_PRO", p.ProrationDate)
	if err != nil || out.Pending || out.Kind != CaseTierUpgrade {
		t.Fatalf("upgrade: %+v %v", out, err)
	}
	changes := h.calls("ChangeCaseTier")
	in := changes[0].Arg.(CaseTierChangeInput)
	if len(changes) != 1 || !in.Charge || in.ProrationDate != p.ProrationDate || in.FromPriceID != "price_watch" ||
		in.NewPriceID != "price_pro" || in.IdempotencyKey == "" {
		t.Fatalf("upgrade Stripe call: %+v", changes)
	}
	if h.store.row.Tier != "CASE_PRO" || h.store.row.ProviderPriceID != "price_pro" {
		t.Fatalf("confirmed price swap not persisted: %+v", h.store.row)
	}
	// Billing tier moved; entitlement did not: Watch's payment is not Pro's.
	if got := h.access(t); !reflect.DeepEqual(got, watchOnly) {
		t.Fatalf("unpaid upgrade leaked Pro: %v", got)
	}
	// Stripe's immediate proration invoice: Pro charge + Watch credit, same end.
	inv := caseInvoice(t, caseLineJSON(-166, "price_watch", h.now, h.end)+","+caseLineJSON(499, "price_pro", h.now, h.end))
	if err := h.s.applyCaseEvent(ctx, ParsedEvent{ID: "evt_upgrade_paid", Type: EventInvoicePaid, Invoice: inv}); err != nil {
		t.Fatal(err)
	}
	last := h.store.applied[len(h.store.applied)-1]
	if last.Tier != "CASE_PRO" || last.PaidTier != "CASE_PRO" || last.PaidThrough == nil || !last.PaidThrough.Equal(h.end) {
		t.Fatalf("proration invoice did not prove Pro coverage: %+v", last)
	}
	h.applyToRow(last)
	if got := h.access(t); !reflect.DeepEqual(got, watchPro) {
		t.Fatalf("paid upgrade access = %v", got)
	}
	// The ORIGINAL Watch invoice for the same period, delivered late, proves
	// only Watch and must not lower paid coverage.
	late := caseInvoice(t, caseLineJSON(499, "price_watch", h.now.Add(-10*24*time.Hour), h.end))
	if err := h.s.applyCaseEvent(ctx, ParsedEvent{ID: "evt_late_watch", Type: EventInvoicePaid, Invoice: late}); err != nil {
		t.Fatalf("late pre-upgrade invoice rejected (Stripe would retry forever): %v", err)
	}
	last = h.store.applied[len(h.store.applied)-1]
	if last.PaidTier != "CASE_WATCH" {
		t.Fatalf("late invoice paid tier = %q", last.PaidTier)
	}
	h.applyToRow(last)
	if got := h.access(t); !reflect.DeepEqual(got, watchPro) {
		t.Fatalf("late Watch invoice revoked paid Pro: %v", got)
	}
	// Retrying the same confirmation reuses the same Stripe idempotency key
	// (the subscription is now on Pro, so it is refused before any write).
	if _, err := h.s.CaseChangeTier(ctx, 10, 20, "CASE_PRO", p.ProrationDate); !errors.Is(err, ErrCaseTierUnchanged) {
		t.Fatalf("double-confirm: %v", err)
	}
	if len(h.calls("ChangeCaseTier")) != 1 {
		t.Fatal("double-confirm issued a second Stripe mutation")
	}
	if h.base.byOrg[10].Plan != "LOW" || h.base.byOrg[10].ProviderSubscriptionID != "sub_base" {
		t.Fatalf("base subscription changed: %+v", h.base.byOrg[10])
	}
}

func TestCaseDeclinedUpgradeStaysPendingWithoutAccessChange(t *testing.T) {
	h := newCaseTierHarness(t, casebilling.Watch, true)
	ctx := context.Background()
	h.provider.SetCaseUpgradeProration(333, true)
	p, err := h.s.CasePreviewTierChange(ctx, 10, 20, "CASE_PRO")
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.s.CaseChangeTier(ctx, 10, 20, "CASE_PRO", p.ProrationDate)
	if err != nil || !out.Pending {
		t.Fatalf("declined upgrade: %+v %v", out, err)
	}
	if h.store.row.Tier != "CASE_WATCH" || h.store.row.ProviderPriceID != "price_watch" {
		t.Fatalf("pending upgrade changed local state: %+v", h.store.row)
	}
	if got := h.access(t); !reflect.DeepEqual(got, watchOnly) {
		t.Fatalf("declined upgrade changed access: %v", got)
	}
	// The failed proration invoice covers the already-paid period: it is a
	// stale failure (the repository keeps ACTIVE), and carries that period.
	failed := caseInvoice(t, caseLineJSON(-166, "price_watch", h.now, h.end)+","+caseLineJSON(499, "price_pro", h.now, h.end))
	failed.Status = "open"
	if err := h.s.applyCaseEvent(ctx, ParsedEvent{ID: "evt_upgrade_failed", Type: EventInvoicePaymentFailed, Invoice: failed}); err != nil {
		t.Fatal(err)
	}
	last := h.store.applied[len(h.store.applied)-1]
	if last.FailedPeriodEnd == nil || !last.FailedPeriodEnd.Equal(h.end) || last.PaidThrough != nil {
		t.Fatalf("failed upgrade invoice state: %+v", last)
	}
	if _, err := h.s.CasePreviewTierChange(ctx, 10, 20, "CASE_PRO"); !errors.Is(err, ErrCaseChangePending) {
		t.Fatalf("second upgrade while one is pending: %v", err)
	}
}

func TestCaseDowngradeKeepsPaidProUntilPeriodEndThenRestoreIsFree(t *testing.T) {
	h := newCaseTierHarness(t, casebilling.Pro, false) // sales paused: never strand subscribers
	ctx := context.Background()
	p, err := h.s.CasePreviewTierChange(ctx, 10, 20, "CASE_WATCH")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != CaseTierDowngrade || p.AmountDueNowCents != 0 || p.EffectiveAt == nil || !p.EffectiveAt.Equal(h.end) ||
		p.NextRenewalAmountCents != 499 {
		t.Fatalf("downgrade preview: %+v", p)
	}
	if len(h.calls("PreviewCaseTierChange")) != 0 {
		t.Fatal("downgrade asked Stripe for a proration preview")
	}
	if _, err := h.s.CaseChangeTier(ctx, 10, 20, "CASE_WATCH", p.ProrationDate); err != nil {
		t.Fatal(err)
	}
	in := h.calls("ChangeCaseTier")[0].Arg.(CaseTierChangeInput)
	if in.Charge {
		t.Fatal("downgrade created a charge/proration")
	}
	if h.store.row.Tier != "CASE_WATCH" || h.store.row.PaidTier != "CASE_PRO" {
		t.Fatalf("downgrade state: %+v", h.store.row)
	}
	if got := h.access(t); !reflect.DeepEqual(got, watchPro) {
		t.Fatalf("paid Pro removed before period end: %v", got)
	}
	// Changing their mind within the paid period costs nothing.
	p, err = h.s.CasePreviewTierChange(ctx, 10, 20, "CASE_PRO")
	if err != nil || p.Kind != CaseTierRestore || p.AmountDueNowCents != 0 {
		t.Fatalf("restore preview: %+v %v", p, err)
	}
	if _, err := h.s.CaseChangeTier(ctx, 10, 20, "CASE_PRO", p.ProrationDate); err != nil {
		t.Fatal(err)
	}
	if in := h.calls("ChangeCaseTier")[1].Arg.(CaseTierChangeInput); in.Charge {
		t.Fatal("restore of an already-paid tier charged again")
	}
	// Downgrade again, then the renewal invoice bills Watch: Pro ends.
	p, _ = h.s.CasePreviewTierChange(ctx, 10, 20, "CASE_WATCH")
	if _, err := h.s.CaseChangeTier(ctx, 10, 20, "CASE_WATCH", p.ProrationDate); err != nil {
		t.Fatal(err)
	}
	next := h.end.Add(30 * 24 * time.Hour)
	renewal := caseInvoice(t, caseLineJSON(499, "price_watch", h.end, next))
	if err := h.s.applyCaseEvent(ctx, ParsedEvent{ID: "evt_renewal", Type: EventInvoicePaid, Invoice: renewal}); err != nil {
		t.Fatal(err)
	}
	last := h.store.applied[len(h.store.applied)-1]
	h.applyToRow(last)
	if last.PaidTier != "CASE_WATCH" || h.store.row.PaidTier != "CASE_WATCH" {
		t.Fatalf("renewal paid tier: %+v", last)
	}
	if got := h.access(t); !reflect.DeepEqual(got, watchOnly) {
		t.Fatalf("after downgraded renewal: %v", got)
	}
}

func TestCaseTierChangeGuards(t *testing.T) {
	ctx := context.Background()
	h := newCaseTierHarness(t, casebilling.Watch, false)
	if _, err := h.s.CasePreviewTierChange(ctx, 10, 20, "CASE_PRO"); !errors.Is(err, ErrCaseDisabled) {
		t.Fatalf("upgrade with sales disabled: %v", err)
	}
	h = newCaseTierHarness(t, casebilling.Watch, true)
	for name, tc := range map[string]struct {
		org, inst int64
		tier      string
		want      error
	}{
		"same tier":        {10, 20, "CASE_WATCH", ErrCaseTierUnchanged},
		"unverified tier":  {10, 20, "CASE_COMMAND", ErrCaseNotPurchasable},
		"base plan key":    {10, 20, "LOW", ErrCaseNotPurchasable},
		"other org":        {11, 20, "CASE_PRO", ErrCaseNotManaged},
		"other install":    {10, 21, "CASE_PRO", ErrCaseNotManaged},
	} {
		if _, err := h.s.CasePreviewTierChange(ctx, tc.org, tc.inst, tc.tier); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	p, err := h.s.CasePreviewTierChange(ctx, 10, 20, "CASE_PRO")
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []int64{0, p.ProrationDate - int64(caseProrationWindow/time.Second) - 5, p.ProrationDate + 3600} {
		if _, err := h.s.CaseChangeTier(ctx, 10, 20, "CASE_PRO", stale); !errors.Is(err, ErrCasePreviewExpired) {
			t.Errorf("proration date %d: %v", stale, err)
		}
	}
	mutations := []struct {
		name string
		mut  func(*caseTierHarness)
		want error
	}{
		{"scheduled cancel", func(h *caseTierHarness) { h.store.row.CancelAtPeriodEnd = true }, ErrCaseCancelScheduled},
		{"founder trial", func(h *caseTierHarness) { h.store.row.Status = "TRIAL" }, ErrCaseNotManaged},
		{"past due", func(h *caseTierHarness) { h.store.row.Status = "PAST_DUE" }, ErrCaseNotManaged},
		{"repointed installation", func(h *caseTierHarness) { v := int64(31); h.store.row.SelectedGameServerID = &v }, ErrCaseNotManaged},
		{"no paid base", func(h *caseTierHarness) { h.base.byOrg[10].Status = repository.SubscriptionPastDue }, ErrCaseBaseRequired},
		{"forged Stripe binding", func(h *caseTierHarness) {
			s, _ := h.provider.GetSubscription(ctx, "sub_case")
			s.Metadata = CaseMetadata(CaseCheckoutInput{AddonID: 8, OrganizationID: 10, InstallationID: 20, GameServerID: 99, Tier: casebilling.Watch})
			h.provider.Put(*s)
		}, repository.ErrCaseWebhookMismatch},
	}
	for _, m := range mutations {
		h := newCaseTierHarness(t, casebilling.Watch, true)
		m.mut(h)
		if _, err := h.s.CasePreviewTierChange(ctx, 10, 20, "CASE_PRO"); !errors.Is(err, m.want) {
			t.Errorf("%s: %v, want %v", m.name, err, m.want)
		}
		if len(h.calls("ChangeCaseTier")) != 0 {
			t.Errorf("%s: Stripe mutated", m.name)
		}
	}
}

// After a tier change the Stripe metadata still names the ORIGINAL tier; the
// webhook and cancellation must follow the configured price instead, and an
// unconfigured price must fail closed.
func TestCaseTierFollowsConfiguredPriceNotMutableMetadata(t *testing.T) {
	h := newCaseTierHarness(t, casebilling.Watch, true)
	ctx := context.Background()
	s, _ := h.provider.GetSubscription(ctx, "sub_case")
	s.PriceID = "price_pro" // e.g. changed and confirmed at Stripe
	h.provider.Put(*s)
	sub := &webhookSubscription{ID: "sub_case", Status: "active", Customer: "cus_case", Metadata: s.Metadata}
	if err := h.s.applyCaseEvent(ctx, ParsedEvent{ID: "evt_sub_updated", Type: EventSubscriptionUpdated, Sub: sub}); err != nil {
		t.Fatal(err)
	}
	last := h.store.applied[len(h.store.applied)-1]
	if last.Tier != "CASE_PRO" || last.PriceID != "price_pro" || last.PaidThrough != nil {
		t.Fatalf("subscription update: %+v", last)
	}
	h.applyToRow(last)
	if _, err := h.s.CaseSetCancellation(ctx, 10, 20, true); err != nil {
		t.Fatalf("cancel after tier change: %v", err)
	}
	s.PriceID = "price_unknown"
	h.provider.Put(*s)
	if err := h.s.applyCaseEvent(ctx, ParsedEvent{ID: "evt_bad_price", Type: EventSubscriptionUpdated, Sub: sub}); !errors.Is(err, repository.ErrCaseWebhookMismatch) {
		t.Fatalf("unconfigured price accepted: %v", err)
	}
	// A negative (credit) line can never prove paid coverage.
	s.PriceID = "price_pro"
	h.provider.Put(*s)
	credit := caseInvoice(t, caseLineJSON(-499, "price_pro", h.now, h.end.Add(time.Hour)))
	if err := h.s.applyCaseEvent(ctx, ParsedEvent{ID: "evt_credit", Type: EventInvoicePaid, Invoice: credit}); !errors.Is(err, repository.ErrCaseWebhookMismatch) {
		t.Fatalf("credit-only invoice granted coverage: %v", err)
	}
}
