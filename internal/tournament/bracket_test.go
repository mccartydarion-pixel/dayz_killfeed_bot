package tournament

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 10, 19, 0, 0, 0, time.UTC)

// fixture builds a LIVE-ready tournament with n checked-in entries (ids 1..n, one player each,
// player ids 100+i) and the given bracket size and seeding.
func fixture(n, size int, seeding string, bestOf int) *Tournament {
	t := &Tournament{ID: 1, InstallationID: 7, GuildID: 1, ServerID: 1, Name: "Test", Status: StatusCheckin, Format: FormatSingleElim,
		TeamSize: 1, BracketSize: size, BestOf: bestOf, Seeding: seeding, StartsAt: t0, CheckinMinutes: 30,
		Rules: Rules{MatchTimerMinutes: 10, ReadyMinutes: 2, Arenas: []Arena{{No: 1, Name: "Arena 1", X: 1000, Z: 1000, Radius: 150}, {No: 2, Name: "Arena 2", X: 5000, Z: 5000, Radius: 100}}}}
	for i := 1; i <= n; i++ {
		at := t0.Add(-time.Duration(60-i) * time.Minute)
		t.Entries = append(t.Entries, &Entry{ID: int64(i), TeamNo: i, Status: EntryActive, CheckedInAt: &at,
			Players: []EntryPlayer{{PlayerID: int64(100 + i), DiscordUserID: fmt.Sprint("d", i), Name: fmt.Sprint("P", i)}}})
	}
	return t
}

// assignIDs gives stored-looking ids to matches, as the repository would, and links them.
func assignIDs(t *Tournament) {
	for i, m := range t.Matches {
		m.ID = int64(100 + i)
	}
	t.LinkNext()
}

func TestSeedOrder(t *testing.T) {
	cases := map[int][]int{
		2:  {1, 2},
		4:  {1, 4, 2, 3},
		8:  {1, 8, 4, 5, 2, 7, 3, 6},
		16: {1, 16, 8, 9, 4, 13, 5, 12, 2, 15, 7, 10, 3, 14, 6, 11},
	}
	for n, want := range cases {
		if got := SeedOrder(n); !reflect.DeepEqual(got, want) {
			t.Errorf("SeedOrder(%d) = %v, want %v", n, got, want)
		}
	}
	o := SeedOrder(32)
	if len(o) != 32 || o[0] != 1 || o[1] != 32 || o[2] != 16 || o[3] != 17 {
		t.Errorf("SeedOrder(32) starts %v", o[:4])
	}
	seen := map[int]bool{}
	for _, s := range o {
		seen[s] = true
	}
	if len(seen) != 32 {
		t.Error("SeedOrder(32) repeats a seed")
	}
}

func TestRoundNames(t *testing.T) {
	cases := []struct {
		size int
		want []string
	}{
		{4, []string{"Semi-final", "Final"}},
		{8, []string{"Quarter-final", "Semi-final", "Final"}},
		{16, []string{"Round of 16", "Quarter-final", "Semi-final", "Final"}},
		{32, []string{"Round 1", "Round of 16", "Quarter-final", "Semi-final", "Final"}},
	}
	for _, c := range cases {
		rounds := Rounds(c.size)
		if rounds != len(c.want) {
			t.Fatalf("Rounds(%d) = %d", c.size, rounds)
		}
		for r := 1; r <= rounds; r++ {
			if got := RoundName(r, rounds); got != c.want[r-1] {
				t.Errorf("size %d round %d: %q, want %q", c.size, r, got, c.want[r-1])
			}
		}
	}
}

func TestBracketFullRankedAllSizes(t *testing.T) {
	for _, size := range []int{4, 8, 16, 32} {
		tr := fixture(size, size, SeedingRanked, 1)
		var ranks []Rank
		for _, e := range tr.Entries {
			ranks = append(ranks, Rank{EntryID: e.ID, RP: int64(1000 - int(e.ID))}) // entry 1 has the most RP
		}
		AssignSeeds(tr, ranks, nil)
		BuildBracket(tr, t0)
		assignIDs(tr)
		if len(tr.Matches) != size-1 {
			t.Fatalf("size %d: %d matches", size, len(tr.Matches))
		}
		for _, e := range tr.Entries {
			if e.Seed == nil || *e.Seed != int(e.ID) {
				t.Fatalf("size %d: entry %d seeded %v", size, e.ID, e.Seed)
			}
		}
		// Round 1 pairs seed s with size+1-s, in standard order; nothing is a bye.
		order := SeedOrder(size)
		for p := 1; p <= size/2; p++ {
			m := tr.MatchAt(1, p)
			if m.EntryA == nil || m.EntryB == nil || *m.EntryA != int64(order[2*(p-1)]) || *m.EntryB != int64(order[2*(p-1)+1]) {
				t.Fatalf("size %d match %d: %v v %v", size, p, m.EntryA, m.EntryB)
			}
			if m.Status != MatchPending {
				t.Fatalf("size %d match %d is %s", size, p, m.Status)
			}
			if *m.EntryA+*m.EntryB != int64(size+1) {
				t.Fatalf("size %d match %d does not pair s with n+1-s", size, p)
			}
		}
		// Next links: round r position p feeds round r+1 position ceil(p/2); the final has none.
		for _, m := range tr.Matches {
			if m.Round == Rounds(size) {
				if m.NextMatchID != nil {
					t.Fatalf("the final links on")
				}
				continue
			}
			next := tr.MatchAt(m.Round+1, (m.Position+1)/2)
			if m.NextMatchID == nil || *m.NextMatchID != next.ID {
				t.Fatalf("size %d round %d pos %d links to %v, want %d", size, m.Round, m.Position, m.NextMatchID, next.ID)
			}
		}
	}
}

