package discord

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/heatmap"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The panels and feeds serve every guild with an active server, each on its
// own routes: guild 7 (the primary guild of the fixture, servers 1 and 2) and
// guild 8 (a customer's guild, server 3) each get their own panel, and an
// event of one guild never reaches the other guild's channel.

const customerGuild = int64(8)

func twoGuilds(context.Context) ([]GuildServers, error) {
	return []GuildServers{{GuildRowID: 7, ServerIDs: []int64{1, 2}}, {GuildRowID: customerGuild, ServerIDs: []int64{3}}}, nil
}

// guildStats answers every board with one entry named after the guild asked for.
type guildStats struct{}

func (guildStats) entry(guildID int64) []repository.LeaderboardEntry {
	return []repository.LeaderboardEntry{{DisplayName: "player-of-guild-" + strconv.FormatInt(guildID, 10), Value: "1"}}
}
func (s guildStats) TopByKills(_ context.Context, g int64, _ int) ([]repository.LeaderboardEntry, error) {
	return s.entry(g), nil
}
func (s guildStats) TopByBestStreak(_ context.Context, g int64, _ int) ([]repository.LeaderboardEntry, error) {
	return s.entry(g), nil
}
func (s guildStats) TopByDeaths(_ context.Context, g int64, _ int) ([]repository.LeaderboardEntry, error) {
	return s.entry(g), nil
}
func (s guildStats) TopLongestKill(_ context.Context, g int64, _ int) ([]repository.LeaderboardEntry, error) {
	return s.entry(g), nil
}

func embedsText(embeds []*discordgo.MessageEmbed) string {
	var sb strings.Builder
	for _, e := range embeds {
		if e != nil {
			sb.WriteString(embedText(e))
		}
	}
	return sb.String()
}

func TestRouteSyncerServesEveryGuild(t *testing.T) {
	f := newRoutedFixture(t)
	restores := 0
	syncer := NewRouteSyncer(f.resolver, twoGuilds, f.panels, f.api, f.setup, "g1", f.guild)
	syncer.SetLegacyRestore(func() { restores++ })
	_ = f.setup.Save(GuildSetup{GuildID: "g1", LinkPanelChannelID: "legacy-link", LinkPanelMessageID: "legacy-link-msg"})
	f.api.seed("legacy-link", "legacy-link-msg")
	ctx := context.Background()

	// Only the customer guild has a route: its panel appears, and the primary
	// guild's legacy panel is left alone (it is not in route mode).
	f.resolver.set(customerGuild, 3, routeKeyLinkGamertag, "link-8")
	syncer.SyncOnce(ctx)
	if f.api.liveIn("link-8") != 1 || f.store.messageFor(customerGuild, routeKeyLinkGamertag, "link-8") == "" {
		t.Fatalf("customer guild must get its own link panel, live=%v", f.api.live())
	}
	if gs, _ := f.setup.Get("g1"); gs.LinkPanelMessageID != "legacy-link-msg" || f.api.liveIn("legacy-link") != 1 {
		t.Fatalf("another guild's route must not retire the primary guild's legacy panel: %+v", gs)
	}

	// The primary guild joins route mode: its own panel, its legacy retired.
	f.resolver.set(f.guild, 1, routeKeyLinkGamertag, "link-7")
	syncer.SyncOnce(ctx)
	if f.api.liveIn("link-7") != 1 || f.store.messageFor(f.guild, routeKeyLinkGamertag, "link-7") == "" {
		t.Fatalf("primary guild must get its own link panel, live=%v", f.api.live())
	}
	if gs, _ := f.setup.Get("g1"); gs.LinkPanelMessageID != "" || f.api.liveIn("legacy-link") != 0 {
		t.Fatalf("primary guild's legacy panel must be retired in route mode: %+v", gs)
	}

	// The customer guild drops its route: its panel is retired, the primary
	// guild's stays, and no legacy restore runs (it has no legacy panel).
	f.resolver.clear(customerGuild, 3, routeKeyLinkGamertag)
	syncer.SyncOnce(ctx)
	if f.api.liveIn("link-8") != 0 || f.api.liveIn("link-7") != 1 || restores != 0 {
		t.Fatalf("retiring one guild's panel must not touch the other, live=%v restores=%d", f.api.live(), restores)
	}
}

