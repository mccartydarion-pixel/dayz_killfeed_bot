package billing

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Customer Onboarding V2 (docs/BILLING.md "No-card trial"): one 14-day trial per Discord
// account, started with no payment method and no Stripe object at all. Stripe is only involved
// when the customer explicitly activates a paid plan, and that checkout never carries a second
// Stripe trial.

// TrialDays is the length of the one no-card trial.
const TrialDays = 14

// TrialInstallations is how many installations (services) a trial may run.
const TrialInstallations = 1

// DefaultPlanInstallations is the capacity of a paid plan whose catalog entry sets no
// limits.installations.
const DefaultPlanInstallations = 1

// Trial statuses returned by the trial API.
const (
	TrialNotStarted  = "NOT_STARTED"  // no subscription row yet - the trial can be started
	TrialActive      = "ACTIVE"       // running no-card trial
	TrialExpired     = "EXPIRED"      // trial over, no paid subscription: billing required
	TrialNotEligible = "NOT_ELIGIBLE" // the account already used its trial: billing required
	TrialConverted   = "CONVERTED"    // a Stripe subscription exists (paid, past due, or ended)
)

// ErrTrialStoreUnavailable is returned when the Store cannot start trials.
var ErrTrialStoreUnavailable = errors.New("trial store unavailable")

// TrialState is the persisted onboarding state the website renders.
type TrialState struct {
	TrialStatus           string
	SubscriptionStatus    string
	SelectedPlan          string // intended plan while not yet paying; the paid plan once converted
	TrialStartedAt        *time.Time
	TrialEndsAt           *time.Time
	DaysRemaining         int
	BillingRequired       bool
	PaymentMethodRequired bool // always false: starting the trial never needs a payment method
	Started               bool // this request started the trial
}

// StateOf derives the onboarding state from a subscription row (nil = no row yet).
func StateOf(sub *repository.Subscription, now time.Time) TrialState {
	if sub == nil {
		return TrialState{TrialStatus: TrialNotStarted, BillingRequired: true}
	}
	st := TrialState{SubscriptionStatus: sub.Status, SelectedPlan: sub.IntendedPlan, TrialEndsAt: sub.TrialEndsAt}
	if sub.TrialEndsAt != nil {
		start := sub.TrialEndsAt.AddDate(0, 0, -TrialDays)
		st.TrialStartedAt = &start
	}
	switch {
	case sub.ProviderSubscriptionID != "":
		st.TrialStatus = TrialConverted
		st.SelectedPlan = sub.Plan
		st.TrialStartedAt, st.TrialEndsAt = nil, nil
		if sub.Status == repository.SubscriptionTrial { // a legacy Stripe trial still running
			st.TrialEndsAt = sub.TrialEndsAt
		}
		st.BillingRequired = sub.Status == repository.SubscriptionCanceled || sub.Status == repository.SubscriptionSuspended
	case sub.Status == repository.SubscriptionTrial:
		if sub.TrialEndsAt != nil && !now.Before(*sub.TrialEndsAt) {
			st.TrialStatus, st.BillingRequired = TrialExpired, true
		} else {
			st.TrialStatus = TrialActive
			if sub.TrialEndsAt != nil {
				st.DaysRemaining = int(math.Ceil(sub.TrialEndsAt.Sub(now).Hours() / 24))
			}
		}
	case sub.Status == repository.SubscriptionInactive:
		st.TrialStatus, st.BillingRequired = TrialNotEligible, true
		if sub.TrialEndsAt != nil {
			st.TrialStatus = TrialExpired
		}
	default:
		// ACTIVE/PAST_DUE/CANCELED/SUSPENDED without a Stripe subscription id is not produced by
		// the webhook path; treat it by status alone.
		st.TrialStatus = TrialConverted
		st.SelectedPlan = sub.Plan
		st.BillingRequired = sub.Status == repository.SubscriptionCanceled || sub.Status == repository.SubscriptionSuspended
	}
	return st
}

// InstallationLimit is how many installations the organization's current subscription allows.
// A trial allows TrialInstallations; a paid plan uses its catalog limits.installations
// (DefaultPlanInstallations when unset). catalog may be nil (billing not configured).
func InstallationLimit(sub *repository.Subscription, catalog *Catalog) int {
	if sub == nil || sub.ProviderSubscriptionID == "" {
		return TrialInstallations
	}
	if catalog != nil {
		if p, ok := catalog.Get(sub.Plan); ok {
			if n := p.Limits["installations"]; n > 0 {
				return n
			}
		}
	}
	return DefaultPlanInstallations
}

// TrialStore is the persistence StartTrial needs; *repository.SubscriptionRepository implements it.
type TrialStore interface {
	StartTrial(ctx context.Context, organizationID, userID int64, trialEndsAt time.Time) (*repository.Subscription, bool, error)
	SetIntendedPlan(ctx context.Context, organizationID int64, planKey string) (*repository.Subscription, error)
}

// StartTrial starts userID's one no-card trial on organizationID, or returns the existing state
// unchanged: a running trial keeps its clock, an expired trial is not restarted, a paid
// subscription is never replaced. planKey (optional) is validated against the public catalog and
// recorded as the intended plan - no checkout, no Stripe call, no charge.
func StartTrial(ctx context.Context, store TrialStore, catalog *Catalog, organizationID, userID int64, planKey string, now time.Time) (TrialState, error) {
	if store == nil {
		return TrialState{}, ErrTrialStoreUnavailable
	}
	planKey = strings.ToUpper(strings.TrimSpace(planKey))
	if planKey != "" {
		if catalog == nil {
			return TrialState{}, ErrUnknownPlan
		}
		if p, ok := catalog.Get(planKey); !ok || !p.IsPublic {
			return TrialState{}, ErrUnknownPlan
		}
	}
	sub, started, err := store.StartTrial(ctx, organizationID, userID, now.AddDate(0, 0, TrialDays))
	if err != nil {
		return TrialState{}, err
	}
	if planKey != "" && sub != nil && sub.ProviderSubscriptionID == "" && sub.IntendedPlan != planKey {
		if sub, err = store.SetIntendedPlan(ctx, organizationID, planKey); err != nil {
			return TrialState{}, err
		}
	}
	st := StateOf(sub, now)
	st.Started = started
	return st, nil
}
