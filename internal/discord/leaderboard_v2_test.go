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
)

func TestPlayerLeaderboardUsesTheSharedRankSystem(t *testing.T) {
	e := presentation.BuildPlayerLeaderboardEmbed("Kills", FixturePlayerLeaderboard(), "Lifetime")
	if len(e.Fields) != 1 || e.Fields[0].Name != "⚔️ TOP KILLERS" {
		t.Fatalf("one ranking field: %#v", e.Fields)
	}
	if e.Fields[0].Value != presentation.FormatRankingBlock(presentation.RankKills, FixturePlayerLeaderboard(), 0) {
		t.Fatal("the player leaderboard must use the shared rank formatter")
	}
	kd := presentation.BuildPlayerLeaderboardEmbed("K/D", []presentation.RankedEntry{{Rank: 1, Name: "A", Value: "4.25"}}, "Lifetime")
	lg := presentation.BuildPlayerLeaderboardEmbed("Longest Kill", []presentation.RankedEntry{{Rank: 1, Name: "A", Value: "98.3m"}}, "Lifetime")
	if kd.Fields[0].Value != "🥇 A • **4.25 K/D**" || lg.Fields[0].Value != "🥇 A • **98.3m**" {
		t.Fatalf("category values: %q / %q", kd.Fields[0].Value, lg.Fields[0].Value)
	}
}

// --- persistent-panel hash ----------------------------------------------------

func TestHashEmbedIncludesFieldContent(t *testing.T) {
	s := FixtureSeasonSnapshot()
	a := BuildAutoLeaderboardEmbeds(s, DefaultLeaderboardConfig())[1]
	s = FixtureSeasonSnapshot()
	s.TopKills[4].Value = "12" // a real ranking change lives only in a field
	b := BuildAutoLeaderboardEmbeds(s, DefaultLeaderboardConfig())[1]
	if a.Title != b.Title || a.Description != b.Description {
		t.Fatal("precondition: header unchanged")
	}
	if hashEmbed(a) == hashEmbed(b) {
		t.Fatal("a leaderboard change inside a field must change the hash (it would be skipped)")
	}
}

func TestHashEmbedIsStableAndIgnoresTheTimestamp(t *testing.T) {
	a := BuildAutoLeaderboardEmbeds(FixtureSeasonSnapshot(), DefaultLeaderboardConfig())[1]
	b := BuildAutoLeaderboardEmbeds(FixtureSeasonSnapshot(), DefaultLeaderboardConfig())[1]
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
