package ownerops

import (
	"strings"
	"testing"
	"time"
)

func live() ServerState {
	return ServerState{InstallationStatus: "READY", ServerActive: true, RuntimeReady: true, WorkerRunning: true, SourceState: "HEALTHY", BotInstalled: true}
}

func TestFeedState(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(*ServerState)
		want   string
	}{
		"live":        {func(*ServerState) {}, FeedLive},
		"quiet":       {func(s *ServerState) { s.SourceState = "QUIET" }, FeedQuiet},
		"lagging":     {func(s *ServerState) { s.SourceState = "SOURCE_LAGGING" }, FeedDegraded},
		"transport":   {func(s *ServerState) { s.SourceState = "TRANSPORT_ERROR" }, FeedDegraded},
		"stalled":     {func(s *ServerState) { s.SourceState = "WORKER_STALLED" }, FeedStalled},
		"no worker":   {func(s *ServerState) { s.WorkerRunning = false }, FeedNoWorker},
		"setting up":  {func(s *ServerState) { s.InstallationStatus = "CONFIGURING"; s.WorkerRunning = false }, FeedSettingUp},
		"suspended":   {func(s *ServerState) { s.InstallationStatus = "SUSPENDED"; s.WorkerRunning = false }, FeedSuspended},
		"degraded ok": {func(s *ServerState) { s.InstallationStatus = "DEGRADED" }, FeedLive},
	} {
		s := live()
		c.mutate(&s)
		if got := FeedState(s); got != c.want {
			t.Errorf("%s: FeedState = %s, want %s", name, got, c.want)
		}
	}
	if !FeedBroken(FeedNoWorker) || !FeedBroken(FeedStalled) || FeedBroken(FeedQuiet) || FeedBroken(FeedDegraded) || FeedBroken(FeedSettingUp) {
		t.Fatal("only a missing or stalled worker is a broken feed")
	}
}

func TestDetect(t *testing.T) {
	if got := Detect(live()); len(got) != 0 {
		t.Fatalf("a healthy server implied incidents: %v", got)
	}
	quiet := live()
	quiet.SourceState = "QUIET"
	if got := Detect(quiet); len(got) != 0 {
		t.Fatalf("a quiet server is not broken: %v", got)
	}

	down := live()
	down.WorkerRunning = false
	down.SourceState = "WORKER_STALLED" // stale engine state must not add a second incident
	if got := Detect(down); len(got) != 1 || got[KindWorkerDown] == "" {
		t.Fatalf("worker down = %v", got)
	}
	stalled := live()
	stalled.SourceState = "WORKER_STALLED"
	if got := Detect(stalled); len(got) != 1 || got[KindFeedStalled] == "" {
		t.Fatalf("stalled = %v", got)
	}

	// Nitrado refusing the token needs the customer; a temporary Nitrado failure is not an incident.
	for class, want := range map[string]bool{"authentication": true, "permission": true, "temporary": false, "": false} {
		s := live()
		s.SourceState, s.SourceErrorClass = "TRANSPORT_ERROR", class
		_, got := Detect(s)[KindNitradoAccess]
		if got != want {
			t.Errorf("transport error class %q: nitrado incident = %v, want %v", class, got, want)
		}
	}
	noBot := live()
	noBot.BotInstalled = false
	if got := Detect(noBot); len(got) != 1 || got[KindDiscordAccess] == "" {
		t.Fatalf("bot removed = %v", got)
	}

	// Nothing is expected of an installation that is not set up, is suspended, has a deactivated
	// server, or while the runtime is still starting.
	for name, mutate := range map[string]func(*ServerState){
		"configuring":     func(s *ServerState) { s.InstallationStatus = "CONFIGURING" },
		"suspended":       func(s *ServerState) { s.InstallationStatus = "SUSPENDED" },
		"server inactive": func(s *ServerState) { s.ServerActive = false },
		"booting":         func(s *ServerState) { s.RuntimeReady = false },
	} {
		s := live()
		s.WorkerRunning, s.BotInstalled = false, false
		mutate(&s)
		if got := Detect(s); len(got) != 0 {
			t.Errorf("%s: implied %v", name, got)
		}
	}

	if !Healable(KindWorkerDown) || !Healable(KindFeedStalled) || Healable(KindNitradoAccess) || Healable(KindDiscordAccess) {
		t.Fatal("only worker problems are healable")
	}
	if NeedsCustomer(KindWorkerDown) || !NeedsCustomer(KindNitradoAccess) || !NeedsCustomer(KindDiscordAccess) {
		t.Fatal("only access problems need the customer")
	}
	for _, k := range Kinds {
		if KindLabel(k) == k {
			t.Errorf("kind %s has no label", k)
		}
	}
}

