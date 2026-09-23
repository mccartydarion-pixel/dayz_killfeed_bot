package app

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func runLayoutWith(t *testing.T, g *layoutGuildFake, w *layoutRoutesFake, existing []repository.ChannelRoute, preserve bool) *channelLayoutResult {
	t.Helper()
	res, err := applyChannelLayout(context.Background(), g, w, channelLayoutInput{OrganizationID: 1, InstallationID: 2, GuildID: "g", Existing: existing, Producers: auditProducers(), Preserve: preserve, SyncPanels: panelsPosted(g, w)})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestLayoutHubHasServerStatusAndVoiceCounter(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{}}
	res := runLayout(t, g, w, auditProducers(), panelsPosted(g, w))

	hub, _ := g.byName("🏆 CHAMPION • HUB")
	status, n := g.byName("📡・server-status")
	if n != 1 || status.ParentID != hub.ID || w.routes["SERVER_STATUS"] != status.ID {
		t.Fatalf("server-status must be one routed channel under HUB, got %+v", status)
	}
	if g.starters[status.ID] != 0 {
		t.Fatal("a persistent-panel channel never gets a starter card")
	}
	var counter discord.RawGuildChannel
	voices := 0
	for _, ch := range g.channels {
		if ch.Type == discordgo.ChannelTypeGuildVoice {
			counter = ch
			voices++
		}
	}
	if voices != 1 || counter.ParentID != hub.ID || !strings.HasPrefix(counter.Name, discord.ChannelOnlinePlayersPrefix) || w.routes["ONLINE_COUNTER"] != counter.ID {
		t.Fatalf("want exactly one routed voice counter under HUB, got %d: %+v", voices, counter)
	}
	if d := report(res, "ONLINE_COUNTER"); d.Health != HealthActive || !d.Voice {
		t.Fatalf("voice counter report: %+v", d)
	}
	// Death feed and PvE share the combat feed: no separate channels.
	for _, name := range []string{"death-feed", "☠️・death-feed", "pvefeed"} {
		if _, n := g.byName(name); n != 0 {
			t.Fatalf("%s must not exist in V2", name)
		}
	}

	// A renamed counter ("... : 17") is still recognized on the next run.
	for i := range g.channels {
		if g.channels[i].ID == counter.ID {
			g.channels[i].Name = discord.OnlineCounterName(17)
		}
	}
	creates := g.createCalls
	runLayout(t, g, w, auditProducers(), panelsPosted(g, w))
	if g.createCalls != creates {
		t.Fatal("the renamed voice counter must be reused, never duplicated")
	}
}

func TestRepairPreservesCustomerRoutes(t *testing.T) {
	g := newLayoutGuildFake(
		discord.RawGuildChannel{ID: "my-feed", Name: "my-feed", Type: discordgo.ChannelTypeGuildText},
		discord.RawGuildChannel{ID: "my-hits", Name: "my-hits", Type: discordgo.ChannelTypeGuildText},
	)
	w := &layoutRoutesFake{routes: map[string]string{"KILLFEED": "my-feed", "HITFEED": "my-hits"}}
	existing := []repository.ChannelRoute{
		{RouteKey: "KILLFEED", ChannelID: "my-feed", ManagedByChampion: false},
		{RouteKey: "HITFEED", ChannelID: "my-hits", ManagedByChampion: false},
	}
	res := runLayoutWith(t, g, w, existing, true)

	if w.routes["KILLFEED"] != "my-feed" || w.routes["HITFEED"] != "my-hits" {
		t.Fatalf("customer routes must be untouched, got %v", w.routes)
	}
	if _, n := g.byName("🎯・hitfeed"); n != 0 {
		t.Fatal("a destination served entirely by a customer channel needs no Champion channel")
	}
	combat, n := g.byName("🔫・combat-feed")
	if n != 1 || w.routes["PVE_FEED"] != combat.ID {
		t.Fatal("the Champion-managed part of a destination still gets the V2 channel")
	}
	if g.starters["my-feed"] != 0 || g.starters["my-hits"] != 0 {
		t.Fatal("Champion never posts into a customer's channel")
	}
	sort.Strings(res.Summary.Preserved)
	if strings.Join(res.Summary.Preserved, ",") != "HITFEED,KILLFEED" {
		t.Fatalf("preserved = %v", res.Summary.Preserved)
	}
	if hit := report(res, "HITFEED"); hit.ChannelID != "my-hits" || hit.Created {
		t.Fatalf("hitfeed report must point at the customer channel: %+v", hit)
	}
	for _, r := range res.Retirable {
		if r.ChannelID == "my-feed" || r.ChannelID == "my-hits" {
			t.Fatal("customer channels are never retirable")
		}
	}
}

