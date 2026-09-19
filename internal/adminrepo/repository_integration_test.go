//go:build integration

package adminrepo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/database"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return db.Pool
}

// world seeds several tenants straight into the tables (no repository shortcuts
// that could hide a cross-tenant mix-up). Every name carries a unique tag so the
// tests can scope searches to their own rows on a shared database.
type world struct {
	t    *testing.T
	pool *pgxpool.Pool
	ctx  context.Context
	tag  string
	n    int
}

func newWorld(t *testing.T) *world {
	return &world{t: t, pool: testPool(t), ctx: context.Background(), tag: fmt.Sprintf("t%d", time.Now().UnixNano())}
}

func (w *world) must(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) user(name string) (id int64, discordID string) {
	w.t.Helper()
	w.n++
	discordID = fmt.Sprintf("%s-u%d", w.tag, w.n)
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO app_users(discord_user_id, discord_username, discord_global_name) VALUES($1,$2,NULLIF($3,'')) RETURNING id`, discordID, "user-"+discordID, name).Scan(&id))
	return
}

func (w *world) org(name string, owner int64, plan, status string) int64 {
	w.t.Helper()
	w.n++
	var id int64
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO organizations(name, slug, owner_user_id) VALUES($1,$2,$3) RETURNING id`, name, fmt.Sprintf("%s-org%d", w.tag, w.n), owner).Scan(&id))
	w.must(w.exec(`INSERT INTO organization_members(organization_id, user_id, role) VALUES($1,$2,'OWNER')`, id, owner))
	if plan != "" {
		trial := time.Now().Add(72 * time.Hour)
		w.must(w.exec(`INSERT INTO subscriptions(organization_id, plan, status, trial_ends_at, provider, provider_customer_id, provider_subscription_id) VALUES($1,$2,$3,$4,'stripe','cus_SECRETCUSTOMER','sub_SECRETSUB')`, id, plan, status, trial))
	}
	return id
}

func (w *world) member(orgID, userID int64, role string) {
	w.t.Helper()
	w.must(w.exec(`INSERT INTO organization_members(organization_id, user_id, role) VALUES($1,$2,$3)`, orgID, userID, role))
}

func (w *world) exec(sql string, args ...any) error {
	_, err := w.pool.Exec(w.ctx, sql, args...)
	return err
}

// installation creates guild + connection + (optional) server + installation +
// setup progress + settings + routes for one org.
func (w *world) installation(orgID int64, guildName, serverName, status string, routes map[string]string) int64 {
	w.t.Helper()
	w.n++
	discordGuild := fmt.Sprintf("%s-g%d", w.tag, w.n)
	var guildRow, connID, serverID, instID int64
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, discordGuild).Scan(&guildRow))
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO discord_guild_connections(organization_id, guild_id, guild_name, bot_installed, permissions_verified) VALUES($1,$2,$3,TRUE,$4) RETURNING id`, orgID, guildRow, guildName, status == "READY").Scan(&connID))
	var serverArg any
	if serverName != "" {
		w.must(w.pool.QueryRow(w.ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, organization_id) VALUES($1,'NITRADO',$2,'DAYZ','PLAYSTATION',$3,'ACTIVE',$4) RETURNING id`, guildRow, fmt.Sprintf("svc-%s-%d", w.tag, w.n), serverName, orgID).Scan(&serverID))
		serverArg = serverID
	}
	w.must(w.pool.QueryRow(w.ctx, `INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,$4) RETURNING id`, orgID, connID, serverArg, status).Scan(&instID))
	w.must(w.exec(`INSERT INTO installation_setup_progress(installation_id, current_step, discord_completed) VALUES($1,'CHANNELS',TRUE)`, instID))
	w.must(w.exec(`INSERT INTO installation_settings(installation_id, timezone, distance_unit) VALUES($1,'Europe/London','FEET')`, instID))
	for key, ch := range routes {
		w.must(w.exec(`INSERT INTO installation_channel_routes(installation_id, route_key, channel_id, managed_by_champion) VALUES($1,$2,$3,$4)`, instID, key, ch, key == "BOUNTY"))
	}
	return instID
}

// scenario is a fixed set of tenants:
//
//	Alpha:   owner + ADMIN + MEMBER, TRIAL/TRIAL, one READY installation with routes
//	Bravo:   ACTIVE/PRO, a DEGRADED installation and a newer CONFIGURING one (no server)
//	Charlie: SUSPENDED/BASIC, no installations
//	Delta:   name contains LIKE metacharacters, ACTIVE/PRO, one DISCONNECTED installation
//	Echo:    no subscription row at all
type scenario struct {
	alpha, bravo, charlie, delta, echo                int64
	iAlpha, iBravoDegraded, iBravoConfiguring, iDelta int64
	alphaOwner, alphaAdmin, alphaMember               int64
}

