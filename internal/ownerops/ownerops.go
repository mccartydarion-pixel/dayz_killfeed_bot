// Package ownerops holds the decisions behind the Owner Hub operations features
// (docs/OWNER_OPS.md): how a server's feed is classified, which incidents a state implies,
// how a customer's health is scored, where an organization sits in the onboarding funnel,
// what counts as recurring revenue, and how the configuration checks and the daily briefing
// are worded. Pure functions over plain values: no database, no Discord, no clock of its own.
package ownerops

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// --- feed state -------------------------------------------------------------------------------------

// Feed states, worst first. QUIET is healthy: a server nobody is fighting on is not broken.
const (
	FeedSuspended = "SUSPENDED"  // the platform owner suspended the installation
	FeedSettingUp = "SETTING_UP" // setup not finished; no feed is expected yet
	FeedNoWorker  = "NO_WORKER"  // set up, but nothing is reading its logs
	FeedStalled   = "STALLED"    // the worker runs but completes no poll cycle
	FeedDegraded  = "DEGRADED"   // Nitrado errors, or the log is behind
	FeedQuiet     = "QUIET"      // reading fine; the log has simply not changed
	FeedLive      = "LIVE"
)

// ADM source states as internal/killfeed reports them (kept as strings here so this package
// stays free of the runtime).
const (
	sourceLagging   = "SOURCE_LAGGING"
	sourceTransport = "TRANSPORT_ERROR"
	sourceStalled   = "WORKER_STALLED"
	sourceQuiet     = "QUIET"
)

// ServerState is everything known about one installation's server at a moment.
type ServerState struct {
	InstallationStatus string // installations.status
	ServerActive       bool   // game_servers.active
	RuntimeReady       bool   // the worker manager exists (false while the bot is still booting)
	WorkerRunning      bool
	SourceState        string // ADM source state; "" when no engine is known
	SourceErrorClass   string // Nitrado error class behind a TRANSPORT_ERROR
	BotInstalled       bool
}

func (s ServerState) operational() bool {
	return s.InstallationStatus == "READY" || s.InstallationStatus == "DEGRADED"
}

// FeedState classifies one server for the fleet wall.
func FeedState(s ServerState) string {
	switch {
	case s.InstallationStatus == "SUSPENDED":
		return FeedSuspended
	case !s.operational():
		return FeedSettingUp
	case !s.WorkerRunning:
		return FeedNoWorker
	case s.SourceState == sourceStalled:
		return FeedStalled
	case s.SourceState == sourceTransport || s.SourceState == sourceLagging:
		return FeedDegraded
	case s.SourceState == sourceQuiet:
		return FeedQuiet
	}
	return FeedLive
}

// FeedBroken reports the states that mean a customer is not getting their feed.
func FeedBroken(state string) bool {
	return state == FeedNoWorker || state == FeedStalled
}

// --- incidents --------------------------------------------------------------------------------------

// Incident kinds (the same strings the repository stores).
const (
	KindWorkerDown    = "WORKER_DOWN"
	KindFeedStalled   = "FEED_STALLED"
	KindNitradoAccess = "NITRADO_ACCESS"
	KindDiscordAccess = "DISCORD_ACCESS"
)

// Kinds is every incident kind, in the order they are evaluated and displayed.
var Kinds = []string{KindWorkerDown, KindFeedStalled, KindNitradoAccess, KindDiscordAccess}

// Detect returns the incident kinds a server's state implies, with a short reason each. An
// installation that is not set up, is suspended, or whose server is deactivated implies none:
// nothing is expected of it. While the runtime is still starting nothing is reported either,
// so a deploy never looks like a fleet-wide outage.
func Detect(s ServerState) map[string]string {
	out := map[string]string{}
	if !s.operational() || !s.ServerActive || !s.RuntimeReady {
		return out
	}
	if !s.BotInstalled {
		out[KindDiscordAccess] = "the Champion bot is no longer in the Discord server"
	}
	if !s.WorkerRunning {
		out[KindWorkerDown] = "no log worker is running for this server"
		return out
	}
	switch s.SourceState {
	case sourceStalled:
		out[KindFeedStalled] = "the log worker has not completed a poll cycle recently"
	case sourceTransport:
		if s.SourceErrorClass == "authentication" || s.SourceErrorClass == "permission" {
			out[KindNitradoAccess] = "Nitrado refuses the saved access token (" + s.SourceErrorClass + ")"
		}
	}
	return out
}