// countingLeaderboard counts the refreshes the syncer asks for.
type countingLeaderboard struct{ refreshes int }

func (c *countingLeaderboard) RefreshOnce(context.Context) error {
	c.refreshes++
	return nil
}

// A guild that connects its first server after startup (the website flow) is
// refreshed as soon as the syncer sees its routes, not on the next 3-hour tick.
func TestRouteSyncerRefreshesLeaderboardForGuildSeenAfterBoot(t *testing.T) {
	f := newRoutedFixture(t)
	guilds := []GuildServers{{GuildRowID: 7, ServerIDs: []int64{1, 2}}}
	syncer := NewRouteSyncer(f.resolver, func(context.Context) ([]GuildServers, error) { return guilds, nil }, f.panels, f.api, f.setup, "g1", f.guild)
	lb := &countingLeaderboard{}
	syncer.SetLeaderboard(lb)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "lb-7")
	ctx := context.Background()
	syncer.SyncOnce(ctx) // startup: the scheduler's own refresh covers guild 7
	if lb.refreshes != 0 {
		t.Fatalf("startup must only record the routes, refreshes=%d", lb.refreshes)
	}
	guilds = append(guilds, GuildServers{GuildRowID: customerGuild, ServerIDs: []int64{3}})
	f.resolver.set(customerGuild, 3, routeKeyAutoLeaderboard, "lb-8")
	syncer.SyncOnce(ctx)
	if lb.refreshes != 1 {
		t.Fatalf("a guild seen after boot with a route must be refreshed once, refreshes=%d", lb.refreshes)
	}
	syncer.SyncOnce(ctx)
	if lb.refreshes != 1 {
		t.Fatalf("unchanged routes must not refresh again, refreshes=%d", lb.refreshes)
	}
}

func TestLeaderboardSchedulerServesEveryGuild(t *testing.T) {
	f := newRoutedFixture(t)
	panel := NewLeaderboardPanel(f.api, "legacy-chan", "", DefaultLeaderboardConfig())
	s := NewLeaderboardScheduler(panel, guildStats{}, f.guild, DefaultLeaderboardConfig(), func(string) {})
	s.SetRouting(f.resolver, twoGuilds, f.panels, nil)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "lb-7")
	f.resolver.set(customerGuild, 3, routeKeyAutoLeaderboard, "lb-8")
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.api.liveIn("lb-7") != 1 || f.api.liveIn("lb-8") != 1 || f.api.liveIn("legacy-chan") != 0 {
		t.Fatalf("one leaderboard per guild, none on the legacy channel, live=%v", f.api.live())
	}
	if f.store.messageFor(f.guild, routeKeyAutoLeaderboard, "lb-7") == "" || f.store.messageFor(customerGuild, routeKeyAutoLeaderboard, "lb-8") == "" {
		t.Fatal("each leaderboard must be recorded under its own guild")
	}
	got8 := embedsText(f.api.embedsIn("lb-8"))
	if !strings.Contains(got8, "player-of-guild-8") || strings.Contains(got8, "player-of-guild-7") {
		t.Fatalf("the customer guild's board must show its own stats only:\n%s", got8)
	}
	if got7 := embedsText(f.api.embedsIn("lb-7")); !strings.Contains(got7, "player-of-guild-7") || strings.Contains(got7, "player-of-guild-8") {
		t.Fatalf("the primary guild's board must show its own stats only:\n%s", got7)
	}
	// A refresh with the same data edits both in place.
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.api.sendCount() != 2 {
		t.Fatalf("a refresh must edit, not repost, sends=%d", f.api.sendCount())
	}
}

