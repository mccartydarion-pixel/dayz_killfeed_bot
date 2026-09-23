package discord

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestSeasonLeaderboardIsOneFieldPerCategory(t *testing.T) {
	e := BuildLeaderboardEmbed(FixtureSeasonSnapshot(), DefaultLeaderboardConfig())
	if e.Title != "🏆 SEASON LEADERBOARD" || e.Author == nil || e.Author.Name != presentation.AuthorName {
		t.Fatalf("hero: %q %#v", e.Title, e.Author)
	}
	if e.Footer == nil || e.Footer.Text != presentation.FooterAutoRefresh {
		t.Fatalf("footer: %#v", e.Footer)
	}
	// Old behavior: 20 fields (one per player). Required: one per category.
	if len(e.Fields) != 3 {
		t.Fatalf("expected 3 category fields, got %d", len(e.Fields))
	}
	want := []string{"⚔️ TOP KILLERS", "🎯 LONGEST KILLS", "📈 BEST K/D"}
	for i, f := range e.Fields {
		if f.Name != want[i] || f.Inline {
			t.Fatalf("field %d = %q inline=%v, want %q", i, f.Name, f.Inline, want[i])
		}
	}
	if n := strings.Count(e.Fields[0].Value, "\n") + 1; n != 10 {
		t.Fatalf("top killers should list 10 rows, got %d", n)
	}
	if n := strings.Count(e.Fields[1].Value, "\n") + 1; n != 5 {
		t.Fatalf("longest should list 5 rows, got %d", n)
	}
	text := allText(e)
	for _, must := range []string{"20 Kills", "98.3m", "4.25 K/D"} {
		if !strings.Contains(text, must) {
			t.Errorf("missing %q", must)
		}
	}
	for _, bad := range []string{"Value 20", "Value 98.3", "Value 4.25", "Value", "TOP KILLERS • #", "CHAMPION KILLFEED"} {
		if strings.Contains(text, bad) {
			t.Errorf("must not contain %q", bad)
		}
	}
	if strings.Count(text, "TOP KILLERS") != 1 {
		t.Error("category heading must appear exactly once")
	}
	assertWithinLimits(t, e)
}

func TestLeaderboardTopThreeMedalsThenRankNumbers(t *testing.T) {
	rows := strings.Split(BuildLeaderboardEmbed(FixtureSeasonSnapshot(), DefaultLeaderboardConfig()).Fields[0].Value, "\n")
	for i, prefix := range []string{"🥇 ", "🥈 ", "🥉 ", "`#4` ", "`#5` "} {
		if !strings.HasPrefix(rows[i], prefix) {
			t.Fatalf("row %d = %q, want prefix %q", i+1, rows[i], prefix)
		}
	}
	if strings.ContainsAny(rows[3], "🥇🥈🥉") {
		t.Fatal("rank 4 must not get a medal")
	}
	if rows[0] != "🥇 IIIIIIIIIIII-I • **20 Kills**" {
		t.Fatalf("rank line format: %q", rows[0])
	}
}

func TestLeaderboardOptionalBlocksAndEmptyState(t *testing.T) {
	s := FixtureSeasonSnapshot()
	s.Points = []repository.LeaderboardEntry{{DisplayName: "A", Value: "125000"}, {DisplayName: "B", Value: "1"}}
	s.LiveEvents = []string{"Double Points active", ""}
	s.ActiveBounties = []string{"PlayerOne • 25,000 pts"}
	e := BuildLeaderboardEmbed(s, DefaultLeaderboardConfig())
	if len(e.Fields) != 6 {
		t.Fatalf("expected 6 fields (3 rankings + points + events + wanted), got %d", len(e.Fields))
	}
	if !fieldsContain(e.Fields, "🏆 CHAMPION POINTS", "**125,000 pts**") || !fieldsContain(e.Fields, "🏆 CHAMPION POINTS", "**1 pt**") {
		t.Fatalf("points formatting: %#v", e.Fields[3])
	}
	if f := fieldNamed(e, "🔥 LIVE EVENTS"); f == nil || f.Value != "• Double Points active" {
		t.Fatalf("live events block: %#v", f)
	}
	if !fieldsContain(e.Fields, "🎯 MOST WANTED", "• PlayerOne • 25,000 pts") {
		t.Fatal("most wanted block")
	}

	empty := BuildLeaderboardEmbed(LeaderboardSnapshot{GeneratedAt: time.Now()}, DefaultLeaderboardConfig())
	if len(empty.Fields) != 0 || !strings.Contains(empty.Description, presentation.EmptyRanking) {
		t.Fatalf("an empty board is one line, not empty fields: %#v %q", empty.Fields, empty.Description)
	}
	partial := BuildLeaderboardEmbed(LeaderboardSnapshot{TopKills: s.TopKills[:2]}, DefaultLeaderboardConfig())
	if len(partial.Fields) != 1 {
		t.Fatalf("empty categories are omitted: %d", len(partial.Fields))
	}
}

