package repository

import (
	"context"
	"fmt"
	"time"
)

// CaseCoverageRecord is one invoice's coverage as shown to the organization. It deliberately
// carries no Stripe identifiers.
type CaseCoverageRecord struct {
	Tier                            string
	PeriodStart, PeriodEnd          time.Time
	AmountPaid, AmountRefunded      int64
	Currency, Status, DisputeStatus string
	UpdatedAt                       time.Time
}

// CaseCoverageEvent is one append-only audit history row (no Stripe identifiers).
type CaseCoverageEvent struct {
	EventType, PreviousStatus, NewStatus, DisputeStatus string
	AmountRefunded                                      *int64
	PaidThroughBefore, PaidThroughAfter                 *time.Time
	PaidTierBefore, PaidTierAfter                       string
	RecordedAt                                          time.Time
}

const caseCoverageHistoryLimit = 100

// ListCaseCoverage returns the invoice coverage ledger (newest period first) and the most recent
// audit history for one installation's add-ons, scoped to the organization: a guessed installation
// id of another organization returns nothing.
func (r *CaseAddonSubscriptionRepository) ListCaseCoverage(ctx context.Context, organizationID, installationID int64) ([]CaseCoverageRecord, []CaseCoverageEvent, error) {
	records, events := make([]CaseCoverageRecord, 0), make([]CaseCoverageEvent, 0)
	if organizationID <= 0 || installationID <= 0 {
		return records, events, nil
	}
	rows, err := r.pool.Query(ctx, `
SELECT v.tier, v.period_start, v.period_end, v.amount_paid_cents, v.amount_refunded_cents,
       COALESCE(v.currency,''), v.status, COALESCE(v.dispute_status,''), v.updated_at
FROM case_addon_invoice_coverage v
JOIN case_addon_subscriptions c ON c.id=v.addon_id
WHERE c.organization_id=$1 AND c.installation_id=$2
ORDER BY v.period_end DESC, v.id DESC`, organizationID, installationID)
	if err != nil {
		return nil, nil, fmt.Errorf("list case coverage: %w", err)
	}
	for rows.Next() {
		var c CaseCoverageRecord
		if err := rows.Scan(&c.Tier, &c.PeriodStart, &c.PeriodEnd, &c.AmountPaid, &c.AmountRefunded,
			&c.Currency, &c.Status, &c.DisputeStatus, &c.UpdatedAt); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan case coverage: %w", err)
		}
		records = append(records, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list case coverage: %w", err)
	}
	rows, err = r.pool.Query(ctx, `
SELECT e.event_type, COALESCE(e.previous_status,''), e.new_status, COALESCE(e.dispute_status,''), e.amount_refunded_cents,
       e.paid_through_before, e.paid_through_after, COALESCE(e.paid_tier_before,''), COALESCE(e.paid_tier_after,''), e.recorded_at
FROM case_addon_coverage_events e
JOIN case_addon_subscriptions c ON c.id=e.addon_id
WHERE c.organization_id=$1 AND c.installation_id=$2
ORDER BY e.recorded_at DESC, e.id DESC
LIMIT $3`, organizationID, installationID, caseCoverageHistoryLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("list case coverage history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e CaseCoverageEvent
		if err := rows.Scan(&e.EventType, &e.PreviousStatus, &e.NewStatus, &e.DisputeStatus, &e.AmountRefunded,
			&e.PaidThroughBefore, &e.PaidThroughAfter, &e.PaidTierBefore, &e.PaidTierAfter, &e.RecordedAt); err != nil {
			return nil, nil, fmt.Errorf("scan case coverage history: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list case coverage history: %w", err)
	}
	return records, events, nil
}