// Healable reports whether the monitor can fix a kind by itself (by restarting the worker).
// Access problems need the customer.
func Healable(kind string) bool { return kind == KindWorkerDown || kind == KindFeedStalled }

// NeedsCustomer reports the kinds only the organization's owner can fix.
func NeedsCustomer(kind string) bool { return kind == KindNitradoAccess || kind == KindDiscordAccess }

// KindLabel is the owner-facing name of an incident kind.
func KindLabel(kind string) string {
	switch kind {
	case KindWorkerDown:
		return "Worker down"
	case KindFeedStalled:
		return "Feed stalled"
	case KindNitradoAccess:
		return "Nitrado access lost"
	case KindDiscordAccess:
		return "Discord access lost"
	}
	return kind
}

// Self-healing limits: how often a worker may be restarted for one incident.
const (
	HealMaxAttempts = 3
	HealMinGap      = 10 * time.Minute
	// DetectGrace is how long a problem must persist before it becomes an incident, so one
	// slow poll or a restart in progress is not reported.
	DetectGrace = 4 * time.Minute
)

// --- customer health --------------------------------------------------------------------------------

// Risk bands.
const (
	RiskHealthy = "HEALTHY"
	RiskWatch   = "WATCH"
	RiskAtRisk  = "AT_RISK"
)

// HealthInput is what one organization's score is computed from.
type HealthInput struct {
	Now time.Time
	// FeedState is the organization's best installation's feed state; "" when it has no
	// installation with a server at all.
	FeedState           string
	ActivePlayers7d     int
	ActivePlayersPrev7d int
	OwnerLastLoginAt    *time.Time
	SubscriptionStatus  string // TRIAL | ACTIVE | PAST_DUE | CANCELED | SUSPENDED | INACTIVE | ""
	TrialEndsAt         *time.Time
	CancelAtPeriodEnd   bool
	OpenIncidents       int
}

// HealthFactor is one reason points were taken off.
type HealthFactor struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Points int    `json:"points"` // always negative
}

type Health struct {
	Score   int            `json:"score"` // 0-100
	Risk    string         `json:"risk"`
	Factors []HealthFactor `json:"factors"`
}

