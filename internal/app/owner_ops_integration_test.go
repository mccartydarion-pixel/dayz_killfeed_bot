//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/ownerops"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/servers"
)

// Owner Hub operations over a real PostgreSQL (docs/OWNER_OPS.md). The database is shared with
// other tests, so every assertion looks only at this world's own organizations.

const ownerOpsCatalogJSON = `[
  {"key": "NORMAL", "name": "Survivor", "description": "d", "features": [], "limits": {"installations": 1},
   "monthly": {"amountCents": 599, "currency": "usd", "stripePriceId": "price_ops_normal_m"}, "isPublic": true, "sortOrder": 1},
  {"key": "PREMIUM", "name": "Champion", "description": "d", "features": [], "limits": {"installations": 3},
   "monthly": {"amountCents": 1499, "currency": "usd", "stripePriceId": "price_ops_premium_m"},
   "yearly": {"amountCents": 14990, "currency": "usd", "stripePriceId": "price_ops_premium_y"}, "isPublic": true, "sortOrder": 2}
]`

type ownerOpsWorld struct {
	*adminWorld
	serverID, guildID int64
	mu                sync.Mutex
	dms               []string // "discordID|text"
	posts             []string // "channelID|text"
}

// newOwnerOpsWorld: organization a1 has a set-up server (READY, active, no worker yet);
// organization b1 never got past connecting Discord.
func newOwnerOpsWorld(t *testing.T) *ownerOpsWorld {
	t.Helper()
	w := &ownerOpsWorld{adminWorld: newOwnerWorld(t)}
	a := w.a
	pool := a.DB.Pool
	a.PlatformOps = repository.NewPlatformOpsRepository(pool)
	a.Servers = repository.NewServerRepository(pool)
	a.Kills = repository.NewKillRepository(pool)
	a.WorkerManager = servers.NewWorkerManager(func(ctx context.Context, _ int64) error {
		<-ctx.Done()
		return nil
	})
	t.Cleanup(a.WorkerManager.StopAll)
	a.ownerOpsReady = func() bool { return true }
	a.ownerOpsDM = func(id, text string) error {
		w.mu.Lock()
		w.dms = append(w.dms, id+"|"+text)
		w.mu.Unlock()
		return nil
	}
	a.ownerOpsChannelPost = func(channel, text string) error {
		w.mu.Lock()
		w.posts = append(w.posts, channel+"|"+text)
		w.mu.Unlock()
		return nil
	}
	cat, err := billing.LoadCatalog(ownerOpsCatalogJSON)
	if err != nil {
		t.Fatal(err)
	}
	a.Billing = billing.NewService(a.SaaSSubscriptions, cat, nil, billing.Options{})

	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT c.guild_id FROM installations i JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id WHERE i.id=$1`, w.a1.InstallationID).Scan(&w.guildID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, organization_id)
VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','Ops Server','ACTIVE',$3) RETURNING id`, w.guildID, fmt.Sprintf("ops-%d", time.Now().UnixNano()), w.a1.OrgID).Scan(&w.serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE installations SET game_server_id=$1, status='READY', setup_completed_at=NOW() WHERE id=$2`, w.serverID, w.a1.InstallationID); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *ownerOpsWorld) put(h adminHandler, target, acting string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set(actingUserHeader, acting)
	rr := httptest.NewRecorder()
	w.a.adminRoute(h)(rr, req)
	return rr
}

func (w *ownerOpsWorld) sent(kind string, contains string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	list := w.dms
	if kind == "post" {
		list = w.posts
	}
	n := 0
	for _, m := range list {
		if strings.Contains(m, contains) {
			n++
		}
	}
	return n
}

func (w *ownerOpsWorld) incidents(status string) []repository.PlatformIncident {
	w.t.Helper()
	all, err := w.a.PlatformOps.ListIncidents(context.Background(), status, 200)
	if err != nil {
		w.t.Fatal(err)
	}
	mine := []repository.PlatformIncident{}
	for _, inc := range all {
		if inc.InstallationID == w.a1.InstallationID {
			mine = append(mine, inc)
		}
	}
	return mine
}

func (w *ownerOpsWorld) setSubscription(orgID int64, plan, status, priceID, interval string) {
	w.t.Helper()
	var provider, sub any
	if priceID != "" {
		provider, sub = "stripe", fmt.Sprintf("sub_ops_%d", orgID)
	}
	if _, err := w.a.DB.Pool.Exec(context.Background(), `
INSERT INTO subscriptions(organization_id, plan, status, provider, provider_subscription_id, provider_price_id, billing_interval, trial_consumed)
VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),TRUE)
ON CONFLICT (organization_id) DO UPDATE SET plan=EXCLUDED.plan, status=EXCLUDED.status, provider=EXCLUDED.provider,
  provider_subscription_id=EXCLUDED.provider_subscription_id, provider_price_id=EXCLUDED.provider_price_id,
  billing_interval=EXCLUDED.billing_interval, trial_consumed=TRUE, trial_ends_at=NULL`, orgID, plan, status, provider, sub, priceID, interval); err != nil {
		w.t.Fatal(err)
	}
}

func findByOrg[T any](items []T, orgID int64, id func(T) int64) *T {
	for i := range items {
		if id(items[i]) == orgID {
			return &items[i]
		}
	}
	return nil
}

func TestOwnerOpsRoutesAreAdminOnly(t *testing.T) {
	w := newOwnerOpsWorld(t)
	a := w.a
	for name, h := range map[string]adminHandler{"config-check": a.handleAdminConfigCheck, "fleet": a.handleAdminFleet, "customer-health": a.handleAdminCustomerHealth,
		"funnel": a.handleAdminFunnel, "revenue": a.handleAdminRevenue, "briefing": a.handleAdminBriefing, "automation": a.handleAdminGetAutomation,
		"incidents": a.handleAdminIncidents, "broadcasts": a.handleAdminBroadcasts, "nitrado-usage": a.handleAdminNitradoUsage} {
		if rr := w.get(h, "/api/admin/"+name, w.a1.OwnerDiscordID, nil); rr.Code != http.StatusForbidden {
			t.Fatalf("%s: a tenant owner must be 403, got %d", name, rr.Code)
		}
		rr := w.get(h, "/api/admin/"+name, adminFounderID, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
		// No response may carry a Stripe price id or the service secret.
		if body := rr.Body.String(); strings.Contains(body, "price_ops_") || strings.Contains(body, "test-secret") {
			t.Fatalf("%s leaks an identifier: %s", name, body)
		}
	}
}

func TestOwnerOpsFleetHealthFunnelRevenueAndConfig(t *testing.T) {
	w := newOwnerOpsWorld(t)
	a := w.a
	ctx := context.Background()
	now := time.Now().UTC()

	type fleet struct {
		Summary fleetSummaryDTO  `json:"summary"`
		Servers []fleetServerDTO `json:"servers"`
	}
	getFleet := func() (fleetServerDTO, fleetServerDTO) {
		t.Helper()
		f := decodeBody[fleet](t, w.get(a.handleAdminFleet, "/api/admin/fleet", adminFounderID, nil))
		mine := findByOrg(f.Servers, w.a1.OrgID, func(s fleetServerDTO) int64 { return s.OrganizationID })
		other := findByOrg(f.Servers, w.b1.OrgID, func(s fleetServerDTO) int64 { return s.OrganizationID })
		if mine == nil || other == nil {
			t.Fatalf("fleet is missing this world's installations")
		}
		return *mine, *other
	}
	mine, other := getFleet()
	if mine.FeedState != ownerops.FeedNoWorker || mine.ServerID == nil || *mine.ServerID != w.serverID || mine.ServerName != "Ops Server" || mine.LastKillAt != nil {
		t.Fatalf("set-up server with no worker = %+v", mine)
	}
	if other.FeedState != ownerops.FeedSettingUp || other.ServerID != nil {
		t.Fatalf("unfinished installation = %+v", other)
	}

	// A worker and a kill: the server is live and its activity shows.
	if err := a.WorkerManager.Start(ctx, w.serverID); err != nil {
		t.Fatal(err)
	}
	var killer, victim int64
	for i, name := range []string{"Ops Killer", "Ops Victim"} {
		id := &killer
		if i == 1 {
			id = &victim
		}
		if err := a.DB.Pool.QueryRow(ctx, `INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,$3) RETURNING id`,
			w.guildID, fmt.Sprintf("ops-%d-%d", time.Now().UnixNano(), i), name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	at := now.Add(-time.Hour)
	if _, err := a.Kills.InsertKillReturning(ctx, repository.KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s",
		Fingerprint: fmt.Sprintf("ops-kill-%d", time.Now().UnixNano()), KillerPlayerID: killer, VictimPlayerID: victim, WeaponRaw: "M4", WeaponDisplay: "M4", EventTime: &at}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Pool.Exec(ctx, `INSERT INTO player_daily_activity(guild_id, server_id, player_id, day, observed_seconds, sessions, first_seen_at, last_seen_at)
