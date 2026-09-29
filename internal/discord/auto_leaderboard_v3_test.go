package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// --- presentation ---------------------------------------------------------------

var autoBoardOrder = []string{AutoLeaderboardTitle, AutoBoardKillsTitle, AutoBoardStreaksTitle, AutoBoardRanksTitle, AutoBoardDeathsTitle, AutoBoardLongestTitle}

func TestAutoLeaderboardIsSixEmbedsInExactOrder(t *testing.T) {
	embeds := BuildAutoLeaderboardEmbeds(FixtureAutoLeaderboardSnapshot(), DefaultLeaderboardConfig())
	if len(embeds) != 6 {
		t.Fatalf("expected 6 embeds, got %d", len(embeds))
	}
	for i, want := range autoBoardOrder {
		if embeds[i].Title != want {
			t.Fatalf("embed %d = %q, want %q", i, embeds[i].Title, want)
		}
	}
	assertPackageWithinLimits(t, embeds)
}

func TestAutoLeaderboardRanksInactiveWithoutAuthoritativeSource(t *testing.T) {
	s := FixtureAutoLeaderboardSnapshot()
	s.RanksEnabled = false // production today: no rank system wired
	embeds := BuildAutoLeaderboardEmbeds(s, DefaultLeaderboardConfig())
	want := []string{AutoLeaderboardTitle, AutoBoardKillsTitle, AutoBoardStreaksTitle, AutoBoardDeathsTitle, AutoBoardLongestTitle}
	if len(embeds) != len(want) {
		t.Fatalf("expected %d embeds without a rank source, got %d", len(want), len(embeds))
	}
	for i := range want {
		if embeds[i].Title != want[i] {
			t.Fatalf("embed %d = %q, want %q", i, embeds[i].Title, want[i])
		}
	}
	for _, e := range embeds {
		if strings.Contains(e.Title, "Rank") {
			t.Fatal("a Ranks board must never render without an authoritative rank source")
		}
	}
}

func TestAutoLeaderboardHeaderIsCompact(t *testing.T) {
	h := BuildAutoLeaderboardEmbeds(FixtureAutoLeaderboardSnapshot(), DefaultLeaderboardConfig())[0]
	if h.Author == nil || h.Author.Name != presentation.AuthorName || h.Footer == nil || h.Footer.Text != presentation.FooterAutoRefresh {
		t.Fatalf("header branding: %#v %#v", h.Author, h.Footer)
	}
	if h.Color != presentation.ChampionGold || len(h.Fields) != 0 {
		t.Fatalf("header must be compact gold: color=%X fields=%d", h.Color, len(h.Fields))
	}
	want := "**Champions Deathmatch**\nLast Updated <t:1790160000:R>\nAuto Refresh • Every 3 Hours"
	if h.Description != want {
		t.Fatalf("header description:\n%q\nwant\n%q", h.Description, want)
	}
	if h.Timestamp != "" {
		t.Fatal("the refresh time is the relative <t:R> line, not the embed timestamp")
	}

	anon := BuildAutoLeaderboardEmbeds(LeaderboardSnapshot{}, DefaultLeaderboardConfig())[0]
	if anon.Description != "Auto Refresh • Every 3 Hours" {
		t.Fatalf("no server name / time: only the cadence line, got %q", anon.Description)
	}
	evil := BuildAutoLeaderboardEmbeds(LeaderboardSnapshot{ServerName: "@everyone **x**"}, DefaultLeaderboardConfig())[0]
	if strings.Contains(evil.Description, "@everyone") || strings.Contains(evil.Description, "**x**") {
		t.Fatalf("server name must be sanitized: %q", evil.Description)
	}
}

func TestAutoLeaderboardRankingEmbedsAreFifteenInlineCells(t *testing.T) {
	embeds := BuildAutoLeaderboardEmbeds(FixtureAutoLeaderboardSnapshot(), DefaultLeaderboardConfig())
	for _, e := range embeds[1:] {
		if len(e.Fields) != 15 {
			t.Fatalf("%s: expected 15 fields, got %d", e.Title, len(e.Fields))
		}
		for i, f := range e.Fields {
			if !f.Inline {
				t.Fatalf("%s field %d must be inline (3-column grid)", e.Title, i)
			}
			if strings.Contains(f.Value, "\n") {
				t.Fatalf("%s field %d is a multi-line block, not one player: %q", e.Title, i, f.Value)
			}
		}
		if e.Author != nil || e.Footer != nil {
			t.Fatalf("%s: ranking embeds rely on their title (no repeated branding)", e.Title)
		}
	}
}

