package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Trial grants (Champion Customer Onboarding V2, docs/BILLING.md "No-card trial"): every Discord
// account gets ONE 14-day trial, with no payment method. trial_grants(user_id PRIMARY KEY,
// organization_id UNIQUE) is the database-enforced record of it, so neither a second organization
// nor a concurrent retry can mint a second trial.

// StartTrial grants userID's one trial to organizationID, if both are still eligible, and returns
// the organization's subscription row afterwards. granted reports whether this call started the
// trial. It never restarts, extends or replaces anything:
//
//   - a row already in TRIAL (running or expired), or holding any Stripe subscription, is returned
//     unchanged - an existing trial clock is never touched and a paid row is never overwritten;
//   - a user who already used their trial (on any organization) gets an INACTIVE row (billing
//     required) instead of a trial;
//   - an organization that was ever trialed is never trialed again, whoever asks.
//
// One transaction: the subscription row is locked, then the grant is inserted with ON CONFLICT DO
// NOTHING - the grant's own uniqueness decides the race, never a read-then-write check.
func (r *SubscriptionRepository) StartTrial(ctx context.Context, organizationID, userID int64, trialEndsAt time.Time) (sub *Subscription, granted bool, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin start trial: %w", err)
	}
	defer tx.Rollback(ctx)

	existing, err := scanSubscription(tx.QueryRow(ctx, `SELECT `+subscriptionCols+` FROM subscriptions WHERE organization_id=$1 FOR UPDATE`, organizationID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, false, fmt.Errorf("lock subscription: %w", err)
	case !trialableRow(existing):
		return &existing, false, tx.Commit(ctx)
	}

	var grantedUser int64
	err = tx.QueryRow(ctx, `INSERT INTO trial_grants(user_id, organization_id) VALUES($1,$2) ON CONFLICT DO NOTHING RETURNING user_id`, userID, organizationID).Scan(&grantedUser)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not eligible: the user (or this organization) already had its trial.
		out, err := scanSubscription(tx.QueryRow(ctx, `
INSERT INTO subscriptions(organization_id, plan, status) VALUES($1,$2,$3)
ON CONFLICT(organization_id) DO UPDATE SET organization_id=subscriptions.organization_id
RETURNING `+subscriptionCols, organizationID, PlanNone, SubscriptionInactive))
		if err != nil {
			return nil, false, fmt.Errorf("ensure inactive subscription: %w", err)
		}
		return &out, false, tx.Commit(ctx)
	}
	if err != nil {
		return nil, false, fmt.Errorf("record trial grant: %w", err)
	}

	out, err := scanSubscription(tx.QueryRow(ctx, `
INSERT INTO subscriptions(organization_id, plan, status, trial_ends_at) VALUES($1,$2,$3,$4)
ON CONFLICT(organization_id) DO UPDATE SET plan=EXCLUDED.plan, status=EXCLUDED.status, trial_ends_at=EXCLUDED.trial_ends_at, updated_at=NOW()
RETURNING `+subscriptionCols, organizationID, SubscriptionTrial, SubscriptionTrial, trialEndsAt))
	if err != nil {
		return nil, false, fmt.Errorf("start trial subscription: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit start trial: %w", err)
	}
	return &out, true, nil
}

// trialableRow reports whether an existing subscription row may still become a trial: only a
// never-trialed, never-subscribed INACTIVE row.
func trialableRow(s Subscription) bool {
	return s.Status == SubscriptionInactive && s.TrialEndsAt == nil && !s.TrialConsumed && s.ProviderSubscriptionID == ""
}

// EnsureInactive creates organizationID's subscription row as INACTIVE (no trial, billing
// required) if none exists, and returns the row either way. Used before a first checkout for an
// organization that never had a trial row; it never alters an existing row.
func (r *SubscriptionRepository) EnsureInactive(ctx context.Context, organizationID int64) (*Subscription, error) {
	out, err := scanSubscription(r.pool.QueryRow(ctx, `
INSERT INTO subscriptions(organization_id, plan, status) VALUES($1,$2,$3)
ON CONFLICT(organization_id) DO UPDATE SET organization_id=subscriptions.organization_id
RETURNING `+subscriptionCols, organizationID, PlanNone, SubscriptionInactive))
	if err != nil {
		return nil, fmt.Errorf("ensure inactive subscription: %w", err)
	}
	return &out, nil
}

// SetIntendedPlan records the plan a customer picked before paying. Only rows without a Stripe
// subscription are touched (a paid row's plan comes from webhooks); returns the row afterwards,
// or nil when the organization has no row.
func (r *SubscriptionRepository) SetIntendedPlan(ctx context.Context, organizationID int64, planKey string) (*Subscription, error) {
	out, err := scanSubscription(r.pool.QueryRow(ctx, `
UPDATE subscriptions SET intended_plan=$2, updated_at=NOW()
WHERE organization_id=$1 AND COALESCE(provider_subscription_id,'')=''
RETURNING `+subscriptionCols, organizationID, planKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return r.GetForOrganization(ctx, organizationID)
	}
	if err != nil {
		return nil, fmt.Errorf("set intended plan: %w", err)
	}
	return &out, nil
}

// HasTrialGrant reports whether userID has already used their one free trial.
func (r *SubscriptionRepository) HasTrialGrant(ctx context.Context, userID int64) (bool, error) {
	var ok bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM trial_grants WHERE user_id=$1)`, userID).Scan(&ok); err != nil {
		return false, fmt.Errorf("check trial grant: %w", err)
	}
	return ok, nil
}