func TestScoreHealth(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	in := func() HealthInput {
		return HealthInput{Now: now, FeedState: FeedLive, ActivePlayers7d: 40, ActivePlayersPrev7d: 38, OwnerLastLoginAt: ago(24 * time.Hour), SubscriptionStatus: "ACTIVE"}
	}

	if h := ScoreHealth(in()); h.Score != 100 || h.Risk != RiskHealthy || len(h.Factors) != 0 {
		t.Fatalf("a healthy customer = %+v", h)
	}

	// A quiet server and steady players cost nothing.
	quiet := in()
	quiet.FeedState = FeedQuiet
	if h := ScoreHealth(quiet); h.Score != 100 {
		t.Fatalf("quiet feed = %+v", h)
	}

	churning := in()
	churning.ActivePlayers7d, churning.CancelAtPeriodEnd = 10, true
	churning.OwnerLastLoginAt = ago(40 * 24 * time.Hour)
	h := ScoreHealth(churning)
	if h.Score != 100-20-30-20 || h.Risk != RiskAtRisk {
		t.Fatalf("churning = %+v", h)
	}
	if h.Factors[0].Key != "canceling" || h.Factors[0].Points != -30 {
		t.Fatalf("factors must be worst first: %+v", h.Factors)
	}

	// A small server losing two players is noise, not churn.
	small := in()
	small.ActivePlayersPrev7d, small.ActivePlayers7d = 4, 1
	if h := ScoreHealth(small); h.Score != 100 {
		t.Fatalf("small server = %+v", h)
	}

	trial := in()
	trial.SubscriptionStatus = "TRIAL"
	end := now.Add(48 * time.Hour)
	trial.TrialEndsAt = &end
	if h := ScoreHealth(trial); h.Score != 85 || h.Factors[0].Key != "trial_ending" {
		t.Fatalf("trial ending = %+v", h)
	}
	end = now.Add(-time.Hour)
	if h := ScoreHealth(trial); h.Factors[0].Key != "trial_expired" {
		t.Fatalf("trial expired = %+v", h)
	}

	dead := HealthInput{Now: now, FeedState: "", SubscriptionStatus: "", OpenIncidents: 2}
	if h := ScoreHealth(dead); h.Score != 100-35-10-40-10 || h.Risk != RiskAtRisk {
		t.Fatalf("no server, no plan = %+v", h)
	}
	worst := HealthInput{Now: now, FeedState: FeedSuspended, ActivePlayersPrev7d: 50, SubscriptionStatus: "PAST_DUE", CancelAtPeriodEnd: true, OpenIncidents: 1}
	if h := ScoreHealth(worst); h.Score != 0 {
		t.Fatalf("the score never goes below zero: %+v", h)
	}
	watch := in()
	watch.FeedState = FeedStalled
	if h := ScoreHealth(watch); h.Score != 70 || h.Risk != RiskWatch {
		t.Fatalf("feed down = %+v", h)
	}
}

func TestStageIndex(t *testing.T) {
	if got := StageIndex(Progress{}); Stages[got] != StageOrgCreated {
		t.Fatalf("nothing done = %s", Stages[got])
	}
	if got := StageIndex(Progress{DiscordConnected: true, NitradoConnected: true}); Stages[got] != StageNitradoConnected {
		t.Fatalf("nitrado connected = %s", Stages[got])
	}
	// A later step implies the earlier ones, whatever their flags say.
	if got := StageIndex(Progress{HasKill: true}); Stages[got] != StageFirstKill {
		t.Fatalf("kills without flags = %s", Stages[got])
	}
	if got := StageIndex(Progress{DiscordConnected: true, ServerSelected: true}); Stages[got] != StageServerSelected {
		t.Fatalf("server selected = %s", Stages[got])
	}
	for _, s := range Stages {
		if StageLabel(s) == s {
			t.Errorf("stage %s has no label", s)
		}
		if s != StageFirstKill && NextStep(s) == "" {
			t.Errorf("stage %s has no next step", s)
		}
	}
}