// ScoreHealth starts every customer at 100 and takes points off for each warning sign. The
// factors are returned so the owner sees why, not just a number.
func ScoreHealth(in HealthInput) Health {
	h := Health{Score: 100, Factors: []HealthFactor{}}
	take := func(key, label string, points int) {
		h.Factors = append(h.Factors, HealthFactor{Key: key, Label: label, Points: -points})
		h.Score -= points
	}

	switch in.FeedState {
	case "":
		take("no_server", "No server connected yet", 35)
	case FeedSettingUp:
		take("setting_up", "Setup not finished", 30)
	case FeedSuspended:
		take("suspended", "Installation suspended", 40)
	case FeedNoWorker, FeedStalled:
		take("feed_down", "Feed is not running", 30)
	case FeedDegraded:
		take("feed_degraded", "Feed is degraded", 10)
	}

	switch {
	case in.ActivePlayersPrev7d >= 5 && in.ActivePlayers7d == 0:
		take("players_gone", fmt.Sprintf("No active players this week (was %d)", in.ActivePlayersPrev7d), 25)
	case in.ActivePlayersPrev7d >= 5 && in.ActivePlayers7d*2 < in.ActivePlayersPrev7d:
		take("players_halved", fmt.Sprintf("Active players fell from %d to %d", in.ActivePlayersPrev7d, in.ActivePlayers7d), 20)
	case in.ActivePlayersPrev7d >= 5 && in.ActivePlayers7d*4 < in.ActivePlayersPrev7d*3:
		take("players_down", fmt.Sprintf("Active players fell from %d to %d", in.ActivePlayersPrev7d, in.ActivePlayers7d), 10)
	}

	if in.OwnerLastLoginAt == nil {
		take("owner_never_logged_in", "Owner has never signed in to the website", 10)
	} else if days := int(in.Now.Sub(*in.OwnerLastLoginAt).Hours() / 24); days >= 30 {
		take("owner_away_30", fmt.Sprintf("Owner last signed in %d days ago", days), 20)
	} else if days >= 14 {
		take("owner_away_14", fmt.Sprintf("Owner last signed in %d days ago", days), 10)
	}

	switch in.SubscriptionStatus {
	case "PAST_DUE":
		take("past_due", "Payment is past due", 25)
	case "CANCELED", "INACTIVE", "SUSPENDED", "":
		take("no_plan", "No active plan", 40)
	case "TRIAL":
		switch {
		case in.TrialEndsAt != nil && !in.TrialEndsAt.After(in.Now):
			take("trial_expired", "Trial has expired", 25)
		case in.TrialEndsAt != nil && in.TrialEndsAt.Sub(in.Now) <= 72*time.Hour:
			take("trial_ending", "Trial ends within 3 days", 15)
		}
	}
	if in.CancelAtPeriodEnd {
		take("canceling", "Subscription is set to cancel", 30)
	}
	if in.OpenIncidents > 0 {
		take("open_incident", fmt.Sprintf("%d open incident(s)", in.OpenIncidents), 10)
	}

	if h.Score < 0 {
		h.Score = 0
	}
	switch {
	case h.Score < 50:
		h.Risk = RiskAtRisk
	case h.Score < 75:
		h.Risk = RiskWatch
	default:
		h.Risk = RiskHealthy
	}
	sort.SliceStable(h.Factors, func(i, j int) bool { return h.Factors[i].Points < h.Factors[j].Points })
	return h
}

// --- onboarding funnel ------------------------------------------------------------------------------

// Funnel stages, in the order an organization passes them.
const (
	StageOrgCreated       = "ORG_CREATED"
	StageDiscordConnected = "DISCORD_CONNECTED"
	StageNitradoConnected = "NITRADO_CONNECTED"
	StageServerSelected   = "SERVER_SELECTED"
	StageSetupComplete    = "SETUP_COMPLETE"
	StageFirstKill        = "FIRST_KILL"
)

// Stages is the funnel in order.
var Stages = []string{StageOrgCreated, StageDiscordConnected, StageNitradoConnected, StageServerSelected, StageSetupComplete, StageFirstKill}

// StageLabel is the owner-facing name of a stage.
func StageLabel(stage string) string {
	switch stage {
	case StageOrgCreated:
		return "Organization created"
	case StageDiscordConnected:
		return "Discord connected"
	case StageNitradoConnected:
		return "Nitrado connected"
	case StageServerSelected:
		return "Server selected"
	case StageSetupComplete:
		return "Setup complete"
	case StageFirstKill:
		return "First kill posted"
	}
	return stage
}

// NextStep is what the customer has to do to leave a stage.
func NextStep(stage string) string {
	switch stage {
	case StageOrgCreated:
		return "Add the Champion bot to their Discord server"
	case StageDiscordConnected:
		return "Connect their Nitrado account"
	case StageNitradoConnected:
		return "Pick which DayZ server to track"
	case StageServerSelected:
		return "Finish channel setup and validation"
	case StageSetupComplete:
		return "Waiting for the first kill on the server"
	}
	return ""
}

// Progress is which steps an organization has done.
type Progress struct {
	DiscordConnected, NitradoConnected, ServerSelected, SetupComplete, HasKill bool
}

