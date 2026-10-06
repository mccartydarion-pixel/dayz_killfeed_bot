package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Platform-owner deletes (docs/ADMIN_API.md "Deleting a community or an installation").
//
// Two deletes exist and both are deliberately narrow:
//
//   - DeleteEmptyOrganization removes an organization that never connected a game server (the
//     "a player pressed set up my server by accident" case).
//   - DeleteInstallation removes one installation and its configuration. Gameplay data is keyed
//     by guild and game server, not by installation, and is kept.
//
// What "safe to delete" means is not a list somebody has to remember to extend. Both deletes
// walk the live foreign-key graph below the row (dependentRows) and refuse when any table that
// is not on deleteConfigTables holds a row the delete would remove. A table added by a later
// migration is therefore refused until someone decides it is configuration.

// Blocker codes: why a delete is refused. The website shows Message; Code is for tests and logs.
const (
	DeleteBlockerServerConnected  = "SERVER_CONNECTED"
	DeleteBlockerLiveSubscription = "LIVE_SUBSCRIPTION"
	DeleteBlockerPaymentHistory   = "PAYMENT_HISTORY"
	DeleteBlockerRecords          = "HAS_RECORDS"
)

// DeleteBlocker is one reason a delete is refused.
type DeleteBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

var (
	// ErrDeleteConfirmMismatch: the typed confirmation is not the organization's exact name (or
	// the installation's id).
	ErrDeleteConfirmMismatch = errors.New("delete confirmation does not match")
	// ErrDeleteBlocked: the target is not safe to delete; the returned plan lists why.
	ErrDeleteBlocked = errors.New("delete is blocked")
)

// deleteConfigTables are the tables a delete may remove rows from: memberships, the Discord
// connection, the installation and its setup and configuration. Everything else below an
// organization or an installation is a record (orders, factions, cases, payments, alerts) and
// blocks the delete while it holds a row.
var deleteConfigTables = map[string]bool{
	// organization level
	"organization_members": true, "discord_guild_connections": true, "installations": true,
	"subscriptions": true, "trial_grants": true, "admin_audit_log": true,
	// installation setup and settings
	"installation_setup_progress": true, "installation_settings": true, "installation_channel_routes": true,
	"installation_embed_templates": true, "installation_embed_activation": true, "installation_role_permissions": true,
	"installation_access_entries": true, "installation_retired_channels": true, "installation_feature_flags": true,
	"installation_feature_settings": true,
	// feature configuration
	"installation_zones": true, "zone_ignore_entries": true, "zone_authorized_entries": true, "zone_presence": true,
	"shop_categories": true, "shop_products": true, "shop_delivery_settings": true, "shop_auto_delivery_settings": true,
	"case_detector_settings": true, "case_alert_settings": true, "case_evidence_optins": true,
	"base_raid_alarm_settings": true, "base_black_box_settings": true, "base_rent_settings": true,
	"perimeter_watch_settings": true, "faction_security_settings": true, "faction_security_preferences": true,
	"security_service_offers": true, "security_store_panels": true, "hub_faction_settings": true,
	"map_rotation_settings": true, "map_rotation_maps": true, "map_rotation_votes": true,
	"map_rotation_vote_options": true, "map_rotation_ballots": true, "map_rotation_switches": true,
	// platform bookkeeping about the installation
	"platform_incidents": true, "platform_broadcast_deliveries": true,
}

// IsDeleteConfigTable reports whether a delete may remove rows from table.
func IsDeleteConfigTable(table string) bool { return deleteConfigTables[table] }

// SubscriptionIsLivePaid reports whether a subscription row is a live Stripe subscription: a
// provider subscription id is recorded and the status is not CANCELED / INACTIVE. A trial or an
// owner grant without a provider subscription id is not paid.
func SubscriptionIsLivePaid(providerSubscriptionID, status string) bool {
	if strings.TrimSpace(providerSubscriptionID) == "" {
		return false
	}
	return status != SubscriptionCanceled && status != SubscriptionInactive
}

// DeleteConfirmationMatches is the typed-confirmation rule: what was typed, with surrounding
// whitespace removed, must equal the expected value exactly (capital letters included).
func DeleteConfirmationMatches(expected, typed string) bool {
	expected = strings.TrimSpace(expected)
	return expected != "" && strings.TrimSpace(typed) == expected
}

