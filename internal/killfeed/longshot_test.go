package killfeed

import (
	"context"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/presentation"
)

func floatPtr(f float64) *float64 { return &f }

// TestIsLongshotEventThreshold pins the authoritative longshot boundary
// (presentation.LongshotDistanceMeters) at the exact meter, one below it, and
// one above it, plus the nil-distance case that must never classify true.
func TestIsLongshotEventThreshold(t *testing.T) {
	cases := []struct {
		name     string
		distance *float64
		want     bool
	}{
		{"99.99m below threshold", floatPtr(99.99), false},
		{"100.0m exactly at threshold", floatPtr(100.0), true},
		{"100.01m above threshold", floatPtr(100.01), true},
		{"nil distance", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := killEvent("victim", "killer", "M4-A1", 0, "16:40:12")
			ev.Distance = tc.distance
			if got := isLongshotEvent(ev); got != tc.want {
				t.Fatalf("isLongshotEvent(distance=%v) = %v, want %v", tc.distance, got, tc.want)
			}
		})
	}
	if isLongshotEvent(nil) {
		t.Fatal("nil event must not be classified as longshot")
	}
}

// TestHeadshotAndLongshotCoexistOnPersistedKill guards independence: a kill
// confirmed as a headshot at long range must persist both booleans true, not
// one suppressing the other.
func TestHeadshotAndLongshotCoexistOnPersistedKill(t *testing.T) {
	store := newFakePersistenceStore()
	pq := NewPersistenceQueue(store, 1, "sess-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	ev := killEvent("victim1", "killer1", "SVD", 150.0, "16:40:12")
	ev.HitZone = "Head"
	pq.Enqueue(ev)
	pq.Close()

	if len(store.kills) != 1 {
		t.Fatalf("expected 1 persisted kill, got %d", len(store.kills))
	}
	rec := store.kills[0]
	if !rec.Headshot {
		t.Fatal("expected headshot to persist true")
	}
	if !rec.Longshot {
		t.Fatal("expected longshot to persist true alongside headshot")
	}
}

// TestLongshotAndServerRecordCoexist guards independence across layers:
// ServerRecord is a presentation-time priority pick (which title/story wins),
// never a persistence field, so it must not suppress the durable, separately
// computed longshot classification for the same kill.
func TestLongshotAndServerRecordCoexist(t *testing.T) {
	ev := killEvent("victim2", "killer2", "Mosin", 150.0, "16:41:00")
	if !isLongshotEvent(ev) {
		t.Fatal("expected distance to classify as longshot")
	}

	story := presentation.SelectPrimary(presentation.Context{Distance: ev.Distance, ServerRecord: true})
	if story != presentation.StoryServerRecord {
		t.Fatal("expected server record to take presentation priority over long range")
	}
	if !isLongshotEvent(ev) {
		t.Fatal("longshot classification must remain true regardless of presentation story selection")
	}
}
