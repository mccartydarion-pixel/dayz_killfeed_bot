package app

import (
	"testing"
	"time"
)

func TestWarCoversOnlyKillsFromItsStart(t *testing.T) {
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if warCovers(&start, start.Add(-time.Minute)) {
		t.Fatal("a kill before the war started is not a war kill")
	}
	if !warCovers(&start, start) || !warCovers(&start, start.Add(time.Hour)) {
		t.Fatal("kills from the start on are war kills")
	}
	if !warCovers(nil, start) || !warCovers(&start, time.Time{}) {
		t.Fatal("an unknown start or kill time keeps the old behaviour")
	}
}
