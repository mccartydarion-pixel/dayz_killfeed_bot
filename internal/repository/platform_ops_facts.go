package repository

import (
	"context"
	"time"
)

// Cross-tenant facts for the Owner Hub operations pages (docs/OWNER_OPS.md). Read-only, bounded,
// fixed statements. Nothing here selects a credential, a token or a provider customer or
// subscription id; the Stripe price id is read only so the caller can resolve it against the
// plan catalog, and is never part of a response.

// FleetFact is one installation with what the database knows about its server.
type FleetFact struct {
	InstallationID   int64
	OrganizationID   int64
	OrganizationName string
	Status           string
	CreatedAt        time.Time
	GuildName        string
	BotInstalled     bool
	ServerID         int64 // 0 when no server is selected
	GuildID          int64 // the server's internal guilds.id (0 when no server)
	ServerName       string
	Platform         string
	ServerActive     bool
	// Activity figures; zero unless requested with stats.
	LastKillAt          *time.Time
	Kills24h            int
	PlayersOnline       int
	ActivePlayers7d     int
	ActivePlayersPrev7d int
	// Nitrado link status as last recorded (never the credential).
	NitradoStatus     string
	NitradoErrorClass string
	NitradoLastOKAt   *time.Time
	NitradoLastFailAt *time.Time
}

