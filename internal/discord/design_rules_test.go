package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// previewSampleNames are the made-up names in the preview data. They are data, not labels, so
// the Title Case check ignores them.
var previewSampleNames = []string{
	"Example Server Alpha", "Example Server Bravo", "Example Community", "Example Hideout", "Example Wolves", "Sample Ravens",
	"Sample_Raven", "Fictional_Fox", "Made_Up_Moose", "Pretend_Panda", "Imaginary_Ibis", "Sniper Sunday", "North West Airfield", "Season 3", "Season 4",
	"Field Rations", "Diamond III", "Gold I",
}

// designRuleExempt lists preview cards that are deliberately not held to the built-in rules,
// with the reason (docs/DISCORD_DESIGN.md "Deliberate exceptions"). Keep it short.
var designRuleExempt = map[string]string{}

// TestDesignRules walks every card of the design preview and asserts the rules of
// docs/DISCORD_DESIGN.md: a palette colour, a calm author line, a sentence-case title with at
// most one leading emoji, sentence-case field labels, and one footer format.
func TestDesignRules(t *testing.T) {
	cards := designPreviewCards()
	if len(cards) < 40 {
		t.Fatalf("the preview set should cover the bot's messages, it has only %d cards", len(cards))
	}
	checked := 0
	for _, c := range cards {
		if reason, ok := designRuleExempt[c.ID]; ok {
			t.Logf("%s is exempt: %s", c.ID, reason)
			continue
		}
		embeds := c.Embeds
		if len(embeds) == 0 && c.Embed != nil {
			embeds = []*discordgo.MessageEmbed{c.Embed}
		}
		for i, e := range embeds {
			checked++
			for _, problem := range presentation.CheckEmbed(e) {
				t.Errorf("%s (embed %d): %s", c.ID, i, problem)
			}
			labels := []string{e.Title}
			for _, f := range e.Fields {
				labels = append(labels, f.Name)
			}
			for _, label := range labels {
				if words := presentation.TitleCaseWords(label, previewSampleNames...); len(words) > 0 {
					t.Errorf("%s (embed %d): label %q is in Title Case (%s); use sentence case", c.ID, i, label, strings.Join(words, ", "))
				}
			}
			// A compact feed card has no title: its first description line is the bold header.
			if e.Title == "" && e.Description != "" {
				header := strings.SplitN(e.Description, "\n", 2)[0]
				if why := presentation.CheckLabel(header); why != "" {
					t.Errorf("%s (embed %d): header line %q %s", c.ID, i, header, why)
				}
			}
			if shouted := presentationShout(e.Description); shouted != "" {
				t.Errorf("%s (embed %d): the description shouts %q", c.ID, i, shouted)
			}
		}
		if c.Content != "" {
			if shouted := presentationShout(c.Content); shouted != "" {
				t.Errorf("%s: the message text shouts %q", c.ID, shouted)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no embed was checked")
	}
}

// presentationShout returns the first run of two or more shouted words in text ("" = none). A
// single capitalised token is allowed in body text: it can be a player's name or a weapon.
func presentationShout(text string) string {
	for _, name := range previewSampleNames {
		text = strings.ReplaceAll(text, name, "")
	}
	words := strings.Fields(text)
	for i := 0; i+1 < len(words); i++ {
		a, b := strings.Trim(words[i], "*_`.,:!?()"), strings.Trim(words[i+1], "*_`.,:!?()")
		if presentation.CheckLabel(a) != "" && presentation.CheckLabel(b) != "" && strings.Contains(presentation.CheckLabel(a), "shouts") && strings.Contains(presentation.CheckLabel(b), "shouts") {
			return a + " " + b
		}
	}
	return ""
}

// TestReplyVoice holds the shared reply sentences to the voice rules: calm, sentence case, no
// alarm emoji, no internal words, and a full stop at the end.
func TestReplyVoice(t *testing.T) {
	replies := map[string]string{
		"ReplyTryAgain":            ReplyTryAgain,
		"ReplyCommandUnavailable":  ReplyCommandUnavailable,
		"ReplyButtonExpired":       ReplyButtonExpired,
		"ReplyNotSetUp":            ReplyNotSetUp,
		"ReplyNoServerSelected":    ReplyNoServerSelected,
		"ReplyNeedsManageServer":   ReplyNeedsManageServer,
		"ReplyNotYourConfirmation": ReplyNotYourConfirmation,
		"ReplyPlayerNotFound":      ReplyPlayerNotFound,
		"ReplyChooseSubcommand":    ReplyChooseSubcommand,
		"ReplyNeedsPermission":     ReplyNeedsPermission("manage servers"),
		"ReplyIsUnavailable":       ReplyIsUnavailable("The economy"),
		"ReplyAreUnavailable":      ReplyAreUnavailable("Stats"),
		"ReplyCouldNot":            ReplyCouldNot("load your stats"),
		"ReplyCouldNotBecause":     ReplyCouldNotBecause("start the season", "a season is already active."),
		"ReplyNotLinked":           ReplyNotLinked("Link your gamertag with `/link` first."),
		"serverConnectUnavailable": serverConnectUnavailable,
		"welcome test failure":     welcomeTestFailed + welcomeFailureSentence("CHANNEL_MISSING"),
	}
	for name, text := range replies {
		if !strings.HasSuffix(text, ".") {
			t.Errorf("%s must end with a full stop: %q", name, text)
		}
		for _, banned := range []string{"❌", "⛔", "🚫", "⚠️", "!", "database", "nil", "error class", "Error class", "_ID", "panic"} {
			if strings.Contains(text, banned) {
				t.Errorf("%s must not contain %q: %q", name, banned, text)
			}
		}
		if shouted := presentationShout(text); shouted != "" {
			t.Errorf("%s shouts %q: %q", name, shouted, text)
		}
		if strings.Contains(text, "  ") || strings.Contains(text, "..") {
			t.Errorf("%s has doubled spaces or full stops: %q", name, text)
		}
	}
	// The reference tone: these three sentences are the ones the interaction router answers with.
	if interactionFailureText != "Something went wrong on our side. Try again in a moment." ||
		unroutedCommandText != "This command is unavailable right now." || unroutedComponentText != "This button is no longer valid." {
		t.Fatal("the router's three reference answers must not drift")
	}
	if got := ReplyNeedsPermission("run `/setup`"); got != "You need the Administrator or Manage Server permission to run `/setup`." {
		t.Fatalf("permission refusal: %q", got)
	}
	if got := ReplyCouldNotBecause("create the war", "  "); got != ReplyCouldNot("create the war") {
		t.Fatalf("no reason falls back to the plain failure: %q", got)
	}
	if got := ReplyCouldNotBecause("create the war", "both factions are the same."); got != "Couldn't create the war: both factions are the same." {
		t.Fatalf("reason sentence: %q", got)
	}
	// Every welcome failure class has a plain sentence; the class name itself is never shown.
	for _, class := range []string{"CHANNEL_MISSING", "PERMISSION_BLOCKED", "RATE_LIMITED", "DISCORD_UNAVAILABLE", "anything else"} {
		if s := welcomeFailureSentence(class); s == "" || strings.Contains(s, class) || strings.Contains(s, "_") {
			t.Errorf("welcome failure %q must read as a sentence: %q", class, s)
		}
	}
}

// TestColoursFollowMeaning pins the palette's meanings on the cards where the colour is the
// message: a kill is crimson, a highlight is gold, healthy is green, attention is amber, failure
// is red, and routine information is neutral.
func TestColoursFollowMeaning(t *testing.T) {
	now := previewAt
	polling := func(lastPoll time.Duration) killfeed.AdmSnapshot {
		return killfeed.AdmSnapshot{State: killfeed.StatePolling, LastPoll: now.Add(-lastPoll), LastLogChange: now.Add(-time.Minute), OnlineCount: 3}
	}
	healthy := ServerStatusSection{ServerName: "A", Seen: true, Snapshot: polling(20 * time.Second)}
	degraded := ServerStatusSection{ServerName: "B", Seen: true, Snapshot: polling(20 * time.Minute)}
	waiting := ServerStatusSection{ServerName: "C"}
	for name, c := range map[string]struct{ got, want int }{
		"standard kill":              {BuildKillEmbed(FixtureStandardKill()).Color, presentation.Crimson},
		"headshot":                   {BuildKillEmbed(FixtureHeadshotKill()).Color, presentation.Crimson},
		"long shot":                  {BuildKillEmbed(FixtureLongshotKill()).Color, presentation.Gold},
		"bounty claimed kill":        {BuildKillEmbed(FixtureBountyKill()).Color, presentation.Gold},
		"death":                      {BuildDeathEmbed(FixtureDeath()).Color, presentation.Neutral},
		"hit":                        {buildHitEmbed(&hitEncounter{attacker: "a", victim: "b", hits: 1}).Color, presentation.Neutral},
		"player joined":              {buildConnectionsEmbed([]killfeed.ConnectionNotice{{Kind: killfeed.ConnectionConnected, Name: "a"}}, 0).Color, presentation.Green},
		"player left":                {buildConnectionsEmbed([]killfeed.ConnectionNotice{{Kind: killfeed.ConnectionDisconnected, Name: "a"}}, 0).Color, presentation.Neutral},
		"bounty placed":              {buildBountyEventEmbed(bounties.Event{Kind: bounties.EventPlaced, Target: "a", Amount: 1}).Color, presentation.Crimson},
		"bounty claimed":             {buildBountyEventEmbed(bounties.Event{Kind: bounties.EventClaimed, Target: "a", Hunter: "b", Amount: 1}).Color, presentation.Gold},
		"bounty expired":             {buildBountyEventEmbed(bounties.Event{Kind: bounties.EventExpired, Target: "a", Amount: 1}).Color, presentation.Neutral},
		"reward":                     {buildEconomyEmbed(economy.Event{Type: economy.TypeSystemReward, PlayerName: "a", Amount: 1, Credit: true}).Color, presentation.Gold},
		"points taken away":          {buildEconomyEmbed(economy.Event{Type: economy.TypeAdminDebit, PlayerName: "a", Amount: 1}).Color, presentation.Amber},
		"points granted":             {buildEconomyEmbed(economy.Event{Type: economy.TypeAdminCredit, PlayerName: "a", Amount: 1, Credit: true}).Color, presentation.Green},
		"status: all connected":      {BuildServerStatusEmbed([]ServerStatusSection{healthy}, now).Color, presentation.Green},
		"status: one degraded":       {BuildServerStatusEmbed([]ServerStatusSection{healthy, degraded}, now).Color, presentation.Amber},
		"status: still waiting":      {BuildServerStatusEmbed([]ServerStatusSection{healthy, waiting}, now).Color, presentation.Neutral},
		"status: no server":          {BuildServerStatusEmbed(nil, now).Color, presentation.Neutral},
		"staff warning":              {BuildAdminAlertEmbed(AdminAlert{Severity: AlertWarning, Headline: "x"}, "").Color, presentation.Amber},
		"staff critical":             {BuildAdminAlertEmbed(AdminAlert{Severity: AlertCritical, Headline: "x"}, "").Color, presentation.Red},
		"staff resolved":             {BuildAdminAlertEmbed(AdminAlert{Severity: AlertResolved, Headline: "x"}, "").Color, presentation.Green},
		"staff notice":               {BuildAdminAlertEmbed(AdminAlert{Severity: AlertInfo, Headline: "x"}, "").Color, presentation.Neutral},
		"log download failed":        {BuildADMDownloadEmbed(killfeed.DownloadReport{Result: "failure"}).Color, presentation.Red},
		"log download recovered":     {BuildADMDownloadEmbed(killfeed.DownloadReport{Result: "recovered"}).Color, presentation.Green},
		"online players (online)":    {OnlinePlayersEmbed([]string{"a"}, true).Color, presentation.Green},
		"online players (offline)":   {OnlinePlayersEmbed(nil, false).Color, presentation.Neutral},
		"leaderboard":                {presentation.BuildPlayerLeaderboardEmbed("Kills", nil, "").Color, presentation.Gold},
		"leaderboard unavailable":    {presentation.BuildLeaderboardErrorEmbed("Kills").Color, presentation.Red},
		"hot zone":                   {BuildHotZoneOpenedEmbed(HotZoneAnnouncement{Name: "x"}).Color, presentation.Crimson},
		"season result":              {BuildSeasonCompletionEmbed("S", "", 0, "", 0, "", 0, "", 0).Color, presentation.Gold},
		"ranks board without season": {ServerRanksInactiveEmbed("x").Color, presentation.Neutral},
	} {
		if c.got != c.want {
			t.Errorf("%s is %s (%06X), want %s", name, presentation.PaletteName(c.got), c.got, presentation.PaletteName(c.want))
		}
	}
}

// The story engine's labels stay in capitals as DATA (they are template variables); only the
// built-in card shows them in sentence case. This is what keeps saved templates unchanged.
func TestStoryLabelsStayDataWhileTheCardIsCalm(t *testing.T) {
	ev := FixtureHeadshotKill()
	p := BuildPresentation(ev)
	if p.Title != "🎯 HEADSHOT" || p.Hero != "HEADSHOT CONFIRMED" {
		t.Fatalf("the classifier's labels are data and must not change: %q / %q", p.Title, p.Hero)
	}
	vars := killfeedVars(ev, "")
	if vars["kill_type"] != "🎯 HEADSHOT" || vars["special_kill"] != "HEADSHOT CONFIRMED" || vars["headshot"] != "HEADSHOT" || vars["range"] != "MID RANGE" {
		t.Fatalf("template variables must resolve exactly as before: %v", vars)
	}
	if card := BuildKillEmbed(ev); card.Title != "🎯 Headshot" || strings.Contains(card.Description, "MID RANGE") || !strings.Contains(card.Description, "Mid range") {
		t.Fatalf("the built-in card shows the same labels calmly: %q / %q", card.Title, card.Description)
	}
	war := FixtureStandardKill()
	war.WarBadge, war.BountyTarget = "⚔️ FACTION WAR", true
	if killfeedVars(war, "")["war_badge"] != "⚔️ FACTION WAR" {
		t.Fatal("{{war_badge}} is the stored badge, unchanged")
	}
}
