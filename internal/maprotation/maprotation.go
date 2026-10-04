// Package maprotation is the rules of map rotation with a player vote (docs/MAP_ROTATION.md):
// which file names an owner may configure, how cfggameplay.json is edited, what a spawn file must
// look like, which map comes next, how a vote is counted and when a vote opens and a switch is
// applied.
//
// Nothing in this package can write to a game server. It has no upload call; the only Nitrado
// surface it uses is Reader, three read methods. The writes live in the sub-package mapswitch.
package maprotation

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Rotation orders and who decided the next map.
const (
	OrderSequence = "SEQUENCE"
	OrderRandom   = "RANDOM"

	DecidedRotation = "ROTATION"
	DecidedVote     = "VOTE"
	DecidedStaff    = "STAFF"
)

// Limits.
const (
	MaxMaps        = 5
	MinEnabledMaps = 2
	MinVoteMinutes = 5
	MaxVoteMinutes = 120
	// VoteCloseLead is how long before a scheduled restart a vote closes and the files are written.
	VoteCloseLead = 5 * time.Minute
	// MinWriteLead: no switch is started closer than this to a known scheduled restart, so a
	// restart cannot land between the two file writes.
	MinWriteLead = 2 * time.Minute

	// File size limits. A file outside them is refused before anything is written.
	MaxGameplayBytes = 1 << 20
	MaxSpawnBytes    = 1 << 20
	MaxMapFileBytes  = 16 << 20
)

// The two mission files a switch writes, and the folder the owner's maps live in.
const (
	GameplayFile    = "cfggameplay.json"
	SpawnPointsFile = "cfgplayerspawnpoints.xml"
	CustomDir       = "custom"
)

var (
	ErrFileName = errors.New("a file name may only use letters, digits, dot, underscore and dash (1 to 80 characters), with no folder")
	ErrMapExt   = errors.New("the map file must be a .json file")
	ErrSpawnExt = errors.New("the spawn file must be a .xml file")
	// ErrReservedName: Champion's own spawner file can never be a map (a switch removes the map
	// files of the rotation from objectSpawnersArr).
	ErrReservedName = errors.New("that file name is used by Champion itself")
)

var fileNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

// reservedNames are Champion's own files inside custom/.
var reservedNames = map[string]bool{"champion_shop_delivery.json": true}

func validateName(name, ext string, extErr error) error {
	if !fileNameRe.MatchString(name) || strings.Contains(name, "..") || strings.HasPrefix(name, ".") {
		return ErrFileName
	}
	if !strings.HasSuffix(strings.ToLower(name), ext) || len(name) <= len(ext) {
		return extErr
	}
	if reservedNames[strings.ToLower(name)] {
		return ErrReservedName
	}
	return nil
}

// ValidateMapFile checks an owner-supplied map file name: a base name only, ending in .json.
func ValidateMapFile(name string) error { return validateName(name, ".json", ErrMapExt) }

// ValidateSpawnFile checks the name of the spawn file an owner uploaded on the website: a base name
// only, ending in .xml. The name is kept for display; the contents are stored in Champion.
func ValidateSpawnFile(name string) error { return validateName(name, ".xml", ErrSpawnExt) }

// Map is one configured map.
type Map struct {
	ID        int64
	Name      string
	MapFile   string
	SpawnFile string
	Enabled   bool
	Position  int
}

