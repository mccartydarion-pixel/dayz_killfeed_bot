package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

// CaseAddonSubscription is a server-bound C.A.S.E. subscription. It never
// replaces the organization's existing base subscriptions row.
type CaseAddonSubscription struct {
	ID, OrganizationID, InstallationID, GameServerID int64
	Tier, Status                                      string
	Provider, ProviderCustomerID                      string
	ProviderSubscriptionID, ProviderPriceID           string
	CurrentPeriodStart, CurrentPeriodEnd              *time.Time
	TrialStartedAt, TrialEndsAt                       *time.Time
	CancelAtPeriodEnd                                 bool
	CreatedAt, UpdatedAt                              time.Time
	// A changed/removed installation server must not transfer paid access.
	SelectedGameServerID *int64
}

// AccessInput requires the caller to supply the independently loaded base
// subscription status and operator rollout/verification flags.
func (s CaseAddonSubscription) AccessInput(baseStatus string, enabled, verified bool) casebilling.AccessInput {
	selectedID := int64(0)
	if s.SelectedGameServerID != nil {
		selectedID = *s.SelectedGameServerID
	}
	return casebilling.AccessInput{
		BillingEnabled: enabled, CapabilitiesVerified: verified,
		OrganizationID: s.OrganizationID, InstallationID: s.InstallationID,
		SelectedGameServerID: selectedID, BaseStatus: baseStatus,
		AddonOrganizationID: s.OrganizationID, AddonInstallationID: s.InstallationID,
		BoundGameServerID: s.GameServerID, Tier: casebilling.Tier(s.Tier),
		Status: s.Status, Provider: s.Provider, ProviderSubscriptionID: s.ProviderSubscriptionID,
		ProviderPriceID: s.ProviderPriceID, CurrentPeriodEnd: s.CurrentPeriodEnd,
		TrialEndsAt: s.TrialEndsAt,
	}
}

const caseAddonColumns = `c.id, c.organization_id, c.installation_id, c.game_server_id,
	c.tier, c.status, COALESCE(c.provider,''), COALESCE(c.provider_customer_id,''),
	COALESCE(c.provider_subscription_id,''), COALESCE(c.provider_price_id,''),
	c.current_period_start, c.current_period_end, c.trial_started_at, c.trial_ends_at,
	c.cancel_at_period_end, c.created_at, c.updated_at, i.game_server_id`

func scanCaseAddon(row pgx.Row) (CaseAddonSubscription, error) {
	var s CaseAddonSubscription
	err := row.Scan(&s.ID, &s.OrganizationID, &s.InstallationID, &s.GameServerID,
		&s.Tier, &s.Status, &s.Provider, &s.ProviderCustomerID,
		&s.ProviderSubscriptionID, &s.ProviderPriceID,
		&s.CurrentPeriodStart, &s.CurrentPeriodEnd, &s.TrialStartedAt, &s.TrialEndsAt,
		&s.CancelAtPeriodEnd, &s.CreatedAt, &s.UpdatedAt, &s.SelectedGameServerID)
	return s, err
}

type CaseAddonSubscriptionRepository struct{ pool *pgxpool.Pool }

func NewCaseAddonSubscriptionRepository(pool *pgxpool.Pool) *CaseAddonSubscriptionRepository {
	return &CaseAddonSubscriptionRepository{pool: pool}
}

// GetScoped returns nil for an absent row OR an installation belonging to
// another organization. The join also supplies the CURRENT selected server
// separately from the purchased/bound server; AccessInput fails closed if they
// differ. This phase intentionally exposes no write path or checkout action.
func (r *CaseAddonSubscriptionRepository) GetScoped(ctx context.Context, organizationID, installationID int64) (*CaseAddonSubscription, error) {
	if organizationID <= 0 || installationID <= 0 {
		return nil, nil
	}
	const q = `SELECT ` + caseAddonColumns + `
FROM case_addon_subscriptions c
JOIN installations i ON i.id=c.installation_id AND i.organization_id=c.organization_id
WHERE c.organization_id=$1 AND c.installation_id=$2`
	s, err := scanCaseAddon(r.pool.QueryRow(ctx, q, organizationID, installationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get scoped case addon: %w", err)
	}
	return &s, nil
}

// ListByOrganization cannot return another organization's add-ons even if an
// installation or game server ID is guessed by a caller.
func (r *CaseAddonSubscriptionRepository) ListByOrganization(ctx context.Context, organizationID int64) ([]CaseAddonSubscription, error) {
	out := make([]CaseAddonSubscription, 0)
	if organizationID <= 0 {
		return out, nil
	}
	const q = `SELECT ` + caseAddonColumns + `
FROM case_addon_subscriptions c
JOIN installations i ON i.id=c.installation_id AND i.organization_id=c.organization_id
WHERE c.organization_id=$1
ORDER BY c.installation_id`
	rows, err := r.pool.Query(ctx, q, organizationID)
	if err != nil {
		return nil, fmt.Errorf("list scoped case addons: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s CaseAddonSubscription
		if err := rows.Scan(&s.ID, &s.OrganizationID, &s.InstallationID, &s.GameServerID,
			&s.Tier, &s.Status, &s.Provider, &s.ProviderCustomerID,
			&s.ProviderSubscriptionID, &s.ProviderPriceID,
			&s.CurrentPeriodStart, &s.CurrentPeriodEnd, &s.TrialStartedAt, &s.TrialEndsAt,
			&s.CancelAtPeriodEnd, &s.CreatedAt, &s.UpdatedAt, &s.SelectedGameServerID); err != nil {
			return nil, fmt.Errorf("scan case addon: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate case addons: %w", err)
	}
	return out, nil
}