func TestLeaderboardLimitsFromConfig(t *testing.T) {
	cfg := DefaultLeaderboardConfig()
	cfg.TopKillsLimit = 3
	e := BuildLeaderboardEmbed(FixtureSeasonSnapshot(), cfg)
	if n := strings.Count(e.Fields[0].Value, "\n") + 1; n != 3 {
		t.Fatalf("limit not applied: %d rows", n)
	}
}

func TestLeaderboardNamesAreSafeAndCapped(t *testing.T) {
	s := LeaderboardSnapshot{TopKills: []repository.LeaderboardEntry{
		{DisplayName: "@everyone", Value: "3"},
		{DisplayName: "<#123> **x**", Value: "2"},
		{DisplayName: strings.Repeat("N", 200), Value: "1"},
	}}
	v := BuildLeaderboardEmbed(s, DefaultLeaderboardConfig()).Fields[0].Value
	if strings.Contains(v, "@everyone") || strings.Contains(v, "<#") || strings.Contains(v, "**x**") {
		t.Fatalf("unsafe name rendered: %q", v)
	}
	if strings.Contains(v, strings.Repeat("N", presentation.MaxRankNameRunes+1)) {
		t.Fatal("rank names must be capped")
	}
}

func TestLeaderboardMaximalDataStaysWithinLimits(t *testing.T) {
	var s LeaderboardSnapshot
	long := strings.Repeat("W🎯", 200)
	for i := 0; i < 60; i++ {
		e := repository.LeaderboardEntry{DisplayName: long, Value: "999999999999"}
		s.TopKills, s.TopKD, s.TopLongest, s.Points = append(s.TopKills, e), append(s.TopKD, e), append(s.TopLongest, e), append(s.Points, e)
		s.LiveEvents, s.ActiveBounties = append(s.LiveEvents, long), append(s.ActiveBounties, long)
	}
	cfg := LeaderboardConfig{TopKillsLimit: 60, TopKDLimit: 60, TopLongestLimit: 60}
	assertWithinLimits(t, BuildLeaderboardEmbed(s, cfg))
}

func TestPlayerLeaderboardUsesTheSharedRankSystem(t *testing.T) {
	e := presentation.BuildPlayerLeaderboardEmbed("Kills", FixturePlayerLeaderboard(), "Lifetime")
	if len(e.Fields) != 1 || e.Fields[0].Name != "⚔️ TOP KILLERS" {
		t.Fatalf("one ranking field: %#v", e.Fields)
	}
	season := BuildLeaderboardEmbed(FixtureSeasonSnapshot(), DefaultLeaderboardConfig())
	if e.Fields[0].Value != season.Fields[0].Value {
		t.Fatal("player and season boards must share one rank style")
	}
	kd := presentation.BuildPlayerLeaderboardEmbed("K/D", []presentation.RankedEntry{{Rank: 1, Name: "A", Value: "4.25"}}, "Lifetime")
	lg := presentation.BuildPlayerLeaderboardEmbed("Longest Kill", []presentation.RankedEntry{{Rank: 1, Name: "A", Value: "98.3m"}}, "Lifetime")
	if kd.Fields[0].Value != "🥇 A • **4.25 K/D**" || lg.Fields[0].Value != "🥇 A • **98.3m**" {
		t.Fatalf("category values: %q / %q", kd.Fields[0].Value, lg.Fields[0].Value)
	}
}

// --- persistent-panel hash ----------------------------------------------------

