package presentation

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestSentenceCase(t *testing.T) {
	for in, want := range map[string]string{
		"PLAYER ELIMINATED":        "Player eliminated",
		"🎯 HEADSHOT":               "🎯 Headshot",
		"☠️ PLAYER ELIMINATED":     "☠️ Player eliminated",
		"NEW #1":                   "New #1",
		"MULTI-KILL":               "Multi-kill",
		"ADM STALE":                "ADM stale",
		"NITRADO API FAILURE":      "Nitrado API failure",
		"PVE DEATH":                "PvE death",
		"CLOSE QUARTERS":           "Close quarters",
		"EVERY KILL TELLS A STORY": "Every kill tells a story",
		"⚔️ FACTION WAR":           "⚔️ Faction war",
		"WARNING":                  "Warning",
		// Mixed text is left alone: names, weapons, sentences that are already calm.
		"Sample_Raven reached Platinum": "Sample_Raven reached Platinum",
		"M4-A1":                         "M4-A1",
		"2× RP is live":                 "2× RP is live",
		"Best K/D":                      "Best K/D",
		"":                              "",
	} {
		if got := SentenceCase(in); got != want {
			t.Errorf("SentenceCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckLabel(t *testing.T) {
	for _, ok := range []string{"☠️ Player eliminated", "Killer", "Best K/D", "ADM monitor", "🏅 Sample_Raven reached Platinum", "⚡ 2× RP is live", "Head to head", "🗺️ PvP heatmap update"} {
		if why := CheckLabel(ok); why != "" {
			t.Errorf("%q should pass: %s", ok, why)
		}
	}
	for label, want := range map[string]string{
		"☠️ PLAYER ELIMINATED": "shouts",
		"KILLER":               "shouts",
		"🎖️ Server ranks 🎖️":   "more than one emoji",
		"Server ranks 🎖️":      "not at the start",
		"📊 AUTO LEADERBOARD 📊": "shouts",
	} {
		if why := CheckLabel(label); !strings.Contains(why, want) {
			t.Errorf("%q: got %q, want it to say %q", label, why, want)
		}
	}
}

func TestCheckFooter(t *testing.T) {
	for _, ok := range []string{"Example Server · Season 3", "Staff only", "Every kill tells a story", "Turn these off with /life recap off"} {
		if why := CheckFooter(ok); why != "" {
			t.Errorf("%q should pass: %s", ok, why)
		}
	}
	for _, bad := range []string{"", "CHAMPION • STAFF INTELLIGENCE", "Champion • Staff", "Updated <t:1:R>", "line one\nline two", "🏆 Season 3", "EVERY KILL TELLS A STORY"} {
		if CheckFooter(bad) == "" {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestCheckEmbed(t *testing.T) {
	good := NewFeedEmbed("☠️ Player eliminated", Crimson)
	good.Fields = []*discordgo.MessageEmbedField{{Name: "Killer", Value: "x"}}
	good.Footer = Footer("Example Server", "Season 3")
	if problems := CheckEmbed(good); len(problems) != 0 {
		t.Fatalf("a card built with the helpers follows the rules: %v", problems)
	}
	bad := &discordgo.MessageEmbed{
		Author: &discordgo.MessageEmbedAuthor{Name: "CHAMPION KILLFEED"}, Title: "☠️ PLAYER ELIMINATED ☠️", Color: 0x123456,
		Fields: []*discordgo.MessageEmbedField{{Name: "KILLER", Value: "x"}}, Footer: &discordgo.MessageEmbedFooter{Text: "CHAMPION • AUTO-REFRESH"},
	}
	if problems := CheckEmbed(bad); len(problems) < 5 {
		t.Fatalf("expected the colour, author, title, field and footer to be refused, got %v", problems)
	}
	if CheckEmbed(nil) != nil {
		t.Fatal("nil is nothing to check")
	}
}

func TestTitleCaseWords(t *testing.T) {
	for _, ok := range []string{"Channel summary", "🏅 Sample_Raven reached Platinum", "See your rank in the Player Hub", "🛡️ Security Store", "Need a base registered first?", "Live: Sniper Sunday", "🚨 Base Raid Alarm · 12,500 pts / 30 days", "Best K/D"} {
		if words := TitleCaseWords(ok, "Sample_Raven", "Platinum", "Sniper Sunday"); len(words) != 0 {
			t.Errorf("%q should pass, flagged %v", ok, words)
		}
	}
	for label, want := range map[string]string{"Channel Summary": "Summary", "Systems Verified": "Verified", "Last Log Change": "Log", "🔫 All Time Top 15 Kills": "Time"} {
		words := TitleCaseWords(label)
		if len(words) == 0 || words[0] != want {
			t.Errorf("%q: flagged %v, want first %q", label, words, want)
		}
	}
	// A safe (markdown-escaped) name is still recognised as the sample name it is.
	if words := TitleCaseWords(`💀 Bounty on Sample\_Raven`, "Sample_Raven"); len(words) != 0 {
		t.Errorf("escaped name flagged: %v", words)
	}
}

func TestEnumLabel(t *testing.T) {
	for in, want := range map[string]string{"BASE_RADAR": "Base radar", "RESTRICTED": "Restricted", "selection_stale": "Selection stale", "PVP": "PvP", "UAV": "UAV", "WARNING": "Warning", "": ""} {
		if got := EnumLabel(in); got != want {
			t.Errorf("EnumLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
