// Package livemap holds the pure parts of the live map (docs/LIVE_MAP.md): the pressure grid the
// public map shades, the server wall-clock and in-game clock estimates, and the next-restart pick
// from Nitrado's scheduled tasks. Nothing here touches the database or the network, so every
// rule is unit-tested on its own.
package livemap

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- pressure --------------------------------------------------------------------------------

// Point is one event that adds pressure to the map: a kill or a hit at (X, Z) at time At.
type Point struct {
	X, Z float64
	At   time.Time
}

// Cell is one aggregated pressure cell. Intensity is Count divided by the busiest cell's count,
// so the busiest cell is always 1.0.
type Cell struct {
	CenterX, CenterZ float64
	Count            int
	Intensity        float64
}

// Pressure aggregates points observed in (to-window, to] into square cells of resolution metres,
// busiest first, at most maxCells cells (the quietest are dropped first). Points outside the
// window - older than it, or in the future of to - contribute nothing: that is the decay, a kill
// counts fully until it leaves the window and not at all after. A non-positive resolution or
// maxCells yields no cells.
func Pressure(points []Point, to time.Time, window time.Duration, resolution float64, maxCells int) []Cell {
	if resolution <= 0 || maxCells <= 0 {
		return []Cell{}
	}
	from := to.Add(-window)
	type key struct{ x, z int64 }
	counts := map[key]int{}
	for _, p := range points {
		if !p.At.After(from) || p.At.After(to) {
			continue
		}
		if math.IsNaN(p.X) || math.IsNaN(p.Z) || math.IsInf(p.X, 0) || math.IsInf(p.Z, 0) {
			continue
		}
		counts[key{int64(math.Floor(p.X / resolution)), int64(math.Floor(p.Z / resolution))}]++
	}
	cells := make([]Cell, 0, len(counts))
	max := 0
	for k, n := range counts {
		cells = append(cells, Cell{CenterX: (float64(k.x) + 0.5) * resolution, CenterZ: (float64(k.z) + 0.5) * resolution, Count: n})
		if n > max {
			max = n
		}
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Count != cells[j].Count {
			return cells[i].Count > cells[j].Count
		}
		if cells[i].CenterZ != cells[j].CenterZ {
			return cells[i].CenterZ < cells[j].CenterZ
		}
		return cells[i].CenterX < cells[j].CenterX
	})
	if len(cells) > maxCells {
		cells = cells[:maxCells]
	}
	for i := range cells {
		cells[i].Intensity = math.Round(float64(cells[i].Count)/float64(max)*1000) / 1000
	}
	return cells
}

// --- server wall clock -----------------------------------------------------------------------

// WallClock advances the latest ADM wall-clock reading (a zone-less server-local time stamped on
// a log line Champion ingested at observedAt) by the real time elapsed since then, and formats it
// as HH:MM:SS. ok is false when the reading is older than maxAge - a server that logged nothing
// for that long may well be down, and a clock that keeps ticking would only pretend otherwise.
func WallClock(localTime, observedAt, now time.Time, maxAge time.Duration) (string, bool) {
	elapsed := now.Sub(observedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	if maxAge > 0 && elapsed > maxAge {
		return "", false
	}
	return localTime.Add(elapsed).Format("15:04:05"), true
}

// --- in-game clock ---------------------------------------------------------------------------

// serverTimeSystem is the serverDZ.cfg value that starts the in-game day at the host's clock.
const serverTimeSystem = "systemtime"

// InGameTime estimates the in-game time of day as HH:MM from the DayZ server settings and the
// boot: the in-game day starts at serverTime when the server boots and then runs at
// serverTimeAcceleration times real time. The night rate is ignored, which is why the result is
// an estimate. serverTime is either "SystemTime" (the day starts at the boot's wall-clock time,
// bootLocal) or "YYYY/M/D/H/MM" (an explicit start). ok is false when any input is missing or
// malformed, or when the boot is in the future.
func InGameTime(serverTime, acceleration string, bootedAt time.Time, bootLocal *time.Time, now time.Time) (string, bool) {
	accel, err := strconv.ParseFloat(strings.TrimSpace(acceleration), 64)
	if err != nil || accel <= 0 || math.IsInf(accel, 0) || math.IsNaN(accel) || accel > 1000 {
		return "", false
	}
	if bootedAt.IsZero() || now.Before(bootedAt) {
		return "", false
	}
	var startOfDay time.Duration // in-game time of day at boot
	st := strings.TrimSpace(serverTime)
	switch {
	case st == "":
		return "", false
	case strings.EqualFold(st, serverTimeSystem):
		if bootLocal == nil || bootLocal.IsZero() {
			return "", false
		}
		h, m, s := bootLocal.Clock()
		startOfDay = time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second
	default:
		h, m, ok := parseServerTime(st)
		if !ok {
			return "", false
		}
		startOfDay = time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
	}
	elapsed := time.Duration(float64(now.Sub(bootedAt)) * accel)
	day := 24 * time.Hour
	tod := (startOfDay + elapsed) % day
	if tod < 0 {
		tod += day
	}
	return time.Time{}.Add(tod).Format("15:04"), true
}

// parseServerTime reads the hour and minute of a "YYYY/M/D/H/MM" serverTime.
func parseServerTime(s string) (hour, minute int, ok bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 5 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[3]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[4]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// --- next restart ----------------------------------------------------------------------------

// Task is one scheduled task as Nitrado lists it (internal/nitrado.ScheduledTask), reduced to
// what the restart pick needs.
type Task struct {
	ActionMethod string
	NextRun      string
}

// taskTimeLayouts are the forms Nitrado has been seen to use for a task's next_run. A layout
// without a zone is read as UTC.
var taskTimeLayouts = []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05.000Z"}

// ParseTaskTime parses a Nitrado task timestamp defensively: RFC 3339 with a zone, or a zone-less
// "YYYY-MM-DD HH:MM[:SS]" (read as UTC). ok is false for anything else.
func ParseTaskTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range taskTimeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// NextRestart picks the earliest future restart among the tasks: any task whose action method
// says restart (case-insensitive, "restart" or "restart_gameserver"-like variants) with a parseable
// next_run after now. ok is false when there is none.
func NextRestart(tasks []Task, now time.Time) (time.Time, bool) {
	var best time.Time
	found := false
	for _, t := range tasks {
		method := strings.ToLower(strings.TrimSpace(t.ActionMethod))
		if !strings.Contains(method, "restart") {
			continue
		}
		at, ok := ParseTaskTime(t.NextRun)
		if !ok || !at.After(now) {
			continue
		}
		if !found || at.Before(best) {
			best, found = at, true
		}
	}
	return best, found
}
