package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func TestBuildADMDownloadEmbedUsesBasenameAndOffsets(t *testing.T) {
	embed := BuildADMDownloadEmbed(killfeed.DownloadReport{
		File:            "/private/profile/DayZ.ADM",
		RemoteSize:      20844,
		DownloadedBytes: 20844,
		PreviousOffset:  19442,
		NewOffset:       20844,
		NewBytes:        1402,
		EventsParsed:    3,
		Result:          "success",
		At:              time.Now(),
	})
	if strings.Contains(embed.Description, "/private") {
		t.Fatal("download embed exposed a private path")
	}
	joined := ""
	for _, field := range embed.Fields {
		joined += field.Name + "=" + field.Value + "\n"
	}
	for _, want := range []string{"File=DayZ.ADM", "New data=1.4 KB", "Events parsed=3", "Processed offset=19.0 KB → 20.4 KB", "Result=Success"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
}

func TestBuildADMDownloadEmbedNoNewEventsIsActionable(t *testing.T) {
	embed := BuildADMDownloadEmbed(killfeed.DownloadReport{File: "DayZ.ADM", NewBytes: 842, Result: "success_no_new_events"})
	found := false
	for _, field := range embed.Fields {
		if field.Name == "Result" && strings.Contains(field.Value, "Waiting for complete ADM line") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected partial-line waiting status")
	}
}