func TestBountyBoardServesEveryGuild(t *testing.T) {
	f := newRoutedFixture(t)
	lister := &fakeBoardLister{}
	lister.set(boardRow{server: 1, target: "Victim7", amount: 100}, boardRow{server: 3, target: "Victim8", amount: 200})
	board := NewBountyBoard(f.resolver, twoGuilds, f.panels, lister)
	f.resolver.set(f.guild, 1, routeKeyBounty, "board-7")
	f.resolver.set(customerGuild, 3, routeKeyBounty, "board-8")
	board.SyncOnce(context.Background())
	if f.api.liveIn("board-7") != 1 || f.api.liveIn("board-8") != 1 {
		t.Fatalf("one board per guild, live=%v", f.api.live())
	}
	f.api.mu.Lock()
	text7, text8 := f.api.lastEmbed["board-7"], f.api.lastEmbed["board-8"]
	f.api.mu.Unlock()
	if !strings.Contains(text8, "Victim8") || strings.Contains(text8, "Victim7") {
		t.Fatalf("the customer guild's board must list its own bounties only:\n%s", text8)
	}
	if !strings.Contains(text7, "Victim7") || strings.Contains(text7, "Victim8") {
		t.Fatalf("the primary guild's board must list its own bounties only:\n%s", text7)
	}
}

func TestBountyTrackerGuildWideEventStaysInItsGuild(t *testing.T) {
	res := newKeyedResolver()
	sender := &fakeHitSender{fail: map[string]error{}}
	tr := NewBountyTracker(sender, res, twoGuilds)
	res.set(7, 1, routeKeyBountyTracking, "track-7")
	res.set(customerGuild, 3, routeKeyBountyTracking, "track-8")
	tr.Notify(bounties.Event{Kind: bounties.EventExpired, GuildID: customerGuild, Target: "Victim8", Amount: 5})
	tr.Flush()
	if len(sender.messages("track-8")) != 1 || len(sender.messages("track-7")) != 0 {
		t.Fatalf("a guild-wide event goes to its own guild's route only: track-8=%d track-7=%d", len(sender.messages("track-8")), len(sender.messages("track-7")))
	}
}

func TestEconomyFeedGuildWideEventStaysInItsGuild(t *testing.T) {
	res := newKeyedResolver()
	sender := &fakeHitSender{fail: map[string]error{}}
	feed := NewEconomyFeed(sender, res, twoGuilds)
	res.set(7, 1, routeKeyEconomy, "eco-7")
	res.set(customerGuild, 3, routeKeyEconomy, "eco-8")
	feed.Notify(economy.Event{Type: economy.TypeAdminCredit, GuildID: customerGuild, PlayerName: "PlayerB", Amount: 10, Credit: true})
	feed.Flush()
	if len(sender.messages("eco-8")) != 1 || len(sender.messages("eco-7")) != 0 {
		t.Fatalf("a guild-wide event goes to its own guild's route only: eco-8=%d eco-7=%d", len(sender.messages("eco-8")), len(sender.messages("eco-7")))
	}
}

func TestCompletionPublisherRoutesByTheEventsGuild(t *testing.T) {
	ctx := context.Background()
	routes := newKeyedResolver()
	p := &LiveCompletionPublisher{}
	p.SetRouting(routes, twoGuilds)
	routes.set(7, 1, routeKeyServerStatus, "status-7")
	if got := p.routedChannel(ctx, customerGuild, routeKeyServerStatus); got != "" {
		t.Fatalf("a guild without a route must not borrow another guild's channel: %q", got)
	}
	routes.set(customerGuild, 3, routeKeyServerStatus, "status-8")
	if got := p.routedChannel(ctx, customerGuild, routeKeyEvents, routeKeyServerStatus); got != "status-8" {
		t.Fatalf("the customer guild's announcements go to its own route: %q", got)
	}
	if got := p.routedChannel(ctx, 7, routeKeyServerStatus); got != "status-7" {
		t.Fatalf("the primary guild keeps its own route: %q", got)
	}
}

