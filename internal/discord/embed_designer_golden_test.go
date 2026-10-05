package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// THE EMBED DESIGNER CONTRACT, AS A TEST.
//
// A server owner's saved template must post exactly what it posted before any change to the
// built-in cards: same variables, same values, same bytes. This test renders a set of saved
// templates - the website designer's own defaults for every route that renders at runtime, and
// richer templates that use every variable a route offers - against fixed events, and compares
// both the variables and the rendered embed, byte for byte, with a golden file.
//
// The golden file (testdata/embed_designer_golden.json) was written by running this same test
// on the code as it was BEFORE the built-in cards were restyled (docs/DISCORD_DESIGN.md). It is
// therefore not a snapshot of today's behaviour but of the behaviour owners already rely on.
// Do not regenerate it to make a failure go away: a failure means a saved template would now
// post something different, which is a bug in the change, not in the golden file.
//
// To add a fixture, add it below and run once with EMBED_DESIGNER_GOLDEN_OUT=/abs/path.json to
// write a golden file that contains it; check that only the new entry differs before replacing
// the checked-in file.

const designerGoldenFile = "testdata/embed_designer_golden.json"

var designerGoldenAt = time.Date(2026, 9, 23, 18, 42, 7, 0, time.UTC)

// designerGoldenStamp is the fixed {{timestamp}} value: publishers fill it with the publish
// time, which a byte-for-byte comparison cannot contain.
const designerGoldenStamp = "2026-09-23 18:42:07 UTC"

type designerCase struct {
	id    string
	route string
	cfg   embedtemplates.Config
	vars  map[string]string
}

type designerGoldenEntry struct {
	Route string            `json:"route"`
	Vars  map[string]string `json:"vars"`
	Embed json.RawMessage   `json:"embed"`
}

func tmplField(key, label, tmpl string, inline bool, order int) embedtemplates.Field {
	return embedtemplates.Field{Key: key, Label: label, Enabled: true, Template: tmpl, Inline: inline, Order: order}
}

// websiteDefault is a template exactly as the website designer serialises its default for a
// route (lib/saas/embedTypes.ts `base(...)`): footer "Champion Killfeed", timestamp on.
func websiteDefault(route, color, title, desc string, fields ...embedtemplates.Field) embedtemplates.Config {
	return embedtemplates.Config{
		RouteKey: route, Enabled: true, Color: color,
		Title:       embedtemplates.Text{Enabled: true, Template: title},
		Description: embedtemplates.Text{Enabled: true, Template: desc},
		Author:      embedtemplates.Author{Enabled: false, Name: "Champion"},
		Footer:      embedtemplates.Footer{Enabled: true, Text: "Champion Killfeed"},
		Timestamp:   true, Fields: fields,
	}
}

func designerKillTemplates() map[string]embedtemplates.Config {
	return map[string]embedtemplates.Config{
		// The website's current KILLFEED default ("Champion Default" preset).
		"website-default": websiteDefault("KILLFEED", "#D4AF37", "☠ PLAYER ELIMINATED", "**{{killer}}** → **{{victim}}**\n\n`{{weapon}}` • `{{distance}}` • {{range}}",
			tmplField("killer-stats", "KILLER STATS", `Kills: {{killer_kills}}\nDeaths: {{killer_deaths}}\nK/D: {{killer_kd}}\nStreak: {{killer_streak}}`, true, 0),
			tmplField("victim-stats", "VICTIM STATS", `Kills: {{victim_kills}}\nDeaths: {{victim_deaths}}\nK/D: {{victim_kd}}`, true, 1),
			tmplField("h2h", "HEAD TO HEAD", "{{h2h_score}}", true, 2)),
		// The website's first KILLFEED default, which early adopters saved.
		"website-first-default": websiteDefault("KILLFEED", "#D4AF37", "{{killer}} eliminated {{victim}}", "A {{weapon}} kill from {{distance}} away.",
			tmplField("killer", "Killer", "{{killer}}", true, 0), tmplField("victim", "Victim", "{{victim}}", true, 1),
			tmplField("weapon", "Weapon", "{{weapon}}", true, 2), tmplField("distance", "Distance", "{{distance}}", true, 3)),
		// An owner's own design leaning on the story engine's labels: these are the variables
		// whose values come from the same classifier the built-in card uses.
		"story-labels": {
			RouteKey: "KILLFEED", Enabled: true, Color: "#8B0000",
			Title:       embedtemplates.Text{Enabled: true, Template: "{{story_title}}"},
			Description: embedtemplates.Text{Enabled: true, Template: "{{kill_type}} | {{special_kill}}\n{{killer}} > {{victim}}\n{{weapon_category}} {{weapon}} {{ammo}} at {{distance}} ({{range}})\n{{headshot}} {{hit_zone}} {{damage}}\n{{killing_spree}} {{streak}} {{streak_ended}} {{ended_streak}}\n{{bounty_claimed}} {{bounty_amount}} {{bounty_target}}\n{{war_badge}} {{event_badges}} {{ranked_rp}}\n{{season_name}} on {{server_name}} at {{timestamp}}"},
			Author:      embedtemplates.Author{Enabled: true, Name: "{{server_name}} feed"},
			Footer:      embedtemplates.Footer{Enabled: true, Text: "{{season_name}} • {{server_name}}"},
			Fields: []embedtemplates.Field{
				tmplField("k", "Killer {{killer}}", "{{killer_kills}}/{{killer_deaths}} ({{killer_kd}}) streak {{killer_streak}}", true, 0),
				tmplField("v", "Victim {{victim}}", "{{victim_kills}}/{{victim_deaths}} ({{victim_kd}})", true, 1),
				tmplField("h", "H2H", "{{h2h_score}} = {{h2h_killer_wins}}:{{h2h_victim_wins}}", false, 2),
			},
		},
	}
}