func TestHashEmbedIncludesFieldContent(t *testing.T) {
	a := BuildLeaderboardEmbed(FixtureSeasonSnapshot(), DefaultLeaderboardConfig())
	s := FixtureSeasonSnapshot()
	s.TopKills[4].Value = "12" // a real ranking change lives only in a field
	b := BuildLeaderboardEmbed(s, DefaultLeaderboardConfig())
	if a.Title != b.Title || a.Description != b.Description {
		t.Fatal("precondition: header unchanged")
	}
	if hashEmbed(a) == hashEmbed(b) {
		t.Fatal("a leaderboard change inside a field must change the hash (it would be skipped)")
	}
}

func TestHashEmbedIsStableAndIgnoresTheTimestamp(t *testing.T) {
	a := BuildLeaderboardEmbed(FixtureSeasonSnapshot(), DefaultLeaderboardConfig())
	b := BuildLeaderboardEmbed(FixtureSeasonSnapshot(), DefaultLeaderboardConfig())
	b.Timestamp = time.Now().UTC().Format(time.RFC3339)
	if hashEmbed(a) != hashEmbed(b) {
		t.Fatal("identical content must hash identically (no unnecessary edits)")
	}
}

func TestHashEmbedCoversEveryVisiblePart(t *testing.T) {
	base := func() *discordgo.MessageEmbed {
		return &discordgo.MessageEmbed{Title: "t", Description: "d", Color: 1,
			Author: &discordgo.MessageEmbedAuthor{Name: "a"}, Footer: &discordgo.MessageEmbedFooter{Text: "f"},
			Fields: []*discordgo.MessageEmbedField{{Name: "n", Value: "v", Inline: true}}}
	}
	h := hashEmbed(base())
	muts := map[string]func(*discordgo.MessageEmbed){
		"title":  func(e *discordgo.MessageEmbed) { e.Title = "x" },
		"desc":   func(e *discordgo.MessageEmbed) { e.Description = "x" },
		"color":  func(e *discordgo.MessageEmbed) { e.Color = 2 },
		"author": func(e *discordgo.MessageEmbed) { e.Author.Name = "x" },
		"footer": func(e *discordgo.MessageEmbed) { e.Footer.Text = "x" },
		"fname":  func(e *discordgo.MessageEmbed) { e.Fields[0].Name = "x" },
		"fvalue": func(e *discordgo.MessageEmbed) { e.Fields[0].Value = "x" },
		"inline": func(e *discordgo.MessageEmbed) { e.Fields[0].Inline = false },
		"thumb":  func(e *discordgo.MessageEmbed) { e.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: "u"} },
		"image":  func(e *discordgo.MessageEmbed) { e.Image = &discordgo.MessageEmbedImage{URL: "u"} },
		"shift": func(e *discordgo.MessageEmbed) { // content moved between parts
			e.Fields[0].Name, e.Fields[0].Value = "nv", ""
		},
	}
	for name, mut := range muts {
		e := base()
		mut(e)
		if hashEmbed(e) == h {
			t.Errorf("%s change not detected", name)
		}
	}
	// nil footer / nil embed never panic (the old hash dereferenced Footer).
	_ = hashEmbed(&discordgo.MessageEmbed{Title: "x"})
	_ = hashEmbed(nil)
}

type countingEditor struct {
	sends, edits int
}

func (r *countingEditor) ChannelMessageSendEmbed(string, *discordgo.MessageEmbed) (*discordgo.Message, error) {
	r.sends++
	return &discordgo.Message{ID: "m1"}, nil
}

func (r *countingEditor) ChannelMessageEditEmbed(string, string, *discordgo.MessageEmbed) (*discordgo.Message, error) {
	r.edits++
	return &discordgo.Message{ID: "m1"}, nil
}

func TestLeaderboardPanelSkipsIdenticalAndEditsRealChanges(t *testing.T) {
	ed := &countingEditor{}
	p := NewLeaderboardPanel(ed, "chan", "", DefaultLeaderboardConfig())
	s := FixtureSeasonSnapshot()
	if _, changed, err := p.Update(s); err != nil || !changed || ed.sends != 1 {
		t.Fatalf("first update posts: %v %v %d", changed, err, ed.sends)
	}
	s.GeneratedAt = time.Now() // volatile, not rendered
	if _, changed, _ := p.Update(s); changed || ed.edits != 0 {
		t.Fatal("an identical board must not be re-edited")
	}
	s.TopKD[0].Value = "9.99"
	if _, changed, _ := p.Update(s); !changed || ed.edits != 1 {
		t.Fatal("a real ranking change must be edited in")
	}
}

