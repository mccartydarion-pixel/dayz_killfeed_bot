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
// convention). Billing is not integrated yet (section 12) - provider fields
// stay empty until it is.
const (
	SubscriptionTrial     = "TRIAL"
	SubscriptionActive    = "ACTIVE"
	SubscriptionPastDue   = "PAST_DUE"
	SubscriptionCanceled  = "CANCELED"
	SubscriptionSuspended = "SUSPENDED"
)

type Subscription struct {
	ID                                                   int64
	OrganizationID                                       int64
	Provider, ProviderCustomerID, ProviderSubscriptionID string
	Plan, Status                                         string
	TrialEndsAt, CurrentPeriodEnd                        *time.Time
	CreatedAt, UpdatedAt                                 time.Time
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
RETURNING id, organization_id, COALESCE(provider,''), COALESCE(provider_customer_id,''), COALESCE(provider_subscription_id,''), plan, status, trial_ends_at, current_period_end, created_at, updated_at`
	var out Subscription
	err := r.pool.QueryRow(ctx, q, organizationID, SubscriptionTrial, SubscriptionTrial, trialEndsAt).
		Scan(&out.ID, &out.OrganizationID, &out.Provider, &out.ProviderCustomerID, &out.ProviderSubscriptionID, &out.Plan, &out.Status, &out.TrialEndsAt, &out.CurrentPeriodEnd, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("ensure trial subscription: %w", err)
	}
	return &out, nil
}

// GetForOrganization returns organizationID's subscription, or nil if none
// exists yet.
func (r *SubscriptionRepository) GetForOrganization(ctx context.Context, organizationID int64) (*Subscription, error) {
	const q = `SELECT id, organization_id, COALESCE(provider,''), COALESCE(provider_customer_id,''), COALESCE(provider_subscription_id,''), plan, status, trial_ends_at, current_period_end, created_at, updated_at FROM subscriptions WHERE organization_id=$1`
	var out Subscription
	err := r.pool.QueryRow(ctx, q, organizationID).
		Scan(&out.ID, &out.OrganizationID, &out.Provider, &out.ProviderCustomerID, &out.ProviderSubscriptionID, &out.Plan, &out.Status, &out.TrialEndsAt, &out.CurrentPeriodEnd, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get organization subscription: %w", err)
	}
	return &out, nil
}
