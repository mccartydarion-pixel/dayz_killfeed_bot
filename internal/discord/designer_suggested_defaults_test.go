package discord

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// WHERE THE DESIGNER'S DEFAULTS LIVE, AND WHAT THIS FILE IS.
//
// There are two different "defaults" and they are defined in two places:
//
//  1. The CHAMPION DEFAULT a route falls back to (no saved template, the template disabled, the
//     owner picked "Champion Default", or a reset = DELETE of the template) is the BUILT-IN CARD
//     of this repository (BuildKillEmbed, buildHitEmbed, ...). The backend never stores or
//     fabricates a template for it. It therefore has the new look automatically.
//
//  2. The STARTING TEMPLATE the Embed Designer shows when an owner begins to customise a route
//     (its "Champion Default" preset) is defined by the WEBSITE, in lib/saas/embedTypes.ts
//     (`definitions[].defaults`). This repository only pins that the backend accepts it
//     (internal/embedtemplates/website_compat_test.go). It is not changed here.
//
// A built-in card cannot be fully written as a template: its title, colour and hero field follow
// the kill's story (headshot, long shot, bounty...) and a template has one static colour and no
// conditions. What CAN be expressed with the existing variables is the calm look - sentence-case
// labels, the palette, one footer - and that is what the starting templates below do. They are
// the suggested replacement for (2), kept here so they are proven valid against this backend:
// every one must validate, render, use only variables the route already approves, and pass the
// same design rules as the built-in cards. Nothing reads them at runtime.
func suggestedDesignerDefaults() map[string]embedtemplates.Config {
	hex := func(c int) string { return "#" + strings.ToUpper(strconv.FormatInt(int64(c)+0x1000000, 16)[1:]) }
	base := func(route string, color int, title, desc string, fields ...embedtemplates.Field) embedtemplates.Config {
		return embedtemplates.Config{
			RouteKey: route, Enabled: true, Color: hex(color),
			Title:       embedtemplates.Text{Enabled: title != "", Template: title},
			Description: embedtemplates.Text{Enabled: true, Template: desc},
			Author:      embedtemplates.Author{Enabled: true, Name: presentation.AuthorName},
			Footer:      embedtemplates.Footer{Enabled: true, Text: "{{server_name}}"},
			Timestamp:   true, Fields: fields,
		}
	}
	return map[string]embedtemplates.Config{
		"KILLFEED": base("KILLFEED", presentation.Crimson, "☠️ Player eliminated", "**{{killer}}** → **{{victim}}**\n\n`{{weapon}}` • {{distance}}",
			tmplField("killer", "Killer", "**{{killer_kills}} K** • **{{killer_deaths}} D** • **{{killer_kd}} K/D**\n🔥 Streak **{{killer_streak}}**", true, 0),
			tmplField("victim", "Victim", "**{{victim_kills}} K** • **{{victim_deaths}} D** • **{{victim_kd}} K/D**", true, 1),
			tmplField("h2h", "Head to head", "**{{h2h_score}}**", true, 2)),
		"HITFEED":         base("HITFEED", presentation.Neutral, "", "🎯 **Hit**  {{attacker}}  ➜  {{victim}}\n{{weapon}} • {{ammo}} • {{distance}}\n{{hit_zone}} • {{hits}} hits • {{damage}} dmg"),
		"PVE_FEED":        base("PVE_FEED", presentation.Neutral, "", "💀 **PvE death**\n{{victim}} died ({{cause}})."),
		"BOUNTY_TRACKING": base("BOUNTY_TRACKING", presentation.Crimson, "", "🎯 **Bounty {{status}}**\n**{{target}}**\nReward **{{amount}} pts**\nClaimed by **{{hunter}}**"),
		"ECONOMY":         base("ECONOMY", presentation.Gold, "", "💰 **{{transaction_type}}**\n{{player}}: {{amount}} pts\nBalance: {{balance}} pts\nFor: {{reason}}"),
		"CONNECTIONS":     base("CONNECTIONS", presentation.Green, "", "🔌 **{{player}}** {{event}} the server.\nSession: {{session}}"),
	}
}