func TestAutoLeaderboardCategoryValues(t *testing.T) {
	embeds := BuildAutoLeaderboardEmbeds(FixtureAutoLeaderboardSnapshot(), DefaultLeaderboardConfig())
	cases := []struct {
		idx          int
		first, last  string
		name1, name4 string
	}{
		{1, "6,053 Kills", "1 Kill", "🥇 PlayerOne", "#4 PlayerFour"},
		{2, "27 Kill Streak", "1 Kill Streak", "🥇 PlayerOne", "#4 PlayerFour"},
		{3, "Diamond III", "Bronze I", "🥇 PlayerOne", "#4 PlayerFour"},
		{4, "5,012 Deaths", "1 Death", "🥇 PlayerOne", "#4 PlayerFour"},
		{5, "1,104.2m", "12.0m", "🥇 PlayerOne", "#4 PlayerFour"},
	}
	for _, c := range cases {
		f := embeds[c.idx].Fields
		if f[0].Value != c.first || f[14].Value != c.last {
			t.Errorf("%s: values %q / %q, want %q / %q", embeds[c.idx].Title, f[0].Value, f[14].Value, c.first, c.last)
		}
		if f[0].Name != c.name1 || f[3].Name != c.name4 {
			t.Errorf("%s: names %q / %q", embeds[c.idx].Title, f[0].Name, f[3].Name)
		}
	}
	if embeds[5].Fields[1].Value != "341.8m" || embeds[5].Fields[4].Value != "215.0m" {
		t.Errorf("longest precision: %q %q", embeds[5].Fields[1].Value, embeds[5].Fields[4].Value)
	}
	for _, e := range embeds {
		text := allText(e)
		for _, bad := range []string{"Value", "Kill(s)", "Death(s)"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s must not contain %q", e.Title, bad)
			}
		}
	}
}

func TestAutoLeaderboardMedalsOnlyOnPodium(t *testing.T) {
	embeds := BuildAutoLeaderboardEmbeds(FixtureAutoLeaderboardSnapshot(), DefaultLeaderboardConfig())
	for _, e := range embeds[1:] {
		for i, prefix := range []string{"🥇 ", "🥈 ", "🥉 "} {
			if !strings.HasPrefix(e.Fields[i].Name, prefix) {
				t.Fatalf("%s rank %d = %q, want %q", e.Title, i+1, e.Fields[i].Name, prefix)
			}
		}
		for i := 3; i < 15; i++ {
			name := e.Fields[i].Name
			if !strings.HasPrefix(name, fmt.Sprintf("#%d ", i+1)) || strings.ContainsAny(name, "🥇🥈🥉") {
				t.Fatalf("%s rank %d = %q", e.Title, i+1, name)
			}
		}
	}
}

func TestAutoLeaderboardEntryCounts(t *testing.T) {
	entries := func(n int) []repository.LeaderboardEntry {
		out := make([]repository.LeaderboardEntry, n)
		for i := range out {
			out[i] = repository.LeaderboardEntry{DisplayName: fmt.Sprintf("P%d", i+1), Value: fmt.Sprint(100 - i)}
		}
		return out
	}
	for _, n := range []int{0, 1, 7, 15, 20} {
		e := entries(n)
		ranks := make([]RankEntry, n)
		for i := range ranks {
			ranks[i] = RankEntry{DisplayName: fmt.Sprintf("P%d", i+1), Rank: "Tier"}
		}
		s := LeaderboardSnapshot{TopKills: e, TopStreaks: e, TopDeaths: e, TopLongest: e, CurrentRanks: ranks, RanksEnabled: true}
		embeds := BuildAutoLeaderboardEmbeds(s, DefaultLeaderboardConfig())
		if len(embeds) != 6 {
			t.Fatalf("n=%d: every category keeps its embed, got %d", n, len(embeds))
		}
		want := n
		if want > 15 {
			want = 15
		}
		for _, emb := range embeds[1:] {
			if len(emb.Fields) != want {
				t.Fatalf("n=%d %s: %d fields, want %d (no blank placeholders, top 15 only)", n, emb.Title, len(emb.Fields), want)
			}
			if n == 0 && emb.Description != presentation.EmptyBoard {
				t.Fatalf("empty %s must say %q, got %q", emb.Title, presentation.EmptyBoard, emb.Description)
			}
			if n > 0 {
				if emb.Title == AutoBoardRanksTitle {
					if emb.Description != "Selected public server • active Ranked season" {
						t.Fatalf("rank scope description: %q", emb.Description)
					}
				} else if emb.Description != "" {
					t.Fatalf("%s: no empty-state line when players qualify", emb.Title)
				}
			}
		}
		assertPackageWithinLimits(t, embeds)
	}
}