func designerKillEvents() map[string]*killfeed.Event {
	war := FixtureStandardKill()
	war.WarBadge, war.ActiveEventBadges, war.RankedTag, war.SeasonName = "⚔️ FACTION WAR", []string{"Double Kill", "NWAF Event"}, "+250 RP ⚡💀", "Season 3"
	war.BountyTarget = true
	melee := fixtureKill("Fists", 1.4, "")
	bare := &killfeed.Event{Type: killfeed.EventPlayerKill, Killer: &killfeed.PlayerRef{Name: "@everyone **x**"}, Victim: &killfeed.PlayerRef{Name: ""}}
	return map[string]*killfeed.Event{
		"standard": FixtureStandardKill(), "headshot": FixtureHeadshotKill(), "longshot": FixtureLongshotKill(), "extreme-range": FixtureExtremeRangeKill(),
		"bounty-claimed": FixtureBountyKill(), "killing-spree": FixtureKillingSpreeKill(), "streak-ended": FixtureStreakEndedKill(),
		"war-and-events": war, "melee": melee, "bare-and-hostile-names": bare,
	}
}

// designerCases is every (saved template, event) pair the golden file covers.
func designerCases() []designerCase {
	var cases []designerCase
	stamp := func(vars map[string]string) map[string]string {
		if _, ok := vars["timestamp"]; ok {
			vars["timestamp"] = designerGoldenStamp
		}
		return vars
	}
	for tname, cfg := range designerKillTemplates() {
		for ename, ev := range designerKillEvents() {
			cases = append(cases, designerCase{"KILLFEED/" + tname + "/" + ename, "KILLFEED", cfg, stamp(killfeedVars(ev, "Northstar"))})
		}
	}

	hit := &hitEncounter{attacker: "WilliamAle--10", victim: "Semillita-azul-_", weapon: "M4-A1", ammo: "Bullet_556x45", zone: "Torso", distance: fptr(86.6), hits: 3, damage: 142, damageHits: 3}
	partial := &hitEncounter{attacker: "WilliamAle--10", victim: "Semillita-azul-_", hits: 2, damage: 40, damageHits: 1}
	hitTemplates := map[string]embedtemplates.Config{
		"website-default": websiteDefault("HITFEED", "#C98B3D", "Hit confirmed", "{{killer}} hit {{victim}} with {{weapon}}.", tmplField("distance", "Distance", "{{distance}}", true, 0)),
		"every-variable": {RouteKey: "HITFEED", Enabled: true, Color: "#4682B4", Title: embedtemplates.Text{Enabled: true, Template: "{{attacker}} hit {{victim}}"},
			Description: embedtemplates.Text{Enabled: true, Template: "{{weapon}} {{ammo}} {{distance}}\n{{hit_zone}} x{{hits}} = {{damage}}\n{{server_name}} {{timestamp}}"}},
	}
	for tname, cfg := range hitTemplates {
		cases = append(cases,
			designerCase{"HITFEED/" + tname + "/full", "HITFEED", cfg, stamp(hitfeedVars(hit, "Northstar"))},
			designerCase{"HITFEED/" + tname + "/partial", "HITFEED", cfg, stamp(hitfeedVars(partial, ""))})
	}

	pveTemplates := map[string]embedtemplates.Config{
		"website-default": websiteDefault("PVE_FEED", "#8C6A2D", "PvE event", "{{victim}} was lost to the wilds.", tmplField("victim", "Player", "{{victim}}", false, 0)),
		"every-variable":  {RouteKey: "PVE_FEED", Enabled: true, Color: "#B7791F", Description: embedtemplates.Text{Enabled: true, Template: "{{victim}} / {{cause}} / {{server_name}} / {{timestamp}}"}},
	}
	for tname, cfg := range pveTemplates {
		for cname, cause := range map[string]killfeed.DeathCause{"suicide": killfeed.DeathCauseSuicide, "infected": killfeed.DeathCauseInfected, "animal": killfeed.DeathCauseAnimal, "environment": killfeed.DeathCauseEnvironment} {
			cases = append(cases, designerCase{"PVE_FEED/" + tname + "/" + cname, "PVE_FEED", cfg, stamp(pveVars(killfeed.PveDeathNotice{Name: "Semillita-azul-_", Cause: cause}, "Northstar"))})
		}
	}

	bountyTemplates := map[string]embedtemplates.Config{
		"website-default": websiteDefault("BOUNTY_TRACKING", "#9E4B4B", "Bounty update", "{{killer}} is tracking {{victim}}.",
			tmplField("hunter", "Hunter", "{{killer}}", true, 0), tmplField("target", "Target", "{{victim}}", true, 1)),
		"every-variable": {RouteKey: "BOUNTY_TRACKING", Enabled: true, Color: "#D4AF37", Title: embedtemplates.Text{Enabled: true, Template: "Bounty {{status}}"},
			Description: embedtemplates.Text{Enabled: true, Template: "{{target}} for {{amount}} pts\n{{hunter}} took {{total}} pts ({{count}})\n{{weapon}} {{distance}}\n{{server_name}}"}},
	}
	bountyEvents := map[string]bounties.Event{
		"placed":    {Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "Semillita-azul-_", Amount: 100000},
		"increased": {Kind: bounties.EventIncreased, GuildID: 7, ServerID: 1, Target: "Semillita-azul-_", Amount: 150000, Automatic: true},
		"claimed":   {Kind: bounties.EventClaimed, GuildID: 7, ServerID: 1, Target: "Semillita-azul-_", Hunter: "WilliamAle--10", Amount: 150000, Count: 2, Weapon: "M4-A1", Distance: fptr(86.4)},
		"expired":   {Kind: bounties.EventExpired, GuildID: 7, ServerID: 1, Target: "Semillita-azul-_", Amount: 100000},
		"cancelled": {Kind: bounties.EventCancelled, GuildID: 7, ServerID: 1, Target: "Semillita-azul-_", Amount: 100000},
	}
	for tname, cfg := range bountyTemplates {
		for ename, ev := range bountyEvents {
			cases = append(cases, designerCase{"BOUNTY_TRACKING/" + tname + "/" + ename, "BOUNTY_TRACKING", cfg, bountyVars(ev, "Northstar")})
		}
	}

	economyTemplates := map[string]embedtemplates.Config{
		"website-default": websiteDefault("ECONOMY", "#4C9A72", "Economy transaction", "{{player}} received {{amount}} pts.",
			tmplField("amount", "Amount", "{{amount}}", true, 0), tmplField("balance", "Balance", "{{balance}}", true, 1)),
		"every-variable": {RouteKey: "ECONOMY", Enabled: true, Color: "#3F8F68", Title: embedtemplates.Text{Enabled: true, Template: "{{transaction_type}}"},
			Description: embedtemplates.Text{Enabled: true, Template: "{{player}}: {{amount}} pts\nBalance {{balance}} pts\nFor {{reason}}\n{{server_name}}"}},
	}
	economyEvents := map[string]economy.Event{
		"bounty-reward": {Type: economy.TypeBountyClaim, GuildID: 7, ServerID: 1, PlayerName: "WilliamAle--10", Amount: 125000, Credit: true, BalanceAfter: 340000},
		"system-reward": {Type: economy.TypeSystemReward, GuildID: 7, ServerID: 1, PlayerName: "WilliamAle--10", Amount: 10, Credit: true, BalanceAfter: 10, Reason: "Daily play reward (3 days in a row)"},
		"admin-credit":  {Type: economy.TypeAdminCredit, GuildID: 7, ServerID: 1, PlayerName: "WilliamAle--10", Amount: 50000, Credit: true},
		"admin-debit":   {Type: economy.TypeAdminDebit, GuildID: 7, ServerID: 1, PlayerName: "Semillita-azul-_", Amount: 25000},
		"shop-purchase": {Type: economy.TypeShopPurchase, GuildID: 7, ServerID: 1, PlayerName: "Semillita-azul-_", Amount: 750, Item: "Care Package"},
	}
	for tname, cfg := range economyTemplates {
		for ename, ev := range economyEvents {
			cases = append(cases, designerCase{"ECONOMY/" + tname + "/" + ename, "ECONOMY", cfg, economyVars(ev, "Northstar")})
		}
	}

	connectionTemplates := map[string]embedtemplates.Config{
		"website-default": websiteDefault("CONNECTIONS", "#4C87A0", "Player connection", "{{player}} {{event}} the server.", tmplField("server", "Server", "{{server_name}}", false, 0)),
		"every-variable":  {RouteKey: "CONNECTIONS", Enabled: true, Color: "#3F8F68", Description: embedtemplates.Text{Enabled: true, Template: "{{player}} {{event}} ({{event_type}})\nSession {{session}}\n{{server_name}} {{timestamp}}"}},
	}
	connectionEvents := map[string]killfeed.ConnectionNotice{
		"joined":            {Kind: killfeed.ConnectionConnected, Name: "WilliamAle--10"},
		"left-with-session": {Kind: killfeed.ConnectionDisconnected, Name: "Semillita-azul-_", Session: 72 * time.Minute},
		"left-no-session":   {Kind: killfeed.ConnectionDisconnected, Name: "Semillita-azul-_"},
	}
	for tname, cfg := range connectionTemplates {
		for ename, ev := range connectionEvents {
			cases = append(cases, designerCase{"CONNECTIONS/" + tname + "/" + ename, "CONNECTIONS", cfg, stamp(connectionVars(ev, "Northstar"))})
		}
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].id < cases[j].id })
	return cases
}

