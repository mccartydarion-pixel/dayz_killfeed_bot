package ownerops

import (
	"testing"
	"time"
)

func TestServerDown(t *testing.T) {
	now := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	cases := []struct {
		name        string
		in          DownInput
		down, known bool
	}{
		{"growing a minute ago", DownInput{Now: now, LastGrowth: ago(time.Minute), LastListing: ago(30 * time.Second)}, false, true},
		{"a restart: still for six minutes", DownInput{Now: now, LastGrowth: ago(6 * time.Minute), LastListing: ago(30 * time.Second)}, false, true},
		{"still for twenty minutes", DownInput{Now: now, LastGrowth: ago(20 * time.Minute), LastListing: ago(30 * time.Second)}, true, true},
		{"the outage of 2026-10-10: still for four hours", DownInput{Now: now, LastGrowth: ago(4 * time.Hour), LastListing: ago(time.Minute)}, true, true},
		{"a shorter wait set by the operator", DownInput{Now: now, LastGrowth: ago(12 * time.Minute), LastListing: ago(time.Minute), After: 10 * time.Minute}, true, true},
		{"no growth seen yet", DownInput{Now: now, LastListing: ago(time.Minute)}, false, false},
		{"the listing is failing", DownInput{Now: now, LastGrowth: ago(time.Hour), LastListing: ago(time.Minute), ListingFailed: true}, false, false},
		{"the listing is old", DownInput{Now: now, LastGrowth: ago(time.Hour), LastListing: ago(10 * time.Minute)}, false, false},
		{"never listed", DownInput{Now: now, LastGrowth: ago(time.Hour)}, false, false},
		{"a server switched off days ago", DownInput{Now: now, LastGrowth: ago(72 * time.Hour), LastListing: ago(time.Minute)}, false, true},
	}
	for _, c := range cases {
		_, down, known := ServerDown(c.in)
		if down != c.down || known != c.known {
			t.Errorf("%s: down=%v known=%v, want down=%v known=%v", c.name, down, known, c.down, c.known)
		}
	}
}

func TestFailedRestartAlertsSooner(t *testing.T) {
	boot := time.Date(2026, 10, 10, 13, 17, 46, 0, time.UTC)
	run := 68 * time.Minute
	stopped := boot.Add(66 * time.Minute) // the logs stop at the scheduled shutdown
	at := func(d time.Duration) DownInput {
		now := stopped.Add(d)
		return DownInput{Now: now, LastGrowth: stopped, LastListing: now.Add(-30 * time.Second), BootAt: boot, RunLength: run}
	}
	if in := at(2 * time.Minute); GameState(in) != GameOnline {
		t.Errorf("two minutes into a restart reads %s", GameState(in))
	}
	// A healthy restart shows nothing new for about ten minutes.
	if in := at(11 * time.Minute); GameState(in) != GameRestarting {
		t.Errorf("eleven minutes into a restart reads %s, want RESTARTING", GameState(in))
	}
	if _, down, _ := ServerDown(at(14 * time.Minute)); down {
		t.Error("14 minutes is still inside what a healthy restart can take to show")
	}
	if _, down, _ := ServerDown(at(15 * time.Minute)); !down {
		t.Error("a restart that has not come back after 15 minutes must alert")
	}
	// Logs that stop in the middle of a run wait the full 20 minutes.
	mid := boot.Add(30 * time.Minute)
	now := mid.Add(15 * time.Minute)
	in := DownInput{Now: now, LastGrowth: mid, LastListing: now, BootAt: boot, RunLength: run}
	if _, down, _ := ServerDown(in); down || in.RestartDue() {
		t.Error("a mid-run pause of 15 minutes must not alert")
	}
	if got := at(0).NextRestart(); !got.Equal(boot.Add(run)) {
		t.Errorf("next restart = %s", got)
	}
}

func TestTypicalRunLength(t *testing.T) {
	base := time.Date(2026, 10, 9, 0, 54, 26, 0, time.UTC)
	var starts []time.Time
	for i := 0; i < 8; i++ {
		starts = append(starts, base.Add(time.Duration(i)*68*time.Minute+time.Duration(i%3)*20*time.Second))
	}
	starts = append(starts, starts[len(starts)-1].Add(10*time.Hour)) // one outage does not move the median
	got := TypicalRunLength(starts)
	if got < 67*time.Minute || got > 69*time.Minute {
		t.Fatalf("run length = %s, want about 68m", got)
	}
	if TypicalRunLength(starts[:2]) != 0 {
		t.Fatal("two starts are not enough to know the schedule")
	}
}