func TestAutoLeaderboardNamesAreSafe(t *testing.T) {
	bad := []string{"@everyone", "@here", "<@&123>", "<#123>", "**name**", strings.Repeat("N", 200), "a‮b​c\x00d"}
	var s LeaderboardSnapshot
	for i, n := range bad {
		s.TopKills = append(s.TopKills, repository.LeaderboardEntry{DisplayName: n, Value: fmt.Sprint(10 - i)})
	}
	e := BuildAutoLeaderboardEmbeds(s, DefaultLeaderboardConfig())[1]
	text := allText(e)
	for _, leak := range []string{"@everyone", "@here", "<@&", "<#", "**name**", "‮", "​", "\x00"} {
		if strings.Contains(text, leak) {
			t.Fatalf("unsafe text %q rendered: %q", leak, text)
		}
	}
	if strings.Contains(text, strings.Repeat("N", presentation.MaxRankNameRunes+1)) {
		t.Fatal("names must be capped so they cannot wreck the grid")
	}
	// Normal PSN tags (<= 16) are never truncated.
	ok := BuildAutoLeaderboardEmbeds(FixtureAutoLeaderboardSnapshot(), DefaultLeaderboardConfig())[1]
	if !strings.Contains(allText(ok), `Semillita-azul-\_`) || strings.Contains(allText(ok), "…") {
		t.Fatal("ordinary gamertags must render whole")
	}
}

func TestAutoLeaderboardPathologicalDataFitsOneMessage(t *testing.T) {
	var s LeaderboardSnapshot
	long := strings.Repeat("W_🎯*", 100)
	for i := 0; i < 40; i++ {
		e := repository.LeaderboardEntry{DisplayName: long, Value: "999999999999"}
		s.TopKills, s.TopStreaks, s.TopDeaths, s.TopLongest = append(s.TopKills, e), append(s.TopStreaks, e), append(s.TopDeaths, e), append(s.TopLongest, e)
		s.CurrentRanks = append(s.CurrentRanks, RankEntry{DisplayName: long, Rank: long})
	}
	s.RanksEnabled, s.ServerName, s.GeneratedAt = true, long, time.Now()
	embeds := BuildAutoLeaderboardEmbeds(s, LeaderboardConfig{TopKillsLimit: 99, TopStreaksLimit: 99, TopRanksLimit: 99, TopDeathsLimit: 99, TopLongestLimit: 99})
	if len(embeds) != 6 {
		t.Fatalf("got %d embeds", len(embeds))
	}
	assertPackageWithinLimits(t, embeds)
}

// assertPackageWithinLimits: per-embed limits plus Discord's per-MESSAGE rules
// (at most 10 embeds, 6000 characters across all embeds combined).
func assertPackageWithinLimits(t *testing.T, embeds []*discordgo.MessageEmbed) {
	t.Helper()
	if len(embeds) > 10 {
		t.Fatalf("%d embeds exceeds Discord's 10 per message", len(embeds))
	}
	total := 0
	for _, e := range embeds {
		assertWithinLimits(t, e)
		total += presentation.EmbedLength(e)
	}
	if total > presentation.LimitTotal {
		t.Fatalf("combined message length %d exceeds %d", total, presentation.LimitTotal)
	}
}

