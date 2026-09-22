package billing

import "github.com/yourname/dayz-killfeed/internal/repository"

// MapStatus translates a Stripe subscription status string into Champion's existing five-value
// subscription status (repository.Subscription*) - the raw Stripe value is never returned by an
// API response (docs/BILLING.md "Subscription status mapping").
//
//	trialing                        -> TRIAL
//	active                          -> ACTIVE
//	past_due                        -> PAST_DUE
//	canceled                        -> CANCELED
//	unpaid, incomplete_expired      -> PAST_DUE  (see docs/BILLING.md "Payment failure handling":
//	                                    entitlement/grace-period policy for this state is a
//	                                    DECISION REQUIRED - Phase 1 does not suspend anything)
//	incomplete                      -> PAST_DUE  (the first invoice hasn't been paid yet)
//	paused                          -> SUSPENDED
//	anything else (future Stripe status) -> PAST_DUE, so an unrecognized value fails toward "needs
//	                                    attention" rather than toward silently-still-active
func MapStatus(stripeStatus string) string {
	switch stripeStatus {
	case "trialing":
		return repository.SubscriptionTrial
	case "active":
		return repository.SubscriptionActive
	case "past_due", "incomplete", "incomplete_expired", "unpaid":
		return repository.SubscriptionPastDue
	case "canceled":
		return repository.SubscriptionCanceled
	case "paused":
		return repository.SubscriptionSuspended
	default:
		return repository.SubscriptionPastDue
	}
}

// MapInterval translates a Stripe Price recurring interval ("month"/"year") into Champion's billing
// interval constants. Anything else (day/week, or a one-time price that should never be on a
// subscription) returns "" - the caller must not overwrite a known interval with an empty one.
func MapInterval(stripeInterval string) string {
	switch stripeInterval {
	case "month":
		return repository.BillingIntervalMonthly
	case "year":
		return repository.BillingIntervalYearly
	}
	return ""
}