VALUES($1,$2,$3,CURRENT_DATE,600,1,NOW(),NOW()), ($1,$2,$4,CURRENT_DATE - 10,600,1,NOW(),NOW())`, w.guildID, w.serverID, killer, victim); err != nil {
		t.Fatal(err)
	}
	mine, _ = getFleet()
	if mine.FeedState != ownerops.FeedLive || !mine.WorkerRunning || mine.Kills24h != 1 || mine.LastKillAt == nil || mine.ActivePlayers7d != 1 || mine.ActivePlayersPrev7d != 1 {
		t.Fatalf("live server = %+v", mine)
	}

	// Customer health: the set-up, paying organization scores above the unfinished trial.
	w.setSubscription(w.a1.OrgID, "PREMIUM", "ACTIVE", "price_ops_premium_y", "YEARLY")
	type health struct {
		Items []customerHealthDTO `json:"items"`
	}
	h := decodeBody[health](t, w.get(a.handleAdminCustomerHealth, "/api/admin/customer-health", adminFounderID, nil))
	good := findByOrg(h.Items, w.a1.OrgID, func(c customerHealthDTO) int64 { return c.OrganizationID })
	bad := findByOrg(h.Items, w.b1.OrgID, func(c customerHealthDTO) int64 { return c.OrganizationID })
	if good == nil || bad == nil || good.Score <= bad.Score || good.FeedState != ownerops.FeedLive || good.OwnerDiscordID != w.a1.OwnerDiscordID {
		t.Fatalf("health: good=%+v bad=%+v", good, bad)
	}
	hasFactor := func(c *customerHealthDTO, key string) bool {
		for _, f := range c.Factors {
			if f.Key == key {
				return true
			}
		}
		return false
	}
	if !hasFactor(bad, "setting_up") || hasFactor(good, "setting_up") || hasFactor(good, "no_plan") {
		t.Fatalf("factors: good=%+v bad=%+v", good.Factors, bad.Factors)
	}

	// Funnel: a1 reached its first kill; b1 is stuck once it has not moved for a day.
	type funnel struct {
		Stages []funnelStageDTO `json:"stages"`
		Stuck  []funnelStuckDTO `json:"stuck"`
	}
	f := decodeBody[funnel](t, w.get(a.handleAdminFunnel, "/api/admin/funnel?days=30", adminFounderID, nil))
	if len(f.Stages) != len(ownerops.Stages) || f.Stages[0].Count < 2 || f.Stages[len(f.Stages)-1].Count < 1 || f.Stages[0].Count < f.Stages[len(f.Stages)-1].Count {
		t.Fatalf("stages = %+v", f.Stages)
	}
	if findByOrg(f.Stuck, w.b1.OrgID, func(s funnelStuckDTO) int64 { return s.OrganizationID }) != nil {
		t.Fatal("an organization created a moment ago is not stuck yet")
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE organizations SET updated_at = NOW() - interval '3 days 1 hour' WHERE id=$1`, w.b1.OrgID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE installations SET updated_at = NOW() - interval '3 days 1 hour' WHERE organization_id=$1`, w.b1.OrgID); err != nil {
		t.Fatal(err)
	}
	f = decodeBody[funnel](t, w.get(a.handleAdminFunnel, "/api/admin/funnel", adminFounderID, nil))
	stuck := findByOrg(f.Stuck, w.b1.OrgID, func(s funnelStuckDTO) int64 { return s.OrganizationID })
	if stuck == nil || stuck.Stage != ownerops.StageDiscordConnected || stuck.DaysStuck != 3 || stuck.NextStep == "" || stuck.OwnerDiscordID != w.b1.OwnerDiscordID {
		t.Fatalf("stuck = %+v", stuck)
	}
	if findByOrg(f.Stuck, w.a1.OrgID, func(s funnelStuckDTO) int64 { return s.OrganizationID }) != nil {
		t.Fatal("an organization with kills is not stuck")
	}
	if rr := w.get(a.handleAdminFunnel, "/api/admin/funnel?days=0", adminFounderID, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("days=0: %d", rr.Code)
	}

	// Revenue: a yearly plan counts a twelfth per month. Other tests' rows share the database,
	// so compare before and after one change.
	type revenue struct {
		MRRCents int64            `json:"mrrCents"`
		ARRCents int64            `json:"arrCents"`
		ByPlan   []revenuePlanDTO `json:"byPlan"`
		Counts   map[string]int   `json:"counts"`
	}
	before := decodeBody[revenue](t, w.get(a.handleAdminRevenue, "/api/admin/revenue", adminFounderID, nil))
	w.setSubscription(w.b1.OrgID, "NORMAL", "ACTIVE", "price_ops_normal_m", "MONTHLY")
	after := decodeBody[revenue](t, w.get(a.handleAdminRevenue, "/api/admin/revenue", adminFounderID, nil))
	if before.MRRCents < 14990/12 || after.MRRCents-before.MRRCents != 599 || after.ARRCents != after.MRRCents*12 || after.Counts["paying"]-before.Counts["paying"] != 1 {
		t.Fatalf("revenue before=%+v after=%+v", before, after)
	}

	// Configuration check: a stored plan that disagrees with its price, and a price the catalog
	// does not know, are both reported by organization - never by price id.
	w.setSubscription(w.a1.OrgID, "NORMAL", "ACTIVE", "price_ops_premium_m", "MONTHLY")
	w.setSubscription(w.b1.OrgID, "NORMAL", "ACTIVE", "price_ops_retired", "MONTHLY")
	type config struct {
		Worst    string                `json:"worst"`
		Switches []configSwitchDTO     `json:"switches"`
		Checks   []ownerops.Check      `json:"checks"`
		Live     []liveInstallationDTO `json:"liveInstallations"`
	}
	rr := w.get(a.handleAdminConfigCheck, "/api/admin/config-check", adminFounderID, nil)
	c := decodeBody[config](t, rr)
	label := func(id int64) string { return fmt.Sprintf("(#%d)", id) }
	check := func(id string) ownerops.Check {
		for _, k := range c.Checks {
			if k.ID == id {
				return k
			}
		}
		t.Fatalf("check %s missing", id)
		return ownerops.Check{}
	}
	if k := check("subscription_plan_matches_price"); k.Severity != ownerops.SeverityFail || !strings.Contains(strings.Join(k.Items, "\n"), label(w.a1.OrgID)) {
		t.Fatalf("mismatch check = %+v", k)
	}
	if k := check("subscription_price_in_catalog"); k.Severity != ownerops.SeverityFail || !strings.Contains(strings.Join(k.Items, "\n"), label(w.b1.OrgID)) {
		t.Fatalf("unknown price check = %+v", k)
	}
	if c.Worst != ownerops.SeverityFail || strings.Contains(rr.Body.String(), "price_ops_") {
		t.Fatalf("worst=%s body leaks a price id: %v", c.Worst, strings.Contains(rr.Body.String(), "price_ops_"))
	}
	live := findByOrg(c.Live, w.a1.OrgID, func(l liveInstallationDTO) int64 { return l.OrganizationID })
	if live == nil || live.InstallationID != w.a1.InstallationID || live.ServerID != w.serverID || !live.WorkerRunning {
		t.Fatalf("live installation = %+v", live)
	}
	if findByOrg(c.Live, w.b1.OrgID, func(l liveInstallationDTO) int64 { return l.OrganizationID }) != nil {
		t.Fatal("an unfinished installation is not live")
	}
	gating := false
	for _, s := range c.Switches {
		if s.Key == "plan_gating" {
			gating = true
			if s.Enabled != entitlements.Enforced() {
				t.Fatalf("plan gating switch = %+v", s)
			}
		}
	}
	if !gating {
		t.Fatal("the plan gating switch is not reported")
	}
	// With gating on, the Survivor organizations are named.
	enforcePlanGating(t)
	c = decodeBody[config](t, w.get(a.handleAdminConfigCheck, "/api/admin/config-check", adminFounderID, nil))
	if k := check("plan_gating_reach"); k.Severity != ownerops.SeverityInfo || !strings.Contains(strings.Join(k.Items, "\n"), label(w.a1.OrgID)) {
		t.Fatalf("gating reach = %+v", k)
	}

	// The briefing is built from the same figures and is always available to read.
	type briefing struct {
		Text string `json:"text"`
	}
	if b := decodeBody[briefing](t, w.get(a.handleAdminBriefing, "/api/admin/briefing", adminFounderID, nil)); !strings.Contains(b.Text, "Champion briefing") || !strings.Contains(b.Text, "MRR") {
		t.Fatalf("briefing = %q", b.Text)
	}
}

func TestOwnerOpsMonitorOpensHealsAndResolves(t *testing.T) {
	w := newOwnerOpsWorld(t)
	a := w.a
	ctx := context.Background()
	now := time.Now().UTC()

	// Defaults: everything that acts or messages is off.
	type automation struct {
		Settings repository.OwnerOpsSettings `json:"settings"`
		Admins   int                         `json:"admins"`
	}
	got := decodeBody[automation](t, w.get(a.handleAdminGetAutomation, "/api/admin/automation", adminFounderID, nil))
	if got.Settings.AlertsEnabled || got.Settings.SelfHealEnabled || got.Settings.CustomerNoticesEnabled || got.Settings.BriefingEnabled || got.Admins != 1 {
		t.Fatalf("defaults = %+v", got)
	}

	// The server is set up and has no worker. One observation is not an incident.
	if err := a.ownerOpsTick(ctx, now); err != nil {
		t.Fatal(err)
	}
	if open := w.incidents(repository.IncidentOpen); len(open) != 0 {
		t.Fatalf("an incident opened on the first observation: %+v", open)
	}
	// Still down after the grace period: an incident, and - with everything off - nothing else.
	later := now.Add(ownerops.DetectGrace + time.Minute)
	if err := a.ownerOpsTick(ctx, later); err != nil {
		t.Fatal(err)
	}
	open := w.incidents(repository.IncidentOpen)
	if len(open) != 1 || open[0].Kind != repository.IncidentWorkerDown || open[0].Attempts != 0 || open[0].OwnerNotifiedAt != nil || open[0].ServerName != "Ops Server" {
		t.Fatalf("open incidents = %+v", open)
	}
	if a.WorkerManager.Running(w.serverID) || w.sent("dm", "Ops Server") != 0 {
		t.Fatal("the monitor acted or messaged with every switch off")
	}
	if err := a.ownerOpsTick(ctx, later.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if again := w.incidents(""); len(again) != 1 {
		t.Fatalf("a second tick opened a second incident: %+v", again)
	}

	// Saving the switches needs a reason and is audited.
	on := map[string]any{"alertsEnabled": true, "selfHealEnabled": true, "customerNoticesEnabled": true, "briefingEnabled": false, "briefingHourUtc": 13}
	if rr := w.put(a.handleAdminPutAutomation, "/api/admin/automation", adminFounderID, on); rr.Code != http.StatusBadRequest {
		t.Fatalf("no reason: %d", rr.Code)
	}
	bad := map[string]any{"reason": "x", "briefingHourUtc": 24}
	if rr := w.put(a.handleAdminPutAutomation, "/api/admin/automation", adminFounderID, bad); rr.Code != http.StatusBadRequest {
		t.Fatalf("hour 24: %d", rr.Code)
	}
	on["reason"] = "turn automation on"
	if rr := w.put(a.handleAdminPutAutomation, "/api/admin/automation", w.a1.OwnerDiscordID, on); rr.Code != http.StatusForbidden {
		t.Fatalf("tenant owner saved automation: %d", rr.Code)
	}
	rr := w.put(a.handleAdminPutAutomation, "/api/admin/automation", adminFounderID, on)
	if rr.Code != http.StatusOK || !decodeBody[automation](t, rr).Settings.SelfHealEnabled {
		t.Fatalf("save automation: %d %s", rr.Code, rr.Body.String())
	}
	t.Cleanup(func() {
		_ = a.PlatformOps.SaveSettings(context.Background(), repository.DefaultOwnerOpsSettings(), "test")
	})

	// Next tick: the admin is told once, and the worker is restarted.
	tick := later.Add(4 * time.Minute)
	if err := a.ownerOpsTick(ctx, tick); err != nil {
		t.Fatal(err)
	}
	if !a.WorkerManager.Running(w.serverID) {
		t.Fatal("self-healing did not start the worker")
	}
	open = w.incidents(repository.IncidentOpen)
	if len(open) != 1 || open[0].Attempts != 1 || open[0].LastAction != "worker_restart" || open[0].OwnerNotifiedAt == nil {
		t.Fatalf("after healing = %+v", open)
	}
	opened := fmt.Sprintf("(installation %d)", w.a1.InstallationID)
	if n := w.sent("dm", opened); n != 1 {
		t.Fatalf("the admin was told %d times, want once", n)
	}
	// A restart is rate limited: claiming again straight away is refused.
	if claimed, err := a.PlatformOps.ClaimIncidentAction(ctx, open[0].ID, "worker_restart", ownerops.HealMaxAttempts, ownerops.HealMinGap); err != nil || claimed {
		t.Fatalf("a second restart was allowed immediately: %v %v", claimed, err)
	}

	// The worker is running, so the next tick closes the incident and says so.
	if err := a.ownerOpsTick(ctx, tick.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if left := w.incidents(repository.IncidentOpen); len(left) != 0 {
		t.Fatalf("still open: %+v", left)
	}
	resolved := w.incidents(repository.IncidentResolved)
	if len(resolved) != 1 || resolved[0].Resolution != "recovered after 1 automatic restart(s)" || resolved[0].ResolvedAt == nil {
		t.Fatalf("resolved = %+v", resolved)
	}
	if w.sent("dm", "Incident resolved: Worker down") < 1 {
		t.Fatal("the admin was not told it resolved")
	}
	actions := strings.Join(w.auditActions(t, "?organizationId="+strconv.FormatInt(w.a1.OrgID, 10)), ",")
	for _, want := range []string{"incident.opened", "incident.self_heal", "incident.resolved"} {
		if !strings.Contains(actions, want) {
			t.Fatalf("audit is missing %s: %s", want, actions)
		}
	}

	// A problem only the customer can fix: the bot was removed. The customer is told once.
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE discord_guild_connections SET bot_installed=FALSE WHERE id=$1`, w.a1.ConnectionID); err != nil {
		t.Fatal(err)
	}
	t2 := tick.Add(10 * time.Minute)
	_ = a.ownerOpsTick(ctx, t2)
	_ = a.ownerOpsTick(ctx, t2.Add(ownerops.DetectGrace+time.Minute))
	_ = a.ownerOpsTick(ctx, t2.Add(ownerops.DetectGrace+3*time.Minute))
	open = w.incidents(repository.IncidentOpen)
	if len(open) != 1 || open[0].Kind != repository.IncidentDiscordAccess || open[0].Attempts != 0 || open[0].CustomerNotifiedAt == nil {
		t.Fatalf("discord access incident = %+v", open)
	}
	if n := w.sent("dm", w.a1.OwnerDiscordID+"|The Champion bot is no longer in the Discord server"); n != 1 {
		t.Fatalf("the customer was told %d times, want once", n)
	}

	// The owner can close an incident by hand; the list endpoint shows both states.
	id := strconv.FormatInt(open[0].ID, 10)
	if rr := w.post(a.handleAdminResolveIncident, "/api/admin/incidents/"+id+"/resolve", adminFounderID, map[string]string{"incidentID": id}, map[string]any{"reason": "bot is being re-added"}); rr.Code != http.StatusOK {
		t.Fatalf("resolve by hand: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.post(a.handleAdminResolveIncident, "/api/admin/incidents/"+id+"/resolve", adminFounderID, map[string]string{"incidentID": id}, map[string]any{"reason": "again"}); rr.Code != http.StatusConflict {
		t.Fatalf("resolve twice: %d", rr.Code)
	}
	if rr := w.post(a.handleAdminResolveIncident, "/api/admin/incidents/999999999/resolve", adminFounderID, map[string]string{"incidentID": "999999999"}, map[string]any{"reason": "x"}); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown incident: %d", rr.Code)
	}
	type list struct {
		Items []incidentDTO `json:"items"`
	}
	all := decodeBody[list](t, w.get(a.handleAdminIncidents, "/api/admin/incidents?status=resolved", adminFounderID, nil))
	found := 0
	for _, it := range all.Items {
		if it.InstallationID == w.a1.InstallationID {
			found++
			if it.Status != repository.IncidentResolved || it.KindLabel == "" {
				t.Fatalf("incident dto = %+v", it)
			}
		}
	}
	if found != 2 {
		t.Fatalf("resolved incidents listed = %d, want 2", found)
	}

	// While the runtime is still starting, the monitor concludes nothing.
	a.ownerOpsReady = func() bool { return false }
	a.WorkerManager.Stop(w.serverID)
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE discord_guild_connections SET bot_installed=TRUE WHERE id=$1`, w.a1.ConnectionID); err != nil {
		t.Fatal(err)
	}
	t3 := t2.Add(time.Hour)
	_ = a.ownerOpsTick(ctx, t3)
	_ = a.ownerOpsTick(ctx, t3.Add(ownerops.DetectGrace+time.Minute))
	if open := w.incidents(repository.IncidentOpen); len(open) != 0 {
		t.Fatalf("an incident opened while the runtime was starting: %+v", open)
	}
}

func TestOwnerOpsBriefingIsSentOnceADay(t *testing.T) {
	w := newOwnerOpsWorld(t)
	a := w.a
	ctx := context.Background()
	// A day far in the future, cleared before and after, keeps this test's claim its own.
	day := time.Date(2099, 1, 1, 14, 0, 0, 0, time.UTC)
	clear := func() {
		_, _ = a.DB.Pool.Exec(context.Background(), `DELETE FROM platform_briefings WHERE day >= DATE '2099-01-01'`)
	}
	clear()
	t.Cleanup(clear)
	s := repository.DefaultOwnerOpsSettings()
	s.BriefingEnabled, s.BriefingHourUTC = true, 13
	if err := a.PlatformOps.SaveSettings(ctx, s, "test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = a.PlatformOps.SaveSettings(context.Background(), repository.DefaultOwnerOpsSettings(), "test")
	})
	a.ownerOpsReady = func() bool { return false } // only the briefing is under test

	// Before the hour: nothing.
	if err := a.ownerOpsTick(ctx, day.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stamp := day.Format("Mon 2 Jan 2006")
	if n := w.sent("dm", stamp); n != 0 {
		t.Fatalf("briefing sent before its hour: %d", n)
	}
	for i := 0; i < 3; i++ {
		if err := a.ownerOpsTick(ctx, day.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.sent("dm", adminFounderID+"|**Champion briefing, "+stamp); n != 1 {
		t.Fatalf("briefing sent %d times, want once", n)
	}
	last, delivered, err := a.PlatformOps.LastBriefing(ctx)
	if err != nil || last == nil || last.Format("2006-01-02") != "2099-01-01" || delivered != 1 {
		t.Fatalf("last briefing = %v %d %v", last, delivered, err)
	}
}

func TestOwnerOpsBroadcasts(t *testing.T) {
	w := newOwnerOpsWorld(t)
	a := w.a
	ctx := context.Background()
	orgPV := func(id int64) map[string]string {
		return map[string]string{"organizationID": strconv.FormatInt(id, 10)}
	}
	customer := func(orgID int64, acting string) (int, []map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Authorization", "Bearer test-secret")
		req.Header.Set(actingUserHeader, acting)
		for k, v := range orgPV(orgID) {
			req.SetPathValue(k, v)
		}
		rr := httptest.NewRecorder()
		a.handleOrganizationBroadcasts(rr, req)
		var out struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out.Items
	}
	has := func(items []map[string]any, title string) bool {
		for _, it := range items {
			if it["title"] == title {
				return true
			}
		}
		return false
	}
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	// Validation and the reason.
	for name, body := range map[string]map[string]any{
		"no reason":    {"title": "T", "body": "B"},
		"no title":     {"reason": "r", "body": "B"},
		"bad severity": {"reason": "r", "title": "T", "body": "B", "severity": "LOUD"},
		"bad days":     {"reason": "r", "title": "T", "body": "B", "days": 31},
	} {
		if rr := w.post(a.handleAdminCreateBroadcast, "/api/admin/broadcasts", adminFounderID, nil, body); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	if rr := w.post(a.handleAdminCreateBroadcast, "/api/admin/broadcasts", w.a1.OwnerDiscordID, nil, map[string]any{"reason": "r", "title": "T", "body": "B"}); rr.Code != http.StatusForbidden {
		t.Fatalf("tenant owner broadcast: %d", rr.Code)
	}

	// A notice to everyone, and one for Champion organizations only.
	everyone, premium := "Maintenance "+tag, "Champion news "+tag
	rr := w.post(a.handleAdminCreateBroadcast, "/api/admin/broadcasts", adminFounderID, nil,
		map[string]any{"reason": "planned restart", "title": everyone, "body": "The bot restarts at 02:00 UTC.", "severity": "maintenance", "days": 2})
	if rr.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	created := decodeBody[map[string]broadcastDTO](t, rr)["broadcast"]
	if !created.Active || created.Severity != repository.BroadcastMaintenance || created.EndsAt == nil || created.PostToDiscord {
		t.Fatalf("created = %+v", created)
	}
	if rr := w.post(a.handleAdminCreateBroadcast, "/api/admin/broadcasts", adminFounderID, nil,
		map[string]any{"reason": "release", "title": premium, "body": "New for Champion.", "audiencePlan": "premium"}); rr.Code != http.StatusOK {
		t.Fatalf("create plan broadcast: %d %s", rr.Code, rr.Body.String())
	}
	w.setSubscription(w.a1.OrgID, "PREMIUM", "ACTIVE", "", "")
	t.Cleanup(func() {
		_, _ = a.DB.Pool.Exec(context.Background(), `UPDATE platform_broadcasts SET ended_at=NOW(), ended_by='test' WHERE ended_at IS NULL AND title LIKE '%'||$1`, tag)
	})

	code, items := customer(w.a1.OrgID, w.a1.OwnerDiscordID)
	if code != http.StatusOK || !has(items, everyone) || !has(items, premium) {
		t.Fatalf("champion organization sees %d %v", code, items)
	}
	if _, items := customer(w.b1.OrgID, w.b1.OwnerDiscordID); !has(items, everyone) || has(items, premium) {
		t.Fatalf("an organization on another plan sees %v", items)
	}
	if code, _ := customer(w.a1.OrgID, w.b1.OwnerDiscordID); code != http.StatusForbidden {
		t.Fatalf("a non-member read another organization's notices: %d", code)
	}

	// Ending takes it down at once.
	id := strconv.FormatInt(created.ID, 10)
	if rr := w.post(a.handleAdminEndBroadcast, "/api/admin/broadcasts/"+id+"/end", adminFounderID, map[string]string{"broadcastID": id}, map[string]any{"reason": "done"}); rr.Code != http.StatusOK {
		t.Fatalf("end: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.post(a.handleAdminEndBroadcast, "/api/admin/broadcasts/"+id+"/end", adminFounderID, map[string]string{"broadcastID": id}, map[string]any{"reason": "again"}); rr.Code != http.StatusConflict {
		t.Fatalf("end twice: %d", rr.Code)
	}
	if _, items := customer(w.a1.OrgID, w.a1.OwnerDiscordID); has(items, everyone) {
		t.Fatal("an ended notice is still shown")
	}

	// The Discord copy goes to each set-up installation's staff alerts channel, exactly once,
	// with mentions left as plain text.
	channel := "chan-" + tag
	if _, err := a.DB.Pool.Exec(ctx, `INSERT INTO installation_channel_routes(installation_id, route_key, channel_id) VALUES($1,'ADMIN_ALERTS',$2)`, w.a1.InstallationID, channel); err != nil {
		t.Fatal(err)
	}
	b, err := a.PlatformOps.CreateBroadcast(ctx, repository.PlatformBroadcast{Title: "Discord copy " + tag, Body: "Hello @everyone", Severity: "INCIDENT",
		AudiencePlan: "PREMIUM", PostToDiscord: true, CreatedBy: adminFounderID})
	if err != nil {
		t.Fatal(err)
	}
	a.deliverBroadcast(ctx, b)
	a.deliverBroadcast(ctx, b)
	if n := w.sent("post", channel+"|**Champion incident: Discord copy "+tag+"**\nHello @everyone"); n != 1 {
		t.Fatalf("posted %d times to the alerts channel, want once", n)
	}
	// Other tests' installations share the database and may be in the audience too.
	stored, err := a.PlatformOps.GetBroadcast(ctx, b.ID)
	if err != nil || stored.DiscordSent < 1 {
		t.Fatalf("delivery counts = %+v %v", stored, err)
	}
	type list struct {
		Items []broadcastDTO `json:"items"`
	}
	listed := decodeBody[list](t, w.get(a.handleAdminBroadcasts, "/api/admin/broadcasts", adminFounderID, nil))
	seen := false
	for _, it := range listed.Items {
		if it.ID == b.ID {
			seen = it.DiscordSent >= 1 && it.PostToDiscord
		}
	}
	if !seen {
		t.Fatal("the broadcast list does not show the Discord delivery")
	}
}

func TestOwnerOpsViewAsIsAuditedAndReadOnly(t *testing.T) {
	w := newOwnerOpsWorld(t)
	a := w.a
	org := strconv.FormatInt(w.a1.OrgID, 10)
	pv := map[string]string{"organizationID": org}
	if rr := w.post(a.handleAdminViewAs, "/api/admin/organizations/"+org+"/view-as", adminFounderID, pv, map[string]any{}); rr.Code != http.StatusBadRequest {
		t.Fatalf("no reason: %d", rr.Code)
	}
	if rr := w.post(a.handleAdminViewAs, "/api/admin/organizations/"+org+"/view-as", w.a1.OwnerDiscordID, pv, map[string]any{"reason": "x"}); rr.Code != http.StatusForbidden {
		t.Fatalf("tenant owner: %d", rr.Code)
	}
	if rr := w.post(a.handleAdminViewAs, "/api/admin/organizations/999999999/view-as", adminFounderID, map[string]string{"organizationID": "999999999"}, map[string]any{"reason": "x"}); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown organization: %d", rr.Code)
	}
	rr := w.post(a.handleAdminViewAs, "/api/admin/organizations/"+org+"/view-as", adminFounderID, pv, map[string]any{"reason": "support ticket 12"})
	if rr.Code != http.StatusOK {
		t.Fatalf("view-as: %d %s", rr.Code, rr.Body.String())
	}
	out := decodeBody[struct {
		OrganizationID int64 `json:"organizationId"`
		ActingUser     struct {
			DiscordID string `json:"discordId"`
		} `json:"actingUser"`
		ExpiresInSeconds int `json:"expiresInSeconds"`
	}](t, rr)
	if out.OrganizationID != w.a1.OrgID || out.ActingUser.DiscordID != w.a1.OwnerDiscordID || out.ExpiresInSeconds != viewAsSeconds {
		t.Fatalf("view-as = %+v", out)
	}
	if actions := strings.Join(w.auditActions(t, "?organizationId="+org), ","); !strings.Contains(actions, "organization.viewed_as") {
		t.Fatalf("view-as was not audited: %s", actions)
	}

	// The service gate refuses writes made in a view-as session, and refuses the session
	// outright when the named impersonator is not a platform admin.
	gate := func(method, impersonator string) int {
		req := httptest.NewRequest(method, "/api/saas/x", nil)
		req.Header.Set("Authorization", "Bearer test-secret")
		req.Header.Set(actingUserHeader, w.a1.OwnerDiscordID)
		if impersonator != "" {
			req.Header.Set(impersonatorHeader, impersonator)
		}
		rr := httptest.NewRecorder()
		if a.requireSaaSServiceAuth(rr, req) {
			return http.StatusOK
		}
		return rr.Code
	}
	for name, c := range map[string]struct {
		method, impersonator string
		want                 int
	}{
		"normal write":           {http.MethodPost, "", http.StatusOK},
		"view-as read":           {http.MethodGet, adminFounderID, http.StatusOK},
		"view-as write":          {http.MethodPost, adminFounderID, http.StatusForbidden},
		"view-as put":            {http.MethodPut, adminFounderID, http.StatusForbidden},
		"view-as delete":         {http.MethodDelete, adminFounderID, http.StatusForbidden},
		"impersonator not admin": {http.MethodGet, w.b1.OwnerDiscordID, http.StatusForbidden},
		"customer impersonating": {http.MethodGet, w.a1.OwnerDiscordID, http.StatusForbidden},
	} {
		if got := gate(c.method, c.impersonator); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
}