// --- hashing --------------------------------------------------------------------

func TestHashEmbedsCoversPackage(t *testing.T) {
	cfg := DefaultLeaderboardConfig()
	base := FixtureAutoLeaderboardSnapshot()
	h := hashEmbeds(BuildAutoLeaderboardEmbeds(base, cfg))
	if hashEmbeds(BuildAutoLeaderboardEmbeds(FixtureAutoLeaderboardSnapshot(), cfg)) != h {
		t.Fatal("identical snapshots must hash identically")
	}

	s := FixtureAutoLeaderboardSnapshot()
	s.TopKills[10].Value = "899" // rank #11 value
	if hashEmbeds(BuildAutoLeaderboardEmbeds(s, cfg)) == h {
		t.Fatal("a change at rank #11 must change the hash")
	}

	swapped := BuildAutoLeaderboardEmbeds(base, cfg)
	swapped[1], swapped[2] = swapped[2], swapped[1]
	if hashEmbeds(swapped) == h {
		t.Fatal("embed order must change the hash")
	}
	fewer := BuildAutoLeaderboardEmbeds(base, cfg)[:5]
	if hashEmbeds(fewer) == h {
		t.Fatal("embed count must change the hash")
	}

	// A real refresh moves "Last Updated", so the message IS edited.
	later := FixtureAutoLeaderboardSnapshot()
	later.GeneratedAt = later.GeneratedAt.Add(LeaderboardRefreshInterval)
	if hashEmbeds(BuildAutoLeaderboardEmbeds(later, cfg)) == h {
		t.Fatal("a new refresh time must be published (visible Last Updated)")
	}
	// The embed timestamp (volatile, not rendered by the board) never forces an edit.
	stamped := BuildAutoLeaderboardEmbeds(base, cfg)
	stamped[0].Timestamp = time.Now().Format(time.RFC3339)
	if hashEmbeds(stamped) != h {
		t.Fatal("embed timestamps must not force edits")
	}

	single := &discordgo.MessageEmbed{Title: "x"}
	if hashPanelContent(PanelContent{Embed: single}) != hashEmbed(single) {
		t.Fatal("single-embed panels keep their existing hash")
	}
}

// --- legacy panel ----------------------------------------------------------------

type countingEditor struct {
	sends, edits int
	last         []*discordgo.MessageEmbed
}

func (r *countingEditor) ChannelMessageSendEmbed(string, *discordgo.MessageEmbed) (*discordgo.Message, error) {
	panic("the auto leaderboard must never be sent as a single embed")
}

func (r *countingEditor) ChannelMessageEditEmbed(string, string, *discordgo.MessageEmbed) (*discordgo.Message, error) {
	panic("the auto leaderboard must never be edited as a single embed")
}

func (r *countingEditor) ChannelMessageSendEmbeds(_ string, embeds []*discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	r.sends++
	r.last = embeds
	return &discordgo.Message{ID: "m1"}, nil
}

func (r *countingEditor) ChannelMessageEditEmbeds(_, _ string, embeds []*discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	r.edits++
	r.last = embeds
	return &discordgo.Message{ID: "m1"}, nil
}

func TestLeaderboardPanelSendsOneMultiEmbedMessage(t *testing.T) {
	ed := &countingEditor{}
	p := NewLeaderboardPanel(ed, "chan", "", DefaultLeaderboardConfig())
	s := FixtureAutoLeaderboardSnapshot()
	if _, changed, err := p.Update(s); err != nil || !changed || ed.sends != 1 || len(ed.last) != 6 {
		t.Fatalf("first update posts one 6-embed message: %v %v sends=%d embeds=%d", changed, err, ed.sends, len(ed.last))
	}
	if _, changed, _ := p.Update(s); changed || ed.edits != 0 {
		t.Fatal("an identical board must not be re-edited")
	}
	s.TopDeaths[12].Value = "201"
	if _, changed, _ := p.Update(s); !changed || ed.edits != 1 || len(ed.last) != 6 || ed.sends != 1 {
		t.Fatal("a real ranking change must be edited into the same message, whole package")
	}
}

type singleEmbedOnlyEditor struct{ calls int }

