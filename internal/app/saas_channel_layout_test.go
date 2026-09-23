package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// layoutGuildFake is an in-memory Discord guild for applyChannelLayout.
type layoutGuildFake struct {
	channels    []discord.RawGuildChannel
	private     map[string]bool // category IDs created private
	botMessages map[string]int  // channel ID -> bot messages present
	starters    map[string]int  // channel ID -> starter cards sent
	missing     map[string][]string
	seq         int
	createCalls int
	listErr     error
}

func newLayoutGuildFake(seed ...discord.RawGuildChannel) *layoutGuildFake {
	return &layoutGuildFake{channels: seed, private: map[string]bool{}, botMessages: map[string]int{}, starters: map[string]int{}, missing: map[string][]string{}}
}

func (f *layoutGuildFake) ListAllGuildChannels(string) ([]discord.RawGuildChannel, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]discord.RawGuildChannel(nil), f.channels...), nil
}
func (f *layoutGuildFake) create(name string, t discordgo.ChannelType, parent string) *discord.RawGuildChannel {
	f.seq++
	f.createCalls++
	ch := discord.RawGuildChannel{ID: fmt.Sprintf("new-%d", f.seq), Name: name, Type: t, ParentID: parent}
	f.channels = append(f.channels, ch)
	return &ch
}
func (f *layoutGuildFake) CreateGuildCategory(_, name string) (*discord.RawGuildChannel, error) {
	return f.create(name, discordgo.ChannelTypeGuildCategory, ""), nil
}
func (f *layoutGuildFake) CreatePrivateGuildCategory(_, name string) (*discord.RawGuildChannel, error) {
	ch := f.create(name, discordgo.ChannelTypeGuildCategory, "")
	f.private[ch.ID] = true
	return ch, nil
}
func (f *layoutGuildFake) CreateGuildTextChannel(_, name, parent string) (*discord.RawGuildChannel, error) {
	return f.create(name, discordgo.ChannelTypeGuildText, parent), nil
}
func (f *layoutGuildFake) Verify(_, channelID string) discord.Verification {
	for _, ch := range f.channels {
		if ch.ID == channelID {
			return discord.Verification{GuildFound: true, ChannelFound: true, Missing: f.missing[channelID]}
		}
	}
	return discord.Verification{GuildFound: true}
}
func (f *layoutGuildFake) SendChannelEmbed(channelID string, _ *discordgo.MessageEmbed) error {
	f.starters[channelID]++
	f.botMessages[channelID]++
	return nil
}
func (f *layoutGuildFake) ChannelHasBotMessage(channelID string) (bool, error) {
	return f.botMessages[channelID] > 0, nil
}
func (f *layoutGuildFake) byName(name string) (discord.RawGuildChannel, int) {
	var found discord.RawGuildChannel
	n := 0
	for _, ch := range f.channels {
		if ch.Name == name {
			found = ch
			n++
		}
	}
	return found, n
}

// layoutRoutesFake records route writes.
type layoutRoutesFake struct {
	routes  map[string]string
	deleted []string
}

func (w *layoutRoutesFake) UpsertRoute(_ context.Context, _, _ int64, key, channelID string, managed bool) error {
	if !managed {
		return errors.New("auto-setup routes must be managed")
	}
	w.routes[key] = channelID
	return nil
}
func (w *layoutRoutesFake) DeleteRoute(_ context.Context, _, _ int64, key string) error {
	delete(w.routes, key)
	w.deleted = append(w.deleted, key)
	return nil
}
func (w *layoutRoutesFake) existing() []repository.ChannelRoute {
	var out []repository.ChannelRoute
	for k, v := range w.routes {
		out = append(out, repository.ChannelRoute{RouteKey: k, ChannelID: v, ManagedByChampion: true})
	}
	return out
}

func auditProducers() map[string]routeProducer {
	out := map[string]routeProducer{}
	for k, v := range routeProducerAudit {
		out[k] = v
	}
	return out
}

// panelsPosted simulates the panel owners posting into every panel channel.
func panelsPosted(g *layoutGuildFake, w *layoutRoutesFake) func(context.Context) {
	return func(context.Context) {
		for _, key := range []string{"BOUNTY", "HEATMAPS", "AUTO_LEADERBOARD", "STATS_LEADERBOARDS", "LINK_GAMERTAG"} {
			if ch := w.routes[key]; ch != "" {
				g.botMessages[ch] = 1
			}
		}
	}
}