func TestSuggestedDesignerDefaultsAreValidAndFollowTheDesignRules(t *testing.T) {
	defaults := suggestedDesignerDefaults()
	for _, route := range embedrender.SupportedRoutes() {
		if _, ok := defaults[route]; !ok {
			t.Errorf("route %s renders saved templates at runtime but has no suggested starting template", route)
		}
	}
	samples := map[string]map[string]string{
		"KILLFEED":        killfeedVars(FixtureStandardKill(), "Northstar"),
		"HITFEED":         hitfeedVars(&hitEncounter{attacker: "WilliamAle--10", victim: "Semillita-azul-_", weapon: "M4-A1", ammo: "Bullet_556x45", zone: "Torso", distance: fptr(86), hits: 3, damage: 84, damageHits: 3}, "Northstar"),
		"PVE_FEED":        pveVars(killfeed.PveDeathNotice{Name: "Semillita-azul-_", Cause: killfeed.DeathCauseSuicide}, "Northstar"),
		"BOUNTY_TRACKING": bountyVars(bounties.Event{Kind: bounties.EventClaimed, Target: "Semillita-azul-_", Hunter: "WilliamAle--10", Amount: 25000, Count: 1}, "Northstar"),
		"ECONOMY":         economyVars(economy.Event{Type: economy.TypeSystemReward, PlayerName: "WilliamAle--10", Amount: 1500, Credit: true, BalanceAfter: 341500, Reason: "Daily play reward"}, "Northstar"),
		"CONNECTIONS":     connectionVars(killfeed.ConnectionNotice{Kind: killfeed.ConnectionDisconnected, Name: "WilliamAle--10", Session: 72 * time.Minute}, "Northstar"),
	}
	for route, cfg := range defaults {
		// Valid as a saved template: only variables the route already approves, within every limit.
		valid, err := embedtemplates.Validate(cfg, route)
		if err != nil {
			t.Errorf("%s: the suggested starting template must be a valid template: %v", route, err)
			continue
		}
		embed, err := embedrender.RenderEvent(valid, route, samples[route], time.Date(2026, 9, 23, 18, 42, 7, 0, time.UTC))
		if err != nil {
			t.Errorf("%s: the suggested starting template must render: %v", route, err)
			continue
		}
		// The same rules as a built-in card: palette colour, calm labels, one footer.
		for _, problem := range presentation.CheckEmbed(embed) {
			t.Errorf("%s: %s", route, problem)
		}
		if embed.Footer == nil || embed.Footer.Text != "Northstar" {
			t.Errorf("%s: the footer names the server: %+v", route, embed.Footer)
		}
		if strings.Contains(embed.Description, "{{") || strings.Contains(embed.Title, "{{") {
			t.Errorf("%s: an unresolved placeholder was posted: %q / %q", route, embed.Title, embed.Description)
		}
	}
	// The suggested kill template lines up with the built-in card where a template can: same
	// title for a standard kill, same colour, same stat strip, same field labels.
	builtIn := BuildKillEmbed(FixtureStandardKill())
	valid, _ := embedtemplates.Validate(defaults["KILLFEED"], "KILLFEED")
	custom, err := embedrender.RenderEvent(valid, "KILLFEED", samples["KILLFEED"], time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if custom.Title != builtIn.Title || custom.Color != builtIn.Color || custom.Author.Name != builtIn.Author.Name {
		t.Errorf("standard kill: template title/colour/author %q %06X %q, built-in %q %06X %q", custom.Title, custom.Color, custom.Author.Name, builtIn.Title, builtIn.Color, builtIn.Author.Name)
	}
	for i, label := range []string{"Killer", "Victim", "Head to head"} {
		if i >= len(custom.Fields) || custom.Fields[i].Name != label || fieldNamed(builtIn, label) == nil {
			t.Errorf("standard kill: field %d should be %q on both cards", i, label)
		}
	}
	if got, want := custom.Fields[1].Value, fieldNamed(builtIn, "Victim").Value; got != want {
		t.Errorf("victim stat strip: template %q, built-in %q", got, want)
	}
}