func (e *singleEmbedOnlyEditor) ChannelMessageSendEmbed(string, *discordgo.MessageEmbed) (*discordgo.Message, error) {
	e.calls++
	return &discordgo.Message{ID: "x"}, nil
}
func (e *singleEmbedOnlyEditor) ChannelMessageEditEmbed(string, string, *discordgo.MessageEmbed) (*discordgo.Message, error) {
	e.calls++
	return &discordgo.Message{ID: "x"}, nil
}

func TestLeaderboardPanelNeverSplitsThePackage(t *testing.T) {
	ed := &singleEmbedOnlyEditor{}
	p := NewLeaderboardPanel(ed, "chan", "", DefaultLeaderboardConfig())
	if _, _, err := p.Update(FixtureAutoLeaderboardSnapshot()); !errors.Is(err, errNoMultiEmbed) || ed.calls != 0 {
		t.Fatalf("without multi-embed support nothing is posted: err=%v calls=%d", err, ed.calls)
	}
}

// --- scheduler (routed + legacy) ----------------------------------------------------

type fixtureStats struct {
	mu    sync.Mutex
	snap  LeaderboardSnapshot
	fail  string // method name that fails
	calls map[string]int
	limit map[string]int
}

func newFixtureStats() *fixtureStats {
	return &fixtureStats{snap: FixtureAutoLeaderboardSnapshot(), calls: map[string]int{}, limit: map[string]int{}}
}

func (f *fixtureStats) get(method string, limit int, rows []repository.LeaderboardEntry) ([]repository.LeaderboardEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	f.limit[method] = limit
	if f.fail == method {
		return nil, errors.New("db down")
	}
	return rows, nil
}
func (f *fixtureStats) TopByKills(_ context.Context, _ int64, n int) ([]repository.LeaderboardEntry, error) {
	return f.get("kills", n, f.snap.TopKills)
}
func (f *fixtureStats) TopByBestStreak(_ context.Context, _ int64, n int) ([]repository.LeaderboardEntry, error) {
	return f.get("streaks", n, f.snap.TopStreaks)
}
func (f *fixtureStats) TopByDeaths(_ context.Context, _ int64, n int) ([]repository.LeaderboardEntry, error) {
	return f.get("deaths", n, f.snap.TopDeaths)
}
func (f *fixtureStats) TopLongestKill(_ context.Context, _ int64, n int) ([]repository.LeaderboardEntry, error) {
	return f.get("longest", n, f.snap.TopLongest)
}

type fixtureRanks struct{ rows []RankEntry }

func (r fixtureRanks) TopCurrentRanks(context.Context, int64, int) ([]RankEntry, error) {
	return r.rows, nil
}

func newV3Scheduler(f *routedFixture, stats AutoLeaderboardReader) *LeaderboardScheduler {
	panel := NewLeaderboardPanel(f.api, "legacy-chan", "", DefaultLeaderboardConfig())
	s := NewLeaderboardScheduler(panel, stats, f.guild, DefaultLeaderboardConfig(), func(string) {})
	servers := func(context.Context) (int64, []int64, error) { return f.guild, f.servers, nil }
	s.SetRouting(f.resolver, servers, f.panels, NewLegacyLeaderboardRetirer(f.api, f.setup, "g1"))
	s.SetServerNames(servers, func(id int64) string {
		if id == 1 {
			return "Champions Deathmatch"
		}
		return "Champions Deathmatch" // two servers, one display name: shown once
	})
	return s
}

func TestSchedulerQueriesEachBoardOnceWithTop15(t *testing.T) {
	f := newRoutedFixture(t)
	stats := newFixtureStats()
	s := newV3Scheduler(f, stats)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "leaderboards")
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"kills", "streaks", "deaths", "longest"} {
		if stats.calls[m] != 1 || stats.limit[m] != 15 {
			t.Fatalf("%s: calls=%d limit=%d, want one bounded query with limit 15", m, stats.calls[m], stats.limit[m])
		}
	}
	embeds := f.api.embedsIn("leaderboards")
	if len(embeds) != 5 || f.api.multiCalls != 1 || f.api.sendCount() != 1 {
		t.Fatalf("one message with the live package (ranks inactive): embeds=%d calls=%d sends=%d", len(embeds), f.api.multiCalls, f.api.sendCount())
	}
	if !strings.Contains(embeds[0].Description, "**Champions Deathmatch**") || strings.Count(embeds[0].Description, "Champions Deathmatch") != 1 {
		t.Fatalf("header server name: %q", embeds[0].Description)
	}
}

