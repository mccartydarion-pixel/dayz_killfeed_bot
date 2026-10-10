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