func runLayout(t *testing.T, g *layoutGuildFake, w *layoutRoutesFake, producers map[string]routeProducer, sync func(context.Context)) *channelLayoutResult {
	t.Helper()
	res, err := applyChannelLayout(context.Background(), g, w, channelLayoutInput{OrganizationID: 1, InstallationID: 2, GuildID: "g", Existing: w.existing(), Producers: producers, SyncPanels: sync})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func report(res *channelLayoutResult, key string) ChannelDestinationReport {
	for _, d := range res.Destinations {
		if d.Key == key {
			return d
		}
	}
	return ChannelDestinationReport{}
}

func TestRouteVocabularyHasNoCasinoAndEveryRouteOneDestination(t *testing.T) {
	seen := map[string]string{}
	for _, d := range championDestinations {
		for _, r := range d.Routes {
			if prev, dup := seen[r]; dup {
				t.Fatalf("route %s mapped to both %s and %s", r, prev, d.Key)
			}
			seen[r] = d.Key
		}
		for _, a := range d.Anchors {
			if seen[a] != d.Key {
				t.Fatalf("anchor %s of %s is not one of its routes", a, d.Key)
			}
		}
	}
	if championRouteKeys["CASINO"] {
		t.Fatal("CASINO must not exist")
	}
	if len(routeProducerAudit) != len(championRouteKeys) {
		t.Fatalf("producer audit covers %d routes, vocabulary has %d", len(routeProducerAudit), len(championRouteKeys))
	}
	for k := range championRouteKeys {
		if _, ok := routeProducerAudit[k]; !ok {
			t.Fatalf("route %s has no audited producer", k)
		}
	}
	want := map[string]string{
		"KILLFEED": "COMBAT_FEED", "PVE_FEED": "COMBAT_FEED", "HITFEED": "HITFEED", "BOUNTY": "BOUNTIES", "BOUNTY_TRACKING": "BOUNTIES",
		"CONNECTIONS": "CONNECTIONS", "HEATMAPS": "HEATMAPS", "AUTO_LEADERBOARD": "LEADERBOARDS", "STATS_LEADERBOARDS": "LEADERBOARDS",
		"LINK_GAMERTAG": "PLAYER_LINK", "ECONOMY": "ECONOMY", "SHOP": "ECONOMY", "ADMIN_LOGS": "ADMIN_LOGS", "ADMIN_ALERTS": "ADMIN_LOGS", "BUILD_FEED": "ADMIN_LOGS",
	}
	for route, dest := range want {
		if seen[route] != dest {
			t.Fatalf("route %s -> %s, want %s", route, seen[route], dest)
		}
	}
}

func TestPlanChannelLayoutSkipsDestinationsWithoutProducers(t *testing.T) {
	plans := planChannelLayout(auditProducers())
	health := map[string]ChannelHealth{}
	for _, p := range plans {
		health[p.Destination.Key] = p.Health
	}
	for _, key := range []string{"COMBAT_FEED", "HITFEED", "BOUNTIES", "CONNECTIONS", "HEATMAPS", "LEADERBOARDS", "PLAYER_LINK", "ECONOMY", "ADMIN_LOGS"} {
		if health[key] != HealthActive {
			t.Fatalf("%s want ACTIVE, got %s", key, health[key])
		}
	}

	// A source-blocked BUILD_FEED never justifies admin-logs on its own.
	producers := auditProducers()
	producers["ADMIN_LOGS"] = routeProducer{HealthBroken, "down"}
	producers["ADMIN_ALERTS"] = routeProducer{HealthBroken, "down"}
	for _, p := range planChannelLayout(producers) {
		if p.Destination.Key == "ADMIN_LOGS" && p.Health != HealthBroken {
			t.Fatalf("admin-logs with only a blocked source must not be ACTIVE, got %s", p.Health)
		}
	}
}

func TestApplyChannelLayoutFreshGuild(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{}}
	res := runLayout(t, g, w, auditProducers(), panelsPosted(g, w))

	for _, cat := range championCategories {
		ch, n := g.byName(cat.Name)
		if n != 1 {
			t.Fatalf("want one %q category, got %d", cat.Name, n)
		}
		if g.private[ch.ID] != cat.Private {
			t.Fatalf("category %q private=%v, want %v", cat.Name, g.private[ch.ID], cat.Private)
		}
	}
	// None of the retired names.
	for _, name := range []string{"casino", "shop", "build-feed", "admin-alerts", "bounty-tracking", "stats-leaderboards", "auto-leaderboard", "death-feed", "pvefeed"} {
		if _, n := g.byName(name); n != 0 {
			t.Fatalf("channel %q must not be created", name)
		}
	}
	adminLogs, _ := g.byName("🛡️・admin-logs")
	staff, _ := g.byName("🔒 CHAMPION • STAFF")
	if adminLogs.ParentID != staff.ID {
		t.Fatal("admin-logs must sit under the private staff category")
	}
	for _, key := range []string{"ADMIN_LOGS", "ADMIN_ALERTS", "BUILD_FEED"} {
		if w.routes[key] != adminLogs.ID {
			t.Fatalf("%s must route to admin-logs, got %q", key, w.routes[key])
		}
	}
	economy, _ := g.byName("💰・economy")
	if w.routes["SHOP"] != economy.ID || w.routes["ECONOMY"] != economy.ID {
		t.Fatal("SHOP and ECONOMY must share the economy channel")
	}
	combat, _ := g.byName("🔫・combat-feed")
	if w.routes["KILLFEED"] != combat.ID || w.routes["PVE_FEED"] != combat.ID {
		t.Fatal("KILLFEED and PVE_FEED must share combat-feed")
	}
	heat, _ := g.byName("🗺️・heatmaps")
	live, _ := g.byName("🏆 CHAMPION • LIVE")
	if w.routes["HEATMAPS"] != heat.ID || heat.ParentID != live.ID {
		t.Fatal("HEATMAPS must route to the heatmaps channel under LIVE")
	}

	// Every created channel shows Champion content: starter for feeds, the
	// panel for panel channels (no starter on top of it).
	for _, d := range res.Destinations {
		if d.Health == HealthBlocked {
			continue
		}
		if d.Health != HealthActive || d.Checks == nil || !d.Checks.passed() {
			t.Fatalf("%s want ACTIVE with all checks passing, got %+v", d.Key, d)
		}
		wantStarter := destinationByKey(d.Key).Starter != nil
		if d.StarterSent != wantStarter || g.starters[d.ChannelID] != map[bool]int{true: 1, false: 0}[wantStarter] {
			t.Fatalf("%s starterSent=%v starters=%d, want starter=%v", d.Key, d.StarterSent, g.starters[d.ChannelID], wantStarter)
		}
	}
	admin := report(res, "ADMIN_LOGS")
	details := map[string]string{}
	for _, r := range admin.Routes {
		details[r.RouteKey] = r.Detail
	}
	health := map[string]ChannelHealth{}
	for _, r := range admin.Routes {
		health[r.RouteKey] = r.Health
	}
	if health["ADMIN_ALERTS"] != HealthActive || health["BUILD_FEED"] != HealthBlocked || !strings.HasPrefix(details["BUILD_FEED"], detailSourceBlocked) {
		t.Fatalf("admin-logs route states wrong: %+v", admin.Routes)
	}
}

