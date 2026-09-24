package discord

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// hit_zone / damage on a kill come from the reliably correlated lethal hit (killfeed.FinalHit)
// when the kill line itself has none; without one they are absent - never approximated.
func TestKillfeedHitVariablesFromCorrelatedFinalHit(t *testing.T) {
	dist, dmg := 74.9605, 22.0527
	ev := &killfeed.Event{Type: killfeed.EventPlayerKill, Killer: &killfeed.PlayerRef{Name: "K"}, Victim: &killfeed.PlayerRef{Name: "V"},
		Weapon: "M4-A1", Distance: &dist, FinalHit: &killfeed.FinalHit{Zone: "Head", ZoneID: "0", Damage: &dmg}}
	m := killfeedVars(ev, "")
	if m["hit_zone"] != "Head" || m["damage"] != "22.1" {
		t.Fatalf("correlated hit: %v %v", m["hit_zone"], m["damage"])
	}
	// The headshot label and the default card keep reading only the kill's own hit fields.
	if _, ok := m["headshot"]; ok {
		t.Fatal("headshot stays tied to Event.HitZone (unchanged statistics and default card)")
	}
	ev.FinalHit = nil
	m = killfeedVars(ev, "")
	if _, ok := m["hit_zone"]; ok {
		t.Fatal("no correlated hit: hit_zone is absent")
	}
	if _, ok := m["damage"]; ok {
		t.Fatal("no correlated hit: damage is absent")
	}
}