func TestMonthlyCents(t *testing.T) {
	if MonthlyCents(1499, "MONTHLY") != 1499 || MonthlyCents(14990, "YEARLY") != 1249 || MonthlyCents(5990, "year") != 499 || MonthlyCents(999, "month") != 999 {
		t.Fatal("monthly conversion")
	}
}

func TestSubscriptionChecks(t *testing.T) {
	planForPrice := func(id string) (string, bool) {
		plans := map[string]string{"price_premium": "PREMIUM", "price_normal": "NORMAL"}
		p, ok := plans[id]
		return p, ok
	}
	known := func(plan string) bool { return plan == "PREMIUM" || plan == "NORMAL" }
	subs := []SubscriptionFact{
		{OrganizationID: 1, OrganizationName: "Alpha", Plan: "PREMIUM", Status: "ACTIVE", StripeBilled: true, PriceID: "price_premium"},
		{OrganizationID: 2, OrganizationName: "Bravo", Plan: "NORMAL", Status: "ACTIVE", StripeBilled: true, PriceID: "price_premium"},
		{OrganizationID: 3, OrganizationName: "Charlie", Plan: "NORMAL", Status: "ACTIVE", StripeBilled: true, PriceID: "price_retired"},
		{OrganizationID: 4, OrganizationName: "Delta", Plan: "GOLD", Status: "ACTIVE"},
		{OrganizationID: 5, OrganizationName: "Echo", Plan: "TRIAL", Status: "TRIAL"},
		{OrganizationID: 6, OrganizationName: "", Plan: "premium", Status: "ACTIVE", StripeBilled: true, PriceID: "price_premium"},
	}
	checks := SubscriptionChecks(subs, planForPrice, known)
	byID := map[string]Check{}
	for _, c := range checks {
		byID[c.ID] = c
	}
	if c := byID["subscription_price_in_catalog"]; c.Severity != SeverityFail || len(c.Items) != 1 || !strings.Contains(c.Items[0], "Charlie (#3)") {
		t.Fatalf("unknown price = %+v", c)
	}
	if c := byID["subscription_plan_matches_price"]; c.Severity != SeverityFail || len(c.Items) != 1 || !strings.Contains(c.Items[0], "Bravo (#2)") || !strings.Contains(c.Items[0], "PREMIUM") {
		t.Fatalf("mismatch = %+v", c)
	}
	if c := byID["subscription_plan_known"]; c.Severity != SeverityWarn || len(c.Items) != 1 || !strings.Contains(c.Items[0], "GOLD") {
		t.Fatalf("unknown plan = %+v", c)
	}
	// A Stripe price id is an identifier this output must never carry.
	for _, c := range checks {
		if strings.Contains(c.Detail+strings.Join(c.Items, " "), "price_") {
			t.Fatalf("check %s leaks a price id: %+v", c.ID, c)
		}
	}
	if Worst(checks) != SeverityFail {
		t.Fatalf("worst = %s", Worst(checks))
	}
	clean := SubscriptionChecks(subs[:1], planForPrice, known)
	if Worst(clean) != SeverityOK {
		t.Fatalf("clean = %+v", clean)
	}
	for _, c := range clean {
		if c.Items == nil {
			t.Fatalf("items must be an empty list, not null: %+v", c)
		}
	}
}

