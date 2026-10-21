package heatmap

import (
	"testing"
	"time"
)

func TestHourWindowSplitsDays(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(3 * 24 * time.Hour)
	w := HourWindow{FromHour: 20, ToHour: 22}.Windows(from, to)
	if len(w) != 3 || w[0][0].Hour() != 20 || w[0][1].Sub(w[0][0]) != 2*time.Hour {
		t.Fatalf("three evenings: %v", w)
	}
	wrap := HourWindow{FromHour: 23, ToHour: 1}.Windows(from, to)
	// Oct 1 00:00-01:00 (from the night before), then 23-01 on each night, cut at `to`.
	var total time.Duration
	for _, x := range wrap {
		total += x[1].Sub(x[0])
	}
	if total != 6*time.Hour {
		t.Fatalf("two hours a night over three nights: %v (%v)", total, wrap)
	}
	if (Request{Type: TypePvPKills, Resolution: 100, From: from, To: to, Hours: &HourWindow{FromHour: 5, ToHour: 5}}).Validate() == nil {
		t.Fatal("an empty window is refused")
	}
}