func TestSchedulerWithRankSourcePublishesSixEmbeds(t *testing.T) {
	f := newRoutedFixture(t)
	s := newV3Scheduler(f, newFixtureStats())
	s.SetRankSource(fixtureRanks{rows: FixtureAutoLeaderboardSnapshot().CurrentRanks})
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "leaderboards")
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	embeds := f.api.embedsIn("leaderboards")
	if len(embeds) != 6 {
		t.Fatalf("expected 6 embeds, got %d", len(embeds))
	}
	for i, want := range autoBoardOrder {
		if embeds[i].Title != want {
			t.Fatalf("embed %d = %q", i, embeds[i].Title)
		}
	}
}

func TestSchedulerFailedQueryKeepsLastGoodBoard(t *testing.T) {
	for _, method := range []string{"kills", "streaks", "deaths", "longest"} {
		f := newRoutedFixture(t)
		stats := newFixtureStats()
		s := newV3Scheduler(f, stats)
		f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "leaderboards")
		if err := s.RefreshOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		good := f.api.embedsIn("leaderboards")
		calls, edits := f.api.multiCalls, len(f.api.edits)

		stats.fail = method
		if err := s.RefreshOnce(context.Background()); err == nil {
			t.Fatalf("%s failure must fail the refresh", method)
		}
		if f.api.multiCalls != calls || len(f.api.edits) != edits || f.api.sendCount() != 1 {
			t.Fatalf("%s failure: nothing may be published (no partial board)", method)
		}
		if hashEmbeds(f.api.embedsIn("leaderboards")) != hashEmbeds(good) {
			t.Fatalf("%s failure changed the live board", method)
		}

		stats.fail = "" // next refresh recovers in place
		if err := s.RefreshOnce(context.Background()); err != nil || f.api.sendCount() != 1 {
			t.Fatalf("%s: recovery must edit the same message: %v sends=%d", method, err, f.api.sendCount())
		}
	}
}

func TestSchedulerRoutedPersistentAcrossRefreshRestartAndRouteMove(t *testing.T) {
	f := newRoutedFixture(t)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "chan-old")
	s := newV3Scheduler(f, newFixtureStats())
	for i := 0; i < 3; i++ { // scheduled + manual refreshes share RefreshOnce
		if err := s.RefreshOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if f.api.sendCount() != 1 || f.api.liveIn("chan-old") != 1 || len(f.api.embedsIn("chan-old")) != 5 {
		t.Fatalf("one persistent multi-embed message: sends=%d live=%v", f.api.sendCount(), f.api.live())
	}

	restarted := newV3Scheduler(f, newFixtureStats()) // same durable RoutePanels store
	if err := restarted.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.api.sendCount() != 1 {
		t.Fatalf("restart must reuse the recorded message, sends=%d", f.api.sendCount())
	}

	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "chan-new")
	if err := restarted.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.api.liveIn("chan-old") != 0 || f.api.liveIn("chan-new") != 1 || len(f.api.embedsIn("chan-new")) != 5 {
		t.Fatalf("route move: one package in the new channel, old retired, live=%v", f.api.live())
	}
	if f.store.count(f.guild, routeKeyAutoLeaderboard) != 1 {
		t.Fatal("AUTO_LEADERBOARD stays ONE recorded panel row, not one per embed")
	}
}

func TestSchedulerCoexistsWithPlayerStatsPanel(t *testing.T) {
	f := newRoutedFixture(t)
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "leaderboards")
	f.resolver.set(f.guild, 1, routeKeyStatsLeaderboards, "leaderboards")
	s := newV3Scheduler(f, newFixtureStats())
	f.syncer.SetLeaderboard(s)
	f.syncer.SyncOnce(context.Background()) // posts the Player Stats panel
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.syncer.SyncOnce(context.Background())
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.api.liveIn("leaderboards") != 2 {
		t.Fatalf("the board and the Player Stats panel must share #leaderboards, live=%v", f.api.live())
	}
	if f.store.count(f.guild, routeKeyAutoLeaderboard) != 1 || f.store.count(f.guild, routeKeyStatsLeaderboards) != 1 {
		t.Fatal("each panel keeps its own single recorded message")
	}
}