// renderDesignerCase renders a saved template the way production does: validated as the
// renderer validates a stored template, then RenderEvent - the one function live publishing,
// the designer's preview and its test send all call.
func renderDesignerCase(t *testing.T, c designerCase) designerGoldenEntry {
	t.Helper()
	cfg, err := embedtemplates.Validate(c.cfg, c.route)
	if err != nil {
		t.Fatalf("%s: the fixture template must be a valid saved template: %v", c.id, err)
	}
	embed, err := embedrender.RenderEvent(cfg, c.route, c.vars, designerGoldenAt)
	if err != nil {
		t.Fatalf("%s: render: %v", c.id, err)
	}
	raw, err := json.Marshal(embed)
	if err != nil {
		t.Fatal(err)
	}
	return designerGoldenEntry{Route: c.route, Vars: c.vars, Embed: raw}
}

func TestSavedTemplatesRenderByteForByteAsBefore(t *testing.T) {
	cases := designerCases()
	routes := map[string]bool{}
	got := map[string]designerGoldenEntry{}
	for _, c := range cases {
		if _, dup := got[c.id]; dup {
			t.Fatalf("duplicate fixture id %s", c.id)
		}
		got[c.id] = renderDesignerCase(t, c)
		routes[c.route] = true
	}
	// Every route that renders custom templates at runtime is covered.
	for _, route := range embedrender.SupportedRoutes() {
		if !routes[route] {
			t.Errorf("route %s renders saved templates at runtime but has no fixture here", route)
		}
	}

	if out := os.Getenv("EMBED_DESIGNER_GOLDEN_OUT"); out != "" {
		if !filepath.IsAbs(out) {
			t.Fatalf("EMBED_DESIGNER_GOLDEN_OUT must be an absolute path, got %q", out)
		}
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d golden entries to %s", len(got), out)
		return
	}

	raw, err := os.ReadFile(designerGoldenFile)
	if err != nil {
		t.Fatalf("the golden file is part of the contract and must exist: %v", err)
	}
	var want map[string]designerGoldenEntry
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("golden file: %v", err)
	}
	if len(want) != len(got) {
		t.Errorf("the golden file has %d entries, the fixtures produce %d", len(want), len(got))
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("%s: in the golden file but no longer produced", id)
			continue
		}
		// (b) no variable was renamed, removed, added to this event, or resolves differently.
		if !reflect.DeepEqual(g.Vars, w.Vars) {
			t.Errorf("%s: the event's template variables changed\n got: %v\nwant: %v", id, g.Vars, w.Vars)
		}
		// (a) the rendered embed is the same, byte for byte. The golden bytes are compacted
		// because the file is stored indented; the embed JSON itself has no optional spacing.
		var compact bytes.Buffer
		if err := json.Compact(&compact, w.Embed); err != nil {
			t.Fatalf("%s: golden embed: %v", id, err)
		}
		if !bytes.Equal(g.Embed, compact.Bytes()) {
			t.Errorf("%s: a saved template no longer renders byte for byte as before\n got: %s\nwant: %s", id, g.Embed, compact.Bytes())
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("%s: produced but missing from the golden file (see the comment at the top of this file)", id)
		}
	}
}

