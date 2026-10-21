package planner

import "testing"

func fullWeek(peak func(d, h int) float64, samples int) []Slot {
	var out []Slot
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			out = append(out, Slot{Weekday: d, Hour: h, AveragePeak: peak(d, h), MaxPeak: int(peak(d, h)), Samples: samples, Kills: int(peak(d, h))})
		}
	}
	return out
}

func TestNoDataGivesNoAdvice(t *testing.T) {
	p := Build(nil, 2, 3)
	if len(p.Grid) != 168 || p.Confidence != "NONE" || len(p.EventWindows) != 0 || len(p.RestartWindows) != 0 || p.WipeDay != nil {
		t.Fatalf("empty data must not recommend anything: %+v", p)
	}
}

func TestRecommendsBusiestBlocksQuietHoursAndBusiestDay(t *testing.T) {
	// Saturday (6) 20:00-22:00 is busiest; every night 04:00 is quietest; Saturday overall busiest.
	peak := func(d, h int) float64 {
		v := 10.0
		if h == 4 {
			v = 1
		}
		if d == 6 {
			v += 5
		}
		if d == 6 && (h == 20 || h == 21) {
			v = 40
		}
		return v
	}
	p := Build(fullWeek(peak, 4), 2, 3)
	if p.Confidence != "HIGH" {
		t.Fatalf("confidence %s", p.Confidence)
	}
	if w := p.EventWindows[0]; w.Weekday != 6 || w.StartHour != 20 || w.Hours != 2 || w.AveragePeak != 40 {
		t.Fatalf("best event window %+v", w)
	}
	for i, a := range p.EventWindows {
		for j, b := range p.EventWindows {
			if i != j && a.Weekday == b.Weekday && (a.StartHour == b.StartHour || a.StartHour+1 == b.StartHour) {
				t.Fatalf("event windows overlap: %+v %+v", a, b)
			}
		}
	}
	for _, w := range p.RestartWindows {
		if w.StartHour != 4 || w.AveragePeak != 1 {
			t.Fatalf("restart windows must be the quiet 04:00 hours, got %+v", w)
		}
	}
	if p.WipeDay == nil || p.WipeDay.Weekday != 6 {
		t.Fatalf("wipe day %+v", p.WipeDay)
	}
}

func TestUnsampledHoursAreNeverRecommended(t *testing.T) {
	obs := []Slot{{Weekday: 1, Hour: 3, AveragePeak: 0.5, Samples: 1}, {Weekday: 2, Hour: 19, AveragePeak: 30, Samples: 3}, {Weekday: 2, Hour: 20, AveragePeak: 25, Samples: 3}}
	p := Build(obs, 2, 5)
	if p.Confidence != "LOW" || len(p.EventWindows) != 1 || p.EventWindows[0].StartHour != 19 {
		t.Fatalf("only the fully sampled block may be advised: %+v", p.EventWindows)
	}
	for _, w := range p.RestartWindows {
		if w.Weekday == 1 && w.StartHour == 3 {
			t.Fatal("a once-seen hour is too noisy to recommend")
		}
	}
	if p.WipeDay != nil {
		t.Fatal("a wipe day needs most of a day observed")
	}
}

func TestBlocksWrapAcrossMidnightAndWeek(t *testing.T) {
	obs := []Slot{{Weekday: 6, Hour: 23, AveragePeak: 50, Samples: 3}, {Weekday: 0, Hour: 0, AveragePeak: 50, Samples: 3}}
	p := Build(obs, 2, 1)
	if len(p.EventWindows) != 1 || p.EventWindows[0].Weekday != 6 || p.EventWindows[0].StartHour != 23 {
		t.Fatalf("Saturday 23:00 into Sunday 00:00 is one block: %+v", p.EventWindows)
	}
}
