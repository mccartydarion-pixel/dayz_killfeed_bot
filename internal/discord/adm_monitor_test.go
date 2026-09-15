package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func TestBuildADMMonitorEmbedSanitizesPathAndShowsIncrementalState(t *testing.T) {
	embed := BuildADMMonitorEmbed(killfeed.AdmSnapshot{
		State:              killfeed.StatePolling,
		CurrentFile:        "/private/profile/DayZServer.ADM",
		FileSize:           2048,
		ProcessedOffset:    1024,
		PendingPartialLine: "partial",
		LastPoll:           time.Now().Add(-8 * time.Second),
		LastDownload:       time.Now().Add(-17 * time.Second),
	}, time.Now())
	if strings.Contains(embed.Description, "/private/") || strings.Contains(embed.Description, "profile") {
		t.Fatal("monitor description exposed a private path")
	}
	joined := ""
	for _, field := range embed.Fields {
		joined += field.Name + "=" + field.Value + "\n"
	}
	if !strings.Contains(joined, "CURRENT ADM=DayZServer.ADM") || !strings.Contains(joined, "PROCESSED=1.0 KB") || !strings.Contains(joined, "UNREAD=1.0 KB") || !strings.Contains(joined, "PENDING PARTIAL LINE=YES") {
		t.Fatalf("missing monitor state: %s", joined)
	}
}
