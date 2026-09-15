package killfeed

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

func TestFilenameTimestampIsSafeAndDeterministic(t *testing.T) {
	got := filenameTimestamp("DayZServer_PS4_x64_2026-09-15_08-41-12.ADM")
	if got != "2026-09-15T08:41:12Z" {
		t.Fatalf("got %q", got)
	}
	if filenameTimestamp("not-an-adm.ADM") != "" {
		t.Fatal("expected no timestamp")
	}
}

func TestNewestCandidateWinsRegardlessOfSize(t *testing.T) {
	now := time.Now().UTC()
	logs := []nitrado.LogFile{
		{Name: "new.ADM", Path: "/new.ADM", Size: 4, Modified: now},
		{Name: "old.ADM", Path: "/old.ADM", Size: 300000, Modified: now.Add(-2 * time.Hour)},
	}
	if got := selectBestCandidate(logs); got.Name != "new.ADM" {
		t.Fatalf("got %q", got.Name)
	}
}
