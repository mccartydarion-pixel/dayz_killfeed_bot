// Package adminrepo is the platform-admin (founder) READ-ONLY view over the SaaS
// tables. It is the one place allowed to read across organizations; every query
// is a fixed statement with bound parameters (search text and filters are never
// interpolated), and every list is keyset-paginated and bounded.
//
// Secrets never enter this package's types: the queries name their columns
// explicitly, and nothing here selects credential ciphertext, nonces, key
// versions, tokens, provider customer/subscription ids or any other secret.
//
// The JSON tags are the wire contract. The base shape is the one the Founder Hub
// website already consumes (website lib/admin/types.ts: flat `discordGuild` /
// `dayzServer` names, `owner` as a display name, embedded `installations`, ...);
// richer, authoritative detail is added under additional keys (`ownerUser`,
// `discord`, `server`, `generalSettings`, ...) so neither side needs a
// transformation. See docs/ADMIN_API.md.
package adminrepo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

const (
	DefaultLimit = 50
	MaxLimit     = 100
	MaxSearchLen = 100
	// maxDetailRows bounds the child lists inside a detail response.
	maxDetailRows = 200
	attentionRows = 50
)

// Installation statuses / subscription statuses / health values that actually
// exist in the backend model (no others are ever reported).
var (
	InstallationStatuses = []string{
		repository.InstallationNotStarted, repository.InstallationDiscordConnected, repository.InstallationNitradoConnected,
		repository.InstallationConfiguring, repository.InstallationReady, repository.InstallationDegraded,
		repository.InstallationDisconnected, repository.InstallationSuspended,
	}
	SubscriptionStatuses = []string{
		repository.SubscriptionTrial, repository.SubscriptionActive, repository.SubscriptionPastDue,
		repository.SubscriptionCanceled, repository.SubscriptionSuspended, repository.SubscriptionInactive,
	}
	HealthValues = []string{"HEALTHY", "DEGRADED", "OFFLINE", "SETTING_UP"}
)

// HealthForStatus is the same status -> health derivation the customer API
// already uses (app.installationHealth); a test keeps the two identical.
func HealthForStatus(status string) string {
	switch status {
	case repository.InstallationReady:
		return "HEALTHY"
	case repository.InstallationDegraded:
		return "DEGRADED"
	case repository.InstallationDisconnected, repository.InstallationSuspended:
		return "OFFLINE"
	default:
		return "SETTING_UP"
	}
}