func TestSchedulerLegacyFallbackUsesTheSamePackage(t *testing.T) {
	f := newRoutedFixture(t)
	s := newV3Scheduler(f, newFixtureStats()) // no route -> legacy channel
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	legacy := f.api.embedsIn("legacy-chan")
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "route-chan")
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	routed := f.api.embedsIn("route-chan")
	if len(legacy) != 5 || len(routed) != 5 {
		t.Fatalf("legacy=%d routed=%d embeds", len(legacy), len(routed))
	}
	for i := range legacy {
		if legacy[i].Title != routed[i].Title || (i > 0 && hashEmbed(legacy[i]) != hashEmbed(routed[i])) {
			t.Fatalf("embed %d differs between legacy and routed mode", i)
		}
	}
}

// --- RoutePanels: single-embed panels unaffected ----------------------------------

// singleOnlyRouteAPI exposes only the original RoutePanelAPI surface (no
// multi-embed methods), like every pre-V3 fake and adapter.
type singleOnlyRouteAPI struct{ f *fakeDiscord }

func (a singleOnlyRouteAPI) ChannelMessageSendComplex(c string, e *discordgo.MessageEmbed, comp []discordgo.MessageComponent) (*discordgo.Message, error) {
	return a.f.ChannelMessageSendComplex(c, e, comp)
}
func (a singleOnlyRouteAPI) ChannelMessageEditComplex(c, m string, e *discordgo.MessageEmbed, comp []discordgo.MessageComponent) (*discordgo.Message, error) {
	return a.f.ChannelMessageEditComplex(c, m, e, comp)
}
func (a singleOnlyRouteAPI) ChannelMessage(c, m string) (*discordgo.Message, error) {
	return a.f.ChannelMessage(c, m)
}
func (a singleOnlyRouteAPI) ChannelMessageDelete(c, m string) error {
	return a.f.ChannelMessageDelete(c, m)
}

func TestRoutePanelsSingleEmbedUnchangedAndMultiNeedsSupport(t *testing.T) {
	api := newFakeDiscord()
	store := newMemPanelStore()
	p := NewRoutePanels(singleOnlyRouteAPI{api}, store)
	res, err := p.Sync(context.Background(), 7, "PLAYER_LINK", []string{"c"}, PanelContent{Embed: &discordgo.MessageEmbed{Description: "one"}}, true, true)
	if err != nil || res.Created != 1 || api.multiCalls != 0 || api.lastEmbed["c"] != "one" {
		t.Fatalf("single-embed panels use the original path: %+v %v multi=%d", res, err, api.multiCalls)
	}
	res, _ = p.Sync(context.Background(), 7, routeKeyAutoLeaderboard, []string{"d"}, PanelContent{Embeds: []*discordgo.MessageEmbed{{Title: "a"}, {Title: "b"}}}, true, true)
	if res.Errors != 1 || res.Created != 0 || api.liveIn("d") != 0 {
		t.Fatalf("a multi-embed package is never split over an API without multi-embed support: %+v", res)
	}

	full := NewRoutePanels(api, store)
	res, _ = full.Sync(context.Background(), 7, routeKeyAutoLeaderboard, []string{"d"}, PanelContent{Embeds: []*discordgo.MessageEmbed{{Title: "a"}, {Title: "b"}}}, true, true)
	if res.Created != 1 || len(api.embedsIn("d")) != 2 {
		t.Fatalf("multi-embed create: %+v", res)
	}
	res, _ = full.Sync(context.Background(), 7, routeKeyAutoLeaderboard, []string{"d"}, PanelContent{Embeds: []*discordgo.MessageEmbed{{Title: "a"}, {Title: "c"}}}, true, true)
	if res.Updated != 1 || api.sendCount() != 2 || api.embedsIn("d")[1].Title != "c" {
		t.Fatalf("multi-embed edit in place: %+v sends=%d", res, api.sendCount())
	}
}
