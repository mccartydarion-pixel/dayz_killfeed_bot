package fights

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 1, 20, 0, 0, 0, time.UTC)

func kill(id int64, offsetSeconds int, killer, victim int64, x, z float64) Kill {
	return Kill{ID: id, At: t0.Add(time.Duration(offsetSeconds) * time.Second), KillerID: killer, VictimID: victim, VictimPos: &Point{X: x, Z: z}}
}

func ids(f Fight) []int64 {
	out := make([]int64, 0, len(f.Kills))
	for _, k := range f.Kills {
		out = append(out, k.ID)
	}
	return out
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGroupJoinsKillsByParticipantOrDistanceWithinTheGap(t *testing.T) {
	kills := []Kill{
		kill(1, 0, 10, 20, 5000, 5000),
		kill(2, 40, 20, 10, 9000, 9000),     // shared participants, far apart (a respawn revenge): same fight
		kill(3, 90, 30, 40, 5300, 5200),     // strangers, 360 m from kill 1: same fight
		kill(4, 100, 50, 60, 12000, 2000),   // strangers, far away: a separate fight at the same time
		kill(5, 170, 60, 50, 12010, 2000),   // answers kill 4
		kill(6, 90+181, 30, 41, 5300, 5200), // 181 s after the first fight's last kill: a new fight
	}
	got := Group(kills, DefaultGap, DefaultRadius)
	if len(got) != 3 {
		t.Fatalf("expected 3 fights, got %d: %v", len(got), got)
	}
	if !equal(ids(got[0]), []int64{1, 2, 3}) || !equal(ids(got[1]), []int64{4, 5}) || !equal(ids(got[2]), []int64{6}) {
		t.Fatalf("fights = %v %v %v", ids(got[0]), ids(got[1]), ids(got[2]))
	}
	if got[0].ID() != 1 || !got[0].Start().Equal(t0) || !got[0].End().Equal(t0.Add(90*time.Second)) {
		t.Fatalf("fight 1 = id %d %s..%s", got[0].ID(), got[0].Start(), got[0].End())
	}
	if !equal(got[0].Participants(), []int64{10, 20, 30, 40}) {
		t.Fatalf("participants = %v", got[0].Participants())
	}
}

func TestGroupIsOrderIndependentAndDoesNotMutateInput(t *testing.T) {
	a := []Kill{kill(3, 90, 30, 40, 5300, 5200), kill(1, 0, 10, 20, 5000, 5000), kill(2, 40, 20, 10, 9000, 9000)}
	got := Group(a, DefaultGap, DefaultRadius)
	if len(got) != 1 || !equal(ids(got[0]), []int64{1, 2, 3}) {
		t.Fatalf("fights = %v", got)
	}
	if a[0].ID != 3 {
		t.Fatal("Group reordered its input")
	}
	if len(Group(nil, DefaultGap, DefaultRadius)) != 0 {
		t.Fatal("no kills produced a fight")
	}
}

func TestGroupWithoutPositionsUsesParticipantsOnly(t *testing.T) {
	blind := func(id int64, offset int, killer, victim int64) Kill {
		return Kill{ID: id, At: t0.Add(time.Duration(offset) * time.Second), KillerID: killer, VictimID: victim}
	}
	got := Group([]Kill{blind(1, 0, 1, 2), blind(2, 10, 3, 4), blind(3, 20, 2, 5)}, DefaultGap, DefaultRadius)
	if len(got) != 2 || !equal(ids(got[0]), []int64{1, 3}) || !equal(ids(got[1]), []int64{2}) {
		t.Fatalf("fights = %v", got)
	}
	if _, ok := got[0].Center(); ok {
		t.Fatal("a fight with no logged position reported a centre")
	}
}

func TestCenterUsesKillerPositionWhenVictimUnknownAndContaining(t *testing.T) {
	f := Fight{Kills: []Kill{
		{ID: 1, At: t0, KillerID: 1, VictimID: 2, VictimPos: &Point{X: 100, Z: 100}, KillerPos: &Point{X: 900, Z: 900}},
		{ID: 2, At: t0, KillerID: 2, VictimID: 1, KillerPos: &Point{X: 300, Z: 500}},
	}}
	c, ok := f.Center()
	if !ok || c.X != 200 || c.Z != 300 {
		t.Fatalf("centre = %+v %v", c, ok)
	}
	if got, ok := Containing([]Fight{f}, 2); !ok || got.ID() != 1 {
		t.Fatalf("Containing = %v %v", got, ok)
	}
	if _, ok := Containing([]Fight{f}, 99); ok {
		t.Fatal("an unknown kill was found in a fight")
	}
}