// --- custom KILLFEED templates keep working over the V2 default ---------------

type v2TemplateSource struct {
	inst int64
	cfg  *embedtemplates.Config
	err  error
}

func (s v2TemplateSource) ResolveTemplate(context.Context, int64, int64, string) (int64, *embedtemplates.Config, error) {
	return s.inst, s.cfg, s.err
}

func validKillTemplate() *embedtemplates.Config {
	return &embedtemplates.Config{
		Enabled: true, RouteKey: "KILLFEED", Color: "#D4AF37",
		Title:       embedtemplates.Text{Enabled: true, Template: "{{killer}} eliminated {{victim}}"},
		Description: embedtemplates.Text{Enabled: true, Template: "{{weapon}} from {{distance}}"},
	}
}

func TestCustomTemplateRegressionOverV2Default(t *testing.T) {
	ev := FixtureStandardKill()
	def := BuildKillEmbed(ev)
	disabled := validKillTemplate()
	disabled.Enabled = false
	malformed := validKillTemplate()
	malformed.Color = "not-a-color"

	cases := []struct {
		name       string
		customizer EmbedCustomizer
		wantCustom bool
	}{
		{"flag disabled (nil customizer)", nil, false},
		{"renderer disabled", embedrender.New(embedrender.Options{Source: v2TemplateSource{inst: 3, cfg: validKillTemplate()}, Enabled: false}), false},
		{"no template", embedrender.New(embedrender.Options{Source: v2TemplateSource{inst: 3}, Enabled: true}), false},
		{"template disabled", embedrender.New(embedrender.Options{Source: v2TemplateSource{inst: 3, cfg: disabled}, Enabled: true}), false},
		{"template malformed", embedrender.New(embedrender.Options{Source: v2TemplateSource{inst: 3, cfg: malformed}, Enabled: true}), false},
		{"template store failure", embedrender.New(embedrender.Options{Source: v2TemplateSource{err: errors.New("db down")}, Enabled: true}), false},
		{"custom valid", embedrender.New(embedrender.Options{Source: v2TemplateSource{inst: 3, cfg: validKillTemplate()}, Enabled: true}), true},
	}
	for _, c := range cases {
		p := &KillfeedPublisher{}
		p.SetRouting(nil, 7, 1)
		p.SetCustomizer(c.customizer, "Northstar")
		card := p.Card(ev)
		if c.wantCustom {
			if !strings.HasPrefix(card.Title, "WilliamAle--10 eliminated Semillita-azul-") || reflect.DeepEqual(card, def) {
				t.Errorf("%s: expected the custom card, got %q", c.name, card.Title)
			}
			continue
		}
		if !reflect.DeepEqual(card, def) {
			t.Errorf("%s: expected the Champion V2 default, got %q", c.name, card.Title)
		}
		if card.Title != "☠️ PLAYER ELIMINATED" {
			t.Errorf("%s: fallback is not the V2 card: %q", c.name, card.Title)
		}
	}
}

func TestSeasonCompletionCardIsCompactAndNeverInventsHolders(t *testing.T) {
	e := BuildSeasonCompletionEmbed("Season 3", "", 20, "", 55, "", 298.4, "", 8)
	if e.Title != "🏆 SEASON COMPLETE" || len(e.Fields) != 4 {
		t.Fatalf("season card: %q %d fields", e.Title, len(e.Fields))
	}
	text := allText(e)
	for _, must := range []string{"**20 Kills**", "**55 Kills**", "**298.4m**", "**8 Kills**", "Season 3"} {
		if !strings.Contains(text, must) {
			t.Errorf("missing %q", must)
		}
	}
	if strings.Contains(text, "Player") || strings.Contains(text, "**SEASON**") {
		t.Fatalf("placeholder or debug label rendered: %q", text)
	}
	named := BuildSeasonCompletionEmbed("S", "WilliamAle--10", 1, "", 0, "", 0, "", 0)
	if named.Fields[0].Value != "**1 Kill**\nWilliamAle--10" {
		t.Fatalf("known holder: %q", named.Fields[0].Value)
	}
}
