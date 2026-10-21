// Package fights groups kills into fights (docs/FIGHT_REPLAY.md). A fight is a run of kills on one
// server that are close in time and connected - by a shared participant or by distance. Grouping is
// pure: the same kills always produce the same fights, and a fight is identified by its first kill.
package fights

import (
	"math"
	"sort"
	"time"
)

// Grouping thresholds. A firefight's kills come seconds to a couple of minutes apart; three
// minutes with no kill ends it. Kills with no shared participant still belong together when they
// happen within shooting distance of each other.
const (
	DefaultGap    = 3 * time.Minute
	DefaultRadius = 800.0 // metres
)

// Point is a horizontal map position in metres.
type Point struct{ X, Z float64 }

// Kill is one persisted kill. Positions are nil when the ADM line carried none.
type Kill struct {
	ID        int64
	At        time.Time
	KillerID  int64
	VictimID  int64
	KillerPos *Point
	VictimPos *Point
}

// Fight is one group of kills, in time order.
type Fight struct {
	Kills []Kill
}

// ID identifies the fight: its first kill.
func (f Fight) ID() int64 { return f.Kills[0].ID }

// Start and End are the first and last kill times.
func (f Fight) Start() time.Time { return f.Kills[0].At }
func (f Fight) End() time.Time   { return f.Kills[len(f.Kills)-1].At }

// Participants returns every killer and victim, in order of first appearance.
func (f Fight) Participants() []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, k := range f.Kills {
		for _, id := range []int64{k.KillerID, k.VictimID} {
			if id != 0 && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// Center is the mean of the fight's known kill positions; ok is false when none was logged.
func (f Fight) Center() (Point, bool) {
	var sum Point
	n := 0
	for _, k := range f.Kills {
		if p := position(k); p != nil {
			sum.X += p.X
			sum.Z += p.Z
			n++
		}
	}
	if n == 0 {
		return Point{}, false
	}
	return Point{X: sum.X / float64(n), Z: sum.Z / float64(n)}, true
}

// position is where a kill happened: where the victim fell, else where the killer stood.
func position(k Kill) *Point {
	if k.VictimPos != nil {
		return k.VictimPos
	}
	return k.KillerPos
}

func distance(a, b Point) float64 { return math.Hypot(a.X-b.X, a.Z-b.Z) }

// connected reports whether k belongs to f: it shares a participant with the fight, or it happened
// within radius of one of the fight's kills.
func connected(f *Fight, k Kill, radius float64) bool {
	kp := position(k)
	for _, other := range f.Kills {
		if k.KillerID == other.KillerID || k.KillerID == other.VictimID || k.VictimID == other.KillerID || k.VictimID == other.VictimID {
			return true
		}
		if op := position(other); kp != nil && op != nil && distance(*kp, *op) <= radius {
			return true
		}
	}
	return false
}

// Group splits kills into fights. A kill joins the open fight it is connected to whose last kill
// is at most gap earlier (the most recent such fight when several qualify); otherwise it starts a
// new one. The result is ordered by first kill.
func Group(kills []Kill, gap time.Duration, radius float64) []Fight {
	sorted := append([]Kill(nil), kills...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].At.Equal(sorted[j].At) {
			return sorted[i].At.Before(sorted[j].At)
		}
		return sorted[i].ID < sorted[j].ID
	})
	var out []*Fight
	for _, k := range sorted {
		var home *Fight
		for i := len(out) - 1; i >= 0; i-- {
			f := out[i]
			if k.At.Sub(f.End()) > gap {
				continue
			}
			if connected(f, k, radius) {
				home = f
				break
			}
		}
		if home == nil {
			home = &Fight{}
			out = append(out, home)
		}
		home.Kills = append(home.Kills, k)
	}
	fights := make([]Fight, 0, len(out))
	for _, f := range out {
		fights = append(fights, *f)
	}
	sort.SliceStable(fights, func(i, j int) bool { return fights[i].Start().Before(fights[j].Start()) })
	return fights
}

// Containing returns the fight that includes killID.
func Containing(fights []Fight, killID int64) (Fight, bool) {
	for _, f := range fights {
		for _, k := range f.Kills {
			if k.ID == killID {
				return f, true
			}
		}
	}
	return Fight{}, false
}
