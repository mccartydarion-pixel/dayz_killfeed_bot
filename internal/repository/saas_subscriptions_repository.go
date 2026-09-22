package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Subscription statuses. Enforced at the application layer (see the role/
// installation-status constants elsewhere in this package for the same
// convention). Champion Billing Phase 1 (docs/BILLING.md) maps Stripe's
// subscription statuses onto these five; provider fields stay empty until an
// organization actually checks out.
const (
	SubscriptionTrial     = "TRIAL"
	SubscriptionActive    = "ACTIVE"
	SubscriptionPastDue   = "PAST_DUE"
	SubscriptionCanceled  = "CANCELED"
	SubscriptionSuspended = "SUSPENDED"
)

// Billing intervals (docs/BILLING.md). YEARLY is designed for but not necessarily priced yet - see
// the plan catalog.
const (
	BillingIntervalMonthly = "MONTHLY"
	BillingIntervalYearly  = "YEARLY"
)

// ProviderStripe is the only billing provider integrated so far.
const ProviderStripe = "stripe"

// ErrSubscriptionNotFound is returned by ApplyProviderState when organizationID has no
// subscription row (should not happen: every organization gets one via EnsureTrial at creation).
var ErrSubscriptionNotFound = errors.New("subscription not found")

type Subscription struct {
	ID                                                   int64
	OrganizationID                                       int64
	Provider, ProviderCustomerID, ProviderSubscriptionID string
	ProviderPriceID                                      string
	Plan, Status                                         string
	BillingInterval                                      string
	TrialEndsAt, CurrentPeriodStart, CurrentPeriodEnd    *time.Time
	CancelAtPeriodEnd                                    bool
	CanceledAt                                           *time.Time
	TrialConsumed                                        bool
	CreatedAt, UpdatedAt                                 time.Time
}

const subscriptionCols = `id, organization_id, COALESCE(provider,''), COALESCE(provider_customer_id,''), COALESCE(provider_subscription_id,''), COALESCE(provider_price_id,''),
plan, status, COALESCE(billing_interval,''), trial_ends_at, current_period_start, current_period_end, cancel_at_period_end, canceled_at, trial_consumed, created_at, updated_at`

func scanSubscription(row pgx.Row) (Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.OrganizationID, &s.Provider, &s.ProviderCustomerID, &s.ProviderSubscriptionID, &s.ProviderPriceID,
		&s.Plan, &s.Status, &s.BillingInterval, &s.TrialEndsAt, &s.CurrentPeriodStart, &s.CurrentPeriodEnd, &s.CancelAtPeriodEnd, &s.CanceledAt, &s.TrialConsumed, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

type SubscriptionRepository struct{ pool *pgxpool.Pool }

func NewSubscriptionRepository(pool *pgxpool.Pool) *SubscriptionRepository {
	return &SubscriptionRepository{pool: pool}
}

// EnsureTrial creates organizationID's subscription row in TRIAL status if
// none exists yet, so GetForOrganization never has to special-case a
// brand-new organization. Idempotent: safe to call more than once.
func (r *SubscriptionRepository) EnsureTrial(ctx context.Context, organizationID int64, trialEndsAt time.Time) (*Subscription, error) {
	const q = `
INSERT INTO subscriptions(organization_id, plan, status, trial_ends_at)
VALUES($1,$2,$3,$4)
ON CONFLICT(organization_id) DO UPDATE SET organization_id=subscriptions.organization_id
RETURNING ` + subscriptionCols
	out, err := scanSubscription(r.pool.QueryRow(ctx, q, organizationID, SubscriptionTrial, SubscriptionTrial, trialEndsAt))
	if err != nil {
		return nil, fmt.Errorf("ensure trial subscription: %w", err)
	}
	return &out, nil
}

// GetForOrganization returns organizationID's subscription, or nil if none
// exists yet.
func (r *SubscriptionRepository) GetForOrganization(ctx context.Context, organizationID int64) (*Subscription, error) {
	out, err := scanSubscription(r.pool.QueryRow(ctx, `SELECT `+subscriptionCols+` FROM subscriptions WHERE organization_id=$1`, organizationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get organization subscription: %w", err)
	}
	return &out, nil
}

// GetByProviderCustomerID finds the subscription owning a Stripe customer id (webhook lookup:
// customer.subscription.* and invoice.* events carry the customer, not the organization).
func (r *SubscriptionRepository) GetByProviderCustomerID(ctx context.Context, provider, customerID string) (*Subscription, error) {
	out, err := scanSubscription(r.pool.QueryRow(ctx, `SELECT `+subscriptionCols+` FROM subscriptions WHERE provider=$1 AND provider_customer_id=$2`, provider, customerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get subscription by provider customer: %w", err)
	}
	return &out, nil
}

// GetByProviderSubscriptionID finds the subscription owning a Stripe subscription id.
func (r *SubscriptionRepository) GetByProviderSubscriptionID(ctx context.Context, provider, subscriptionID string) (*Subscription, error) {
	out, err := scanSubscription(r.pool.QueryRow(ctx, `SELECT `+subscriptionCols+` FROM subscriptions WHERE provider=$1 AND provider_subscription_id=$2`, provider, subscriptionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get subscription by provider subscription: %w", err)
	}
	return &out, nil
}

// SetProviderCustomer records a newly created (or reused) Stripe customer id on an organization's
// subscription row, so a retried checkout call reuses it instead of creating a second Stripe
// customer. It never touches plan/status.
func (r *SubscriptionRepository) SetProviderCustomer(ctx context.Context, organizationID int64, provider, customerID string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE subscriptions SET provider=$2, provider_customer_id=$3, updated_at=NOW() WHERE organization_id=$1`, organizationID, provider, customerID)
	if err != nil {
		return fmt.Errorf("set provider customer: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set provider customer: no subscription row for organization %d", organizationID)
	}
	return nil
}

// ProviderState is what a webhook (or an explicit reconciliation) reconciles into Champion's
// subscription row. Plan is the Champion plan key resolved from the Stripe price id (empty = leave
// the stored plan unchanged, e.g. an event whose price the catalog no longer recognizes).
type ProviderState struct {
	Provider, ProviderCustomerID, ProviderSubscriptionID, ProviderPriceID string
	Plan                                                                  string
	Status                                                                string
	BillingInterval                                                       string
	TrialEndsAt, CurrentPeriodStart, CurrentPeriodEnd                     *time.Time
	CancelAtPeriodEnd                                                     bool
	CanceledAt                                                            *time.Time
}

// ApplyProviderState upserts Stripe's view of a subscription into the organization's one row,
// by organization id (checkout.session.completed carries client_reference_id) or, when orgID is 0,
// by provider_subscription_id (later subscription/invoice events, which carry only the Stripe ids -
// resolved by the caller via GetByProviderCustomerID/GetByProviderSubscriptionID first). It is a
// single UPDATE against the row that subscriptions' own UNIQUE(organization_id) already serializes,
// so two webhooks for the same organization can never interleave into a mixed state. trial_consumed
// is set once a provider_subscription_id is first recorded and never cleared.
func (r *SubscriptionRepository) ApplyProviderState(ctx context.Context, organizationID int64, s ProviderState) (*Subscription, error) {
	const q = `
UPDATE subscriptions SET
  provider=$2, provider_customer_id=$3, provider_subscription_id=$4, provider_price_id=$5,
  plan=CASE WHEN $6='' THEN plan ELSE $6 END,
  status=$7, billing_interval=CASE WHEN $8='' THEN billing_interval ELSE $8 END,
  trial_ends_at=$9, current_period_start=$10, current_period_end=$11, cancel_at_period_end=$12, canceled_at=$13,
  trial_consumed = trial_consumed OR ($4 <> ''),
  updated_at=NOW()
WHERE organization_id=$1
RETURNING ` + subscriptionCols
	out, err := scanSubscription(r.pool.QueryRow(ctx, q, organizationID, s.Provider, s.ProviderCustomerID, s.ProviderSubscriptionID, s.ProviderPriceID,
		s.Plan, s.Status, s.BillingInterval, s.TrialEndsAt, s.CurrentPeriodStart, s.CurrentPeriodEnd, s.CancelAtPeriodEnd, s.CanceledAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSubscriptionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("apply provider state: %w", err)
	}
	return &out, nil
}

// RecordWebhookEventOnce inserts a processed-event marker; inserted is false when this (provider,
// eventID) pair was already recorded, so the caller must skip reprocessing it. Race-safe: the
// uniqueness is a database constraint, not a read-then-write check.
func (r *SubscriptionRepository) RecordWebhookEventOnce(ctx context.Context, provider, eventID, eventType string, organizationID *int64) (inserted bool, err error) {
	var id int64
	err = r.pool.QueryRow(ctx, `INSERT INTO billing_webhook_events(provider, event_id, event_type, organization_id) VALUES($1,$2,$3,$4)
ON CONFLICT ON CONSTRAINT uq_billing_webhook_events DO NOTHING RETURNING id`, provider, eventID, eventType, organizationID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record webhook event: %w", err)
	}
	return true, nil
}
