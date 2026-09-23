package discord

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

func killEv(victim, killer, weapon string, dist float64, hitZone string) *killfeed.Event {
	d := dist
	return &killfeed.Event{
		Type:      killfeed.EventPlayerKill,
		Timestamp: time.Date(2026, 9, 8, 16, 40, 12, 0, time.UTC),
		Victim:    &killfeed.PlayerRef{Name: victim, ID: "vid-1"},
		Killer:    &killfeed.PlayerRef{Name: killer, ID: "kid-1"},
		Weapon:    weapon,
		Distance:  &d,
		HitZone:   hitZone,
	}
}

// fieldsContain reports whether any field named name has a value containing substr.
func fieldsContain(fields []*discordgo.MessageEmbedField, name, substr string) bool {
	for _, f := range fields {
		if f.Name == name && strings.Contains(f.Value, substr) {
			return true
		}
	}
	return false
}

func fieldNamed(e *discordgo.MessageEmbed, name string) *discordgo.MessageEmbedField {
	for _, f := range e.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// allText is every visible string of a card, for leak/duplication checks.
func allText(e *discordgo.MessageEmbed) string {
	parts := []string{e.Title, e.Description}
	if e.Author != nil {
		parts = append(parts, e.Author.Name)
	}
	if e.Footer != nil {
		parts = append(parts, e.Footer.Text)
	}
	for _, f := range e.Fields {
		parts = append(parts, f.Name, f.Value)
	}
	return strings.Join(parts, "\n")
}

// assertV2Card checks the shared Champion V2 card contract.
func assertV2Card(t *testing.T, e *discordgo.MessageEmbed) {
	t.Helper()
	if e.Author == nil || e.Author.Name != presentation.AuthorName {
		t.Fatalf("author must carry the brand once: %#v", e.Author)
	}
	if e.Footer == nil || !strings.HasSuffix(e.Footer.Text, presentation.ChampionSlogan) {
		t.Fatalf("footer must be the slogan: %#v", e.Footer)
	}
	text := allText(e)
	if strings.Contains(text, "CHAMPION KILLFEED") {
		t.Fatalf("legacy repeated brand in card: %q", text)
	}
	if strings.Contains(e.Title, "CHAMPION") {
		t.Fatalf("title must be the event only: %q", e.Title)
	}
	if strings.Contains(text, "────") {
		t.Fatal("ASCII divider in card")
	}
	for _, f := range e.Fields {
		if f.Name == "\u200b" || f.Name == "" {
			t.Fatalf("spacer field in card: %#v", f)
		}
		if strings.Contains(f.Value, "Kills:") || strings.Contains(f.Value, "Deaths:") || strings.Contains(f.Value, "Value") {
			t.Fatalf("debug-style key/value in field: %q", f.Value)
		}
	}
	assertWithinLimits(t, e)
}

func assertWithinLimits(t *testing.T, e *discordgo.MessageEmbed) {
	t.Helper()
	if utf8.RuneCountInString(e.Title) > presentation.LimitTitle || utf8.RuneCountInString(e.Description) > presentation.LimitDescription {
		t.Fatal("title/description over Discord limit")
	}
	if len(e.Fields) > presentation.LimitFields {
		t.Fatalf("%d fields", len(e.Fields))
	}
	for _, f := range e.Fields {
		if utf8.RuneCountInString(f.Name) > presentation.LimitFieldName || utf8.RuneCountInString(f.Value) > presentation.LimitFieldValue {
			t.Fatalf("field over limit: %d/%d", utf8.RuneCountInString(f.Name), utf8.RuneCountInString(f.Value))
		}
	}
	if e.Footer != nil && utf8.RuneCountInString(e.Footer.Text) > presentation.LimitFooter {
		t.Fatal("footer over limit")
	}
	if presentation.EmbedLength(e) > presentation.LimitTotal {
		t.Fatalf("combined embed %d > 6000", presentation.EmbedLength(e))
	}
}

func TestStandardKillCardV2(t *testing.T) {
	e := BuildKillEmbed(FixtureStandardKill())
	assertV2Card(t, e)
	if e.Title != "☠️ PLAYER ELIMINATED" || e.Color != presentation.ChampionGold {
		t.Fatalf("title/color: %q %x", e.Title, e.Color)
	}
	if !strings.HasPrefix(e.Description, `**WilliamAle--10** → **Semillita-azul-\_**`) {
		t.Fatalf("matchup must lead the description: %q", e.Description)
	}
	if !strings.Contains(e.Description, "🔫 **Controlled Burst**\n`M4-A1` • 11.7m • Close Quarters") {
		t.Fatalf("weapon block: %q", e.Description)
	}
	if e.Footer.Text != "EVERY KILL TELLS A STORY" || e.Timestamp == "" {
		t.Fatalf("footer/timestamp: %q %q", e.Footer.Text, e.Timestamp)
	}
	// Compact: KILLER | VICTIM | H2H side by side, then FINAL HIT.
	if len(e.Fields) < 2 || len(e.Fields) > 6 {
		t.Fatalf("standard kill should have 2-6 fields, got %d", len(e.Fields))
	}
	k, v, h := fieldNamed(e, "KILLER"), fieldNamed(e, "VICTIM"), fieldNamed(e, "H2H")
	if k == nil || v == nil || h == nil || !k.Inline || !v.Inline || !h.Inline {
		t.Fatalf("killer/victim/H2H must be inline side by side: %#v", e.Fields)
	}
	if k.Value != "**9 K** • **0 D** • **9.00 K/D**\n🔥 Streak **1**" || v.Value != "**2 K** • **2 D** • **1.00 K/D**" {
		t.Fatalf("compact stats: %q / %q", k.Value, v.Value)
	}
	if h.Value != "**4–0**\nWilliamAle--10 leads" {
		t.Fatalf("H2H: %q", h.Value)
	}
	if !fieldsContain(e.Fields, "FINAL HIT", "Torso • 34.2 dmg") {
		t.Fatalf("final hit: %#v", e.Fields)
	}
	if strings.Count(allText(e), "KILLFEED") != 1 {
		t.Fatal("brand must appear exactly once")
	}
}

func TestHeadshotCardV2(t *testing.T) {
	e := BuildKillEmbed(FixtureHeadshotKill())
	assertV2Card(t, e)
	if e.Title != "🎯 HEADSHOT" || e.Color != presentation.CombatRed {
		t.Fatalf("title/color: %q %x", e.Title, e.Color)
	}
	if strings.Contains(e.Description, "🎯 Headshot") {
		t.Fatal("primary story must not be repeated as a badge")
	}
	if fieldNamed(e, "FINAL HIT") != nil {
		t.Fatal("FINAL HIT Head would repeat the title")
	}
	if !strings.Contains(e.Description, "_Precision finish_") {
		t.Fatalf("story line: %q", e.Description)
	}
}

func TestLongshotCardV2(t *testing.T) {
	e := BuildKillEmbed(FixtureLongshotKill())
	assertV2Card(t, e)
	if e.Title != "🎯 LONGSHOT" || e.Color != presentation.Steel {
		t.Fatalf("title/color: %q %x", e.Title, e.Color)
	}
	if len(e.Fields) == 0 || e.Fields[0].Name != "DISTANCE" || e.Fields[0].Value != "**173.8m**" {
		t.Fatalf("distance must be the hero metric: %#v", e.Fields)
	}
	if strings.Count(allText(e), "173.8m") != 1 {
		t.Fatal("distance must not be repeated")
	}
	if strings.Contains(e.Description, "Long Shot") {
		t.Fatal("longshot story must not be repeated as a badge")
	}
}

func TestExtremeRangeCardV2(t *testing.T) {
	e := BuildKillEmbed(FixtureExtremeRangeKill())
	assertV2Card(t, e)
	if e.Title != "👑 EXTREME RANGE" || e.Color != presentation.EventGold {
		t.Fatalf("title/color: %q %x", e.Title, e.Color)
	}
	if e.Fields[0].Name != "DISTANCE" || e.Fields[0].Value != "**247.3m**" {
		t.Fatalf("hero distance: %#v", e.Fields[0])
	}
	// The head hit survives as a secondary badge, exactly once.
	if !strings.Contains(e.Description, "🎯 Headshot") || strings.Count(allText(e), "Head") != 1 {
		t.Fatalf("secondary headshot badge once: %q", allText(e))
	}
	if fieldNamed(e, "FINAL HIT") != nil {
		t.Fatal("FINAL HIT Head would repeat the headshot badge")
	}
}

func TestBountyClaimCardV2(t *testing.T) {
	e := BuildKillEmbed(FixtureBountyKill())
	assertV2Card(t, e)
	if e.Title != "💰 BOUNTY CLAIMED" || e.Color != presentation.EventGold {
		t.Fatalf("title/color: %q %x", e.Title, e.Color)
	}
	if !fieldsContain(e.Fields, "REWARD", "**12,500 pts**") {
		t.Fatalf("reward in Champion Points: %#v", e.Fields)
	}
}

func TestKillingSpreeCardV2(t *testing.T) {
	e := BuildKillEmbed(FixtureKillingSpreeKill())
	assertV2Card(t, e)
	if e.Title != "🔥 KILLING SPREE" {
		t.Fatalf("title: %q", e.Title)
	}
	if !fieldsContain(e.Fields, "STREAK", "**5**") {
		t.Fatalf("streak hero: %#v", e.Fields)
	}
	if strings.Contains(fieldNamed(e, "KILLER").Value, "Streak") || strings.Contains(e.Description, "Killing Spree") {
		t.Fatal("streak must not be duplicated")
	}
}

func TestStreakEndedCardV2(t *testing.T) {
	e := BuildKillEmbed(FixtureStreakEndedKill())
	assertV2Card(t, e)
	if e.Title != "💀 STREAK ENDED" {
		t.Fatalf("title: %q", e.Title)
	}
	if !strings.Contains(e.Description, "Ended a **8-kill streak**") {
		t.Fatalf("ended streak from persisted count: %q", e.Description)
	}
	// Without the persisted count, no number is invented.
	ev := FixtureStreakEndedKill()
	ev.EndedStreakCount = nil
	if strings.Contains(BuildKillEmbed(ev).Description, "-kill streak") {
		t.Fatal("streak length must not be fabricated")
	}
}

func TestExtremeRangeBeatsHeadshot(t *testing.T) {
	e := BuildKillEmbed(killEv("V", "K", "M4-A1", 250.0, "Head"))
	if e.Title != "👑 EXTREME RANGE" || !strings.Contains(e.Description, "🎯 Headshot") {
		t.Fatalf("extreme range primary with a secondary headshot badge: %q / %q", e.Title, e.Description)
	}
}

func TestHeadshotBeatsCloseRange(t *testing.T) {
	e := BuildKillEmbed(killEv("V", "K", "M4-A1", 8.0, "Head"))
	if e.Title != "🎯 HEADSHOT" || !strings.Contains(e.Description, "🔥 Close Range") {
		t.Fatalf("headshot primary with close-range badge: %q / %q", e.Title, e.Description)
	}
}

func TestSecondaryBadgesAreCappedAndNeverDuplicated(t *testing.T) {
	ev := killEv("V", "K", "M4-A1", 250.0, "Head")
	ev.BountyTarget = true
	ev.StreakEnded = true
	ev.ActiveEventBadges = []string{"🏆 King of NWAF", "🏆 Double Points"}
	ev.WarBadge = "⚔️ War"
	p := BuildPresentation(ev)
	if len(p.Badges) > maxVisibleBadges {
		t.Fatalf("badges not capped: %v", p.Badges)
	}
	seen := map[string]bool{}
	for _, b := range p.Badges {
		if seen[b] {
			t.Fatalf("duplicate badge %q", b)
		}
		seen[b] = true
	}
}

func TestDistanceRoundingAndPrecision(t *testing.T) {
	ev := killEv("V", "K", "M4-A1", 62.1978, "Torso")
	e := BuildKillEmbed(ev)
	if !strings.Contains(e.Description, "62.2m") {
		t.Fatalf("expected distance rounded to 62.2m, got %q", e.Description)
	}
	if *ev.Distance != 62.1978 {
		t.Fatalf("expected full precision retained internally, got %v", *ev.Distance)
	}
}

func TestKillStatFieldsOnlyRenderWhenPresent(t *testing.T) {
	e := BuildKillEmbed(&killfeed.Event{Killer: &killfeed.PlayerRef{Name: "K"}, Victim: &killfeed.PlayerRef{Name: "V"}})
	for _, name := range []string{"KILLER", "VICTIM", "H2H", "FINAL HIT", "DISTANCE"} {
		if fieldNamed(e, name) != nil {
			t.Fatalf("%s rendered without source data", name)
		}
	}
	assertV2Card(t, e)
	if strings.Contains(allText(e), "N/A") || strings.Contains(allText(e), "Unknown Ammo") {
		t.Fatal("placeholder values must be omitted")
	}
}

func TestZeroDeathKDFormattingUsesCombatRecordKD(t *testing.T) {
	ev := FixtureStandardKill()
	e := BuildKillEmbed(ev)
	want := presentation.FormatKD(ev.KillerStats.KD()) + " K/D"
	if !strings.Contains(fieldNamed(e, "KILLER").Value, want) {
		t.Fatalf("K/D must be CombatRecord.KD() formatted: %q", fieldNamed(e, "KILLER").Value)
	}
}

func TestCoordinatesOnlyWhenExplicitlyEnabled(t *testing.T) {
	ev := &killfeed.Event{Killer: &killfeed.PlayerRef{Name: "K", Position: &killfeed.Position{X: 4496, Y: 2, Z: 3}}, Victim: &killfeed.PlayerRef{Name: "V"}}
	if strings.Contains(allText(BuildKillEmbed(ev)), "4496") {
		t.Fatal("coordinates must be private by default")
	}
	if !fieldsContain(BuildKillEmbedWithOptions(ev, KillEmbedOptions{LocationMode: LocationCoordinates}).Fields, "LOCATION", "4496.0 • 2.0 • 3.0") {
		t.Fatal("expected coordinates in explicit mode")
	}
}

func TestMentionAndMarkdownSafety(t *testing.T) {
	names := []string{"@everyone", "@here", "<@&123456>", "<#987654>", "**bold**_x_`y`", "a\u202Eb\u200bc", "ctl\x07x"}
	for _, n := range names {
		ev := killEv(n, n, "M4-A1", 10, "Torso")
		ev.KillerStats = &killfeed.CombatRecord{Kills: 1}
		ev.Encounters = &killfeed.HeadToHead{KillerWins: 1}
		ev.SeasonName = n
		for _, e := range []*discordgo.MessageEmbed{BuildKillEmbed(ev), BuildDeathEmbed(&killfeed.Event{Type: killfeed.EventPlayerDeath, Player: &killfeed.PlayerRef{Name: n}})} {
			text := allText(e)
			for _, bad := range []string{"@everyone", "@here", "<@&", "<#", "\u202E", "\u200b", "\x07"} {
				if strings.Contains(text, bad) {
					t.Fatalf("%q leaked %q into %q", n, bad, text)
				}
			}
			if strings.Contains(e.Description, "**bold**") || strings.Contains(fieldsText(e), "**bold**") { // footers never render markdown
				t.Fatalf("markdown not escaped: %q", text)
			}
		}
	}
	// Player IDs and raw ADM never appear.
	ev := killEv("V", "K", "M4-A1", 10, "Torso")
	ev.Raw = `Player "V" (DEAD) (id=vid-1 pos=<1.0, 2.0, 3.0>)`
	text := allText(BuildKillEmbed(ev))
	for _, bad := range []string{"vid-1", "kid-1", "pos=", "DEAD"} {
		if strings.Contains(text, bad) {
			t.Fatalf("%q leaked into card", bad)
		}
	}
}

func TestEmbedLimitsWithMaximalInputs(t *testing.T) {
	long := strings.Repeat("VeryLongPlayerNameWithUnicode🎯_*", 40)
	ev := killEv(long, long, strings.Repeat("W", 3000), 5, strings.Repeat("Z", 500))
	ev.SeasonName = long
	ev.ActiveEventBadges = []string{long, long, long, long}
	ev.WarBadge = long
	ev.Ammo = long
	ev.Damage = fptr(1e9)
	ev.KillerStats = &killfeed.CombatRecord{Kills: 1 << 60, Deaths: 1 << 60}
	ev.VictimStats = &killfeed.CombatRecord{Kills: 1 << 60}
	ev.KillerStreak = iptr(1 << 30)
	ev.Encounters = &killfeed.HeadToHead{KillerWins: 1 << 30, VictimWins: 1}
	e := BuildKillEmbedWithOptions(ev, KillEmbedOptions{LocationMode: LocationCoordinates})
	if e == nil {
		t.Fatal("expected an embed even for extreme inputs")
	}
	assertWithinLimits(t, e)
	d := BuildDeathEmbed(&killfeed.Event{Type: killfeed.EventSuicideAction, Player: &killfeed.PlayerRef{Name: long}, Weapon: long, HitZone: long, SeasonName: long, PlayerStats: &killfeed.CombatRecord{Kills: 1}})
	assertWithinLimits(t, d)
}

func TestTimestampSetOnlyWhenAbsolute(t *testing.T) {
	if BuildKillEmbed(killEv("V", "K", "M4-A1", 10, "Torso")).Timestamp == "" {
		t.Fatal("expected embed timestamp from valid absolute event time")
	}
	e := BuildKillEmbed(&killfeed.Event{Type: killfeed.EventPlayerKill, TimeOfDay: "16:40:12", Victim: &killfeed.PlayerRef{Name: "V"}, Killer: &killfeed.PlayerRef{Name: "K"}})
	if e.Timestamp != "" {
		t.Fatalf("expected no timestamp when only time-of-day is known, got %q", e.Timestamp)
	}
}

func TestSeasonFooter(t *testing.T) {
	ev := FixtureStandardKill()
	ev.SeasonName = "Season 3"
	if got := BuildKillEmbed(ev).Footer.Text; got != "CHAMPION • Season 3 • EVERY KILL TELLS A STORY" {
		t.Fatalf("season footer: %q", got)
	}
}

func TestSpecialKillTemplateVariableUnchanged(t *testing.T) {
	// {{special_kill}} is fed from the story Hero; saved templates depend on it.
	cases := map[*killfeed.Event]string{
		FixtureHeadshotKill():     "HEADSHOT CONFIRMED",
		FixtureLongshotKill():     "LONG RANGE SHOT",
		FixtureExtremeRangeKill(): "EXTREME RANGE ELIMINATION",
		FixtureBountyKill():       "BOUNTY CLAIMED",
	}
	for ev, want := range cases {
		if got := killfeedVars(ev, "")["special_kill"]; got != want {
			t.Fatalf("special_kill = %q, want %q", got, want)
		}
	}
	if _, ok := killfeedVars(FixtureStandardKill(), "")["special_kill"]; ok {
		t.Fatal("a standard kill has no special_kill")
	}
}

func TestDeathCardV2(t *testing.T) {
	e := BuildDeathEmbed(FixtureDeath())
	assertV2Card(t, e)
	if e.Title != "☠️ PLAYER DEATH" || e.Description != "**Ceiyxe**" || e.Color != presentation.NeutralGraphite {
		t.Fatalf("death card: %q %q %x", e.Title, e.Description, e.Color)
	}
	if len(e.Fields) != 1 || e.Fields[0].Name != "PLAYER STATS" || e.Fields[0].Value != "**1 K** • **50 D** • **0.02 K/D**" {
		t.Fatalf("compact stats only: %#v", e.Fields)
	}
	for _, f := range e.Fields {
		if f.Name == "CAUSE" || f.Name == "DEATH DETAILS" {
			t.Fatal("no cause may be fabricated")
		}
	}
	// No stats -> no fields at all.
	bare := BuildDeathEmbed(&killfeed.Event{Type: killfeed.EventPlayerDeath, Player: &killfeed.PlayerRef{Name: "victim1"}})
	if len(bare.Fields) != 0 {
		t.Fatalf("expected no fields without data: %#v", bare.Fields)
	}
}

func TestDeathCardProvenCauseAndFinalHit(t *testing.T) {
	e := BuildDeathEmbed(&killfeed.Event{Type: killfeed.EventPlayerDeath, Player: &killfeed.PlayerRef{Name: "P"}, Cause: killfeed.DeathCauseInfected, HitZone: "Head"})
	c, h := fieldNamed(e, "CAUSE"), fieldNamed(e, "FINAL HIT")
	if c == nil || c.Value != "Infected" || !c.Inline || h == nil || h.Value != "Head" || !h.Inline {
		t.Fatalf("cause/final hit inline: %#v", e.Fields)
	}
}

func TestSuicideCardV2(t *testing.T) {
	e := BuildDeathEmbed(FixtureSuicide())
	assertV2Card(t, e)
	if e.Title != "💀 SUICIDE" || e.Color != presentation.WarningAmber {
		t.Fatalf("title/color: %q %x", e.Title, e.Color)
	}
	if fieldNamed(e, "CAUSE") != nil {
		t.Fatal("the title already says suicide; no cause field")
	}
	if !fieldsContain(e.Fields, "WEAPON", "`M4-A1`") || !fieldsContain(e.Fields, "PLAYER STATS", "**1 K**") || fieldsContain(e.Fields, "PLAYER STATS", "Streak") {
		t.Fatalf("suicide fields: %#v", e.Fields)
	}
	if len(e.Fields) > 3 {
		t.Fatalf("death cards stay within 0-3 fields: %d", len(e.Fields))
	}
}

func fieldsText(e *discordgo.MessageEmbed) string {
	var b strings.Builder
	for _, f := range e.Fields {
		b.WriteString(f.Name + "\n" + f.Value + "\n")
	}
	return b.String()
}