func TestInstallationChecks(t *testing.T) {
	rows := []InstallationFact{
		{InstallationID: 11, OrganizationID: 1, OrganizationName: "Alpha", Status: "READY", ServerID: 1, ServerName: "Lost City", ServerActive: true, WorkerRunning: true},
		{InstallationID: 12, OrganizationID: 1, OrganizationName: "Alpha", Status: "SUSPENDED", ServerID: 1, ServerName: "Lost City"},
		{InstallationID: 20, OrganizationID: 2, OrganizationName: "Bravo", Status: "READY", ServerID: 2, ServerName: "Livonia", ServerActive: true},
		{InstallationID: 21, OrganizationID: 2, OrganizationName: "Bravo", Status: "DEGRADED", ServerID: 3, ServerName: "Sakhal", ServerActive: true, WorkerRunning: true},
		{InstallationID: 30, OrganizationID: 3, OrganizationName: "Charlie", Status: "CONFIGURING", ServerID: 3, ServerName: "Sakhal", ServerActive: true},
		{InstallationID: 31, OrganizationID: 3, OrganizationName: "Charlie", Status: "NOT_STARTED"},
	}
	byID := map[string]Check{}
	for _, c := range InstallationChecks(rows, true) {
		byID[c.ID] = c
	}
	if c := byID["live_installations_have_worker"]; c.Severity != SeverityFail || len(c.Items) != 1 || !strings.Contains(c.Items[0], "installation 20") {
		t.Fatalf("no worker = %+v", c)
	}
	// A suspended spare does not count as a second claim; an unfinished one on another org's server does.
	if c := byID["one_installation_per_server"]; c.Severity != SeverityWarn || len(c.Items) != 1 || !strings.Contains(c.Items[0], "server 3") || !strings.Contains(c.Items[0], "21, 30") {
		t.Fatalf("shared server = %+v", c)
	}
	if c := byID["live_installations_per_organization"]; c.Severity != SeverityInfo || len(c.Items) != 1 || !strings.Contains(c.Items[0], "Bravo (#2)") {
		t.Fatalf("multi live = %+v", c)
	}
	// While the runtime is still starting, a missing worker is not a finding.
	for _, c := range InstallationChecks(rows, false) {
		if c.ID == "live_installations_have_worker" && c.Severity != SeverityOK {
			t.Fatalf("worker check ran before the runtime was ready: %+v", c)
		}
	}
}

func TestBriefingText(t *testing.T) {
	b := Briefing{Day: time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC), NewUsers24h: 4, NewOrganizations: 2, MRRCents: 1499, Currency: "usd",
		PaidCents24h: 1499, FailedPayments24h: 1, StuckOnboarding: 3, LiveServers: 5, BrokenServers: 1, PlayersOnline: 22, Kills24h: 310,
		ActiveSubscribers: 1, TrialOrganizations: 6, OpenIncidents: []string{"Worker down: Alpha / Lost City"}, AtRisk: []string{"Bravo (35)"},
		TrialsEndingSoon: []string{"Charlie"}, ConfigProblems: 2}
	text := BriefingText(b)
	for _, want := range []string{"Thu 1 Oct 2026", "$14.99 MRR from 1 paying, 6 on trial", "1 failed payment(s)", "4 new account(s), 2 new organization(s), 3 stuck",
		"5 server(s) live, 1 broken", "Open incidents (1)", "Worker down: Alpha / Lost City", "At-risk customers (1)", "Bravo (35)", "Trials ending within 3 days (1)", "2 configuration check(s)"} {
		if !strings.Contains(text, want) {
			t.Errorf("briefing is missing %q:\n%s", want, text)
		}
	}
	quiet := BriefingText(Briefing{Day: b.Day, ResolvedIncidents: 2})
	if !strings.Contains(quiet, "No open incidents; 2 resolved in the last 24h.") || strings.Contains(quiet, "At-risk") {
		t.Fatalf("quiet day:\n%s", quiet)
	}
	many := Briefing{Day: b.Day}
	for i := 0; i < 400; i++ {
		many.OpenIncidents = append(many.OpenIncidents, strings.Repeat("é", 40))
	}
	if long := BriefingText(many); len([]rune(long)) > 2000 || !strings.Contains(long, "and 394 more") {
		t.Fatalf("a long briefing must stay under Discord's limit and summarise: %d runes", len([]rune(long)))
	}
}
