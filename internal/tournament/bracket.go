package tournament

import (
	"fmt"
	"math/rand"
	"sort"
	"time"
)

// SeedOrder is the standard single-elimination slot order for a bracket of size n (a power of
// two): the seeds in the order they fill the first round from top to bottom, so that 1 meets n,
// 2 meets n-1, and the top seeds cannot meet before the final (1 v 16, 8 v 9, 4 v 13, ...).
func SeedOrder(n int) []int {
	order := []int{1}
	for len(order) < n {
		next := make([]int, 0, len(order)*2)
		size := len(order) * 2
		for _, s := range order {
			next = append(next, s, size+1-s)
		}
		order = next
	}
	return order
}

// Rounds is how many rounds a bracket of size n has.
func Rounds(n int) int {
	r := 0
	for v := n; v > 1; v /= 2 {
		r++
	}
	return r
}

// RoundName names round r (1-based) of a bracket with rounds rounds: the last is the Final, then
// the Semi-final, the Quarter-final and the Round of 16; anything earlier is "Round r".
func RoundName(r, rounds int) string {
	switch rounds - r {
	case 0:
		return "Final"
	case 1:
		return "Semi-final"
	case 2:
		return "Quarter-final"
	case 3:
		return "Round of 16"
	}
	return fmt.Sprintf("Round %d", r)
}

// Rank is an entry's standing for RANKED seeding: more RP seeds higher; ties go to the earlier
// check-in.
type Rank struct {
	EntryID int64
	RP      int64
}

// AssignSeeds gives every checked-in ACTIVE entry a seed from 1. RANKED orders by rank (an entry
// without a rank is 0 RP), RANDOM shuffles with rng (a seeded source makes the draw reproducible).
// Entries that did not check in get no seed.
func AssignSeeds(t *Tournament, ranks []Rank, rng *rand.Rand) {
	rp := map[int64]int64{}
	for _, r := range ranks {
		rp[r.EntryID] = r.RP
	}
	var in []*Entry
	for _, e := range t.Entries {
		e.Seed = nil
		if e.Status == EntryActive && e.CheckedIn() {
			in = append(in, e)
		}
	}
	// A deterministic base order first, so the same input always gives the same draw.
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if !a.CheckedInAt.Equal(*b.CheckedInAt) {
			return a.CheckedInAt.Before(*b.CheckedInAt)
		}
		return a.ID < b.ID
	})
	if t.Seeding == SeedingRanked {
		sort.SliceStable(in, func(i, j int) bool { return rp[in[i].ID] > rp[in[j].ID] })
	} else if rng != nil {
		rng.Shuffle(len(in), func(i, j int) { in[i], in[j] = in[j], in[i] })
	}
	for i, e := range in {
		seed := i + 1
		e.Seed = &seed
	}
}

// BuildBracket creates the matches of a single-elimination bracket from the seeded entries:
// bracketSize slots in standard order, a bye wherever no entry holds the seed, next links, and
// every bye already advanced (a match without an opponent is DONE with its lone entry as the
// winner; a match with no entries at all is DONE with no winner). Match ids are 0 until stored;
// links are by round and position (NextPos) until then.
func BuildBracket(t *Tournament, now time.Time) {
	n := t.BracketSize
	rounds := Rounds(n)
	bySeed := map[int]*Entry{}
	for _, e := range t.Entries {
		if e.Seed != nil {
			bySeed[*e.Seed] = e
		}
	}
	t.Matches = t.Matches[:0]
	order := SeedOrder(n)
	for r := 1; r <= rounds; r++ {
		count := n >> r
		for p := 1; p <= count; p++ {
			m := &Match{Round: r, Position: p, RoundName: RoundName(r, rounds), Status: MatchPending}
			if r < rounds {
				m.NextPos = (p + 1) / 2
			}
			if r == 1 {
				if e := bySeed[order[2*(p-1)]]; e != nil {
					m.EntryA = ptrInt64(e.ID)
				}
				if e := bySeed[order[2*(p-1)+1]]; e != nil {
					m.EntryB = ptrInt64(e.ID)
				}
			}
			t.Matches = append(t.Matches, m)
		}
	}
	// Byes, round by round: a match whose feeders are settled and that has at most one entry is
	// over before it starts.
	for r := 1; r <= rounds; r++ {
		count := n >> r
		for p := 1; p <= count; p++ {
			m := t.MatchAt(r, p)
			if r > 1 {
				fa, fb := t.MatchAt(r-1, 2*p-1), t.MatchAt(r-1, 2*p)
				if !fa.Finished() || !fb.Finished() {
					continue
				}
			}
			if m.EntryA != nil && m.EntryB != nil {
				continue
			}
			m.Status = MatchDone
			m.EndedAt = ptrTime(now)
			if m.EntryA != nil {
				m.WinnerEntry = m.EntryA
			} else if m.EntryB != nil {
				m.WinnerEntry = m.EntryB
			}
			t.advance(m)
		}
	}
}

// advance puts a finished match's winner into the next match's slot (A for an odd position, B
// for an even one).
func (t *Tournament) advance(m *Match) {
	if m.Round >= Rounds(t.BracketSize) || m.WinnerEntry == nil {
		return
	}
	next := t.MatchAt(m.Round+1, m.NextPos)
	if next == nil {
		return
	}
	if m.Position%2 == 1 {
		next.EntryA = m.WinnerEntry
	} else {
		next.EntryB = m.WinnerEntry
	}
}

// LinkNext fills NextMatchID from NextPos once matches have ids.
func (t *Tournament) LinkNext() {
	for _, m := range t.Matches {
		m.NextMatchID = nil
		if m.NextPos > 0 {
			if next := t.MatchAt(m.Round+1, m.NextPos); next != nil && next.ID != 0 {
				m.NextMatchID = ptrInt64(next.ID)
			}
		}
	}
}

// RestoreNextPos fills NextPos from NextMatchID for a tournament loaded from the store.
func (t *Tournament) RestoreNextPos() {
	for _, m := range t.Matches {
		m.NextPos = 0
		if m.NextMatchID != nil {
			if next := t.Match(*m.NextMatchID); next != nil {
				m.NextPos = next.Position
			}
		}
	}
}
