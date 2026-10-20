package livemap

import (
	"testing"
	"time"
)

func TestPressureAggregatesAndDecays(t *testing.T) {
	now := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	points := []Point{
		{X: 4612, Z: 10390, At: now.Add(-time.Minute)},     // cell (9, 20)
		{X: 4810, Z: 10330, At: now.Add(-5 * time.Minute)}, // same cell
		{X: 4999, Z: 10001, At: now.Add(-29 * time.Minute)},
		{X: 100, Z: 100, At: now.Add(-10 * time.Minute)},    // cell (0, 0)
		{X: 7600, Z: 12700, At: now.Add(-31 * time.Minute)}, // outside the window: decayed
		{X: 7600, Z: 12700, At: now.Add(time.Second)},       // in the future: never counted
	}
	cells := Pressure(points, now, 30*time.Minute, 500, 400)
	if len(cells) != 2 {
		t.Fatalf("cells = %+v", cells)
	}
	if cells[0].CenterX != 4750 || cells[0].CenterZ != 10250 || cells[0].Count != 3 || cells[0].Intensity != 1 {
		t.Fatalf("busiest cell = %+v", cells[0])
	}
	if cells[1].CenterX != 250 || cells[1].CenterZ != 250 || cells[1].Count != 1 || cells[1].Intensity != 0.333 {
		t.Fatalf("second cell = %+v", cells[1])
	}
	// The cap keeps the busiest cells; intensity is still relative to the overall maximum.
	if capped := Pressure(points, now, 30*time.Minute, 500, 1); len(capped) != 1 || capped[0].Count != 3 {
		t.Fatalf("capped = %+v", capped)
	}
	if got := Pressure(points, now, 30*time.Minute, 0, 400); len(got) != 0 {
		t.Fatalf("zero resolution produced cells: %+v", got)
	}
	if got := Pressure(nil, now, 30*time.Minute, 500, 400); got == nil || len(got) != 0 {
		t.Fatalf("no points should give an empty (non-nil) slice: %#v", got)
	}
}

func TestWallClockAdvancesByRealTime(t *testing.T) {
	local := time.Date(2026, 10, 3, 21, 14, 8, 0, time.UTC) // zone-less ADM stamp
	observed := time.Date(2026, 10, 3, 19, 14, 10, 0, time.UTC)
	if got, ok := WallClock(local, observed, observed.Add(90*time.Second), time.Hour); !ok || got != "21:15:38" {
		t.Fatalf("wall clock = %q ok=%v", got, ok)
	}
	// Wraps past midnight.
	if got, _ := WallClock(time.Date(2026, 10, 3, 23, 59, 30, 0, time.UTC), observed, observed.Add(45*time.Second), 0); got != "00:00:15" {
		t.Fatalf("wrap = %q", got)
	}
	// A reading older than maxAge is not advanced into fiction.
	if _, ok := WallClock(local, observed, observed.Add(2*time.Hour), time.Hour); ok {
		t.Fatal("a stale reading was still reported")
	}
	// Clock skew (observed in the future) counts as zero elapsed.
	if got, _ := WallClock(local, observed, observed.Add(-time.Minute), time.Hour); got != "21:14:08" {
		t.Fatalf("skewed = %q", got)
	}
}

func TestInGameTimeEstimate(t *testing.T) {
	booted := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	bootLocal := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC) // server-local wall clock at boot (UTC+2)
	// Explicit start, 12x: 30 real minutes = 6 in-game hours.
	if got, ok := InGameTime("2024/6/1/8/30", "12", booted, nil, booted.Add(30*time.Minute)); !ok || got != "14:30" {
		t.Fatalf("explicit = %q ok=%v", got, ok)
	}
	// SystemTime: the day starts at the boot's local wall clock.
	if got, ok := InGameTime("SystemTime", "1.0", booted, &bootLocal, booted.Add(2*time.Hour)); !ok || got != "22:00" {
		t.Fatalf("system = %q ok=%v", got, ok)
	}
	// Wraps around midnight.
	if got, _ := InGameTime("2024/6/1/23/0", "4", booted, nil, booted.Add(time.Hour)); got != "03:00" {
		t.Fatalf("wrap = %q", got)
	}
	// Missing or bad inputs: no estimate rather than a wrong one.
	for name, c := range map[string]struct {
		st, accel string
		local     *time.Time
		now       time.Time
	}{
		"no serverTime":             {"", "12", nil, booted.Add(time.Minute)},
		"bad serverTime":            {"8:30", "12", nil, booted.Add(time.Minute)},
		"hour out of range":         {"2024/6/1/25/0", "12", nil, booted.Add(time.Minute)},
		"zero acceleration":         {"2024/6/1/8/0", "0", nil, booted.Add(time.Minute)},
		"bad acceleration":          {"2024/6/1/8/0", "fast", nil, booted.Add(time.Minute)},
		"SystemTime without boot":   {"SystemTime", "12", nil, booted.Add(time.Minute)},
		"boot in the future":        {"2024/6/1/8/0", "12", nil, booted.Add(-time.Minute)},
		"absurd acceleration":       {"2024/6/1/8/0", "100000", nil, booted.Add(time.Minute)},
		"SystemTime zero boot time": {"SystemTime", "12", &time.Time{}, booted.Add(time.Minute)},
	} {
		if got, ok := InGameTime(c.st, c.accel, booted, c.local, c.now); ok {
			t.Errorf("%s: estimated %q", name, got)
		}
	}
	if _, ok := InGameTime("2024/6/1/8/0", "12", time.Time{}, nil, booted); ok {
		t.Error("a zero boot time produced an estimate")
	}
}

func TestNextRestartParsing(t *testing.T) {
	now := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	tasks := []Task{
		{ActionMethod: "backup", NextRun: "2026-10-03 20:05:00"},        // not a restart
		{ActionMethod: "restart", NextRun: "2026-10-03 19:00:00"},       // already happened
		{ActionMethod: "restart", NextRun: "2026-10-04T02:00:00+02:00"}, // 00:00 UTC
		{ActionMethod: "Restart", NextRun: "2026-10-03 22:00:00"},       // the earliest future one
		{ActionMethod: "restart", NextRun: "soon"},                      // unparseable
		{ActionMethod: "restart", NextRun: ""},
	}
	at, ok := NextRestart(tasks, now)
	if !ok || !at.Equal(time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("next restart = %v ok=%v", at, ok)
	}
	if _, ok := NextRestart(tasks[:2], now); ok {
		t.Fatal("a past restart or a backup was picked")
	}
	if _, ok := NextRestart(nil, now); ok {
		t.Fatal("no tasks produced a restart")
	}
	for raw, want := range map[string]time.Time{
		"2026-10-03T22:00:00Z":      time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC),
		"2026-10-03T22:00:00+01:00": time.Date(2026, 10, 3, 21, 0, 0, 0, time.UTC),
		"2026-10-03 22:00":          time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC),
		" 2026-10-03T22:00:00 ":     time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC),
	} {
		if got, ok := ParseTaskTime(raw); !ok || !got.Equal(want) {
			t.Errorf("ParseTaskTime(%q) = %v ok=%v, want %v", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "yesterday", "03/10/2026", "2026-13-40 10:00:00"} {
		if _, ok := ParseTaskTime(raw); ok {
			t.Errorf("ParseTaskTime(%q) parsed", raw)
		}
	}
}