// StatusesForHealth is the inverse: the installation statuses that map to h.
func StatusesForHealth(h string) []string {
	var out []string
	for _, s := range InstallationStatuses {
		if HealthForStatus(s) == h {
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// IsInstallationStatus / IsSubscriptionStatus / IsHealth validate filter input.
func IsInstallationStatus(v string) bool { return contains(InstallationStatuses, v) }
func IsSubscriptionStatus(v string) bool { return contains(SubscriptionStatuses, v) }
func IsHealth(v string) bool             { return contains(HealthValues, v) }

// Repository reads the SaaS tables across tenants.
type Repository struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Ping reports whether the database answers.
func (r *Repository) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }

// --- contract types -------------------------------------------------------------------

// UserRef is the owner as a full reference (the website's `owner` is just the name).
type UserRef struct {
	UserID      int64  `json:"id"`
	DisplayName string `json:"displayName"`
	DiscordID   string `json:"discordId"`
}

// SubscriptionInfo is the website's AdminSubscription. Billing state comes from Stripe via
// docs/BILLING.md's reconciliation, but no Stripe id or payment detail is ever exposed here -
// only what the founder needs to see (docs/BILLING.md "Admin visibility").
type SubscriptionInfo struct {
	Plan              string   `json:"plan"`
	Status            string   `json:"status"`
	TrialEndsAt       *string  `json:"trialEndsAt"`
	CurrentPeriodEnd  *string  `json:"currentPeriodEnd"`
	BillingInterval   string   `json:"billingInterval,omitempty"`
	CancelAtPeriodEnd bool     `json:"cancelAtPeriodEnd"`
	Entitlements      []string `json:"entitlements"`
	CreatedAt         *string  `json:"createdAt"`
	UpdatedAt         *string  `json:"updatedAt"`
}

// MemberRow is the website's AdminMember (`id` is a string there).
type MemberRow struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	DiscordID   string `json:"discordId"`
	Role        string `json:"role"`
	JoinedAt    string `json:"joinedAt"`
}

type DiscordInfo struct {
	GuildID             string `json:"guildId"`
	GuildName           string `json:"guildName"`
	BotInstalled        bool   `json:"botInstalled"`
	PermissionsVerified bool   `json:"permissionsVerified"`
}

type ServerInfo struct {
	ID          int64  `json:"id"`
	ServiceID   string `json:"serviceId"`
	DisplayName string `json:"displayName"`
	Platform    string `json:"platform"`
	Status      string `json:"status"`
	Active      bool   `json:"active"`
}

// InstallationSummary is the website's AdminInstallation (flat guild/server
// names, current setup step) plus additive authoritative detail.
type InstallationSummary struct {
	ID                int64   `json:"id"`
	InstallationID    int64   `json:"installationId"`
	OrganizationID    int64   `json:"organizationId"`
	Organization      string  `json:"organization"`
	DiscordGuild      *string `json:"discordGuild"`
	DayZServer        *string `json:"dayzServer"`
	Platform          *string `json:"platform"`
	Plan              string  `json:"plan"`
	Status            string  `json:"status"`
	Health            string  `json:"health"`
	CurrentSetupStep  *string `json:"currentSetupStep"`
	SetupCompletedAt  *string `json:"setupCompletedAt"`
	LastHealthCheckAt *string `json:"lastHealthCheckAt"`
	CreatedAt         string  `json:"createdAt"`

	Discord DiscordInfo `json:"discord"`
	Server  *ServerInfo `json:"server"`
}

// Organization is the website's AdminOrganization. In a list, Installations holds
// only the primary installation (READY one if any, else the newest) first-and-only,
// InstallationCount says how many there are; the detail carries all of them,
// primary first, and the members.
type Organization struct {
	ID                int64                 `json:"id"`
	Name              string                `json:"name"`
	Slug              string                `json:"slug"`
	CreatedAt         *string               `json:"createdAt"`
	Owner             *string               `json:"owner"`
	OwnerUser         *UserRef              `json:"ownerUser"`
	MemberCount       int                   `json:"memberCount"`
	InstallationCount int                   `json:"installationCount"`
	DiscordGuild      *string               `json:"discordGuild"`
	DayZServer        *string               `json:"dayzServer"`
	Subscription      *SubscriptionInfo     `json:"subscription"`
	Installations     []InstallationSummary `json:"installations"`
	Members           []MemberRow           `json:"members,omitempty"`
}

// SubscriptionRow is the website's AdminSubscription & {organization, installationCount}.
type SubscriptionRow struct {
	ID               int64   `json:"id"`
	Organization     string  `json:"organization"`
	OrganizationID   int64   `json:"organizationId"`
	OrganizationName string  `json:"organizationName"`
	Plan             string  `json:"plan"`
	Status           string  `json:"status"`
	TrialEndsAt      *string `json:"trialEndsAt"`
	CurrentPeriodEnd *string `json:"currentPeriodEnd"`
	// BillingInterval/CancelAtPeriodEnd are populated once billing.Service has reconciled a Stripe
	// subscription (docs/BILLING.md); "" / false for an organization still on the internal trial.
	// No Stripe id and no payment detail is ever exposed here (docs/BILLING.md "Admin visibility").
	BillingInterval   string   `json:"billingInterval,omitempty"`
	CancelAtPeriodEnd bool     `json:"cancelAtPeriodEnd"`
	Entitlements      []string `json:"entitlements"`
	InstallationCount int      `json:"installationCount"`
	CreatedAt         *string  `json:"createdAt"`
	UpdatedAt         *string  `json:"updatedAt"`
}

type SetupProgress struct {
	CurrentStep         string  `json:"currentStep"`
	DiscordCompleted    bool    `json:"discordCompleted"`
	NitradoCompleted    bool    `json:"nitradoCompleted"`
	ServerSelected      bool    `json:"serverSelected"`
	ChannelsCompleted   bool    `json:"channelsCompleted"`
	ValidationCompleted bool    `json:"validationCompleted"`
	CompletedAt         *string `json:"completedAt"`
}

type GeneralSettings struct {
	Timezone             string `json:"timezone"`
	DistanceUnit         string `json:"distanceUnit"`
	OnlineDisplayEnabled bool   `json:"onlineDisplayEnabled"`
	LeaderboardEnabled   bool   `json:"leaderboardEnabled"`
}

// ChannelRoute carries the stored routing. The database stores the channel id
// only; ChannelName stays null unless the caller fills it from the Discord cache.
type ChannelRoute struct {
	RouteKey          string  `json:"routeKey"`
	ChannelID         string  `json:"channelId"`
	ChannelName       *string `json:"channelName"`
	ManagedByChampion bool    `json:"managedByChampion"`
}

// NitradoStatus is the non-secret state of the organization's Nitrado link
// (never the credential envelope).
type NitradoStatus struct {
	Connected       bool    `json:"connected"`
	Status          string  `json:"status"`
	LastValidatedAt *string `json:"lastValidatedAt"`
	LastSuccessAt   *string `json:"lastSuccessAt"`
	LastFailureAt   *string `json:"lastFailureAt"`
	LastErrorClass  string  `json:"lastErrorClass,omitempty"`
}

// InstallationDetail is the website's AdminInstallation & {organization,
// subscription} with channelRoutes and settings (also exposed as generalSettings).
type InstallationDetail struct {
	InstallationSummary
	OrganizationSlug  string            `json:"organizationSlug"`
	UpdatedAt         string            `json:"updatedAt"`
	Subscription      *SubscriptionInfo `json:"subscription"`
	SetupProgress     *SetupProgress    `json:"setupProgress"`
	Settings          *GeneralSettings  `json:"settings"`
	GeneralSettings   *GeneralSettings  `json:"generalSettings"`
	ChannelRoutes     []ChannelRoute    `json:"channelRoutes"`
	NitradoConnection *NitradoStatus    `json:"nitradoConnection"`
}

// InstallationCounts / SubscriptionCounts use the real status vocabulary. Anything
// unrecognised is counted in Other rather than dropped or renamed.
type InstallationCounts struct {
	Total            int `json:"total"`
	NotStarted       int `json:"notStarted"`
	DiscordConnected int `json:"discordConnected"`
	NitradoConnected int `json:"nitradoConnected"`
	Configuring      int `json:"configuring"`
	Ready            int `json:"ready"`
	Degraded         int `json:"degraded"`
	Disconnected     int `json:"disconnected"`
	Suspended        int `json:"suspended"`
	Other            int `json:"other"`
}

type SubscriptionCounts struct {
	Total     int `json:"total"`
	Trial     int `json:"trial"`
	Active    int `json:"active"`
	PastDue   int `json:"pastDue"`
	Canceled  int `json:"canceled"`
	Suspended int `json:"suspended"`
	Other     int `json:"other"`
	// TrialExpired is derived: TRIAL subscriptions whose trial_ends_at has passed.
	TrialExpired int `json:"trialExpired"`
}

// Overview carries the website's flat fields and the structured breakdown.
// ActiveInstallations = READY + DEGRADED (set up and not disconnected/suspended).
// BackendStatus is filled by the HTTP layer from the runtime health registry.
type Overview struct {
	TotalOrganizations       int    `json:"totalOrganizations"`
	TotalUsers               int    `json:"totalUsers"`
	ActiveInstallations      int    `json:"activeInstallations"`
	ReadyInstallations       int    `json:"readyInstallations"`
	ConfiguringInstallations int    `json:"configuringInstallations"`
	DegradedInstallations    int    `json:"degradedInstallations"`
	Trials                   int    `json:"trials"`
	ActiveSubscriptions      int    `json:"activeSubscriptions"`
	SuspendedSubscriptions   int    `json:"suspendedSubscriptions"`
	BackendStatus            string `json:"backendStatus"`

	Organizations int                `json:"organizations"`
	Users         int                `json:"users"`
	Installations InstallationCounts `json:"installations"`
	Subscriptions SubscriptionCounts `json:"subscriptions"`
}

// HealthInstallation is one row of the website's AdminHealth.installations.
type HealthInstallation struct {
	ID                  int64   `json:"id"`
	OrganizationID      int64   `json:"organizationId"`
	Organization        string  `json:"organization"`
	Status              string  `json:"status"`
	Health              string  `json:"health"`
	DiscordBotInstalled *bool   `json:"discordBotInstalled"`
	PermissionsVerified bool    `json:"permissionsVerified"`
	ServerStatus        *string `json:"serverStatus"`
	LastHealthCheckAt   *string `json:"lastHealthCheckAt"`
}

// HealthSummary is the database side of GET /api/admin/health: stored state only,
// no live calls.
type HealthSummary struct {
	Total                 int            `json:"total"`
	ByStatus              map[string]int `json:"byStatus"`
	ByHealth              map[string]int `json:"byHealth"`
	ServersByStatus       map[string]int `json:"serversByStatus"`
	BotNotInstalled       int            `json:"botNotInstalled"`
	PermissionsUnverified int            `json:"permissionsUnverified"`
	ReadyNeverChecked     int            `json:"readyNeverChecked"`
}

// --- query helpers ------------------------------------------------------------------------

type qb struct {
	args  []any
	conds []string
}

func (b *qb) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

func (b *qb) where(cond string) { b.conds = append(b.conds, cond) }

func (b *qb) whereSQL() string {
	if len(b.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(b.conds, " AND ")
}

// NormalizeLimit is the repository-level backstop for the page size (the HTTP
// layer rejects bad input first): never zero, never above MaxLimit.
func NormalizeLimit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

// likePattern turns user text into a bound ILIKE pattern with LIKE
// metacharacters escaped (ESCAPE '\').
func likePattern(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > MaxSearchLen {
		s = string(r[:MaxSearchLen])
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return "%" + s + "%"
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func tsv(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := ts(t)
	return &s
}

func tsp(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := ts(*t)
	return &s
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func entitlementKeys(plan string) []string {
	keys := entitlements.Resolve(plan)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, string(k))
	}
	return out
}

// --- overview -----------------------------------------------------------------------------

func (r *Repository) Overview(ctx context.Context) (Overview, error) {
	var out Overview
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM organizations), (SELECT COUNT(*) FROM app_users)`).Scan(&out.Organizations, &out.Users); err != nil {
		return Overview{}, fmt.Errorf("overview totals: %w", err)
	}
	rows, err := r.pool.Query(ctx, `SELECT status, COUNT(*) FROM installations GROUP BY status`)
	if err != nil {
		return Overview{}, fmt.Errorf("overview installations: %w", err)
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return Overview{}, err
		}
		out.Installations.Total += n
		switch status {
		case repository.InstallationNotStarted:
			out.Installations.NotStarted += n
		case repository.InstallationDiscordConnected:
			out.Installations.DiscordConnected += n
		case repository.InstallationNitradoConnected:
			out.Installations.NitradoConnected += n
		case repository.InstallationConfiguring:
			out.Installations.Configuring += n
		case repository.InstallationReady:
			out.Installations.Ready += n
		case repository.InstallationDegraded:
			out.Installations.Degraded += n
		case repository.InstallationDisconnected:
			out.Installations.Disconnected += n
		case repository.InstallationSuspended:
			out.Installations.Suspended += n
		default:
			out.Installations.Other += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Overview{}, err
	}

	rows, err = r.pool.Query(ctx, `SELECT status, COUNT(*), COUNT(*) FILTER (WHERE status = 'TRIAL' AND trial_ends_at IS NOT NULL AND trial_ends_at < NOW()) FROM subscriptions GROUP BY status`)
	if err != nil {
		return Overview{}, fmt.Errorf("overview subscriptions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n, expired int
		if err := rows.Scan(&status, &n, &expired); err != nil {
			return Overview{}, err
		}
		out.Subscriptions.Total += n
		out.Subscriptions.TrialExpired += expired
		switch status {
		case repository.SubscriptionTrial:
			out.Subscriptions.Trial += n
		case repository.SubscriptionActive:
			out.Subscriptions.Active += n
		case repository.SubscriptionPastDue:
			out.Subscriptions.PastDue += n
		case repository.SubscriptionCanceled:
			out.Subscriptions.Canceled += n
		case repository.SubscriptionSuspended:
			out.Subscriptions.Suspended += n
		default:
			out.Subscriptions.Other += n
		}
	}
	if err := rows.Err(); err != nil {
		return Overview{}, err
	}

	out.TotalOrganizations, out.TotalUsers = out.Organizations, out.Users
	out.ReadyInstallations = out.Installations.Ready
	out.ConfiguringInstallations = out.Installations.Configuring
	out.DegradedInstallations = out.Installations.Degraded
	out.ActiveInstallations = out.Installations.Ready + out.Installations.Degraded
	out.Trials = out.Subscriptions.Trial
	out.ActiveSubscriptions = out.Subscriptions.Active
	out.SuspendedSubscriptions = out.Subscriptions.Suspended
	return out, nil
}

// --- installations (shared row shape) -------------------------------------------------------

type InstallationFilter struct {
	Limit          int
	Cursor         int64 // last installation id
	Search         string
	Status         string
	Health         string
	OrganizationID int64
}

const installationSelect = `
SELECT i.id, o.id, o.name, COALESCE(NULLIF(i.plan,''), s.plan, ''), i.status,
       g.discord_guild_id, COALESCE(dgc.guild_name,''), dgc.bot_installed, dgc.permissions_verified,
       gs.id, COALESCE(gs.provider_service_id,''), COALESCE(gs.display_name,''), COALESCE(gs.platform,''), COALESCE(gs.status,''), COALESCE(gs.active, FALSE),
       COALESCE(sp.current_step,''), COALESCE(sp.completed_at, i.setup_completed_at),
       i.last_health_check_at, i.created_at
FROM installations i
JOIN organizations o ON o.id = i.organization_id
LEFT JOIN subscriptions s ON s.organization_id = o.id
JOIN discord_guild_connections dgc ON dgc.id = i.discord_guild_connection_id
JOIN guilds g ON g.id = dgc.guild_id
LEFT JOIN game_servers gs ON gs.id = i.game_server_id
LEFT JOIN installation_setup_progress sp ON sp.installation_id = i.id`

func scanInstallation(row pgx.Row) (InstallationSummary, error) {
	var in InstallationSummary
	var gsID *int64
	var srv ServerInfo
	var step string
	var setupDone, check *time.Time
	var created time.Time
	err := row.Scan(&in.ID, &in.OrganizationID, &in.Organization, &in.Plan, &in.Status,
		&in.Discord.GuildID, &in.Discord.GuildName, &in.Discord.BotInstalled, &in.Discord.PermissionsVerified,
		&gsID, &srv.ServiceID, &srv.DisplayName, &srv.Platform, &srv.Status, &srv.Active,
		&step, &setupDone, &check, &created)
	if err != nil {
		return InstallationSummary{}, err
	}
	in.InstallationID = in.ID
	in.Health = HealthForStatus(in.Status)
	in.DiscordGuild = strOrNil(in.Discord.GuildName)
	if gsID != nil {
		srv.ID = *gsID
		in.Server = &srv
		in.DayZServer = strOrNil(srv.DisplayName)
		in.Platform = strOrNil(srv.Platform)
	}
	in.CurrentSetupStep = strOrNil(step)
	in.SetupCompletedAt = tsp(setupDone)
	in.LastHealthCheckAt = tsp(check)
	in.CreatedAt = ts(created)
	return in, nil
}

func (r *Repository) queryInstallations(ctx context.Context, sql string, args []any, limit int) ([]InstallationSummary, int64, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list installations: %w", err)
	}
	defer rows.Close()
	var out []InstallationSummary
	for rows.Next() {
		in, err := scanInstallation(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if limit > 0 && len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

func (r *Repository) ListInstallations(ctx context.Context, f InstallationFilter) ([]InstallationSummary, int64, error) {
	limit := NormalizeLimit(f.Limit)
	var b qb
	if f.Cursor > 0 {
		b.where("i.id < " + b.arg(f.Cursor))
	}
	if f.OrganizationID > 0 {
		b.where("i.organization_id = " + b.arg(f.OrganizationID))
	}
	if f.Status != "" {
		b.where("i.status = " + b.arg(f.Status))
	}
	if f.Health != "" {
		b.where("i.status = ANY(" + b.arg(StatusesForHealth(f.Health)) + ")")
	}
	if f.Search != "" {
		p := b.arg(likePattern(f.Search))
		b.where(`(o.name ILIKE ` + p + ` ESCAPE '\' OR o.slug ILIKE ` + p + ` ESCAPE '\' OR dgc.guild_name ILIKE ` + p + ` ESCAPE '\' OR gs.display_name ILIKE ` + p + ` ESCAPE '\')`)
	}
	return r.queryInstallations(ctx, installationSelect+b.whereSQL()+" ORDER BY i.id DESC LIMIT "+b.arg(limit+1), b.args, limit)
}

// installationsByID fetches specific installations in one statement.
func (r *Repository) installationsByID(ctx context.Context, ids []int64) (map[int64]InstallationSummary, error) {
	out := make(map[int64]InstallationSummary, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, _, err := r.queryInstallations(ctx, installationSelect+" WHERE i.id = ANY($1)", []any{ids}, 0)
	if err != nil {
		return nil, err
	}
	for _, in := range rows {
		out[in.ID] = in
	}
	return out, nil
}

// --- organizations ---------------------------------------------------------------------------

type OrganizationFilter struct {
	Limit              int
	Cursor             int64 // last organization id of the previous page; 0 = first page
	Search             string
	Plan               string
	SubscriptionStatus string
	InstallationStatus string
}

// The primary installation is the organization's READY one if any, else its
// newest; its id comes from the same statement (LATERAL) and the summaries of a
// whole page are then fetched in ONE further statement - never per row.
const orgSelect = `
SELECT o.id, o.name, o.slug, o.created_at,
       u.id, COALESCE(NULLIF(u.discord_global_name,''), u.discord_username), u.discord_user_id,
       (SELECT COUNT(*) FROM organization_members m WHERE m.organization_id = o.id),
       s.plan, s.status, s.trial_ends_at, s.current_period_end, COALESCE(s.billing_interval,''), COALESCE(s.cancel_at_period_end,false), s.created_at, s.updated_at,
       (SELECT COUNT(*) FROM installations ic WHERE ic.organization_id = o.id),
       pi.id
FROM organizations o
JOIN app_users u ON u.id = o.owner_user_id
LEFT JOIN subscriptions s ON s.organization_id = o.id
LEFT JOIN LATERAL (
    SELECT i.id FROM installations i WHERE i.organization_id = o.id
    ORDER BY (i.status = 'READY') DESC, i.id DESC LIMIT 1
) pi ON TRUE`

type orgRec struct {
	org       Organization
	primaryID *int64
}

func scanOrg(row pgx.Row) (orgRec, error) {
	var rec orgRec
	o := &rec.org
	var created time.Time
	var owner UserRef
	var plan, status *string
	var interval string
	var cancelAtPeriodEnd bool
	var trial, period, sCreated, sUpdated *time.Time
	err := row.Scan(&o.ID, &o.Name, &o.Slug, &created,
		&owner.UserID, &owner.DisplayName, &owner.DiscordID,
		&o.MemberCount, &plan, &status, &trial, &period, &interval, &cancelAtPeriodEnd, &sCreated, &sUpdated,
		&o.InstallationCount, &rec.primaryID)
	if err != nil {
		return orgRec{}, err
	}
	o.CreatedAt = tsv(created)
	o.Owner = &owner.DisplayName
	o.OwnerUser = &owner
	if plan != nil && status != nil {
		o.Subscription = &SubscriptionInfo{Plan: *plan, Status: *status, TrialEndsAt: tsp(trial), CurrentPeriodEnd: tsp(period),
			BillingInterval: interval, CancelAtPeriodEnd: cancelAtPeriodEnd, Entitlements: entitlementKeys(*plan), CreatedAt: tsp(sCreated), UpdatedAt: tsp(sUpdated)}
	}
	o.Installations = []InstallationSummary{}
	return rec, nil
}

// ListOrganizations returns one page (newest first) and the cursor for the next
// (0 = no more). Two statements per page regardless of page size.
func (r *Repository) ListOrganizations(ctx context.Context, f OrganizationFilter) ([]Organization, int64, error) {
	limit := NormalizeLimit(f.Limit)
	var b qb
	if f.Cursor > 0 {
		b.where("o.id < " + b.arg(f.Cursor))
	}
	if f.Search != "" {
		p := b.arg(likePattern(f.Search))
		b.where(`(o.name ILIKE ` + p + ` ESCAPE '\' OR o.slug ILIKE ` + p + ` ESCAPE '\' OR EXISTS (
    SELECT 1 FROM installations si
    JOIN discord_guild_connections sg ON sg.id = si.discord_guild_connection_id
    LEFT JOIN game_servers ss ON ss.id = si.game_server_id
    WHERE si.organization_id = o.id AND (sg.guild_name ILIKE ` + p + ` ESCAPE '\' OR ss.display_name ILIKE ` + p + ` ESCAPE '\')))`)
	}
	if f.Plan != "" {
		b.where("UPPER(s.plan) = " + b.arg(strings.ToUpper(f.Plan)))
	}
	if f.SubscriptionStatus != "" {
		b.where("s.status = " + b.arg(f.SubscriptionStatus))
	}
	if f.InstallationStatus != "" {
		b.where("EXISTS (SELECT 1 FROM installations fi WHERE fi.organization_id = o.id AND fi.status = " + b.arg(f.InstallationStatus) + ")")
	}
	rows, err := r.pool.Query(ctx, orgSelect+b.whereSQL()+" ORDER BY o.id DESC LIMIT "+b.arg(limit+1), b.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list organizations: %w", err)
	}
	var recs []orgRec
	for rows.Next() {
		rec, err := scanOrg(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		recs = append(recs, rec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(recs) > limit {
		recs = recs[:limit]
		next = recs[len(recs)-1].org.ID
	}

	var primaryIDs []int64
	for _, rec := range recs {
		if rec.primaryID != nil {
			primaryIDs = append(primaryIDs, *rec.primaryID)
		}
	}
	prim, err := r.installationsByID(ctx, primaryIDs)
	if err != nil {
		return nil, 0, err
	}
	out := make([]Organization, 0, len(recs))
	for _, rec := range recs {
		o := rec.org
		if rec.primaryID != nil {
			if in, ok := prim[*rec.primaryID]; ok {
				o.Installations = []InstallationSummary{in}
				o.DiscordGuild, o.DayZServer = in.DiscordGuild, in.DayZServer
			}
		}
		out = append(out, o)
	}
	return out, next, nil
}

// GetOrganization returns nil, nil when the organization does not exist.
func (r *Repository) GetOrganization(ctx context.Context, id int64) (*Organization, error) {
	rec, err := scanOrg(r.pool.QueryRow(ctx, orgSelect+" WHERE o.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get organization: %w", err)
	}
	o := rec.org
	o.Members = []MemberRow{}
	mrows, err := r.pool.Query(ctx, `
SELECT u.id, COALESCE(NULLIF(u.discord_global_name,''), u.discord_username), u.discord_user_id, m.role, m.created_at
FROM organization_members m JOIN app_users u ON u.id = m.user_id
WHERE m.organization_id = $1
ORDER BY (m.role = 'OWNER') DESC, (m.role = 'ADMIN') DESC, m.id LIMIT $2`, id, maxDetailRows)
	if err != nil {
		return nil, fmt.Errorf("organization members: %w", err)
	}
	for mrows.Next() {
		var m MemberRow
		var uid int64
		var joined time.Time
		if err := mrows.Scan(&uid, &m.DisplayName, &m.DiscordID, &m.Role, &joined); err != nil {
			mrows.Close()
			return nil, err
		}
		m.ID = strconv.FormatInt(uid, 10)
		m.JoinedAt = ts(joined)
		o.Members = append(o.Members, m)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return nil, err
	}
	insts, _, err := r.ListInstallations(ctx, InstallationFilter{Limit: maxDetailRows, OrganizationID: id})
	if err != nil {
		return nil, err
	}
	// Primary first, as in the list, so installations[0] means the same thing.
	for i, in := range insts {
		if rec.primaryID != nil && in.ID == *rec.primaryID {
			insts[0], insts[i] = insts[i], insts[0]
			break
		}
	}
	if insts != nil {
		o.Installations = insts
		o.DiscordGuild, o.DayZServer = insts[0].DiscordGuild, insts[0].DayZServer
	}
	return &o, nil
}

// --- subscriptions ----------------------------------------------------------------------------

type SubscriptionFilter struct {
	Limit  int
	Cursor int64 // last subscription id
	Search string
	Status string
	Plan   string
}

func (r *Repository) ListSubscriptions(ctx context.Context, f SubscriptionFilter) ([]SubscriptionRow, int64, error) {
	limit := NormalizeLimit(f.Limit)
	var b qb
	if f.Cursor > 0 {
		b.where("s.id < " + b.arg(f.Cursor))
	}
	if f.Search != "" {
		p := b.arg(likePattern(f.Search))
		b.where(`(o.name ILIKE ` + p + ` ESCAPE '\' OR o.slug ILIKE ` + p + ` ESCAPE '\')`)
	}
	if f.Status != "" {
		b.where("s.status = " + b.arg(f.Status))
	}
	if f.Plan != "" {
		b.where("UPPER(s.plan) = " + b.arg(strings.ToUpper(f.Plan)))
	}
	sql := `
SELECT s.id, o.id, o.name, s.plan, s.status, s.trial_ends_at, s.current_period_end,
       COALESCE(s.billing_interval,''), s.cancel_at_period_end,
       (SELECT COUNT(*) FROM installations ic WHERE ic.organization_id = o.id),
       s.created_at, s.updated_at
FROM subscriptions s JOIN organizations o ON o.id = s.organization_id` + b.whereSQL() + " ORDER BY s.id DESC LIMIT " + b.arg(limit+1)
	rows, err := r.pool.Query(ctx, sql, b.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list subscriptions: %w", err)
	}
	defer rows.Close()
	var out []SubscriptionRow
	for rows.Next() {
		var s SubscriptionRow
		var trial, period *time.Time
		var created, updated time.Time
		if err := rows.Scan(&s.ID, &s.OrganizationID, &s.OrganizationName, &s.Plan, &s.Status, &trial, &period, &s.BillingInterval, &s.CancelAtPeriodEnd, &s.InstallationCount, &created, &updated); err != nil {
			return nil, 0, err
		}
		s.Organization = s.OrganizationName
		s.TrialEndsAt, s.CurrentPeriodEnd = tsp(trial), tsp(period)
		s.CreatedAt, s.UpdatedAt = tsv(created), tsv(updated)
		s.Entitlements = entitlementKeys(s.Plan)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

// GetInstallation returns nil, nil when the installation does not exist. Every
// child read is keyed by this installation's id (or its organization), so routes,
// settings and the Nitrado state can never belong to another tenant.
func (r *Repository) GetInstallation(ctx context.Context, id int64) (*InstallationDetail, error) {
	sum, err := scanInstallation(r.pool.QueryRow(ctx, installationSelect+" WHERE i.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get installation: %w", err)
	}
	d := &InstallationDetail{InstallationSummary: sum, ChannelRoutes: []ChannelRoute{}}

	var instUpdated time.Time
	var plan, status *string
	var interval string
	var cancelAtPeriodEnd bool
	var trial, period, sCreated, sUpdated *time.Time
	if err := r.pool.QueryRow(ctx, `
SELECT o.slug, i.updated_at, s.plan, s.status, s.trial_ends_at, s.current_period_end, COALESCE(s.billing_interval,''), COALESCE(s.cancel_at_period_end,false), s.created_at, s.updated_at
FROM installations i JOIN organizations o ON o.id = i.organization_id
LEFT JOIN subscriptions s ON s.organization_id = o.id WHERE i.id = $1`, id).
		Scan(&d.OrganizationSlug, &instUpdated, &plan, &status, &trial, &period, &interval, &cancelAtPeriodEnd, &sCreated, &sUpdated); err != nil {
		return nil, fmt.Errorf("installation organization: %w", err)
	}
	d.UpdatedAt = ts(instUpdated)
	if plan != nil && status != nil {
		d.Subscription = &SubscriptionInfo{Plan: *plan, Status: *status, TrialEndsAt: tsp(trial), CurrentPeriodEnd: tsp(period),
			BillingInterval: interval, CancelAtPeriodEnd: cancelAtPeriodEnd, Entitlements: entitlementKeys(*plan), CreatedAt: tsp(sCreated), UpdatedAt: tsp(sUpdated)}
	}

	var sp SetupProgress
	var spDone *time.Time
	err = r.pool.QueryRow(ctx, `SELECT current_step, discord_completed, nitrado_completed, server_selected, channels_completed, validation_completed, completed_at FROM installation_setup_progress WHERE installation_id = $1`, id).
		Scan(&sp.CurrentStep, &sp.DiscordCompleted, &sp.NitradoCompleted, &sp.ServerSelected, &sp.ChannelsCompleted, &sp.ValidationCompleted, &spDone)
	switch {
	case err == nil:
		sp.CompletedAt = tsp(spDone)
		d.SetupProgress = &sp
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("setup progress: %w", err)
	}

	var gsettings GeneralSettings
	err = r.pool.QueryRow(ctx, `SELECT timezone, distance_unit, online_display_enabled, leaderboard_enabled FROM installation_settings WHERE installation_id = $1`, id).
		Scan(&gsettings.Timezone, &gsettings.DistanceUnit, &gsettings.OnlineDisplayEnabled, &gsettings.LeaderboardEnabled)
	switch {
	case err == nil:
		d.Settings, d.GeneralSettings = &gsettings, &gsettings
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("installation settings: %w", err)
	}

	rrows, err := r.pool.Query(ctx, `SELECT route_key, channel_id, managed_by_champion FROM installation_channel_routes WHERE installation_id = $1 ORDER BY route_key LIMIT $2`, id, maxDetailRows)
	if err != nil {
		return nil, fmt.Errorf("channel routes: %w", err)
	}
	for rrows.Next() {
		var cr ChannelRoute
		if err := rrows.Scan(&cr.RouteKey, &cr.ChannelID, &cr.ManagedByChampion); err != nil {
			rrows.Close()
			return nil, err
		}
		d.ChannelRoutes = append(d.ChannelRoutes, cr)
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return nil, err
	}

	// Only status columns are read - never the credential ciphertext/nonce/key version.
	var ns NitradoStatus
	var validated, success, failure *time.Time
	var errClass *string
	err = r.pool.QueryRow(ctx, `SELECT n.status, n.last_validated_at, n.last_success_at, n.last_failure_at, n.last_error_class FROM nitrado_connections n JOIN installations i ON i.organization_id = n.organization_id WHERE i.id = $1`, id).
		Scan(&ns.Status, &validated, &success, &failure, &errClass)
	switch {
	case err == nil:
		ns.Connected = true
		ns.LastValidatedAt, ns.LastSuccessAt, ns.LastFailureAt = tsp(validated), tsp(success), tsp(failure)
		if errClass != nil {
			ns.LastErrorClass = *errClass
		}
		d.NitradoConnection = &ns
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("nitrado status: %w", err)
	}
	return d, nil
}

// --- health ------------------------------------------------------------------------------------

// InstallationHealth summarises stored installation state and lists (bounded)
// the installations that need attention. It performs no live Nitrado or Discord
// calls.
func (r *Repository) InstallationHealth(ctx context.Context) (HealthSummary, []HealthInstallation, error) {
	sum := HealthSummary{ByStatus: map[string]int{}, ByHealth: map[string]int{}, ServersByStatus: map[string]int{}}
	items := []HealthInstallation{}
	rows, err := r.pool.Query(ctx, `SELECT status, COUNT(*) FROM installations GROUP BY status`)
	if err != nil {
		return sum, items, fmt.Errorf("health by status: %w", err)
	}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			rows.Close()
			return sum, items, err
		}
		sum.Total += n
		sum.ByStatus[s] += n
		sum.ByHealth[HealthForStatus(s)] += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return sum, items, err
	}

	if err := r.pool.QueryRow(ctx, `
SELECT COUNT(*) FILTER (WHERE NOT dgc.bot_installed),
       COUNT(*) FILTER (WHERE NOT dgc.permissions_verified),
       COUNT(*) FILTER (WHERE i.status = 'READY' AND i.last_health_check_at IS NULL)
FROM installations i JOIN discord_guild_connections dgc ON dgc.id = i.discord_guild_connection_id`).
		Scan(&sum.BotNotInstalled, &sum.PermissionsUnverified, &sum.ReadyNeverChecked); err != nil {
		return sum, items, fmt.Errorf("health discord: %w", err)
	}

	srows, err := r.pool.Query(ctx, `SELECT gs.status, COUNT(*) FROM installations i JOIN game_servers gs ON gs.id = i.game_server_id GROUP BY gs.status`)
	if err != nil {
		return sum, items, fmt.Errorf("health servers: %w", err)
	}
	for srows.Next() {
		var s string
		var n int
		if err := srows.Scan(&s, &n); err != nil {
			srows.Close()
			return sum, items, err
		}
		sum.ServersByStatus[s] += n
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return sum, items, err
	}

	var b qb
	b.where("i.status = ANY(" + b.arg([]string{repository.InstallationDegraded, repository.InstallationDisconnected, repository.InstallationSuspended}) + ")")
	attention, _, err := r.queryInstallations(ctx, installationSelect+b.whereSQL()+" ORDER BY i.last_health_check_at ASC NULLS FIRST, i.id DESC LIMIT "+b.arg(attentionRows), b.args, 0)
	if err != nil {
		return sum, items, err
	}
	for _, in := range attention {
		bot := in.Discord.BotInstalled
		h := HealthInstallation{ID: in.ID, OrganizationID: in.OrganizationID, Organization: in.Organization, Status: in.Status, Health: in.Health,
			DiscordBotInstalled: &bot, PermissionsVerified: in.Discord.PermissionsVerified, LastHealthCheckAt: in.LastHealthCheckAt}
		if in.Server != nil {
			h.ServerStatus = strOrNil(in.Server.Status)
		}
		items = append(items, h)
	}
	return sum, items, nil
}
