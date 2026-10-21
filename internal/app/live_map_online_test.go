package app

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
)

func TestPickLiveMapPlayersOnline(t *testing.T) {
	now := time.Date(2026, 10, 4, 23, 0, 0, 0, time.UTC)
	fresh := onlineCounterStatus{ServerID: 1, Reading: discord.CounterReading{Known: true, Count: 3}, EvaluatedAt: now.Add(-time.Minute)}
	if got := pickLiveMapPlayersOnline(fresh, 1, 0, now); got != 3 {
		t.Fatalf("fresh counter reading should win over the log count: got %d", got)
	}
	if got := pickLiveMapPlayersOnline(fresh, 2, 4, now); got != 4 {
		t.Fatalf("another server keeps its log count: got %d", got)
	}
	stale := fresh
	stale.EvaluatedAt = now.Add(-10 * time.Minute)
	if got := pickLiveMapPlayersOnline(stale, 1, 2, now); got != 2 {
		t.Fatalf("a stale reading is not used: got %d", got)
	}
	unknown := fresh
	unknown.Reading.Known = false
	if got := pickLiveMapPlayersOnline(unknown, 1, 2, now); got != 2 {
		t.Fatalf("an unknown reading is not used: got %d", got)
	}
	if got := pickLiveMapPlayersOnline(onlineCounterStatus{}, 1, 5, now); got != 5 {
		t.Fatalf("no counter keeps the log count: got %d", got)
	}
}
