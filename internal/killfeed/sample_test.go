package killfeed

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

func TestSelectSampleLinesPrefersGameplayTerms(t *testing.T) {
	content := strings.Join([]string{
		"server started",
		"12:00:01 Player Survivor connected",
		"12:00:05 idle tick",
		"12:00:10 Player Bandit hit Survivor",
		"12:00:12 Survivor died",
		"12:00:20 heartbeat",
		"12:00:30 Player Survivor disconnected",
	}, "\n")

	sample := SelectSampleLines(content, 45)
	if len(sample) == 0 {
		t.Fatal("expected a non-empty sample")
	}
	joined := strings.Join(sample, "\n")
	if !strings.Contains(joined, "connected") {
		t.Error("expected a connect line in the sample")
	}
	if !strings.Contains(joined, "died") {
		t.Error("expected a death line in the sample")
	}
}

func TestSelectSampleLinesRedactsIPsOnly(t *testing.T) {
	content := "12:00:01 Player Survivor connected from 192.168.1.10:2302"
	sample := SelectSampleLines(content, 45)
	if len(sample) != 1 {
		t.Fatalf("expected 1 line, got %d", len(sample))
	}
	if strings.Contains(sample[0], "192.168.1.10") {
		t.Fatalf("expected IP to be redacted, got %q", sample[0])
	}
	if !strings.Contains(sample[0], "[REDACTED]") {
		t.Fatalf("expected [REDACTED] placeholder, got %q", sample[0])
	}
	if !strings.Contains(sample[0], "Survivor connected") {
		t.Fatalf("expected gameplay wording preserved, got %q", sample[0])
	}
}

func TestSelectSampleLinesFallbackWhenNoTerms(t *testing.T) {
	// File content with no recognizable gameplay terms still returns real lines.
	content := "alpha\nbeta\ngamma\n"
	sample := SelectSampleLines(content, 45)
	if len(sample) == 0 {
		t.Fatal("expected fallback to return real lines")
	}
}

func TestSelectBestCandidateNeverPrefersSizeOverRecency(t *testing.T) {
	now := time.Now()
	// A large, stale ADM from a previous rotated-out session must never win
	// over the small, current ADM just because it is bigger.
	logs := []nitrado.LogFile{
		{Name: "DayZServer_PS4_x64_new.ADM", Path: "/c/new.ADM", Size: 3000, Modified: now},
		{Name: "DayZServer_PS4_x64_old.ADM", Path: "/c/old.ADM", Size: 260087, Modified: now.Add(-time.Hour)},
	}
	best := selectBestCandidate(logs)
	if best.Path != "/c/new.ADM" {
		t.Fatalf("expected the newest ADM regardless of size, got %q", best.Path)
	}
}

func TestSelectBestCandidateFallsBackToNewestWhenAllEmpty(t *testing.T) {
	now := time.Now()
	logs := []nitrado.LogFile{
		{Name: "a.ADM", Path: "/c/a.ADM", Size: 100, Modified: now},
		{Name: "b.ADM", Path: "/c/b.ADM", Size: 50, Modified: now.Add(-time.Minute)},
	}
	best := selectBestCandidate(logs)
	if best.Path != "/c/a.ADM" {
		t.Fatalf("expected newest fallback when all candidates are empty, got %q", best.Path)
	}
}