// FleetFacts lists every installation, oldest first. With stats false the activity subqueries
// are not evaluated, which is what the monitor's frequent tick uses.
func (r *PlatformOpsRepository) FleetFacts(ctx context.Context, now time.Time, stats bool) ([]FleetFact, error) {
	rows, err := r.pool.Query(ctx, `
SELECT i.id, i.organization_id, o.name, i.status, i.created_at, COALESCE(c.guild_name, ''), c.bot_installed,
       COALESCE(gs.id, 0), COALESCE(gs.guild_id, 0), COALESCE(gs.display_name, ''), COALESCE(gs.platform, ''), COALESCE(gs.active, FALSE),
       CASE WHEN $2 THEN (SELECT MAX(k.created_at) FROM kills k WHERE k.guild_id = gs.guild_id AND k.server_id = gs.id) END,
       CASE WHEN $2 THEN (SELECT COUNT(*) FROM kills k WHERE k.guild_id = gs.guild_id AND k.server_id = gs.id AND k.created_at > $1::timestamptz - interval '24 hours') ELSE 0 END::int,
       CASE WHEN $2 THEN (SELECT COUNT(*) FROM player_server_activity a WHERE a.server_id = gs.id AND a.currently_connected AND a.last_observed_at > $1::timestamptz - interval '5 minutes') ELSE 0 END::int,
       CASE WHEN $2 THEN (SELECT COUNT(DISTINCT d.player_id) FROM player_daily_activity d WHERE d.server_id = gs.id AND d.day > ($1::timestamptz AT TIME ZONE 'UTC')::date - 7) ELSE 0 END::int,
       CASE WHEN $2 THEN (SELECT COUNT(DISTINCT d.player_id) FROM player_daily_activity d WHERE d.server_id = gs.id AND d.day > ($1::timestamptz AT TIME ZONE 'UTC')::date - 14 AND d.day <= ($1::timestamptz AT TIME ZONE 'UTC')::date - 7) ELSE 0 END::int,
       COALESCE(n.status, ''), COALESCE(n.last_error_class, ''), n.last_success_at, n.last_failure_at
FROM installations i
JOIN organizations o ON o.id = i.organization_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
LEFT JOIN game_servers gs ON gs.id = i.game_server_id
LEFT JOIN nitrado_connections n ON n.guild_id = gs.guild_id
ORDER BY i.id LIMIT 5000`, now.UTC(), stats)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FleetFact{}
	for rows.Next() {
		var f FleetFact
		if err := rows.Scan(&f.InstallationID, &f.OrganizationID, &f.OrganizationName, &f.Status, &f.CreatedAt, &f.GuildName, &f.BotInstalled,
			&f.ServerID, &f.GuildID, &f.ServerName, &f.Platform, &f.ServerActive,
			&f.LastKillAt, &f.Kills24h, &f.PlayersOnline, &f.ActivePlayers7d, &f.ActivePlayersPrev7d,
			&f.NitradoStatus, &f.NitradoErrorClass, &f.NitradoLastOKAt, &f.NitradoLastFailAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// OrganizationFact is one organization with its owner, subscription and onboarding progress.
type OrganizationFact struct {
	ID        int64
	Name      string
	CreatedAt time.Time
	// Owner.
	OwnerUserID      int64
	OwnerDiscordID   string
	OwnerName        string
	OwnerLastLoginAt *time.Time
	// Subscription ("" plan and status when the organization has no row).
	Plan              string
	Status            string
	TrialEndsAt       *time.Time
	CurrentPeriodEnd  *time.Time
	CancelAtPeriodEnd bool
	CanceledAt        *time.Time
	StripeBilled      bool
	BillingInterval   string
	PriceID           string // resolved against the catalog by the caller; never returned
	IntendedPlan      string
	TrialConsumed     bool
	OwnerGrant        bool
	SubscriptionSince *time.Time
	// Onboarding progress.
	DiscordConnected bool
	NitradoConnected bool
	ServerSelected   bool
	SetupComplete    bool
	HasKill          bool
	LastProgressAt   time.Time // newest of the organization's and its installations' updated_at
}

// OrganizationFacts lists every organization, newest first.
func (r *PlatformOpsRepository) OrganizationFacts(ctx context.Context) ([]OrganizationFact, error) {
	rows, err := r.pool.Query(ctx, `
SELECT o.id, o.name, o.created_at,
       u.id, u.discord_user_id, COALESCE(NULLIF(u.discord_global_name, ''), u.discord_username), u.last_login_at,
       COALESCE(s.plan, ''), COALESCE(s.status, ''), s.trial_ends_at, s.current_period_end, COALESCE(s.cancel_at_period_end, FALSE), s.canceled_at,
       (s.provider IS NOT NULL AND s.provider_subscription_id IS NOT NULL), COALESCE(s.billing_interval, ''), COALESCE(s.provider_price_id, ''),
       COALESCE(s.intended_plan, ''), COALESCE(s.trial_consumed, FALSE), (s.owner_grant_until IS NOT NULL OR s.owner_grant_reason IS NOT NULL), s.created_at,
       EXISTS (SELECT 1 FROM discord_guild_connections c WHERE c.organization_id = o.id),
       (EXISTS (SELECT 1 FROM nitrado_connections n WHERE n.organization_id = o.id)
          OR EXISTS (SELECT 1 FROM installations i JOIN installation_setup_progress p ON p.installation_id = i.id WHERE i.organization_id = o.id AND p.nitrado_completed)),
       EXISTS (SELECT 1 FROM installations i WHERE i.organization_id = o.id AND i.game_server_id IS NOT NULL),
       EXISTS (SELECT 1 FROM installations i WHERE i.organization_id = o.id AND (i.setup_completed_at IS NOT NULL OR i.status IN ('READY','DEGRADED'))),
       EXISTS (SELECT 1 FROM installations i JOIN game_servers gs ON gs.id = i.game_server_id
               WHERE i.organization_id = o.id AND EXISTS (SELECT 1 FROM kills k WHERE k.guild_id = gs.guild_id AND k.server_id = gs.id)),
       GREATEST(o.updated_at, COALESCE((SELECT MAX(i.updated_at) FROM installations i WHERE i.organization_id = o.id), o.updated_at))
FROM organizations o
JOIN app_users u ON u.id = o.owner_user_id
LEFT JOIN subscriptions s ON s.organization_id = o.id
ORDER BY o.id DESC LIMIT 5000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OrganizationFact{}
	for rows.Next() {
		var f OrganizationFact
		if err := rows.Scan(&f.ID, &f.Name, &f.CreatedAt,
			&f.OwnerUserID, &f.OwnerDiscordID, &f.OwnerName, &f.OwnerLastLoginAt,
			&f.Plan, &f.Status, &f.TrialEndsAt, &f.CurrentPeriodEnd, &f.CancelAtPeriodEnd, &f.CanceledAt,
			&f.StripeBilled, &f.BillingInterval, &f.PriceID,
			&f.IntendedPlan, &f.TrialConsumed, &f.OwnerGrant, &f.SubscriptionSince,
			&f.DiscordConnected, &f.NitradoConnected, &f.ServerSelected, &f.SetupComplete, &f.HasKill, &f.LastProgressAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SignupCounts counts website accounts created since a moment, and how many of those belong to
// no organization (signed in, never created or joined one).
func (r *PlatformOpsRepository) SignupCounts(ctx context.Context, since time.Time) (accounts, withoutOrganization int, err error) {
	err = r.pool.QueryRow(ctx, `
SELECT COUNT(*)::int,
       COUNT(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM organization_members m WHERE m.user_id = u.id))::int
FROM app_users u WHERE u.created_at >= $1`, since.UTC()).Scan(&accounts, &withoutOrganization)
	return accounts, withoutOrganization, err
}

// PaymentMonth is one calendar month (UTC) of billing_transactions in one currency.
type PaymentMonth struct {
	Month       time.Time
	Currency    string
	PaidCents   int64
	PaidCount   int
	FailedCount int
}

// PaymentsByMonth returns the last months of recorded payments, oldest first.
func (r *PlatformOpsRepository) PaymentsByMonth(ctx context.Context, now time.Time, months int) ([]PaymentMonth, error) {
	if months < 1 || months > 36 {
		months = 12
	}
	rows, err := r.pool.Query(ctx, `
SELECT date_trunc('month', COALESCE(paid_at, failed_at, created_at) AT TIME ZONE 'UTC') AS month, LOWER(currency),
       COALESCE(SUM(amount_cents) FILTER (WHERE status = 'PAID'), 0)::bigint,
       COUNT(*) FILTER (WHERE status = 'PAID')::int, COUNT(*) FILTER (WHERE status = 'FAILED')::int
FROM billing_transactions
WHERE COALESCE(paid_at, failed_at, created_at) >= date_trunc('month', $1::timestamptz AT TIME ZONE 'UTC') - make_interval(months => $2)
GROUP BY 1, 2 ORDER BY 1, 2`, now.UTC(), months-1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PaymentMonth{}
	for rows.Next() {
		var m PaymentMonth
		if err := rows.Scan(&m.Month, &m.Currency, &m.PaidCents, &m.PaidCount, &m.FailedCount); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PaymentsSince sums the payments recorded since a moment.
func (r *PlatformOpsRepository) PaymentsSince(ctx context.Context, since time.Time) (paidCents int64, failed int, err error) {
	err = r.pool.QueryRow(ctx, `
SELECT COALESCE(SUM(amount_cents) FILTER (WHERE status = 'PAID'), 0)::bigint, COUNT(*) FILTER (WHERE status = 'FAILED')::int
FROM billing_transactions WHERE COALESCE(paid_at, failed_at, created_at) >= $1`, since.UTC()).Scan(&paidCents, &failed)
	return paidCents, failed, err
}

// AddonCount is how many C.A.S.E. add-on subscriptions of one tier are active.
type AddonCount struct {
	Tier   string
	Active int
}

// ActiveAddons counts the active C.A.S.E. add-on subscriptions per tier.
func (r *PlatformOpsRepository) ActiveAddons(ctx context.Context) ([]AddonCount, error) {
	rows, err := r.pool.Query(ctx, `SELECT tier, COUNT(*)::int FROM case_addon_subscriptions WHERE status = 'ACTIVE' GROUP BY tier ORDER BY tier`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AddonCount{}
	for rows.Next() {
		var a AddonCount
		if err := rows.Scan(&a.Tier, &a.Active); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolvedIncidentsSince counts incidents closed since a moment.
func (r *PlatformOpsRepository) ResolvedIncidentsSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM platform_incidents WHERE status = 'RESOLVED' AND resolved_at >= $1`, since.UTC()).Scan(&n)
	return n, err
}

// ViewAsTarget is the account an owner "view as customer" session acts as: the organization's
// owner. found is false for an unknown organization.
func (r *PlatformOpsRepository) ViewAsTarget(ctx context.Context, organizationID int64) (discordID, displayName, organizationName string, found bool, err error) {
	rows, err := r.pool.Query(ctx, `
SELECT u.discord_user_id, COALESCE(NULLIF(u.discord_global_name, ''), u.discord_username), o.name
FROM organizations o JOIN app_users u ON u.id = o.owner_user_id WHERE o.id = $1`, organizationID)
	if err != nil {
		return "", "", "", false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", "", "", false, rows.Err()
	}
	if err := rows.Scan(&discordID, &displayName, &organizationName); err != nil {
		return "", "", "", false, err
	}
	return discordID, displayName, organizationName, true, rows.Err()
}