func (w *world) scenario() scenario {
	var s scenario
	uAlpha, _ := w.user("Alice Owner")
	s.alphaOwner = uAlpha
	s.alphaAdmin, _ = w.user("Adam Admin")
	s.alphaMember, _ = w.user("Mia Member")
	uBravo, _ := w.user("Bob Owner")
	uCharlie, _ := w.user("Carol Owner")
	uDelta, _ := w.user("Dan Owner")
	uEcho, _ := w.user("Eve Owner")

	s.alpha = w.org("Alpha-"+w.tag, uAlpha, "TRIAL", "TRIAL")
	w.member(s.alpha, s.alphaAdmin, "ADMIN")
	w.member(s.alpha, s.alphaMember, "MEMBER")
	s.bravo = w.org("Bravo-"+w.tag, uBravo, "PRO", "ACTIVE")
	s.charlie = w.org("Charlie-"+w.tag, uCharlie, "BASIC", "SUSPENDED")
	s.delta = w.org("Delta%_-"+w.tag, uDelta, "PRO", "ACTIVE")
	s.echo = w.org("Echo-"+w.tag, uEcho, "", "")

	s.iAlpha = w.installation(s.alpha, "AlphaGuild-"+w.tag, "AlphaServer-"+w.tag, "READY", map[string]string{"KILLFEED": "c-alpha-kf", "BOUNTY": "c-alpha-bounty"})
	s.iBravoDegraded = w.installation(s.bravo, "BravoGuild-"+w.tag, "BravoServer-"+w.tag, "DEGRADED", map[string]string{"KILLFEED": "c-bravo-kf"})
	s.iBravoConfiguring = w.installation(s.bravo, "BravoSecond-"+w.tag, "", "CONFIGURING", nil)
	s.iDelta = w.installation(s.delta, "DeltaGuild-"+w.tag, "DeltaServer-"+w.tag, "DISCONNECTED", map[string]string{"KILLFEED": "c-delta-kf"})
	return s
}

func orgIDs(rows []Organization) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func eqIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (w *world) orgs(f OrganizationFilter) []Organization {
	w.t.Helper()
	f.Limit = 100
	rows, _, err := New(w.pool).ListOrganizations(w.ctx, f)
	w.must(err)
	return rows
}

// --- overview ---------------------------------------------------------------------------

func TestAdminOverviewCountsRealState(t *testing.T) {
	w := newWorld(t)
	repo := New(w.pool)
	before, err := repo.Overview(w.ctx)
	w.must(err)
	w.scenario()
	after, err := repo.Overview(w.ctx)
	w.must(err)

	delta := func(name string, got, want int) {
		if got != want {
			t.Errorf("%s: expected +%d, got +%d", name, want, got)
		}
	}
	delta("organizations", after.Organizations-before.Organizations, 5)
	delta("users", after.Users-before.Users, 7)
	delta("installations.total", after.Installations.Total-before.Installations.Total, 4)
	delta("installations.ready", after.Installations.Ready-before.Installations.Ready, 1)
	delta("installations.degraded", after.Installations.Degraded-before.Installations.Degraded, 1)
	delta("installations.configuring", after.Installations.Configuring-before.Installations.Configuring, 1)
	delta("installations.disconnected", after.Installations.Disconnected-before.Installations.Disconnected, 1)
	delta("installations.suspended", after.Installations.Suspended-before.Installations.Suspended, 0)
	delta("subscriptions.total", after.Subscriptions.Total-before.Subscriptions.Total, 4)
	delta("subscriptions.trial", after.Subscriptions.Trial-before.Subscriptions.Trial, 1)
	delta("subscriptions.active", after.Subscriptions.Active-before.Subscriptions.Active, 2)
	delta("subscriptions.suspended", after.Subscriptions.Suspended-before.Subscriptions.Suspended, 1)
	delta("subscriptions.trialExpired", after.Subscriptions.TrialExpired-before.Subscriptions.TrialExpired, 0)
	// The website's flat fields are the same authoritative counts.
	delta("totalOrganizations", after.TotalOrganizations-before.TotalOrganizations, 5)
	delta("totalUsers", after.TotalUsers-before.TotalUsers, 7)
	delta("readyInstallations", after.ReadyInstallations-before.ReadyInstallations, 1)
	delta("configuringInstallations", after.ConfiguringInstallations-before.ConfiguringInstallations, 1)
	delta("degradedInstallations", after.DegradedInstallations-before.DegradedInstallations, 1)
	delta("activeInstallations (READY+DEGRADED)", after.ActiveInstallations-before.ActiveInstallations, 2)
	delta("trials", after.Trials-before.Trials, 1)
	delta("activeSubscriptions", after.ActiveSubscriptions-before.ActiveSubscriptions, 2)
	delta("suspendedSubscriptions", after.SuspendedSubscriptions-before.SuspendedSubscriptions, 1)

	// A TRIAL whose end has passed is derived as trialExpired; nothing is invented otherwise.
	u, _ := w.user("Old Trial")
	org := w.org("Expired-"+w.tag, u, "TRIAL", "TRIAL")
	w.must(w.exec(`UPDATE subscriptions SET trial_ends_at = NOW() - INTERVAL '1 day' WHERE organization_id=$1`, org))
	again, err := repo.Overview(w.ctx)
	w.must(err)
	delta("subscriptions.trialExpired after expiry", again.Subscriptions.TrialExpired-after.Subscriptions.TrialExpired, 1)
	if again.Installations.Other != before.Installations.Other || again.Subscriptions.Other != before.Subscriptions.Other {
		t.Error("real statuses must not spill into 'other'")
	}
}

