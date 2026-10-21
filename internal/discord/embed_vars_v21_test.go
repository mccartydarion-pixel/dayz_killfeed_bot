package discord

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

func richKill() *killfeed.Event {
	dist, dmg := 11.7, 98.4
	streak, ended := 1, 8
	return &killfeed.Event{
		Type:   killfeed.EventPlayerKill,
		Killer: &killfeed.PlayerRef{Name: "WilliamAle--10"}, Victim: &killfeed.PlayerRef{Name: "Semillita-azul-_"},
		Weapon: "M4-A1", Ammo: "5.56x45", Distance: &dist, HitZone: "Head", Damage: &dmg,
		KillerStats: &killfeed.CombatRecord{Kills: 9, Deaths: 0}, VictimStats: &killfeed.CombatRecord{Kills: 2, Deaths: 2},
		KillerStreak: &streak, Encounters: &killfeed.HeadToHead{KillerWins: 4, VictimWins: 0},
		KillingSpree: true, StreakEnded: true, EndedStreakCount: &ended,
		BountyTarget: true, BountyClaimed: true, BountyPoints: 500,
		ActiveEventBadges: []string{"Double Kill", " ", "NWAF Event"}, WarBadge: "WAR KILL", SeasonName: "Season 3",
	}
}

func TestKillfeedVariablesComeFromTheEvent(t *testing.T) {
	ev := richKill()
	m := killfeedVars(ev, "Champions Deathmatch")
	story := BuildPresentation(ev)
	want := map[string]string{
		"killer": "WilliamAle--10", "victim": "Semillita-azul-_", "weapon": "M4-A1", "ammo": "5.56x45",
		"weapon_category": "Assault Rifle", "distance": "11.7m", "range": "CLOSE QUARTERS", "hit_zone": "Head", "damage": "98.4", "headshot": "HEADSHOT",
		"killer_kills": "9", "killer_deaths": "0", "killer_kd": "9.00", "killer_streak": "1", "streak": "1",
		"victim_kills": "2", "victim_deaths": "2", "victim_kd": "1.00",
		"h2h_killer_wins": "4", "h2h_victim_wins": "0", "h2h_score": "4–0",
		"ended_streak": "8", "killing_spree": "KILLING SPREE", "streak_ended": "STREAK ENDED",
		"kill_type": story.Title, "story_title": story.Icon + " " + story.Title, "special_kill": story.Hero,
		"bounty_amount": "500", "bounty_target": "MOST WANTED", "bounty_claimed": "BOUNTY CLAIMED",
		"season_name": "Season 3", "war_badge": "WAR KILL", "event_badges": "Double Kill • NWAF Event",
		"server_name": "Champions Deathmatch",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	// Every produced variable is approved for the route (none invented).
	approved := map[string]bool{}
	for _, name := range embedtemplates.Variables("KILLFEED") {
		approved[name] = true
	}
	for k := range m {
		if !approved[k] {
			t.Errorf("%s is not an approved KILLFEED variable", k)
		}
	}
	if presentation.FormatKD(ev.KillerStats.KD()) != m["killer_kd"] {
		t.Fatal("killer_kd must be CombatRecord.KD formatted by FormatKD")
	}
}

func TestKillfeedVariablesAreAbsentWithoutData(t *testing.T) {
	ev := &killfeed.Event{Type: killfeed.EventPlayerKill, Killer: &killfeed.PlayerRef{Name: "A"}, Victim: &killfeed.PlayerRef{Name: "B"}}
	m := killfeedVars(ev, "")
	for _, k := range []string{"killer_kills", "killer_deaths", "killer_kd", "victim_kills", "victim_deaths", "victim_kd", "killer_streak", "streak",
		"ended_streak", "killing_spree", "streak_ended", "h2h_killer_wins", "h2h_victim_wins", "h2h_score", "hit_zone", "damage", "headshot",
		"range", "weapon_category", "bounty_amount", "bounty_target", "bounty_claimed", "season_name", "war_badge", "event_badges", "special_kill"} {
		if v, ok := m[k]; ok {
			t.Errorf("%s must be absent, not fabricated: %q", k, v)
		}
	}
	// A streak that ended without a known count shows no ended_streak (never 0).
	ended := &killfeed.Event{Type: killfeed.EventPlayerKill, StreakEnded: true}
	if _, ok := killfeedVars(ended, "")["ended_streak"]; ok {
		t.Fatal("ended_streak needs EndedStreakCount")
	}
	if formatDamage(98) != "98" || formatDamage(98.44) != "98.4" {
		t.Fatalf("damage format: %q %q", formatDamage(98), formatDamage(98.44))
	}
}

func renderKill(t *testing.T, cfg embedtemplates.Config, ev *killfeed.Event) map[string]string {
	t.Helper()
	cfg.Enabled, cfg.Color = true, "#D4AF37"
	e, err := embedrender.RenderEvent(cfg, "KILLFEED", killfeedVars(ev, "Champions Deathmatch"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range e.Fields {
		out[f.Name] = f.Value
	}
	out["_description"] = e.Description
	return out
}

func TestSpecExampleTemplates(t *testing.T) {
	cfg := embedtemplates.Config{Fields: []embedtemplates.Field{
		{Key: "k", Label: "KILLER STATS", Enabled: true, Order: 0, Template: `Kills: {{killer_kills}}\nDeaths: {{killer_deaths}}\nK/D: {{killer_kd}}\nStreak: {{killer_streak}}`},
		{Key: "v", Label: "VICTIM STATS", Enabled: true, Order: 1, Template: `Kills: {{victim_kills}}\nDeaths: {{victim_deaths}}\nK/D: {{victim_kd}}`},
		{Key: "h", Label: "H2H", Enabled: true, Order: 2, Template: `{{h2h_score}}`},
	}}
	got := renderKill(t, cfg, richKill())
	if got["KILLER STATS"] != "Kills: 9\nDeaths: 0\nK/D: 9.00\nStreak: 1" {
		t.Errorf("killer stats: %q", got["KILLER STATS"])
	}
	if got["VICTIM STATS"] != "Kills: 2\nDeaths: 2\nK/D: 1.00" {
		t.Errorf("victim stats: %q", got["VICTIM STATS"])
	}
	if got["H2H"] != "4–0" {
		t.Errorf("h2h: %q", got["H2H"])
	}

	// No combat record loaded: the stats fields are omitted, never shown as zeros.
	bare := richKill()
	bare.KillerStats, bare.VictimStats = nil, nil
	got = renderKill(t, cfg, bare)
	if _, ok := got["KILLER STATS"]; ok {
		t.Error("killer stats must be omitted without a combat record")
	}
	if _, ok := got["VICTIM STATS"]; ok {
		t.Error("victim stats must be omitted without a combat record")
	}
}

// The variable builders run on every kill and hit: they must read only the event.
// Their bodies reference no repository, store, database or context - so they cannot
// issue a query per Discord event.
func TestVariableBuildersHaveNoDatabaseAccess(t *testing.T) {
	checkNoDB(t, "embed_custom.go", "killfeedVars", "hitfeedVars", "pveVars", "connectionVars", "bountyVars", "economyVars")
	checkNoDB(t, "../embedrender/render.go", "Render", "ExpandNewlines", "substitute")
	checkNoDB(t, "../embedrender/renderer.go", "RenderEvent", "approvedOnly")
}

func checkNoDB(t *testing.T, file string, funcs ...string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, fn := range funcs {
		want[fn] = true
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || !want[fd.Name.Name] {
			continue
		}
		delete(want, fd.Name.Name)
		ast.Inspect(fd, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				switch id.Name {
				case "repository", "pgxpool", "pgx", "sql", "ctx", "context", "store", "Store", "Query", "QueryRow", "Exec":
					t.Errorf("%s in %s references %q", fd.Name.Name, file, id.Name)
				}
			}
			return true
		})
	}
	for fn := range want {
		t.Errorf("%s not found in %s", fn, file)
	}
}

func TestKillfeedHitLabelAndTimeAgo(t *testing.T) {
	ev := richKill() // a head hit
	ev.Timestamp = time.Date(2026, 10, 10, 13, 41, 9, 0, time.UTC)
	m := killfeedVars(ev, "")
	if m["hit_label"] != "CRITICAL HIT" {
		t.Errorf("hit_label on a head hit = %q, want CRITICAL HIT", m["hit_label"])
	}
	if want := presentation.Timestamp(ev.Timestamp, 'R'); m["time_ago"] != want {
		t.Errorf("time_ago = %q, want the kill's own time %q", m["time_ago"], want)
	}

	body := richKill()
	body.HitZone = "Torso"
	if got := killfeedVars(body, "")["hit_label"]; got != "HIT" {
		t.Errorf("hit_label on a torso hit = %q, want HIT", got)
	}

	// No hit zone: no label, so a line built on it is left out. The time is always there.
	bare := &killfeed.Event{Type: killfeed.EventPlayerKill, Killer: &killfeed.PlayerRef{Name: "A"}, Victim: &killfeed.PlayerRef{Name: "B"}}
	m = killfeedVars(bare, "")
	if _, ok := m["hit_label"]; ok {
		t.Errorf("hit_label without a hit zone = %q, want absent", m["hit_label"])
	}
	if !strings.HasPrefix(m["time_ago"], "<t:") || !strings.HasSuffix(m["time_ago"], ":R>") {
		t.Errorf("time_ago without an event time = %q, want a Discord relative time", m["time_ago"])
	}
}

func TestKillfeedWeaponWithAmmo(t *testing.T) {
	for _, c := range []struct{ weapon, ammo, want string }{
		{"M4-A1", "Bullet_556x45", "M4-A1 (556x45)"},
		{"M4-A1", "", "M4-A1"},
		{"Mosin", " 762x54 ", "Mosin (762x54)"},
		{"", "Bullet_556x45", ""},
	} {
		if got := weaponWithAmmo(c.weapon, c.ammo); got != c.want {
			t.Errorf("weaponWithAmmo(%q, %q) = %q, want %q", c.weapon, c.ammo, got, c.want)
		}
	}

	// A kill line without ammunition: the weapon still shows, alone.
	ev := &killfeed.Event{Type: killfeed.EventPlayerKill, Killer: &killfeed.PlayerRef{Name: "A"}, Victim: &killfeed.PlayerRef{Name: "B"}, Weapon: "M4-A1"}
	m := killfeedVars(ev, "")
	if m["weapon_with_ammo"] != "M4-A1" {
		t.Errorf("weapon_with_ammo without ammunition = %q, want the weapon alone", m["weapon_with_ammo"])
	}
	if _, ok := m["ammo"]; ok {
		t.Errorf("ammo = %q, want absent", m["ammo"])
	}

	// The matched lethal hit supplies the ammunition the kill line left out.
	ev.FinalHit = &killfeed.FinalHit{Zone: "Head", Ammo: "Bullet_556x45"}
	m = killfeedVars(ev, "")
	if m["ammo"] != "Bullet_556x45" || m["weapon_with_ammo"] != "M4-A1 (556x45)" {
		t.Errorf("with a matched hit: ammo=%q weapon_with_ammo=%q", m["ammo"], m["weapon_with_ammo"])
	}

	// The kill line's own ammunition wins over the hit's.
	ev.Ammo = "Bullet_762x39"
	if got := killfeedVars(ev, "")["weapon_with_ammo"]; got != "M4-A1 (762x39)" {
		t.Errorf("kill-line ammunition must win: %q", got)
	}
}