func TestBracketByesAdvance(t *testing.T) {
	// 5 entries in an 8 bracket: seeds 1, 2 and 3 get byes (they would meet 8, 7 and 6).
	tr := fixture(5, 8, SeedingRanked, 1)
	var ranks []Rank
	for _, e := range tr.Entries {
		ranks = append(ranks, Rank{EntryID: e.ID, RP: 100 - e.ID})
	}
	AssignSeeds(tr, ranks, nil)
	BuildBracket(tr, t0)
	assignIDs(tr)
	byes, pending := 0, 0
	for p := 1; p <= 4; p++ {
		m := tr.MatchAt(1, p)
		if m.Status == MatchDone {
			byes++
			if m.WinnerEntry == nil || m.EndedAt == nil {
				t.Fatalf("bye at %d has no winner", p)
			}
		} else {
			pending++
		}
	}
	if byes != 3 || pending != 1 {
		t.Fatalf("byes=%d pending=%d", byes, pending)
	}
	// Seed 1 (match 1) and the winner of 4 v 5 (match 2) meet in the first semi; 2 and 3 in the other.
	semi1, semi2 := tr.MatchAt(2, 1), tr.MatchAt(2, 2)
	if semi1.EntryA == nil || *semi1.EntryA != 1 || semi1.EntryB != nil || semi1.Status != MatchPending {
		t.Fatalf("semi 1: %+v", semi1)
	}
	if semi2.EntryA == nil || semi2.EntryB == nil || *semi2.EntryA != 2 || *semi2.EntryB != 3 {
		t.Fatalf("semi 2: %v v %v", semi2.EntryA, semi2.EntryB)
	}
	// The only callable match is 4 v 5.
	if next := tr.NextPending(); next == nil || next.Round != 1 || next.Position != 2 {
		t.Fatalf("next pending: %+v", next)
	}
}

func TestBracketTwoEntriesIn32CascadesByes(t *testing.T) {
	tr := fixture(2, 32, SeedingRanked, 1)
	AssignSeeds(tr, []Rank{{1, 10}, {2, 5}}, nil)
	BuildBracket(tr, t0)
	assignIDs(tr)
	final := tr.Final()
	if final.Round != 5 || final.EntryA == nil || final.EntryB == nil || *final.EntryA != 1 || *final.EntryB != 2 || final.Status != MatchPending {
		t.Fatalf("final: %+v", final)
	}
	for _, m := range tr.Matches {
		if m.Round < 5 && m.Status != MatchDone {
			t.Fatalf("round %d pos %d not settled: %s", m.Round, m.Position, m.Status)
		}
	}
	if next := tr.NextPending(); next != final {
		t.Fatalf("next pending must be the final")
	}
}

func TestRandomSeedingIsReproducibleAndCoversEveryone(t *testing.T) {
	a, b := fixture(8, 8, SeedingRandom, 1), fixture(8, 8, SeedingRandom, 1)
	AssignSeeds(a, nil, rand.New(rand.NewSource(42)))
	AssignSeeds(b, nil, rand.New(rand.NewSource(42)))
	seen := map[int]bool{}
	moved := false
	for i := range a.Entries {
		if *a.Entries[i].Seed != *b.Entries[i].Seed {
			t.Fatal("the same seed must give the same draw")
		}
		seen[*a.Entries[i].Seed] = true
		if *a.Entries[i].Seed != i+1 {
			moved = true
		}
	}
	if len(seen) != 8 || !moved {
		t.Fatalf("seeds %v", seen)
	}
	c := fixture(8, 8, SeedingRandom, 1)
	AssignSeeds(c, nil, rand.New(rand.NewSource(7)))
	same := true
	for i := range a.Entries {
		if *a.Entries[i].Seed != *c.Entries[i].Seed {
			same = false
		}
	}
	if same {
		t.Fatal("different seeds should (almost surely) give different draws")
	}
}

func TestAssignSeedsSkipsAbsent(t *testing.T) {
	tr := fixture(4, 4, SeedingRanked, 1)
	tr.Entries[2].CheckedInAt = nil
	tr.Entries[3].Status = EntryWithdrawn
	AssignSeeds(tr, nil, nil)
	if tr.Entries[2].Seed != nil || tr.Entries[3].Seed != nil || tr.Entries[0].Seed == nil || tr.Entries[1].Seed == nil {
		t.Fatalf("seeds: %v %v %v %v", tr.Entries[0].Seed, tr.Entries[1].Seed, tr.Entries[2].Seed, tr.Entries[3].Seed)
	}
	// With no ranks, RANKED seeding is check-in order.
	if *tr.Entries[0].Seed != 1 || *tr.Entries[1].Seed != 2 {
		t.Fatalf("check-in order: %d %d", *tr.Entries[0].Seed, *tr.Entries[1].Seed)
	}
}