func TestApplyChannelLayoutIsIdempotent(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{}}
	first := runLayout(t, g, w, auditProducers(), panelsPosted(g, w))
	creates := g.createCalls
	second := runLayout(t, g, w, auditProducers(), panelsPosted(g, w))
	if g.createCalls != creates {
		t.Fatalf("second run created %d more channels", g.createCalls-creates)
	}
	for k, v := range first.Routes {
		if second.Routes[k].ChannelID != v.ChannelID {
			t.Fatalf("route %s moved from %s to %s", k, v.ChannelID, second.Routes[k].ChannelID)
		}
	}
	for ch, n := range g.starters {
		if n != 1 {
			t.Fatalf("channel %s got %d starter cards, want exactly one", ch, n)
		}
	}
	if len(second.Retirable) != 0 {
		t.Fatalf("nothing is retirable on a rerun, got %+v", second.Retirable)
	}
}

func TestApplyChannelLayoutMigratesV1WithoutDeleting(t *testing.T) {
	g := newLayoutGuildFake(
		discord.RawGuildChannel{ID: "old-cat", Name: "CHAMPION KILLFEED", Type: discordgo.ChannelTypeGuildCategory},
		discord.RawGuildChannel{ID: "old-killfeed", Name: "killfeed", Type: discordgo.ChannelTypeGuildText, ParentID: "old-cat"},
		discord.RawGuildChannel{ID: "old-casino", Name: "casino", Type: discordgo.ChannelTypeGuildText, ParentID: "old-cat"},
		discord.RawGuildChannel{ID: "old-heatmaps", Name: "heatmaps", Type: discordgo.ChannelTypeGuildText, ParentID: "old-cat"},
	)
	w := &layoutRoutesFake{routes: map[string]string{"KILLFEED": "old-killfeed", "CASINO": "old-casino", "HEATMAPS": "old-heatmaps"}}
	before := len(g.channels)
	res := runLayout(t, g, w, auditProducers(), panelsPosted(g, w))

	if _, ok := w.routes["CASINO"]; ok {
		t.Fatal("the CASINO route must be removed")
	}
	heat, _ := g.byName("🗺️・heatmaps")
	if w.routes["HEATMAPS"] != heat.ID {
		t.Fatal("HEATMAPS must move to the V2 heatmaps channel")
	}
	combat, _ := g.byName("🔫・combat-feed")
	if w.routes["KILLFEED"] != combat.ID {
		t.Fatal("KILLFEED must move to the V2 combat-feed channel")
	}
	if len(g.channels) < before {
		t.Fatal("Champion must never delete Discord channels")
	}
	retired := map[string]bool{}
	for _, r := range res.Retirable {
		retired[r.ChannelID] = true
	}
	for _, id := range []string{"old-killfeed", "old-casino", "old-heatmaps"} {
		if !retired[id] {
			t.Fatalf("%s must be reported as retirable, got %+v", id, res.Retirable)
		}
	}
}

