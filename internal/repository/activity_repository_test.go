package repository

import (
	"testing"
	"time"
)

func TestObservedPlaytimeIncludesActiveSession(t *testing.T) {
	start := time.Unix(1000, 0)
	a := &PlayerActivity{TotalObservedSeconds: 120, CurrentlyConnected: true, CurrentSessionStartedAt: &start}
	if got := a.Effective(start.Add(180 * time.Second)); got != 300*time.Second {
		t.Fatalf("got %s", got)
	}
}
func TestObservedPlaytimeRejectsReversedTime(t *testing.T) {
	start := time.Unix(1000, 0)
	a := &PlayerActivity{CurrentlyConnected: true, CurrentSessionStartedAt: &start}
	if got := a.Effective(start.Add(-time.Second)); got != 0 {
		t.Fatalf("got %s", got)
	}
}