// recordBlockers turns the dependent-row counts into blockers: one per non-configuration table
// that holds rows. removed is what the delete takes with it (configuration tables only).
func recordBlockers(counts map[string]int64) (removed map[string]int64, blockers []DeleteBlocker) {
	removed = map[string]int64{}
	var tables []string
	for table, n := range counts {
		if n <= 0 {
			continue
		}
		if IsDeleteConfigTable(table) {
			removed[table] = n
			continue
		}
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		blockers = append(blockers, DeleteBlocker{Code: DeleteBlockerRecords,
			Message: fmt.Sprintf("It has %d %s that would be lost (%s).", counts[table], recordLabel(table), table)})
	}
	return removed, blockers
}

// recordLabel is a plain-English name for the commonest record tables.
func recordLabel(table string) string {
	switch {
	case table == "billing_transactions":
		return "payment record(s)"
	case strings.HasPrefix(table, "case_addon"):
		return "CASE add-on billing record(s)"
	case strings.HasPrefix(table, "shop_"):
		return "shop order record(s)"
	case strings.HasPrefix(table, "hub_faction"):
		return "faction record(s)"
	case strings.HasPrefix(table, "case_"):
		return "CASE review record(s)"
	case strings.HasPrefix(table, "base_"), strings.HasPrefix(table, "security_"), strings.HasPrefix(table, "perimeter_"), strings.HasPrefix(table, "faction_security"):
		return "base security record(s)"
	case strings.HasPrefix(table, "zone_"):
		return "zone record(s)"
	default:
		return "record(s)"
	}
}

// --- foreign-key walk -----------------------------------------------------------------------------

type fkEdge struct {
	child, parent         string
	childCols, parentCols []string
	action                string // pg_constraint.confdeltype: c cascade, r restrict, a no action, n set null, d set default
}