func TestRepairIsIdempotent(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{}}
	runLayoutWith(t, g, w, w.existing(), true)
	creates := g.createCalls
	second := runLayoutWith(t, g, w, w.existing(), true)
	s := second.Summary
	if g.createCalls != creates || len(s.Created) != 0 || len(s.Remapped) != 0 || len(s.StartersSent) != 0 || len(s.PanelsRepaired) != 0 || len(s.Broken) != 0 {
		t.Fatalf("a second repair must change nothing, got %+v", s)
	}
	if len(s.Reused) == 0 {
		t.Fatal("the second repair reuses every channel")
	}
}

func TestRepairReportsRepairedPanel(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{}}
	runLayoutWith(t, g, w, w.existing(), true)
	board, _ := g.byName("📊・leaderboards")
	delete(g.botMessages, board.ID) // a moderator deleted the panel
	res := runLayoutWith(t, g, w, w.existing(), true)
	if len(res.Summary.PanelsRepaired) != 1 || res.Summary.PanelsRepaired[0] != "LEADERBOARDS" {
		t.Fatalf("want the leaderboard panel reported repaired, got %v", res.Summary.PanelsRepaired)
	}
	if g.starters[board.ID] != 0 {
		t.Fatal("a panel channel is repaired with its panel, never a starter card")
	}
}

func TestLayoutPartialDiscordFailureRecoversWithoutDuplicates(t *testing.T) {
	g := newLayoutGuildFake()
	g.failCreate = "🎯・hitfeed"
	w := &layoutRoutesFake{routes: map[string]string{}}
	if _, err := applyChannelLayout(context.Background(), g, w, channelLayoutInput{GuildID: "g", Existing: w.existing(), Producers: auditProducers()}); err == nil {
		t.Fatal("a failed channel create must be reported")
	}
	if len(g.channels) == 0 {
		t.Fatal("expected the channels created before the failure to remain (nothing is rolled back by deleting)")
	}
	g.failCreate = ""
	runLayout(t, g, w, auditProducers(), panelsPosted(g, w))
	seen := map[string]int{}
	for _, ch := range g.channels {
		seen[ch.Name]++
		if seen[ch.Name] > 1 {
			t.Fatalf("retry duplicated %q", ch.Name)
		}
	}
	if _, n := g.byName("🎯・hitfeed"); n != 1 {
		t.Fatal("the retry completes the layout")
	}
}

func TestInspectChannelLayoutIsReadOnly(t *testing.T) {
	g := newLayoutGuildFake()
	w := &layoutRoutesFake{routes: map[string]string{}}

	before, err := inspectChannelLayout(g, "g", nil, auditProducers())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range before {
		if d.Health == HealthActive {
			t.Fatalf("nothing is set up yet, %s cannot be ACTIVE", d.Key)
		}
	}
	if g.createCalls != 0 || len(g.starters) != 0 {
		t.Fatal("inspection must never create or post")
	}

	runLayout(t, g, w, auditProducers(), panelsPosted(g, w))
	after, err := inspectChannelLayout(g, "g", w.existing(), auditProducers())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range after {
		if d.Health != HealthActive {
			t.Fatalf("%s want ACTIVE after setup, got %s (%s)", d.Key, d.Health, d.Detail)
		}
	}

	// A channel deleted in Discord shows up as BROKEN.
	hit, _ := g.byName("🎯・hitfeed")
	var kept []discord.RawGuildChannel
	for _, ch := range g.channels {
		if ch.ID != hit.ID {
			kept = append(kept, ch)
		}
	}
	g.channels = kept
	again, _ := inspectChannelLayout(g, "g", w.existing(), auditProducers())
	for _, d := range again {
		if d.Key == "HITFEED" && (d.Health != HealthBroken || !strings.Contains(d.Detail, "no longer exists")) {
			t.Fatalf("a deleted channel must be BROKEN, got %+v", d)
		}
	}
}

