// Package planner turns observed hourly activity into scheduling advice for
// server owners: when to run events, when to restart, which day to wipe. It is
// pure (no I/O) so every recommendation is unit tested.
package planner

import "sort"

// Slot is one (weekday, hour) cell in the owner's time zone. AveragePeak is the
// mean of observed hourly player peaks; Samples is how many such hours were
// observed (0 = never sampled, so no claim is made about it). Kills is the number
// of PvP kills recorded in that slot over the window.
type Slot struct {
	Weekday     int     `json:"weekday"` // 0 = Sunday
	Hour        int     `json:"hour"`
	AveragePeak float64 `json:"averagePeak"`
	MaxPeak     int     `json:"maxPeak"`
	Samples     int     `json:"samples"`
	Kills       int     `json:"kills"`
}

// Window is a recommended start time.
type Window struct {
	Weekday     int     `json:"weekday"`
	StartHour   int     `json:"startHour"`
	Hours       int     `json:"hours"`
	AveragePeak float64 `json:"averagePeak"`
	Kills       int     `json:"kills"`
}

// WipeDay is a recommended weekday for a wipe or new season: the busiest day,
// announced the day before.
type WipeDay struct {
	Weekday     int     `json:"weekday"`
	AveragePeak float64 `json:"averagePeak"`
}

// Plan is the planner output.
type Plan struct {
	Grid           []Slot   `json:"grid"`           // always 7 x 24, Sunday 00:00 first
	EventWindows   []Window `json:"eventWindows"`   // busiest consecutive blocks
	RestartWindows []Window `json:"restartWindows"` // quietest single hours
	WipeDay        *WipeDay `json:"wipeDay"`        // nil until enough data
	SampledHours   int      `json:"sampledHours"`
	Confidence     string   `json:"confidence"` // NONE | LOW | MEDIUM | HIGH
}

const minSamplesForAdvice = 2 // a slot seen fewer times is too noisy to recommend

// Build fills a full 7x24 grid from the observed slots and derives
// recommendations. eventHours is the length of an event block (clamped 1..6);
// n is how many windows of each kind to return (clamped 1..5).
func Build(observed []Slot, eventHours, n int) Plan {
	if eventHours < 1 {
		eventHours = 1
	}
	if eventHours > 6 {
		eventHours = 6
	}
	if n < 1 {
		n = 1
	}
	if n > 5 {
		n = 5
	}
	grid := make([]Slot, 7*24)
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			grid[d*24+h] = Slot{Weekday: d, Hour: h}
		}
	}
	sampled := 0
	for _, s := range observed {
		if s.Weekday < 0 || s.Weekday > 6 || s.Hour < 0 || s.Hour > 23 {
			continue
		}
		grid[s.Weekday*24+s.Hour] = s
		sampled += s.Samples
	}
	p := Plan{Grid: grid, SampledHours: sampled, EventWindows: []Window{}, RestartWindows: []Window{}}
	p.Confidence = confidence(sampled)
	if p.Confidence == "NONE" {
		return p
	}

	// Event windows: consecutive blocks (wrapping across midnight into the next
	// day) where every hour was sampled; ranked by mean peak, then kills.
	var blocks []Window
	for start := 0; start < 7*24; start++ {
		w := Window{Weekday: start / 24, StartHour: start % 24, Hours: eventHours}
		ok, sum := true, 0.0
		for i := 0; i < eventHours; i++ {
			s := grid[(start+i)%(7*24)]
			if s.Samples < minSamplesForAdvice {
				ok = false
				break
			}
			sum += s.AveragePeak
			w.Kills += s.Kills
		}
		if !ok {
			continue
		}
		w.AveragePeak = round1(sum / float64(eventHours))
		blocks = append(blocks, w)
	}
	sort.SliceStable(blocks, func(i, j int) bool {
		if blocks[i].AveragePeak != blocks[j].AveragePeak {
			return blocks[i].AveragePeak > blocks[j].AveragePeak
		}
		return blocks[i].Kills > blocks[j].Kills
	})
	p.EventWindows = pickNonOverlapping(blocks, n, eventHours)

	// Restart windows: quietest sampled single hours (fewest players, then fewest fights).
	var quiet []Window
	for _, s := range grid {
		if s.Samples < minSamplesForAdvice {
			continue
		}
		quiet = append(quiet, Window{Weekday: s.Weekday, StartHour: s.Hour, Hours: 1, AveragePeak: round1(s.AveragePeak), Kills: s.Kills})
	}
	sort.SliceStable(quiet, func(i, j int) bool {
		if quiet[i].AveragePeak != quiet[j].AveragePeak {
			return quiet[i].AveragePeak < quiet[j].AveragePeak
		}
		return quiet[i].Kills < quiet[j].Kills
	})
	p.RestartWindows = pickNonOverlapping(quiet, n, 1)

	// Wipe day: the weekday with the highest mean peak across its sampled hours.
	best := -1.0
	for d := 0; d < 7; d++ {
		sum, cnt := 0.0, 0
		for h := 0; h < 24; h++ {
			if s := grid[d*24+h]; s.Samples >= minSamplesForAdvice {
				sum += s.AveragePeak
				cnt++
			}
		}
		if cnt >= 6 && sum/float64(cnt) > best { // need a reasonable share of the day observed
			best = sum / float64(cnt)
			p.WipeDay = &WipeDay{Weekday: d, AveragePeak: round1(best)}
		}
	}
	return p
}

// pickNonOverlapping keeps the first n windows whose hours do not overlap an
// already chosen window (so the advice is n genuinely different times).
func pickNonOverlapping(sorted []Window, n, span int) []Window {
	out := []Window{}
	taken := map[int]bool{}
	for _, w := range sorted {
		start := w.Weekday*24 + w.StartHour
		clash := false
		for i := 0; i < span; i++ {
			if taken[(start+i)%(7*24)] {
				clash = true
				break
			}
		}
		if clash {
			continue
		}
		for i := 0; i < span; i++ {
			taken[(start+i)%(7*24)] = true
		}
		out = append(out, w)
		if len(out) == n {
			break
		}
	}
	return out
}

func confidence(sampledHours int) string {
	switch {
	case sampledHours == 0:
		return "NONE"
	case sampledHours < 7*24:
		return "LOW" // under a week of observed hours
	case sampledHours < 3*7*24:
		return "MEDIUM"
	default:
		return "HIGH"
	}
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