// Enabled returns the enabled maps in rotation order.
func Enabled(maps []Map) []Map {
	out := make([]Map, 0, len(maps))
	for _, m := range maps {
		if m.Enabled {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

// Find returns the map with id.
func Find(maps []Map, id int64) (Map, bool) {
	for _, m := range maps {
		if m.ID == id && id != 0 {
			return m, true
		}
	}
	return Map{}, false
}

// RotationOrder is the enabled maps in the order the rotation reaches them, starting with the one
// after the current map (the current map itself comes last). With no known current map it is the
// plain rotation order.
func RotationOrder(maps []Map, currentID int64) []Map {
	en := Enabled(maps)
	cur, ok := Find(maps, currentID)
	if !ok || len(en) == 0 {
		return en
	}
	split := len(en)
	for i, m := range en {
		if m.Position > cur.Position {
			split = i
			break
		}
	}
	var out []Map
	for _, m := range append(append([]Map{}, en[split:]...), en[:split]...) {
		if m.ID != cur.ID {
			out = append(out, m)
		}
	}
	if cur.Enabled {
		out = append(out, cur)
	}
	return out
}

// NextInSequence is the next enabled map after the current one, wrapping around.
func NextInSequence(maps []Map, currentID int64) (Map, bool) {
	order := RotationOrder(maps, currentID)
	if len(order) == 0 {
		return Map{}, false
	}
	return order[0], true
}

// NextRandom picks an enabled map other than the current one (pick(n) returns 0..n-1). Only when
// the current map is the single enabled map is it returned again.
func NextRandom(maps []Map, currentID int64, pick func(n int) int) (Map, bool) {
	var others []Map
	for _, m := range Enabled(maps) {
		if m.ID != currentID {
			others = append(others, m)
		}
	}
	if len(others) == 0 {
		cur, ok := Find(maps, currentID)
		return cur, ok && cur.Enabled
	}
	i := 0
	if pick != nil && len(others) > 1 {
		if i = pick(len(others)); i < 0 || i >= len(others) {
			i = 0
		}
	}
	return others[i], true
}

// NextByRotation applies the configured order.
func NextByRotation(order string, maps []Map, currentID int64, pick func(n int) int) (Map, bool) {
	if order == OrderRandom {
		return NextRandom(maps, currentID, pick)
	}
	return NextInSequence(maps, currentID)
}

// VoteOptions are the maps players choose between, in rotation order: every enabled map except the
// current one when that leaves at least two; otherwise every enabled map.
func VoteOptions(maps []Map, currentID int64) []Map {
	order := RotationOrder(maps, currentID)
	var others []Map
	for _, m := range order {
		if m.ID != currentID {
			others = append(others, m)
		}
	}
	if len(others) >= 2 {
		return others
	}
	return order
}

// Tally counts ballots. optionIDs is the vote's options in rotation order; ballots holds one map
// id per voter. A ballot for a map that is not an option is ignored. The winner has the most
// votes; among tied maps the one earliest in optionIDs wins. With no valid ballot winner is 0.
func Tally(optionIDs []int64, ballots []int64) (counts map[int64]int, winner int64, total int) {
	counts = make(map[int64]int, len(optionIDs))
	for _, id := range optionIDs {
		counts[id] = 0
	}
	for _, b := range ballots {
		if _, ok := counts[b]; ok {
			counts[b]++
			total++
		}
	}
	if total == 0 {
		return counts, 0, 0
	}
	best := -1
	for _, id := range optionIDs {
		if counts[id] > best {
			best, winner = counts[id], id
		}
	}
	return counts, winner, total
}

// ResolveVote decides the next map when a vote closes: a staff choice wins, then the vote, then
// (no votes) the rotation.
func ResolveVote(order string, maps []Map, currentID, staffID int64, optionIDs []int64, counts map[int64]int, pick func(n int) int) (Map, string, bool) {
	if m, ok := Find(maps, staffID); ok && m.Enabled {
		return m, DecidedStaff, true
	}
	best, winner := 0, int64(0)
	for _, id := range optionIDs {
		if m, ok := Find(maps, id); ok && m.Enabled && counts[id] > best {
			best, winner = counts[id], id
		}
	}
	if m, ok := Find(maps, winner); ok {
		return m, DecidedVote, true
	}
	m, ok := NextByRotation(order, maps, currentID, pick)
	return m, DecidedRotation, ok
}

// --- restart counting --------------------------------------------------------------------------

// RestartsUntilSwitch is how many restarts are left before the map changes: with every = N the
// map changes on the Nth restart since the last switch. It is never below 1.
func RestartsUntilSwitch(every, sinceSwitch int) int {
	if every < 1 {
		every = 1
	}
	if n := every - sinceSwitch; n > 1 {
		return n
	}
	return 1
}

// FinalPeriod reports whether the next restart is the one that changes the map.
func FinalPeriod(every, sinceSwitch int) bool { return RestartsUntilSwitch(every, sinceSwitch) == 1 }

// --- timing ------------------------------------------------------------------------------------

// VoteWindow is when a vote opens and closes in the final period. With a known scheduled restart
// it opens voteMinutes + 5 minutes before it and closes 5 minutes before it. Without one it opens
// when the period begins and stays open for voteMinutes.
func VoteWindow(periodStart time.Time, nextRestart *time.Time, voteMinutes int) (openAt, closeAt time.Time, scheduled bool) {
	d := time.Duration(voteMinutes) * time.Minute
	if nextRestart != nil {
		closeAt = nextRestart.Add(-VoteCloseLead)
		return closeAt.Add(-d), closeAt, true
	}
	return periodStart, periodStart.Add(d), false
}

// Phases of a period (the time between two restarts).
const (
	PhaseIdle    = "IDLE"
	PhaseVoting  = "VOTING"
	PhaseDecided = "DECIDED"
	PhaseDone    = "DONE"
)

// Settings are the owner's choices the planner needs.
type Settings struct {
	EveryRestarts int
	Order         string
	VoteEnabled   bool
	VoteMinutes   int
}

// State is what Champion remembers between ticks.
type State struct {
	CurrentMapID        int64
	LastBootFile        string
	RestartsSinceSwitch int
	Phase               string
	PhaseStartedAt      time.Time
	NextMapID           int64
	NextDecidedBy       string
	StaffNextMapID      int64
	VoteOpen            bool
	VoteClosesAt        time.Time
}

// Observation is what a tick sees of the server.
type Observation struct {
	BootFile    string     // the current boot's ADM file ("" = no boot recorded yet)
	BootAt      time.Time  // when that boot was first seen
	NextRestart *time.Time // the next scheduled restart, nil when unknown
}

// StepKind is the one thing a tick does next.
type StepKind string

const (
	StepNone      StepKind = "NONE"
	StepBaseline  StepKind = "BASELINE"   // remember the current boot; nothing is counted
	StepRestart   StepKind = "RESTART"    // a new boot: count it (or activate an applied switch)
	StepOpenVote  StepKind = "OPEN_VOTE"  // open a vote closing at ClosesAt over Options
	StepCloseVote StepKind = "CLOSE_VOTE" // close the vote and decide
	StepDecide    StepKind = "DECIDE"     // the next map is Map, decided by DecidedBy
	StepApply     StepKind = "APPLY"      // write the files for the decided map
)

// Step is the planner's answer.
type Step struct {
	Kind      StepKind
	ClosesAt  time.Time
	Options   []Map
	Map       Map
	DecidedBy string
}

// Plan decides the next step. It is a pure function: the same inputs always give the same step
// (pick supplies the randomness of the RANDOM order).
func Plan(cfg Settings, st State, maps []Map, obs Observation, now time.Time, pick func(n int) int) Step {
	if obs.BootFile == "" {
		return Step{Kind: StepNone}
	}
	if st.LastBootFile == "" {
		return Step{Kind: StepBaseline}
	}
	if obs.BootFile != st.LastBootFile {
		return Step{Kind: StepRestart}
	}
	if len(Enabled(maps)) < MinEnabledMaps || !FinalPeriod(cfg.EveryRestarts, st.RestartsSinceSwitch) {
		return Step{Kind: StepNone}
	}
	switch st.Phase {
	case PhaseDone:
		return Step{Kind: StepNone}
	case PhaseDecided:
		if next, ok := Find(maps, st.NextMapID); !ok || !next.Enabled {
			return decide(cfg, st, maps, pick) // the decided map was removed: decide again
		}
		// A staff choice made after the decision still wins, as long as nothing was written.
		if staff, ok := Find(maps, st.StaffNextMapID); ok && staff.Enabled {
			return Step{Kind: StepDecide, Map: staff, DecidedBy: DecidedStaff}
		}
		if obs.NextRestart != nil && obs.NextRestart.After(now) && obs.NextRestart.Sub(now) < MinWriteLead {
			return Step{Kind: StepNone} // too close to the restart: wait for the next period
		}
		return Step{Kind: StepApply}
	case PhaseVoting:
		if !st.VoteOpen {
			return decide(cfg, st, maps, pick)
		}
		if !now.Before(st.VoteClosesAt) {
			return Step{Kind: StepCloseVote}
		}
		return Step{Kind: StepNone}
	}
	// IDLE.
	start := st.PhaseStartedAt
	if start.IsZero() {
		start = obs.BootAt
	}
	openAt, closeAt, scheduled := VoteWindow(start, obs.NextRestart, cfg.VoteMinutes)
	staff, hasStaff := Find(maps, st.StaffNextMapID)
	hasStaff = hasStaff && staff.Enabled
	carried, hasCarried := Find(maps, st.NextMapID)
	hasCarried = hasCarried && carried.Enabled && st.NextDecidedBy != ""
	if cfg.VoteEnabled && !hasStaff && !hasCarried {
		if now.Before(openAt) {
			return Step{Kind: StepNone}
		}
		if !scheduled {
			// Opened as soon as the final period begins, open for the full length from now.
			return Step{Kind: StepOpenVote, ClosesAt: now.Add(time.Duration(cfg.VoteMinutes) * time.Minute), Options: VoteOptions(maps, st.CurrentMapID)}
		}
		if now.Before(closeAt) {
			return Step{Kind: StepOpenVote, ClosesAt: closeAt, Options: VoteOptions(maps, st.CurrentMapID)}
		}
		// Too late for a vote in this period: the rotation decides.
	}
	if now.Before(closeAt) {
		return Step{Kind: StepNone}
	}
	return decide(cfg, st, maps, pick)
}

func decide(cfg Settings, st State, maps []Map, pick func(n int) int) Step {
	if m, ok := Find(maps, st.StaffNextMapID); ok && m.Enabled {
		return Step{Kind: StepDecide, Map: m, DecidedBy: DecidedStaff}
	}
	if m, ok := Find(maps, st.NextMapID); ok && m.Enabled && st.NextDecidedBy != "" {
		return Step{Kind: StepDecide, Map: m, DecidedBy: st.NextDecidedBy}
	}
	m, ok := NextByRotation(cfg.Order, maps, st.CurrentMapID, pick)
	if !ok {
		return Step{Kind: StepNone}
	}
	return Step{Kind: StepDecide, Map: m, DecidedBy: DecidedRotation}
}