// --- organizations: listing, summaries, search, filters, pagination ----------------------------------

func TestAdminOrganizationListSummaries(t *testing.T) {
	w := newWorld(t)
	s := w.scenario()
	rows := w.orgs(OrganizationFilter{Search: w.tag})
	if want := []int64{s.echo, s.delta, s.charlie, s.bravo, s.alpha}; !eqIDs(orgIDs(rows), want) {
		t.Fatalf("expected the five tenants newest-first %v, got %v", want, orgIDs(rows))
	}
	by := map[int64]Organization{}
	for _, r := range rows {
		by[r.ID] = r
	}

	a := by[s.alpha]
	if a.OwnerUser == nil || a.OwnerUser.UserID != s.alphaOwner || a.OwnerUser.DisplayName != "Alice Owner" || !strings.HasPrefix(a.OwnerUser.DiscordID, w.tag) ||
		a.Owner == nil || *a.Owner != "Alice Owner" {
		t.Errorf("alpha owner: %+v / %v", a.OwnerUser, a.Owner)
	}
	if a.MemberCount != 3 || a.InstallationCount != 1 || a.Slug == "" || a.CreatedAt == nil {
		t.Errorf("alpha counts: members=%d installations=%d slug=%q created=%v", a.MemberCount, a.InstallationCount, a.Slug, a.CreatedAt)
	}
	if a.Subscription == nil || a.Subscription.Plan != "TRIAL" || a.Subscription.Status != "TRIAL" || a.Subscription.TrialEndsAt == nil || len(a.Subscription.Entitlements) == 0 ||
		a.Subscription.CreatedAt == nil || a.Subscription.UpdatedAt == nil {
		t.Errorf("alpha subscription: %+v", a.Subscription)
	}
	// The list carries exactly the primary installation, first and only.
	if len(a.Installations) != 1 {
		t.Fatalf("alpha list row must embed its primary installation only: %+v", a.Installations)
	}
	p := a.Installations[0]
	if p.ID != s.iAlpha || p.Status != "READY" || p.Health != "HEALTHY" || deref(p.DiscordGuild) != "AlphaGuild-"+w.tag || deref(p.DayZServer) != "AlphaServer-"+w.tag || deref(p.Platform) != "PLAYSTATION" || p.Discord.GuildID == "" || !p.Discord.BotInstalled {
		t.Errorf("alpha primary installation: %+v", p)
	}
	if deref(a.DiscordGuild) != "AlphaGuild-"+w.tag || deref(a.DayZServer) != "AlphaServer-"+w.tag {
		t.Errorf("the organization row's flat guild/server come from its primary installation: %v %v", a.DiscordGuild, a.DayZServer)
	}
	// Bravo: no READY installation, so the primary is its newest one (CONFIGURING, no server).
	b := by[s.bravo]
	if b.InstallationCount != 2 || len(b.Installations) != 1 || b.Installations[0].ID != s.iBravoConfiguring || b.Installations[0].Health != "SETTING_UP" || b.Installations[0].DayZServer != nil || b.DayZServer != nil {
		t.Errorf("bravo: count=%d installations=%+v", b.InstallationCount, b.Installations)
	}
	if c := by[s.charlie]; c.InstallationCount != 0 || len(c.Installations) != 0 || c.Installations == nil || c.Subscription == nil || c.Subscription.Status != "SUSPENDED" || c.DiscordGuild != nil {
		t.Errorf("charlie: %+v", c)
	}
	// No subscription row: reported as absent, not fabricated.
	if e := by[s.echo]; e.Subscription != nil || e.MemberCount != 1 {
		t.Errorf("echo: %+v", e)
	}
	if d := by[s.delta]; len(d.Installations) != 1 || d.Installations[0].Health != "OFFLINE" {
		t.Errorf("delta primary: %+v", d.Installations)
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestAdminOrganizationSearchIsParameterizedAndScoped(t *testing.T) {
	w := newWorld(t)
	s := w.scenario()
	names := func(f OrganizationFilter) []int64 { return orgIDs(w.orgs(f)) }

	// organization name, Discord guild name and DayZ server name; case-insensitive.
	if got := names(OrganizationFilter{Search: "alpha-" + w.tag}); !eqIDs(got, []int64{s.alpha}) {
		t.Errorf("org name search: %v", got)
	}
	if got := names(OrganizationFilter{Search: "AlphaGuild-" + w.tag}); !eqIDs(got, []int64{s.alpha}) {
		t.Errorf("guild name search: %v", got)
	}
	if got := names(OrganizationFilter{Search: "bravoserver-" + strings.ToUpper(w.tag)}); !eqIDs(got, []int64{s.bravo}) {
		t.Errorf("server name search: %v", got)
	}
	if got := names(OrganizationFilter{Search: "BravoSecond-" + w.tag}); !eqIDs(got, []int64{s.bravo}) {
		t.Errorf("a tenant's second installation must be searchable: %v", got)
	}
	// LIKE metacharacters are literal: '%' and '_' match only the name that contains them.
	if got := names(OrganizationFilter{Search: "Delta%_-" + w.tag}); !eqIDs(got, []int64{s.delta}) {
		t.Errorf("literal metacharacters: %v", got)
	}
	if got := names(OrganizationFilter{Search: "_-" + w.tag}); !eqIDs(got, []int64{s.delta}) {
		t.Errorf("'_' must be a literal (not a one-character wildcard), got %v", got)
	}
	if got := names(OrganizationFilter{Search: "%" + "_-" + w.tag}); !eqIDs(got, []int64{s.delta}) {
		t.Errorf("'%%' must be a literal, got %v", got)
	}
	// Injection attempts are just text: no rows, no error, nothing dropped.
	for _, evil := range []string{`'; DROP TABLE organizations; --`, `" OR 1=1 --`, `x') OR ('1'='1`, "nul\x00byte"} {
		rows, _, err := New(w.pool).ListOrganizations(w.ctx, OrganizationFilter{Search: evil, Limit: 100})
		if err != nil || len(rows) != 0 {
			t.Errorf("search %q: rows=%d err=%v", evil, len(rows), err)
		}
	}
	var n int
	w.must(w.pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM organizations WHERE name LIKE $1`, "%"+w.tag).Scan(&n))
	if n != 5 {
		t.Fatalf("the organizations table must be intact, found %d of 5 tenants", n)
	}
}

func TestAdminOrganizationFilters(t *testing.T) {
	w := newWorld(t)
	s := w.scenario()
	f := func(mod func(*OrganizationFilter)) []int64 {
		fl := OrganizationFilter{Search: w.tag}
		mod(&fl)
		return orgIDs(w.orgs(fl))
	}
	if got := f(func(o *OrganizationFilter) { o.Plan = "pro" }); !eqIDs(got, []int64{s.delta, s.bravo}) {
		t.Errorf("plan=pro: %v", got)
	}
	if got := f(func(o *OrganizationFilter) { o.SubscriptionStatus = "SUSPENDED" }); !eqIDs(got, []int64{s.charlie}) {
		t.Errorf("subscriptionStatus: %v", got)
	}
	if got := f(func(o *OrganizationFilter) { o.InstallationStatus = "DEGRADED" }); !eqIDs(got, []int64{s.bravo}) {
		t.Errorf("installationStatus=DEGRADED: %v", got)
	}
	if got := f(func(o *OrganizationFilter) { o.InstallationStatus = "READY" }); !eqIDs(got, []int64{s.alpha}) {
		t.Errorf("installationStatus=READY: %v", got)
	}
	if got := f(func(o *OrganizationFilter) {
		o.Plan = "PRO"
		o.SubscriptionStatus = "ACTIVE"
		o.InstallationStatus = "DISCONNECTED"
	}); !eqIDs(got, []int64{s.delta}) {
		t.Errorf("combined filters: %v", got)
	}
	if got := f(func(o *OrganizationFilter) { o.Plan = "PRO"; o.SubscriptionStatus = "SUSPENDED" }); len(got) != 0 {
		t.Errorf("contradictory filters must match nothing: %v", got)
	}
}

func TestAdminOrganizationPaginationNeverDuplicatesOrSkips(t *testing.T) {
	w := newWorld(t)
	s := w.scenario()
	repo := New(w.pool)
	want := []int64{s.echo, s.delta, s.charlie, s.bravo, s.alpha}

	var got []int64
	var cursor int64
	pages := 0
	for {
		rows, next, err := repo.ListOrganizations(w.ctx, OrganizationFilter{Search: w.tag, Limit: 2, Cursor: cursor})
		w.must(err)
		got = append(got, orgIDs(rows)...)
		pages++
		if next == 0 {
			break
		}
		if next != rows[len(rows)-1].ID || (cursor != 0 && next >= cursor) {
			t.Fatalf("cursor must be the last id and strictly decrease: cursor=%d next=%d", cursor, next)
		}
		cursor = next
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if !eqIDs(got, want) || pages != 3 {
		t.Fatalf("pages of 2 must cover exactly %v in 3 pages, got %v in %d", want, got, pages)
	}
	// A page exactly the size of the result has no next cursor.
	rows, next, err := repo.ListOrganizations(w.ctx, OrganizationFilter{Search: w.tag, Limit: 5})
	if err != nil || len(rows) != 5 || next != 0 {
		t.Fatalf("an exactly-full last page must end the cursor: %d rows next=%d err=%v", len(rows), next, err)
	}
	// The repository backstop: a huge limit is capped, zero is the default, never unbounded.
	if NormalizeLimit(1_000_000) != MaxLimit || NormalizeLimit(0) != DefaultLimit || NormalizeLimit(-4) != DefaultLimit {
		t.Fatal("limit normalisation")
	}
}

func TestAdminOrganizationDetail(t *testing.T) {
	w := newWorld(t)
	s := w.scenario()
	repo := New(w.pool)
	d, err := repo.GetOrganization(w.ctx, s.alpha)
	w.must(err)
	if d == nil || d.ID != s.alpha || d.Slug == "" || d.MemberCount != 3 || d.InstallationCount != 1 {
		t.Fatalf("alpha detail: %+v", d)
	}
	if len(d.Members) != 3 || d.Members[0].Role != "OWNER" || d.Members[1].Role != "ADMIN" || d.Members[2].Role != "MEMBER" ||
		d.Members[0].DisplayName != "Alice Owner" || d.Members[1].DisplayName != "Adam Admin" || d.Members[2].DiscordID == "" ||
		d.Members[0].ID != strconv.FormatInt(s.alphaOwner, 10) {
		t.Fatalf("members must be OWNER, ADMIN, MEMBER with names, Discord ids and string ids: %+v", d.Members)
	}
	if d.Subscription == nil || d.Subscription.Plan != "TRIAL" || len(d.Subscription.Entitlements) == 0 {
		t.Fatalf("subscription: %+v", d.Subscription)
	}
	if len(d.Installations) != 1 || d.Installations[0].ID != s.iAlpha || d.Installations[0].OrganizationID != s.alpha {
		t.Fatalf("only alpha's installation may appear: %+v", d.Installations)
	}
	bravo, err := repo.GetOrganization(w.ctx, s.bravo)
	w.must(err)
	// All of bravo's installations, the primary (its newest here) first.
	if len(bravo.Installations) != 2 || bravo.Installations[0].ID != s.iBravoConfiguring || bravo.Installations[1].ID != s.iBravoDegraded {
		t.Fatalf("bravo's two installations, primary first: %+v", bravo.Installations)
	}
	// With a READY installation that is NOT the newest, the primary (READY) still comes first.
	w.must(w.exec(`UPDATE installations SET status='READY' WHERE id=$1`, s.iBravoDegraded))
	bravo, err = repo.GetOrganization(w.ctx, s.bravo)
	w.must(err)
	if bravo.Installations[0].ID != s.iBravoDegraded || deref(bravo.DayZServer) != "BravoServer-"+w.tag {
		t.Fatalf("the READY installation must lead the detail: %+v", bravo.Installations)
	}
	echo, err := repo.GetOrganization(w.ctx, s.echo)
	w.must(err)
	if echo.Subscription != nil || echo.Installations == nil || len(echo.Installations) != 0 {
		t.Fatalf("no subscription and an empty (non-null) installation list: %+v", echo)
	}
	missing, err := repo.GetOrganization(w.ctx, -1)
	if err != nil || missing != nil {
		t.Fatalf("an unknown organization is (nil, nil), got %v %v", missing, err)
	}
}

// --- subscriptions ---------------------------------------------------------------------------------------

func TestAdminSubscriptionsListAndFilter(t *testing.T) {
	w := newWorld(t)
	s := w.scenario()
	repo := New(w.pool)
	list := func(f SubscriptionFilter) []SubscriptionRow {
		f.Limit = 100
		rows, _, err := repo.ListSubscriptions(w.ctx, f)
		w.must(err)
		return rows
	}
	rows := list(SubscriptionFilter{Search: w.tag})
	if len(rows) != 4 {
		t.Fatalf("four tenants have a subscription (Echo has none), got %d", len(rows))
	}
	for _, r := range rows {
		if r.OrganizationID == s.echo {
			t.Fatal("an organization without a subscription row is not a subscription")
		}
		if r.OrganizationID == s.bravo && (r.InstallationCount != 2 || r.Plan != "PRO" || r.Status != "ACTIVE" || len(r.Entitlements) == 0 || r.CreatedAt == nil || r.UpdatedAt == nil || r.Organization != r.OrganizationName) {
			t.Errorf("bravo subscription: %+v", r)
		}
		if r.OrganizationID == s.alpha && (r.TrialEndsAt == nil || r.OrganizationName != "Alpha-"+w.tag) {
			t.Errorf("alpha subscription: %+v", r)
		}
	}
	if got := list(SubscriptionFilter{Search: w.tag, Status: "ACTIVE"}); len(got) != 2 {
		t.Errorf("status=ACTIVE: %d", len(got))
	}
	if got := list(SubscriptionFilter{Search: w.tag, Plan: "basic"}); len(got) != 1 || got[0].OrganizationID != s.charlie {
		t.Errorf("plan=basic: %+v", got)
	}
	if got := list(SubscriptionFilter{Search: "bravo-" + w.tag}); len(got) != 1 || got[0].OrganizationID != s.bravo {
		t.Errorf("search by organization name: %+v", got)
	}
	// Pagination: pages of 3 cover the four rows exactly once.
	var seen []int64
	var cursor int64
	for i := 0; i < 5; i++ {
		page, next, err := repo.ListSubscriptions(w.ctx, SubscriptionFilter{Search: w.tag, Limit: 3, Cursor: cursor})
		w.must(err)
		for _, r := range page {
			seen = append(seen, r.ID)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	uniq := map[int64]bool{}
	for _, id := range seen {
		uniq[id] = true
	}
	if len(seen) != 4 || len(uniq) != 4 {
		t.Errorf("pagination duplicated or skipped rows: %v", seen)
	}
}

// --- installations ---------------------------------------------------------------------------------------------

func (w *world) installations(f InstallationFilter) []InstallationSummary {
	w.t.Helper()
	f.Limit = 100
	rows, _, err := New(w.pool).ListInstallations(w.ctx, f)
	w.must(err)
	return rows
}

func instIDs(rows []InstallationSummary) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.InstallationID)
	}
	return out
}

func TestAdminInstallationListSummariesFiltersAndSearch(t *testing.T) {
	w := newWorld(t)
	s := w.scenario()
	all := w.installations(InstallationFilter{Search: w.tag})
	if want := []int64{s.iDelta, s.iBravoConfiguring, s.iBravoDegraded, s.iAlpha}; !eqIDs(instIDs(all), want) {
		t.Fatalf("expected %v, got %v", want, instIDs(all))
	}
	by := map[int64]InstallationSummary{}
	for _, r := range all {
		by[r.InstallationID] = r
	}
	a := by[s.iAlpha]
	if a.ID != a.InstallationID || a.OrganizationID != s.alpha || a.Organization != "Alpha-"+w.tag || a.Plan != "TRIAL" || a.Status != "READY" || a.Health != "HEALTHY" {
		t.Errorf("alpha row: %+v", a)
	}
	if a.Discord.GuildName != "AlphaGuild-"+w.tag || !strings.HasPrefix(a.Discord.GuildID, w.tag) || !a.Discord.BotInstalled || !a.Discord.PermissionsVerified || deref(a.DiscordGuild) != "AlphaGuild-"+w.tag {
		t.Errorf("alpha discord: %+v", a.Discord)
	}
	if a.Server == nil || a.Server.DisplayName != "AlphaServer-"+w.tag || a.Server.Platform != "PLAYSTATION" || a.Server.Status != "ACTIVE" || a.Server.ServiceID == "" || a.Server.ID == 0 ||
		deref(a.DayZServer) != "AlphaServer-"+w.tag || deref(a.Platform) != "PLAYSTATION" {
		t.Errorf("alpha server: %+v", a.Server)
	}
	if deref(a.CurrentSetupStep) != "CHANNELS" || a.CreatedAt == "" {
		t.Errorf("alpha setup: %v", a.CurrentSetupStep)
	}
	if c := by[s.iBravoConfiguring]; c.DayZServer != nil || c.Health != "SETTING_UP" || c.Plan != "PRO" {
		t.Errorf("a server-less installation reports no server: %+v", c)
	}

	if got := instIDs(w.installations(InstallationFilter{Search: w.tag, Status: "DEGRADED"})); !eqIDs(got, []int64{s.iBravoDegraded}) {
		t.Errorf("status=DEGRADED: %v", got)
	}
	if got := instIDs(w.installations(InstallationFilter{Search: w.tag, Health: "OFFLINE"})); !eqIDs(got, []int64{s.iDelta}) {
		t.Errorf("health=OFFLINE: %v", got)
	}
	if got := instIDs(w.installations(InstallationFilter{Search: w.tag, Health: "HEALTHY"})); !eqIDs(got, []int64{s.iAlpha}) {
		t.Errorf("health=HEALTHY: %v", got)
	}
	if got := instIDs(w.installations(InstallationFilter{Search: w.tag, Health: "SETTING_UP"})); !eqIDs(got, []int64{s.iBravoConfiguring}) {
		t.Errorf("health=SETTING_UP: %v", got)
	}
	if got := instIDs(w.installations(InstallationFilter{Search: w.tag, OrganizationID: s.bravo})); !eqIDs(got, []int64{s.iBravoConfiguring, s.iBravoDegraded}) {
		t.Errorf("organizationId filter: %v", got)
	}
	if got := instIDs(w.installations(InstallationFilter{Search: "BravoServer-" + w.tag})); !eqIDs(got, []int64{s.iBravoDegraded}) {
		t.Errorf("search by server name: %v", got)
	}
	if got := instIDs(w.installations(InstallationFilter{Search: "DeltaGuild-" + w.tag})); !eqIDs(got, []int64{s.iDelta}) {
		t.Errorf("search by guild name: %v", got)
	}
	if got := instIDs(w.installations(InstallationFilter{Search: "bravo-" + w.tag})); !eqIDs(got, []int64{s.iBravoConfiguring, s.iBravoDegraded}) {
		t.Errorf("search by organization name: %v", got)
	}

	// Pagination over installations.
	var seen []int64
	var cursor int64
	for i := 0; i < 6; i++ {
		page, next, err := New(w.pool).ListInstallations(w.ctx, InstallationFilter{Search: w.tag, Limit: 3, Cursor: cursor})
		w.must(err)
		seen = append(seen, instIDs(page)...)
		if next == 0 {
			break
		}
		cursor = next
	}
	if !eqIDs(seen, instIDs(all)) {
		t.Errorf("paged installations differ from the full list: %v vs %v", seen, instIDs(all))
	}
}

func TestAdminInstallationDetailIsolatesRoutesPerInstallation(t *testing.T) {
	w := newWorld(t)
	s := w.scenario()
	repo := New(w.pool)
	// Nitrado state for Alpha only, with recognisable secret-shaped bytes in the credential envelope.
	w.must(w.exec(`INSERT INTO nitrado_connections(guild_id, organization_id, credential_ciphertext, credential_nonce, credential_key_version, status, last_error_class) VALUES(NULL,$1,$2,$3,7,'ACTIVE','')`,
		s.alpha, []byte("NITRADO-TOKEN-CIPHERTEXT-"+w.tag), []byte("NONCE-"+w.tag)))

	alpha, err := repo.GetInstallation(w.ctx, s.iAlpha)
	w.must(err)
	if alpha == nil || alpha.OrganizationID != s.alpha || alpha.Organization != "Alpha-"+w.tag || alpha.OrganizationSlug == "" || alpha.Subscription == nil || alpha.Subscription.Plan != "TRIAL" || alpha.ID != s.iAlpha || alpha.Health != "HEALTHY" || alpha.UpdatedAt == "" {
		t.Fatalf("alpha detail: %+v", alpha)
	}
	if alpha.Discord.GuildName != "AlphaGuild-"+w.tag || alpha.Server == nil || alpha.Server.DisplayName != "AlphaServer-"+w.tag || deref(alpha.DayZServer) != "AlphaServer-"+w.tag {
		t.Errorf("alpha discord/server: %+v %+v", alpha.Discord, alpha.Server)
	}
	if alpha.SetupProgress == nil || alpha.SetupProgress.CurrentStep != "CHANNELS" || !alpha.SetupProgress.DiscordCompleted || alpha.SetupProgress.NitradoCompleted {
		t.Errorf("setup progress: %+v", alpha.SetupProgress)
	}
	if alpha.Settings == nil || alpha.GeneralSettings == nil || *alpha.Settings != *alpha.GeneralSettings {
		t.Errorf("settings and generalSettings must carry the same values: %+v %+v", alpha.Settings, alpha.GeneralSettings)
	}
	for _, r := range alpha.ChannelRoutes {
		if r.ChannelName != nil {
			t.Errorf("the repository never invents a channel name: %+v", r)
		}
	}
	if g := alpha.GeneralSettings; g == nil || g.Timezone != "Europe/London" || g.DistanceUnit != "FEET" || !g.OnlineDisplayEnabled || !g.LeaderboardEnabled {
		t.Errorf("general settings: %+v", g)
	}
	routes := map[string]ChannelRoute{}
	for _, r := range alpha.ChannelRoutes {
		routes[r.RouteKey] = r
	}
	if len(routes) != 2 || routes["KILLFEED"].ChannelID != "c-alpha-kf" || routes["BOUNTY"].ChannelID != "c-alpha-bounty" || !routes["BOUNTY"].ManagedByChampion || routes["KILLFEED"].ManagedByChampion {
		t.Errorf("alpha must have exactly its own two routes: %+v", alpha.ChannelRoutes)
	}
	if n := alpha.NitradoConnection; n == nil || !n.Connected || n.Status != "ACTIVE" {
		t.Errorf("nitrado status: %+v", n)
	}
	// The whole detail, serialised, carries none of the credential envelope.
	body, _ := json.Marshal(alpha)
	for _, leak := range []string{"NITRADO-TOKEN-CIPHERTEXT", "NONCE-", "credential_", "ciphertext", "cus_SECRETCUSTOMER", "sub_SECRETSUB", "stripe"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("installation detail leaked %q: %s", leak, body)
		}
	}

	bravo, err := repo.GetInstallation(w.ctx, s.iBravoDegraded)
	w.must(err)
	if len(bravo.ChannelRoutes) != 1 || bravo.ChannelRoutes[0].ChannelID != "c-bravo-kf" || bravo.OrganizationID != s.bravo || bravo.NitradoConnection != nil {
		t.Errorf("bravo must see only its own route and no Nitrado link: %+v", bravo)
	}
	conf, err := repo.GetInstallation(w.ctx, s.iBravoConfiguring)
	w.must(err)
	if conf.Server != nil || conf.DayZServer != nil || conf.ChannelRoutes == nil || len(conf.ChannelRoutes) != 0 {
		t.Errorf("a server-less installation: server=%v routes=%v", conf.Server, conf.ChannelRoutes)
	}
	missing, err := repo.GetInstallation(w.ctx, -1)
	if err != nil || missing != nil {
		t.Fatalf("unknown installation is (nil, nil): %v %v", missing, err)
	}
}

// --- health -----------------------------------------------------------------------------------------------------------

func TestAdminInstallationHealthFromStoredState(t *testing.T) {
	w := newWorld(t)
	repo := New(w.pool)
	before, _, err := repo.InstallationHealth(w.ctx)
	w.must(err)
	s := w.scenario()
	after, items, err := repo.InstallationHealth(w.ctx)
	w.must(err)

	if after.Total-before.Total != 4 || after.ByStatus["READY"]-before.ByStatus["READY"] != 1 || after.ByStatus["DEGRADED"]-before.ByStatus["DEGRADED"] != 1 ||
		after.ByHealth["HEALTHY"]-before.ByHealth["HEALTHY"] != 1 || after.ByHealth["OFFLINE"]-before.ByHealth["OFFLINE"] != 1 || after.ByHealth["SETTING_UP"]-before.ByHealth["SETTING_UP"] != 1 {
		t.Errorf("status/health counts: before=%+v after=%+v", before, after)
	}
	if after.ServersByStatus["ACTIVE"]-before.ServersByStatus["ACTIVE"] != 3 {
		t.Errorf("three seeded installations have a server: %+v", after.ServersByStatus)
	}
	if after.ReadyNeverChecked-before.ReadyNeverChecked != 1 {
		t.Errorf("the READY installation was never health-checked: %d", after.ReadyNeverChecked-before.ReadyNeverChecked)
	}
	// Alpha READY has permissions verified; the other three do not.
	if after.PermissionsUnverified-before.PermissionsUnverified != 3 || after.BotNotInstalled != before.BotNotInstalled {
		t.Errorf("discord flags: %+v", after)
	}
	flagged := map[int64]HealthInstallation{}
	for _, r := range items {
		flagged[r.ID] = r
	}
	if len(items) > attentionRows {
		t.Errorf("the attention list is bounded, got %d", len(items))
	}
	// Never-checked rows sort first and the newest first among them, so the two
	// problem installations lead the list even on a busy shared database.
	if _, ok := flagged[s.iAlpha]; ok {
		t.Error("READY installations do not need attention")
	}
	if _, ok := flagged[s.iBravoConfiguring]; ok {
		t.Error("CONFIGURING installations do not need attention")
	}
	deg, delta := flagged[s.iBravoDegraded], flagged[s.iDelta]
	if deg.ID == 0 || delta.ID == 0 {
		t.Fatalf("DEGRADED and DISCONNECTED installations must be flagged: %v", flagged)
	}
	if deg.Organization != "Bravo-"+w.tag || deg.Health != "DEGRADED" || deg.DiscordBotInstalled == nil || !*deg.DiscordBotInstalled || deg.ServerStatus == nil || *deg.ServerStatus != "ACTIVE" {
		t.Errorf("degraded item: %+v", deg)
	}
	if delta.Health != "OFFLINE" || delta.Status != "DISCONNECTED" {
		t.Errorf("disconnected item: %+v", delta)
	}
}

// --- N+1 -------------------------------------------------------------------------------------------

type queryCounter struct{ n atomic.Int64 }

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// The number of statements a list makes must not grow with the number of rows: a
// page of 1 and a page of 5 tenants cost the same.
func TestAdminListsDoNotIssuePerRowQueries(t *testing.T) {
	w := newWorld(t)
	w.scenario()
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	w.must(err)
	counter := &queryCounter{}
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(w.ctx, cfg)
	w.must(err)
	defer pool.Close()
	repo := New(pool)

	count := func(name string, fn func()) int64 {
		before := counter.n.Load()
		fn()
		return counter.n.Load() - before
	}
	orgs := func(limit int) int64 {
		return count("orgs", func() {
			rows, _, err := repo.ListOrganizations(w.ctx, OrganizationFilter{Search: w.tag, Limit: limit})
			w.must(err)
			if limit == 100 && len(rows) != 5 {
				t.Fatalf("expected 5 tenants, got %d", len(rows))
			}
		})
	}
	few, many := orgs(2), orgs(100) // the newest two include an organization with an installation
	if few != many || many > 2 {
		t.Errorf("ListOrganizations must cost the same for 2 and 5 rows and at most 2 statements: %d vs %d", few, many)
	}
	if n := count("subs", func() {
		_, _, err := repo.ListSubscriptions(w.ctx, SubscriptionFilter{Search: w.tag, Limit: 100})
		w.must(err)
	}); n != 1 {
		t.Errorf("ListSubscriptions must be one statement, got %d", n)
	}
	if n := count("installs", func() {
		_, _, err := repo.ListInstallations(w.ctx, InstallationFilter{Search: w.tag, Limit: 100})
		w.must(err)
	}); n != 1 {
		t.Errorf("ListInstallations must be one statement, got %d", n)
	}
	// Detail reads are a fixed handful, independent of how many children exist.
	if n := count("orgdetail", func() { _, err := repo.GetOrganization(w.ctx, w.mustOrgID("Bravo-"+w.tag)); w.must(err) }); n > 6 {
		t.Errorf("GetOrganization must be a fixed handful of statements, got %d", n)
	}
}

func (w *world) mustOrgID(name string) int64 {
	var id int64
	w.must(w.pool.QueryRow(w.ctx, `SELECT id FROM organizations WHERE name=$1`, name).Scan(&id))
	return id
}
