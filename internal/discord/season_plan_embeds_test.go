package discord

import (
	"strings"
	"testing"
	"time"
)

func TestSeasonPlanEmbedNamesWhichSeason(t *testing.T) {
	at := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	stats := BuildSeasonPlanEmbed(SeasonPlanCard{Stage: "SCHEDULED", Kind: "STATS_SEASON", RunAt: at, NewSeasonName: "Season 2"})
	if !strings.Contains(stats.Title, "STATS SEASON") || strings.Contains(stats.Description, "Ranked") || len(stats.Fields) != 2 {
		t.Fatalf("stats notice: %+v", stats)
	}
	ranked := BuildSeasonPlanEmbed(SeasonPlanCard{Stage: "DONE", Kind: "RANKED_RESET", ServerName: "Main", RunAt: at})
	if !strings.Contains(ranked.Title, "RANKED") || len(ranked.Fields) != 1 {
		t.Fatalf("ranked done card: %+v", ranked)
	}
}