func TestLegacyRetirablesOnlyReplacedChampionChannels(t *testing.T) {
	gs := &discord.GuildSetup{
		CategoryID:          "legacy-cat",
		WelcomeChannelID:    "welcome",
		KillfeedChannelID:   "combat", // already the V2 channel
		DeathChannelID:      "old-death",
		ADMMonitorChannelID: "old-adm",
		LinkPanelChannelID:  "old-link",
	}
	routes := map[string]ChannelRouteInfo{"KILLFEED": {ChannelID: "combat"}, "ADMIN_LOGS": {ChannelID: "admin-logs"}}
	got := legacyRetirablesFor(gs, routes, map[string]bool{"live": true})
	ids := map[string]repository.RetiredChannel{}
	for _, r := range got {
		ids[r.ChannelID] = r
	}
	if ids["old-death"].LegacyField != "DeathChannelID" || ids["old-death"].Source != "LEGACY_SETUP" {
		t.Fatalf("the legacy death feed is replaced by combat-feed: %+v", got)
	}
	if _, ok := ids["old-adm"]; !ok {
		t.Fatal("the legacy ADM monitor is replaced by admin-logs")
	}
	for _, id := range []string{"combat", "welcome", "old-link"} {
		if _, ok := ids[id]; ok {
			t.Fatalf("%s must not be retirable (in use, no replacement, or not routed)", id)
		}
	}
	if ids["legacy-cat"].Kind != "CATEGORY" {
		t.Fatal("the legacy category is retirable once V2 categories exist")
	}
}

// retiredStoreFake is an in-memory retiredChannelStore.
type retiredStoreFake struct {
	rows       []repository.RetiredChannel
	referenced map[string]bool
	forgotten  []string
}

func (f *retiredStoreFake) List(context.Context, int64, int64) ([]repository.RetiredChannel, error) {
	return f.rows, nil
}
func (f *retiredStoreFake) Forget(_ context.Context, _, _ int64, id string) error {
	f.forgotten = append(f.forgotten, id)
	return nil
}
func (f *retiredStoreFake) ChannelReferenced(_ context.Context, id string) (bool, error) {
	return f.referenced[id], nil
}

type cleanupGuildFake struct {
	channels []discord.RawGuildChannel
	deleted  []string
}

func (f *cleanupGuildFake) ListAllGuildChannels(string) ([]discord.RawGuildChannel, error) {
	return f.channels, nil
}
func (f *cleanupGuildFake) DeleteGuildChannel(id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func TestCleanupDeletesOnlyProvenRetiredChannels(t *testing.T) {
	store := &retiredStoreFake{
		rows: []repository.RetiredChannel{
			{ChannelID: "old-casino", Kind: "CHANNEL", Source: "ROUTE"},
			{ChannelID: "old-death", Kind: "CHANNEL", Source: "LEGACY_SETUP", LegacyField: "DeathChannelID"},
			{ChannelID: "reused", Kind: "CHANNEL", Source: "ROUTE"},
			{ChannelID: "old-cat", Kind: "CATEGORY", Source: "ROUTE"},
			{ChannelID: "busy-cat", Kind: "CATEGORY", Source: "LEGACY_SETUP", LegacyField: "CategoryID"},
			{ChannelID: "mislabeled", Kind: "CATEGORY", Source: "ROUTE"},
		},
		referenced: map[string]bool{"reused": true},
	}
	guild := &cleanupGuildFake{channels: []discord.RawGuildChannel{
		{ID: "old-cat", Type: discordgo.ChannelTypeGuildCategory},
		{ID: "old-casino", Name: "casino", Type: discordgo.ChannelTypeGuildText, ParentID: "old-cat"},
		{ID: "old-death", Name: "death-feed", Type: discordgo.ChannelTypeGuildText},
		{ID: "reused", Type: discordgo.ChannelTypeGuildText},
		{ID: "busy-cat", Type: discordgo.ChannelTypeGuildCategory},
		{ID: "customer-general", Name: "general", Type: discordgo.ChannelTypeGuildText, ParentID: "busy-cat"},
		{ID: "mislabeled", Type: discordgo.ChannelTypeGuildText},
	}}
	ids := []string{"old-cat", "old-casino", "old-death", "reused", "busy-cat", "customer-general", "mislabeled", "old-casino"}
	resp, clear, err := cleanupRetired(context.Background(), store, guild, 1, 2, "g", ids)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(guild.deleted)
	if strings.Join(guild.deleted, ",") != "old-casino,old-cat,old-death" {
		t.Fatalf("deleted %v", guild.deleted)
	}
	reasons := map[string]string{}
	for _, s := range resp.Skipped {
		reasons[s.ChannelID] = s.Reason
	}
	want := map[string]string{"reused": "REFERENCED", "busy-cat": "NOT_EMPTY", "customer-general": "NOT_RETIRABLE", "mislabeled": "NOT_RETIRABLE"}
	for id, reason := range want {
		if reasons[id] != reason {
			t.Fatalf("%s: want %s, got %q (all: %v)", id, reason, reasons[id], reasons)
		}
	}
	if len(clear) != 1 || clear[0] != "DeathChannelID" {
		t.Fatalf("the deleted legacy channel's field must be cleared, got %v", clear)
	}
}