func TestApplyChannelLayoutBrokenProducerCreatesNoChannel(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{"BOUNTY": "stale", "BOUNTY_TRACKING": "stale"}}
	producers := auditProducers()
	producers["BOUNTY"] = routeProducer{HealthBroken, "bounty board is not running"}
	res := runLayout(t, g, w, producers, panelsPosted(g, w))

	if _, n := g.byName("💀・bounties"); n != 0 {
		t.Fatal("no bounties channel while the board is broken")
	}
	if _, ok := w.routes["BOUNTY_TRACKING"]; ok {
		t.Fatal("bounty routes must be unmapped with their destination")
	}
	if d := report(res, "BOUNTIES"); d.Health != HealthBroken || d.Detail != "bounty board is not running" || d.ChannelID != "" {
		t.Fatalf("want BROKEN bounties with the producer detail and no channel, got %+v", d)
	}
}

func TestApplyChannelLayoutReportsBlankOrUnwritableChannels(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{}}
	// Panels never appear, and the bot cannot write in one feed channel.
	res, err := applyChannelLayout(context.Background(), g, w, channelLayoutInput{GuildID: "g", Producers: auditProducers(), SyncPanels: func(context.Context) {
		if ch, n := g.byName("🎯・hitfeed"); n == 1 {
			g.missing[ch.ID] = []string{"SendMessages"}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if d := report(res, "PLAYER_LINK"); d.Health != HealthBroken || d.Checks.VisibleContent || !strings.Contains(d.Detail, "panel") {
		t.Fatalf("a panel channel without its panel must be BROKEN, got %+v", d)
	}
	hit := report(res, "HITFEED")
	if hit.Health != HealthBroken || hit.Checks.BotCanSend || hit.StarterSent || g.starters[hit.ChannelID] != 0 {
		t.Fatalf("an unwritable channel must be BROKEN with no starter attempt, got %+v", hit)
	}
}

func TestApplyChannelLayoutRequiresKillfeed(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{}}
	producers := auditProducers()
	producers["KILLFEED"] = routeProducer{HealthBroken, "down"}
	producers["PVE_FEED"] = routeProducer{HealthBroken, "down"}
	_, err := applyChannelLayout(context.Background(), g, w, channelLayoutInput{GuildID: "g", Producers: producers})
	if !errors.Is(err, errKillfeedUnavailable) {
		t.Fatalf("want errKillfeedUnavailable, got %v", err)
	}
	if g.createCalls != 0 || len(w.routes) != 0 {
		t.Fatal("nothing may be created or mapped when the killfeed cannot work")
	}
}

func TestApplyChannelLayoutNameRecoveryStaysInCategory(t *testing.T) {
	g := newLayoutGuildFake(
		discord.RawGuildChannel{ID: "live", Name: "🏆 CHAMPION • LIVE", Type: discordgo.ChannelTypeGuildCategory},
		discord.RawGuildChannel{ID: "mine", Name: "🔫・combat-feed", Type: discordgo.ChannelTypeGuildText, ParentID: "live"},
		discord.RawGuildChannel{ID: "elsewhere", Name: "🎯・hitfeed", Type: discordgo.ChannelTypeGuildText, ParentID: "other"},
	)
	w := &layoutRoutesFake{routes: map[string]string{}}
	runLayout(t, g, w, auditProducers(), panelsPosted(g, w))
	if w.routes["KILLFEED"] != "mine" {
		t.Fatalf("combat-feed under the LIVE category must be reused, got %q", w.routes["KILLFEED"])
	}
	if w.routes["HITFEED"] == "elsewhere" {
		t.Fatal("a same-named channel outside the category must never be adopted")
	}
}

func TestChannelRouteProducersDowngradeMissingRuntime(t *testing.T) {
	got := (&App{}).channelRouteProducers()
	for _, key := range []string{"KILLFEED", "BOUNTY", "HEATMAPS", "AUTO_LEADERBOARD", "LINK_GAMERTAG", "ECONOMY", "SHOP", "ADMIN_ALERTS"} {
		if got[key].Health != HealthBroken {
			t.Fatalf("%s must be BROKEN when its runtime is absent, got %+v", key, got[key])
		}
	}
	if got["BUILD_FEED"].Health != HealthBlocked || !strings.HasPrefix(got["BUILD_FEED"].Detail, detailSourceBlocked) {
		t.Fatalf("BUILD_FEED stays SOURCE_BLOCKED until a build line is parsed, got %+v", got["BUILD_FEED"])
	}

	// A parsed build line proves the source - but only with routing running.
	a := &App{}
	a.buildActionsSeen.Add(1)
	if got := a.channelRouteProducers()["BUILD_FEED"]; got.Health != HealthBlocked {
		t.Fatalf("without routing BUILD_FEED cannot be ACTIVE, got %+v", got)
	}
	a.ChannelRoutes = routing.NewResolver(nil, 0)
	if got := a.channelRouteProducers()["BUILD_FEED"]; got.Health != HealthActive {
		t.Fatalf("a parsed build line makes BUILD_FEED ACTIVE, got %+v", got)
	}
}

func TestApplyChannelLayoutKeepsCustomerRouteOfSkippedDestination(t *testing.T) {
	g := newLayoutGuildFake(discord.RawGuildChannel{ID: "mine", Name: "my-heat", Type: discordgo.ChannelTypeGuildText})
	w := &layoutRoutesFake{routes: map[string]string{}}
	existing := []repository.ChannelRoute{{RouteKey: "HEATMAPS", ChannelID: "mine", ManagedByChampion: false}}
	w.routes["HEATMAPS"] = "mine"
	producers := auditProducers()
	producers["HEATMAPS"] = routeProducer{HealthBroken, "heatmap publisher is not running"}
	res, err := applyChannelLayout(context.Background(), g, w, channelLayoutInput{GuildID: "g", Existing: existing, Producers: producers, SyncPanels: panelsPosted(g, w)})
	if err != nil {
		t.Fatal(err)
	}
	if w.routes["HEATMAPS"] != "mine" {
		t.Fatal("a customer-owned route must never be unmapped by auto-setup")
	}
	for _, r := range res.Retirable {
		if r.ChannelID == "mine" {
			t.Fatal("a customer channel must never be reported as retirable")
		}
	}
}