// StageIndex is how far an organization got: the index into Stages of its furthest step. A
// later step implies the earlier ones (a server cannot have kills without being selected), so
// the furthest true flag decides, whatever the earlier flags say.
func StageIndex(p Progress) int {
	flags := []bool{true, p.DiscordConnected, p.NitradoConnected, p.ServerSelected, p.SetupComplete, p.HasKill}
	furthest := 0
	for i, done := range flags {
		if done {
			furthest = i
		}
	}
	return furthest
}

// --- revenue ----------------------------------------------------------------------------------------

// MonthlyCents converts one billing period's price to a monthly figure.
func MonthlyCents(amountCents int64, interval string) int64 {
	if strings.EqualFold(interval, "YEARLY") || strings.EqualFold(interval, "year") {
		return amountCents / 12
	}
	return amountCents
}

// --- configuration checks ---------------------------------------------------------------------------

// Check severities, worst last.
const (
	SeverityOK   = "OK"
	SeverityInfo = "INFO"
	SeverityWarn = "WARN"
	SeverityFail = "FAIL"
)

// Check is one configuration or consistency finding.
type Check struct {
	ID       string   `json:"id"`
	Severity string   `json:"severity"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
	Items    []string `json:"items"`
}

// SubscriptionFact is one Stripe-billed or owner-set subscription as the checks need it. The
// price id is used to resolve the catalog plan and is never part of any output.
type SubscriptionFact struct {
	OrganizationID   int64
	OrganizationName string
	Plan             string
	Status           string
	StripeBilled     bool
	PriceID          string
}

func orgLabel(id int64, name string) string {
	if name == "" {
		return fmt.Sprintf("organization %d", id)
	}
	return fmt.Sprintf("%s (#%d)", name, id)
}

// SubscriptionChecks compares every subscription with the plan catalog. planForPrice resolves
// a Stripe price to its catalog plan; knownPlan reports whether a plan key is in the catalog.
func SubscriptionChecks(subs []SubscriptionFact, planForPrice func(priceID string) (string, bool), knownPlan func(plan string) bool) []Check {
	var unknownPrice, mismatch, unknownPlan []string
	for _, s := range subs {
		label := orgLabel(s.OrganizationID, s.OrganizationName)
		if s.StripeBilled && s.PriceID != "" {
			plan, ok := planForPrice(s.PriceID)
			switch {
			case !ok:
				unknownPrice = append(unknownPrice, label+": stored plan "+s.Plan)
			case !strings.EqualFold(plan, s.Plan):
				mismatch = append(mismatch, fmt.Sprintf("%s: stored plan %s, but its price belongs to %s", label, s.Plan, plan))
			}
			continue
		}
		switch strings.ToUpper(s.Plan) {
		case "", "TRIAL", "NONE":
		default:
			if !knownPlan(s.Plan) {
				unknownPlan = append(unknownPlan, label+": "+s.Plan)
			}
		}
	}
	out := []Check{}
	add := func(id, title string, failing []string, severity, ok, bad string) {
		c := Check{ID: id, Title: title, Severity: SeverityOK, Detail: ok, Items: []string{}}
		if len(failing) > 0 {
			c.Severity, c.Detail, c.Items = severity, bad, failing
		}
		out = append(out, c)
	}
	add("subscription_price_in_catalog", "Every Stripe price maps to a catalog plan", unknownPrice, SeverityFail,
		"Every Stripe-billed subscription is on a price the plan catalog knows.",
		"These subscriptions bill a Stripe price the plan catalog does not list, so their plan falls back to stale checkout metadata. Add the price to CHAMPION_BILLING_PLANS_JSON, then reconcile them.")
	add("subscription_plan_matches_price", "Stored plans match their Stripe price", mismatch, SeverityFail,
		"Every Stripe-billed subscription's stored plan is the plan its price belongs to.",
		"These subscriptions are stored under a different plan than the price Stripe bills. Reconcile them from the customer page.")
	add("subscription_plan_known", "Granted plans exist in the catalog", unknownPlan, SeverityWarn,
		"Every plan set without Stripe is a catalog plan.",
		"These organizations hold a plan key the catalog does not define; they keep every feature, but the plan has no price or limits.")
	return out
}

// InstallationFact is one installation as the live-installation checks need it.
type InstallationFact struct {
	InstallationID   int64
	OrganizationID   int64
	OrganizationName string
	Status           string
	ServerID         int64 // 0 when no server is selected
	ServerName       string
	ServerActive     bool
	WorkerRunning    bool
}

// InstallationChecks finds installations that disagree with the runtime: a set-up installation
// nobody is reading, an organization with more than one live installation, and two
// installations pointing at one server. runtimeReady false skips the worker check.
func InstallationChecks(rows []InstallationFact, runtimeReady bool) []Check {
	var noWorker, sharedServer, multiLive []string
	byServer := map[int64][]InstallationFact{}
	liveByOrg := map[int64][]InstallationFact{}
	for _, r := range rows {
		live := r.Status == "READY" || r.Status == "DEGRADED"
		if r.ServerID > 0 && r.Status != "SUSPENDED" {
			byServer[r.ServerID] = append(byServer[r.ServerID], r)
		}
		if live && r.ServerID > 0 {
			liveByOrg[r.OrganizationID] = append(liveByOrg[r.OrganizationID], r)
			if runtimeReady && r.ServerActive && !r.WorkerRunning {
				noWorker = append(noWorker, fmt.Sprintf("%s: installation %d (%s)", orgLabel(r.OrganizationID, r.OrganizationName), r.InstallationID, r.ServerName))
			}
		}
	}
	serverIDs := make([]int64, 0, len(byServer))
	for id := range byServer {
		serverIDs = append(serverIDs, id)
	}
	sort.Slice(serverIDs, func(i, j int) bool { return serverIDs[i] < serverIDs[j] })
	for _, id := range serverIDs {
		if list := byServer[id]; len(list) > 1 {
			ids := make([]string, len(list))
			for i, r := range list {
				ids[i] = fmt.Sprint(r.InstallationID)
			}
			sharedServer = append(sharedServer, fmt.Sprintf("server %d (%s): installations %s", id, list[0].ServerName, strings.Join(ids, ", ")))
		}
	}
	orgIDs := make([]int64, 0, len(liveByOrg))
	for id := range liveByOrg {
		orgIDs = append(orgIDs, id)
	}
	sort.Slice(orgIDs, func(i, j int) bool { return orgIDs[i] < orgIDs[j] })
	for _, id := range orgIDs {
		if list := liveByOrg[id]; len(list) > 1 {
			ids := make([]string, len(list))
			for i, r := range list {
				ids[i] = fmt.Sprintf("%d (%s)", r.InstallationID, r.ServerName)
			}
			multiLive = append(multiLive, fmt.Sprintf("%s: %s", orgLabel(id, list[0].OrganizationName), strings.Join(ids, ", ")))
		}
	}
	out := []Check{}
	add := func(id, title string, failing []string, severity, ok, bad string) {
		c := Check{ID: id, Title: title, Severity: SeverityOK, Detail: ok, Items: []string{}}
		if len(failing) > 0 {
			c.Severity, c.Detail, c.Items = severity, bad, failing
		}
		out = append(out, c)
	}
	add("live_installations_have_worker", "Every live installation has a running worker", noWorker, SeverityFail,
		"Every set-up installation with an active server has a log worker running.",
		"These installations are set up but nothing is reading their server's logs. Restart the worker from the installation page.")
	add("one_installation_per_server", "No server is claimed by two installations", sharedServer, SeverityWarn,
		"No game server is attached to more than one installation.",
		"More than one installation points at the same game server. Suspend the spares so settings and plan checks have one owner.")
	add("live_installations_per_organization", "Organizations with several live installations", multiLive, SeverityInfo,
		"No organization runs more than one live installation.",
		"These organizations run several live installations. That is allowed on multi-server plans; check it is intended.")
	return out
}

// Worst returns the most severe severity among checks.
func Worst(checks []Check) string {
	rank := map[string]int{SeverityOK: 0, SeverityInfo: 1, SeverityWarn: 2, SeverityFail: 3}
	worst := SeverityOK
	for _, c := range checks {
		if rank[c.Severity] > rank[worst] {
			worst = c.Severity
		}
	}
	return worst
}

// --- daily briefing ---------------------------------------------------------------------------------

// Briefing is the numbers the daily summary reports.
type Briefing struct {
	Day                time.Time
	NewUsers24h        int
	NewOrganizations   int
	MRRCents           int64
	Currency           string
	PaidCents24h       int64
	FailedPayments24h  int
	TrialsEndingSoon   []string // organization names whose trial ends within 3 days
	AtRisk             []string // "name (score)" for the lowest-scoring customers
	OpenIncidents      []string // "kind: organization / server"
	ResolvedIncidents  int      // closed in the last 24 hours
	StuckOnboarding    int
	ConfigProblems     int // checks at WARN or FAIL
	LiveServers        int
	BrokenServers      int
	PlayersOnline      int
	Kills24h           int
	ActiveSubscribers  int
	TrialOrganizations int
}

func money(cents int64, currency string) string {
	if currency == "" {
		currency = "usd"
	}
	symbol := strings.ToUpper(currency) + " "
	if strings.EqualFold(currency, "usd") {
		symbol = "$"
	}
	return fmt.Sprintf("%s%d.%02d", symbol, cents/100, cents%100)
}

func bullets(items []string, max int) string {
	var b strings.Builder
	for i, item := range items {
		if i == max {
			fmt.Fprintf(&b, "- and %d more\n", len(items)-max)
			break
		}
		b.WriteString("- " + item + "\n")
	}
	return b.String()
}

// BriefingText renders the briefing as Discord markdown, under Discord's 2000-character limit.
func BriefingText(b Briefing) string {
	var s strings.Builder
	fmt.Fprintf(&s, "**Champion briefing, %s**\n", b.Day.UTC().Format("Mon 2 Jan 2006"))
	fmt.Fprintf(&s, "Revenue: %s MRR from %d paying, %d on trial. Collected %s in the last 24h", money(b.MRRCents, b.Currency), b.ActiveSubscribers, b.TrialOrganizations, money(b.PaidCents24h, b.Currency))
	if b.FailedPayments24h > 0 {
		fmt.Fprintf(&s, ", %d failed payment(s)", b.FailedPayments24h)
	}
	s.WriteString(".\n")
	fmt.Fprintf(&s, "Growth: %d new account(s), %d new organization(s), %d stuck in onboarding.\n", b.NewUsers24h, b.NewOrganizations, b.StuckOnboarding)
	fmt.Fprintf(&s, "Fleet: %d server(s) live, %d broken, %d player(s) online, %d kill(s) in 24h.\n", b.LiveServers, b.BrokenServers, b.PlayersOnline, b.Kills24h)
	if len(b.OpenIncidents) > 0 {
		fmt.Fprintf(&s, "\n**Open incidents (%d)**\n%s", len(b.OpenIncidents), bullets(b.OpenIncidents, 6))
	} else {
		s.WriteString("No open incidents")
		if b.ResolvedIncidents > 0 {
			fmt.Fprintf(&s, "; %d resolved in the last 24h", b.ResolvedIncidents)
		}
		s.WriteString(".\n")
	}
	if len(b.AtRisk) > 0 {
		fmt.Fprintf(&s, "\n**At-risk customers (%d)**\n%s", len(b.AtRisk), bullets(b.AtRisk, 6))
	}
	if len(b.TrialsEndingSoon) > 0 {
		fmt.Fprintf(&s, "\n**Trials ending within 3 days (%d)**\n%s", len(b.TrialsEndingSoon), bullets(b.TrialsEndingSoon, 6))
	}
	if b.ConfigProblems > 0 {
		fmt.Fprintf(&s, "\n%d configuration check(s) need attention.\n", b.ConfigProblems)
	}
	out := s.String()
	if r := []rune(out); len(r) > 1900 {
		out = string(r[:1900]) + "\n(truncated)"
	}
	return out
}
