package discord

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEventAnnouncementCards(t *testing.T) {
	start := time.Unix(1790000000, 0)
	end := start.Add(2 * time.Hour)
	up := BuildEventAnnouncementEmbed(EventAnnouncementCard{Kind: "UPCOMING", Name: "Sniper Weekend", Type: "LONGEST_KILL", Config: json.RawMessage(`{"minimum_distance":200}`), StartsAt: &start, EndsAt: &end, Prizes: [3]int{2000, 1000, 0}})
	text := allText(up)
	for _, want := range []string{"Upcoming event", "Sniper Weekend", "Longest single kill wins (200m minimum).", "<t:1790000000:F>", "🥇 2,000 pts", "🥈 1,000 pts"} {
		if !strings.Contains(text, want) {
			t.Errorf("upcoming card missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "🥉") {
		t.Error("a zero prize is not listed")
	}
	started := BuildEventAnnouncementEmbed(EventAnnouncementCard{Kind: "STARTED", Name: "@everyone **x**", Type: "WEAPON_CHALLENGE", Config: json.RawMessage(`{"weapon_names":["M4-A1","@here"]}`), EndsAt: &end})
	text = allText(started)
	if !strings.Contains(text, "Event started") || strings.Contains(text, "@everyone") || strings.Contains(text, "@here") || strings.Contains(text, "Starts") {
		t.Fatalf("started card: %s", text)
	}
	assertWithinLimits(t, started)
}