func loadFKEdges(ctx context.Context, tx pgx.Tx) ([]fkEdge, error) {
	rows, err := tx.Query(ctx, `
SELECT cc.relname::text, pc.relname::text, c.confdeltype::text,
       (SELECT array_agg(a.attname::text ORDER BY k.ord) FROM unnest(c.conkey) WITH ORDINALITY k(attnum, ord)
          JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum),
       (SELECT array_agg(a.attname::text ORDER BY k.ord) FROM unnest(c.confkey) WITH ORDINALITY k(attnum, ord)
          JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.attnum)
FROM pg_constraint c
JOIN pg_class cc ON cc.oid = c.conrelid
JOIN pg_class pc ON pc.oid = c.confrelid
JOIN pg_namespace n ON n.oid = cc.relnamespace
WHERE c.contype = 'f' AND c.conrelid <> c.confrelid AND n.nspname = current_schema() AND pc.relnamespace = cc.relnamespace
ORDER BY 2, 1, c.conname`)
	if err != nil {
		return nil, fmt.Errorf("load foreign keys: %w", err)
	}
	defer rows.Close()
	var out []fkEdge
	for rows.Next() {
		var e fkEdge
		if err := rows.Scan(&e.child, &e.parent, &e.action, &e.childCols, &e.parentCols); err != nil {
			return nil, err
		}
		if len(e.childCols) == 0 || len(e.childCols) != len(e.parentCols) {
			return nil, fmt.Errorf("foreign key %s -> %s has no usable columns", e.child, e.parent)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const fkWalkMaxDepth = 8

// dependentRows counts, per table, the rows a DELETE of the rows selected by rootSet (a SELECT
// over rootTable taking arg as $1) would remove through ON DELETE CASCADE, plus the rows that
// would refuse it (RESTRICT / NO ACTION). SET NULL references are not counted: those rows stay.
// Tables without such rows are absent from the result. When a table is reachable along several
// paths the largest count is reported.
func dependentRows(ctx context.Context, tx pgx.Tx, rootTable, rootSet string, arg any) (map[string]int64, error) {
	edges, err := loadFKEdges(ctx, tx)
	if err != nil {
		return nil, err
	}
	byParent := map[string][]fkEdge{}
	for _, e := range edges {
		byParent[e.parent] = append(byParent[e.parent], e)
	}
	type probe struct{ table, set string }
	var probes []probe
	var walk func(table, set string, path []string) error
	walk = func(table, set string, path []string) error {
		for _, e := range byParent[table] {
			if e.action == "n" || e.action == "d" {
				continue
			}
			conds := make([]string, len(e.childCols))
			for i := range e.childCols {
				conds[i] = "c." + pgx.Identifier{e.childCols[i]}.Sanitize() + " = p." + pgx.Identifier{e.parentCols[i]}.Sanitize()
			}
			childSet := "SELECT c.* FROM " + pgx.Identifier{e.child}.Sanitize() + " c WHERE EXISTS (SELECT 1 FROM (" + set + ") p WHERE " + strings.Join(conds, " AND ") + ")"
			probes = append(probes, probe{e.child, childSet})
			if e.action != "c" {
				continue
			}
			seen := false
			for _, t := range path {
				if t == e.child {
					seen = true
					break
				}
			}
			if seen {
				continue
			}
			if len(path) >= fkWalkMaxDepth {
				return fmt.Errorf("foreign-key graph below %s is deeper than %d levels", rootTable, fkWalkMaxDepth)
			}
			if err := walk(e.child, childSet, append(append([]string(nil), path...), e.child)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(rootTable, rootSet, []string{rootTable}); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	batch := &pgx.Batch{}
	for _, p := range probes {
		batch.Queue("SELECT COUNT(*) FROM ("+p.set+") t", arg)
	}
	if len(probes) == 0 {
		return out, nil
	}
	res := tx.SendBatch(ctx, batch)
	for _, p := range probes {
		var n int64
		if err := res.QueryRow().Scan(&n); err != nil {
			_ = res.Close()
			return nil, fmt.Errorf("count dependent rows in %s: %w", p.table, err)
		}
		if n > out[p.table] {
			out[p.table] = n
		}
	}
	if err := res.Close(); err != nil {
		return nil, fmt.Errorf("count dependent rows: %w", err)
	}
	return out, nil
}

// --- organization ---------------------------------------------------------------------------------

// OrganizationDeletePlan is what deleting an organization would do, and whether it is allowed.
// It doubles as the audit snapshot of a delete that happened.
type OrganizationDeletePlan struct {
	OrganizationID          int64            `json:"organizationId"`
	Name                    string           `json:"name"`
	Slug                    string           `json:"slug"`
	CreatedAt               time.Time        `json:"createdAt"`
	OwnerUserID             int64            `json:"ownerUserId"`
	OwnerDiscordID          string           `json:"ownerDiscordId"`
	OwnerName               string           `json:"ownerName"`
	Members                 int64            `json:"members"`
	Installations           int64            `json:"installations"`
	InstallationsWithServer int64            `json:"installationsWithServer"`
	GameServers             int64            `json:"gameServers"`
	DiscordGuilds           []string         `json:"discordGuilds"`
	SubscriptionPlan        string           `json:"subscriptionPlan"`
	SubscriptionStatus      string           `json:"subscriptionStatus"`
	LivePaidSubscription    bool             `json:"livePaidSubscription"`
	PaymentRecords          int64            `json:"paymentRecords"`
	NitradoCredential       bool             `json:"nitradoCredential"`
	Removes                 map[string]int64 `json:"removes"`
	Blockers                []DeleteBlocker  `json:"blockers"`
}

// Deletable reports whether the organization is empty (see planOrganizationDelete).
func (p *OrganizationDeletePlan) Deletable() bool { return p != nil && len(p.Blockers) == 0 }

// planOrganizationDelete builds the plan inside tx. With lock it first locks the organization,
// its installations and its subscription, so nothing can attach a game server, add an
// installation or turn the subscription live between the check and the delete.
//
// An organization is EMPTY, and so deletable, when all of these hold:
//
//  1. no installation of it has a game server, and no game_servers row belongs to it;
//  2. it has no live paid subscription (SubscriptionIsLivePaid);
//  3. it has no payment records (billing_transactions);
//  4. nothing below it or its installations holds a row outside deleteConfigTables.
func planOrganizationDelete(ctx context.Context, tx pgx.Tx, organizationID int64, lock bool) (*OrganizationDeletePlan, error) {
	forUpdate := ""
	if lock {
		forUpdate = " FOR UPDATE OF o"
	}
	p := &OrganizationDeletePlan{OrganizationID: organizationID, DiscordGuilds: []string{}, Removes: map[string]int64{}, Blockers: []DeleteBlocker{}}
	err := tx.QueryRow(ctx, `
SELECT o.name, o.slug, o.created_at, u.id, u.discord_user_id, COALESCE(NULLIF(u.discord_global_name,''), u.discord_username)
FROM organizations o JOIN app_users u ON u.id = o.owner_user_id WHERE o.id = $1`+forUpdate, organizationID).
		Scan(&p.Name, &p.Slug, &p.CreatedAt, &p.OwnerUserID, &p.OwnerDiscordID, &p.OwnerName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delete plan: organization: %w", err)
	}
	if lock {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM installations WHERE organization_id = $1 ORDER BY id FOR UPDATE`, organizationID); err != nil {
			return nil, fmt.Errorf("delete plan: lock installations: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM subscriptions WHERE organization_id = $1 FOR UPDATE`, organizationID); err != nil {
			return nil, fmt.Errorf("delete plan: lock subscription: %w", err)
		}
	}
	var providerSub string
	err = tx.QueryRow(ctx, `
SELECT (SELECT COUNT(*) FROM organization_members WHERE organization_id = $1),
       (SELECT COUNT(*) FROM installations WHERE organization_id = $1),
       (SELECT COUNT(*) FROM installations WHERE organization_id = $1 AND game_server_id IS NOT NULL),
       (SELECT COUNT(*) FROM game_servers WHERE organization_id = $1),
       (SELECT COUNT(*) FROM billing_transactions WHERE organization_id = $1),
       EXISTS (SELECT 1 FROM nitrado_connections WHERE organization_id = $1),
       COALESCE((SELECT plan FROM subscriptions WHERE organization_id = $1), ''),
       COALESCE((SELECT status FROM subscriptions WHERE organization_id = $1), ''),
       COALESCE((SELECT provider_subscription_id FROM subscriptions WHERE organization_id = $1), ''),
       COALESCE((SELECT array_agg(COALESCE(NULLIF(c.guild_name,''), g.discord_guild_id) ORDER BY c.id)
                 FROM discord_guild_connections c JOIN guilds g ON g.id = c.guild_id WHERE c.organization_id = $1), '{}')`,
		organizationID).Scan(&p.Members, &p.Installations, &p.InstallationsWithServer, &p.GameServers, &p.PaymentRecords,
		&p.NitradoCredential, &p.SubscriptionPlan, &p.SubscriptionStatus, &providerSub, &p.DiscordGuilds)
	if err != nil {
		return nil, fmt.Errorf("delete plan: organization counts: %w", err)
	}
	p.LivePaidSubscription = SubscriptionIsLivePaid(providerSub, p.SubscriptionStatus)

	if p.InstallationsWithServer > 0 || p.GameServers > 0 {
		msg := fmt.Sprintf("It has %d installation(s) with a game server connected. Remove each of those installations first.", p.InstallationsWithServer)
		if p.InstallationsWithServer == 0 {
			msg = fmt.Sprintf("%d game server(s) are still registered to it.", p.GameServers)
		}
		p.Blockers = append(p.Blockers, DeleteBlocker{Code: DeleteBlockerServerConnected, Message: msg})
	}
	if p.LivePaidSubscription {
		p.Blockers = append(p.Blockers, DeleteBlocker{Code: DeleteBlockerLiveSubscription,
			Message: "It has a live paid subscription in Stripe. Cancel the subscription first, then delete."})
	}
	if p.PaymentRecords > 0 {
		p.Blockers = append(p.Blockers, DeleteBlocker{Code: DeleteBlockerPaymentHistory,
			Message: fmt.Sprintf("It has %d payment record(s). Communities that have paid are kept for the accounts.", p.PaymentRecords)})
	}
	counts, err := dependentRows(ctx, tx, "organizations", `SELECT * FROM organizations WHERE id = $1`, organizationID)
	if err != nil {
		return nil, err
	}
	delete(counts, "billing_transactions") // reported above as PAYMENT_HISTORY
	removed, blockers := recordBlockers(counts)
	p.Removes = removed
	p.Blockers = append(p.Blockers, blockers...)
	return p, nil
}

// PlanOrganizationDelete reports what deleting the organization would remove and whether it is
// allowed, changing nothing. nil, nil when the organization does not exist.
func (r *PlatformOwnerRepository) PlanOrganizationDelete(ctx context.Context, organizationID int64) (*OrganizationDeletePlan, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("delete plan: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	return planOrganizationDelete(ctx, tx, organizationID, false)
}

// DeleteEmptyOrganization permanently deletes an organization that never connected a game
// server, in one transaction: the emptiness check, the typed confirmation, the delete and the
// platform_audit_log row all commit together or not at all.
//
// Removed: the organization, its memberships, its Discord connection(s), its installations that
// have no game server with their setup and configuration, its subscription row and trial marker,
// its organization audit log, and a Nitrado credential stored for it that no game server uses.
//
// Kept: every user account (the owner becomes an account without an organization), the guilds
// row with all player links and gameplay data keyed by guild, and everything belonging to any
// other organization.
//
// Returns nil, nil when the organization does not exist; ErrDeleteConfirmMismatch when
// confirmName is not its exact name; ErrDeleteBlocked with the plan when it is not empty.
func (r *PlatformOwnerRepository) DeleteEmptyOrganization(ctx context.Context, organizationID int64, confirmName, actorDiscordID, reason string) (*OrganizationDeletePlan, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("delete organization: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	plan, err := planOrganizationDelete(ctx, tx, organizationID, true)
	if err != nil || plan == nil {
		return nil, err
	}
	if !DeleteConfirmationMatches(plan.Name, confirmName) {
		return plan, ErrDeleteConfirmMismatch
	}
	if !plan.Deletable() {
		return plan, ErrDeleteBlocked
	}
	// The credential would otherwise stay behind with no owner (the reference is SET NULL). One
	// that sits on a guild which still has a game server is that guild's, and is left alone.
	if _, err := tx.Exec(ctx, `
DELETE FROM nitrado_connections nc
WHERE nc.organization_id = $1
  AND (nc.guild_id IS NULL OR NOT EXISTS (SELECT 1 FROM game_servers gs WHERE gs.guild_id = nc.guild_id))`, organizationID); err != nil {
		return nil, fmt.Errorf("delete organization: credential: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, organizationID)
	if err != nil {
		return nil, fmt.Errorf("delete organization: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("delete organization: %d rows deleted, expected 1", tag.RowsAffected())
	}
	if err := recordDeleteAudit(ctx, tx, actorDiscordID, "organization.deleted", "organization", organizationID, organizationID, reason, plan); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("delete organization: commit: %w", err)
	}
	return plan, nil
}

func recordDeleteAudit(ctx context.Context, tx pgx.Tx, actorDiscordID, action, targetType string, targetID, organizationID int64, reason string, snapshot any) error {
	before, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("%s: audit snapshot: %w", action, err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO platform_audit_log(actor_discord_id, action, target_type, target_id, organization_id, reason, before_state, after_state, result)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'OK')`, actorDiscordID, action, targetType, targetID, organizationID, reason, before, json.RawMessage(`{"deleted":true}`)); err != nil {
		return fmt.Errorf("%s: audit: %w", action, err)
	}
	return nil
}

// --- installation ---------------------------------------------------------------------------------

// InstallationDeletePlan is what removing an installation would do, and whether it is allowed.
type InstallationDeletePlan struct {
	InstallationID       int64            `json:"installationId"`
	OrganizationID       int64            `json:"organizationId"`
	OrganizationName     string           `json:"organizationName"`
	OwnerDiscordID       string           `json:"ownerDiscordId"`
	Status               string           `json:"status"`
	CreatedAt            time.Time        `json:"createdAt"`
	DiscordGuild         string           `json:"discordGuild"`
	GameServerID         *int64           `json:"gameServerId"`
	GameServerName       string           `json:"gameServerName"`
	SubscriptionStatus   string           `json:"subscriptionStatus"`
	LivePaidSubscription bool             `json:"livePaidSubscription"`
	Removes              map[string]int64 `json:"removes"`
	Blockers             []DeleteBlocker  `json:"blockers"`
}

func (p *InstallationDeletePlan) Deletable() bool { return p != nil && len(p.Blockers) == 0 }

func planInstallationDelete(ctx context.Context, tx pgx.Tx, installationID int64, lock bool) (*InstallationDeletePlan, error) {
	forUpdate := ""
	if lock {
		forUpdate = " FOR UPDATE OF i"
	}
	p := &InstallationDeletePlan{InstallationID: installationID, Removes: map[string]int64{}, Blockers: []DeleteBlocker{}}
	err := tx.QueryRow(ctx, `
SELECT i.organization_id, o.name, u.discord_user_id, i.status, i.created_at,
       COALESCE(NULLIF(c.guild_name,''), g.discord_guild_id), i.game_server_id,
       COALESCE(NULLIF(gs.display_name,''), gs.provider_service_id, '')
FROM installations i
JOIN organizations o ON o.id = i.organization_id
JOIN app_users u ON u.id = o.owner_user_id
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = c.guild_id
LEFT JOIN game_servers gs ON gs.id = i.game_server_id
WHERE i.id = $1`+forUpdate, installationID).
		Scan(&p.OrganizationID, &p.OrganizationName, &p.OwnerDiscordID, &p.Status, &p.CreatedAt, &p.DiscordGuild, &p.GameServerID, &p.GameServerName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("delete plan: installation: %w", err)
	}
	subLock := ""
	if lock {
		subLock = " FOR UPDATE"
	}
	var providerSub string
	err = tx.QueryRow(ctx, `SELECT status, COALESCE(provider_subscription_id,'') FROM subscriptions WHERE organization_id = $1`+subLock, p.OrganizationID).Scan(&p.SubscriptionStatus, &providerSub)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("delete plan: subscription: %w", err)
	}
	p.LivePaidSubscription = SubscriptionIsLivePaid(providerSub, p.SubscriptionStatus)
	if p.LivePaidSubscription {
		p.Blockers = append(p.Blockers, DeleteBlocker{Code: DeleteBlockerLiveSubscription,
			Message: "This community has a live paid subscription in Stripe. Cancel the subscription first, then remove the installation."})
	}
	counts, err := dependentRows(ctx, tx, "installations", `SELECT * FROM installations WHERE id = $1`, installationID)
	if err != nil {
		return nil, err
	}
	removed, blockers := recordBlockers(counts)
	p.Removes = removed
	p.Blockers = append(p.Blockers, blockers...)
	return p, nil
}

// PlanInstallationDelete reports what removing the installation would do, changing nothing.
// nil, nil when the installation does not exist.
func (r *PlatformOwnerRepository) PlanInstallationDelete(ctx context.Context, installationID int64) (*InstallationDeletePlan, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("delete plan: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	return planInstallationDelete(ctx, tx, installationID, false)
}

// DeleteInstallation removes one installation and its configuration in one transaction, with
// the platform_audit_log row.
//
// Removed: the installation row and the configuration that hangs off it (setup progress,
// settings, channel routes, embed templates, feature flags, permissions, zones, shop catalogue,
// feature settings).
//
// Its game server, if it has one, is NOT deleted: it is switched off (active = FALSE, status
// DISCONNECTED, exactly what a suspension does, so no worker, scheduler or poster picks it up)
// and released from the organization. Kills, deaths, players, player links, stats and seasons
// are keyed by guild and game server and are all kept. The organization, its members, its
// Discord connection and its Nitrado credential are kept.
//
// Refused (ErrDeleteBlocked) while the organization has a live paid subscription, or while the
// installation holds records that are not configuration (shop orders, factions, cases, add-on
// billing): suspend such an installation instead. confirm must be the installation id.
//
// The caller stops the running worker for the returned GameServerID after the commit.
func (r *PlatformOwnerRepository) DeleteInstallation(ctx context.Context, installationID int64, confirm, actorDiscordID, reason string) (*InstallationDeletePlan, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("delete installation: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	plan, err := planInstallationDelete(ctx, tx, installationID, true)
	if err != nil || plan == nil {
		return nil, err
	}
	if !DeleteConfirmationMatches(strconv.FormatInt(installationID, 10), confirm) {
		return plan, ErrDeleteConfirmMismatch
	}
	if !plan.Deletable() {
		return plan, ErrDeleteBlocked
	}
	tag, err := tx.Exec(ctx, `DELETE FROM installations WHERE id = $1`, installationID)
	if err != nil {
		return nil, fmt.Errorf("delete installation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("delete installation: %d rows deleted, expected 1", tag.RowsAffected())
	}
	if plan.GameServerID != nil {
		if _, err := tx.Exec(ctx, `
UPDATE game_servers SET active = FALSE, status = 'DISCONNECTED', organization_id = NULL, updated_at = NOW()
WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM installations i WHERE i.game_server_id = $1)`, *plan.GameServerID); err != nil {
			return nil, fmt.Errorf("delete installation: switch off game server: %w", err)
		}
	}
	if err := recordDeleteAudit(ctx, tx, actorDiscordID, "installation.deleted", "installation", installationID, plan.OrganizationID, reason, plan); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("delete installation: commit: %w", err)
	}
	return plan, nil
}