func TestHeatmapBoardServesEveryGuild(t *testing.T) {
	f := newRoutedFixture(t)
	hm := &heatmapFake{results: map[int64]*heatmap.Result{}}
	b := NewHeatmapBoard(f.resolver, twoGuilds, f.panels, hm, 30*time.Minute)
	f.resolver.set(f.guild, 1, routeKeyHeatmaps, "heat-7")
	f.resolver.set(customerGuild, 3, routeKeyHeatmaps, "heat-8")
	b.SyncOnce(context.Background())
	if f.api.liveIn("heat-7") != 1 || f.api.liveIn("heat-8") != 1 {
		t.Fatalf("one heatmap summary per guild, live=%v", f.api.live())
	}
	if f.store.messageFor(customerGuild, routeKeyHeatmaps, "heat-8") == "" {
		t.Fatal("the customer guild's summary must be recorded under its own guild")
	}
	for _, q := range hm.queries {
		if (q.ServerID == 3) != (q.GuildID == customerGuild) {
			t.Fatalf("every aggregate query must carry its own guild: %+v", q)
		}
	}
}

func TestServerStatusBoardServesEveryGuild(t *testing.T) {
	f := newRoutedFixture(t)
	b := NewServerStatusBoard(f.resolver, twoGuilds, f.panels)
	f.resolver.set(f.guild, 1, routeKeyServerStatus, "status-7")
	f.resolver.set(customerGuild, 3, routeKeyServerStatus, "status-8")
	b.SyncOnce(context.Background())
	if f.api.liveIn("status-7") != 1 || f.api.liveIn("status-8") != 1 {
		t.Fatalf("one status message per guild, live=%v", f.api.live())
	}
	if f.store.messageFor(customerGuild, routeKeyServerStatus, "status-8") == "" || f.store.messageFor(f.guild, routeKeyServerStatus, "status-7") == "" {
		t.Fatal("each status message must be recorded under its own guild")
	}
}

// failingGuildLeaderboard fails every refresh for one guild only.
type failingGuildLeaderboard struct {
	refreshes int
	bad       int64
}

func (f *failingGuildLeaderboard) RefreshOnce(context.Context) error {
	f.refreshes++
	return errors.New("guild board failed")
}
func (f *failingGuildLeaderboard) RefreshFailed(guildRowID int64) bool { return guildRowID == f.bad }

// One guild whose board fails on every refresh keeps being retried, but a guild whose board went
// up is recorded: it no longer makes the syncer refresh every guild's board on every pass.
func TestRouteSyncerRecordsGuildsWhoseBoardWentUp(t *testing.T) {
	f := newRoutedFixture(t)
	guilds := []GuildServers{{GuildRowID: 7, ServerIDs: []int64{1, 2}}}
	syncer := NewRouteSyncer(f.resolver, func(context.Context) ([]GuildServers, error) { return guilds, nil }, f.panels, f.api, f.setup, "g1", f.guild)
	lb := &failingGuildLeaderboard{bad: 99}
	syncer.SetLeaderboard(lb)
	ctx := context.Background()
	syncer.SyncOnce(ctx) // startup
	guilds = append(guilds, GuildServers{GuildRowID: customerGuild, ServerIDs: []int64{3}}, GuildServers{GuildRowID: 99, ServerIDs: []int64{9}})
	f.resolver.set(customerGuild, 3, routeKeyAutoLeaderboard, "lb-8")
	f.resolver.set(99, 9, routeKeyAutoLeaderboard, "lb-99")
	syncer.SyncOnce(ctx)
	if lb.refreshes != 1 {
		t.Fatalf("two new guilds share one refresh, refreshes=%d", lb.refreshes)
	}
	if st := syncer.stateFor(customerGuild, routeKeyAutoLeaderboard); st.sig != "lb-8" {
		t.Fatalf("the guild whose board went up is recorded, sig=%q", st.sig)
	}
	if st := syncer.stateFor(99, routeKeyAutoLeaderboard); st.sig == "lb-99" {
		t.Fatal("the failing guild is not recorded, so it is retried")
	}
	syncer.SyncOnce(ctx)
	if lb.refreshes != 2 {
		t.Fatalf("the failing guild is retried, refreshes=%d", lb.refreshes)
	}
}