// goldenTemplateSource serves one saved template for one route, like the repository does.
type goldenTemplateSource struct {
	route string
	cfg   embedtemplates.Config
}

func (s goldenTemplateSource) ResolveTemplate(_ context.Context, _, _ int64, routeKey string) (int64, *embedtemplates.Config, error) {
	if routeKey != s.route {
		return 3, nil, nil
	}
	cfg := s.cfg
	return 3, &cfg, nil
}

// A saved template wins over the built-in card on the live kill path: what the publisher posts
// is the template's rendering, not the built-in card - whatever the built-in card looks like.
func TestSavedTemplateTakesPriorityOverTheBuiltInCard(t *testing.T) {
	for tname, cfg := range designerKillTemplates() {
		for ename, ev := range designerKillEvents() {
			renderer := embedrender.New(embedrender.Options{Source: goldenTemplateSource{route: "KILLFEED", cfg: cfg}, Enabled: true})
			p := &KillfeedPublisher{}
			p.SetRouting(nil, 7, 1)
			p.SetCustomizer(renderer, "Northstar")
			card := p.Card(ev)
			builtIn := BuildKillEmbed(ev)
			if reflect.DeepEqual(card, builtIn) {
				t.Fatalf("%s/%s: the built-in card was posted although a saved template exists", tname, ename)
			}
			// Apart from the publish time, it is exactly the golden rendering.
			vars := killfeedVars(ev, "Northstar")
			valid, err := embedtemplates.Validate(cfg, "KILLFEED")
			if err != nil {
				t.Fatal(err)
			}
			want, err := embedrender.RenderEvent(valid, "KILLFEED", vars, designerGoldenAt)
			if err != nil {
				t.Fatal(err)
			}
			blankPublishTime(card, vars["timestamp"])
			blankPublishTime(want, vars["timestamp"])
			if !reflect.DeepEqual(card, want) {
				t.Fatalf("%s/%s: the live path posts something other than the saved template's rendering\n got: %+v\nwant: %+v", tname, ename, card, want)
			}
			if s := renderer.Stats(); s.CustomRender != 1 || s.DefaultRender != 0 || s.FallbackRender != 0 {
				t.Fatalf("%s/%s: expected exactly one custom render, got %+v", tname, ename, s)
			}
		}
	}
}

// blankPublishTime removes the one value that differs between two renders made a moment
// apart: the {{timestamp}} text and the embed timestamp.
func blankPublishTime(e *discordgo.MessageEmbed, stamp string) {
	e.Timestamp = ""
	if stamp == "" {
		return
	}
	// The publish time is compared to the minute by length only: both renders carry a stamp
	// of the same shape in the same place.
	replace := func(s string) string {
		for i := 0; i+len(designerGoldenStamp) <= len(s); i++ {
			if s[i+4] == '-' && s[i+7] == '-' && s[i+10] == ' ' && s[i+len(designerGoldenStamp)-3:i+len(designerGoldenStamp)] == "UTC" {
				return s[:i] + designerGoldenStamp + s[i+len(designerGoldenStamp):]
			}
		}
		return s
	}
	e.Title, e.Description = replace(e.Title), replace(e.Description)
	for _, f := range e.Fields {
		f.Name, f.Value = replace(f.Name), replace(f.Value)
	}
	if e.Footer != nil {
		e.Footer.Text = replace(e.Footer.Text)
	}
}
