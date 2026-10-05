package discord

import (
	"strings"
	"testing"
	"time"
)

func TestSeasonPlanEmbedNamesWhichSeason(t *testing.T) {
	at := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	stats := BuildSeasonPlanEmbed(SeasonPlanCard{Stage: "SCHEDULED", Kind: "STATS_SEASON", RunAt: at, NewSeasonName: "Season 2"})
	if !strings.Contains(stats.Title, "Stats season") || strings.Contains(stats.Description, "Ranked") || len(stats.Fields) != 2 {
		t.Fatalf("stats notice: %+v", stats)
	}
	ranked := BuildSeasonPlanEmbed(SeasonPlanCard{Stage: "DONE", Kind: "RANKED_RESET", ServerName: "Main", RunAt: at})
	// Ranked is per server: the card names the server, in the footer like every card.
	if !strings.Contains(ranked.Title, "ranked") || len(ranked.Fields) != 0 || ranked.Footer == nil || ranked.Footer.Text != "Main · Ranked" {
		t.Fatalf("ranked done card: %+v %+v", ranked, ranked.Footer)
	}
}
