// Package tournament is tournament mode (docs/TOURNAMENTS.md): single-elimination 1v1 and 2v2
// tournaments played on one server, scored from the kill feed. The bracket, the match flow and
// the kill attribution are pure functions over one in-memory Tournament; the Service loads a
// tournament under a row lock, applies one of them and writes the result back in the same
// transaction, then tells the Notifier (Discord) what happened.
package tournament

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Tournament statuses.
const (
	StatusDraft     = "DRAFT"
	StatusSignup    = "SIGNUP"
	StatusCheckin   = "CHECKIN"
	StatusLive      = "LIVE"
	StatusPaused    = "PAUSED"
	StatusFinished  = "FINISHED"
	StatusCancelled = "CANCELLED"
)

// Entry statuses.
const (
	EntryActive     = "ACTIVE"
	EntryEliminated = "ELIMINATED"
	EntryWinner     = "WINNER"
	EntryDQ         = "DQ"
	EntryWithdrawn  = "WITHDRAWN"
)

// Match statuses.
const (
	MatchPending = "PENDING"
	MatchCalled  = "CALLED"
	MatchLive    = "LIVE"
	MatchDone    = "DONE"
	MatchForfeit = "FORFEIT"
)

// Round flags: why a round was not counted (or, MANUAL, that an admin decided it).
const (
	FlagWeapon       = "WEAPON"
	FlagOutsideArena = "OUTSIDE_ARENA"
	FlagInterference = "INTERFERENCE"
	FlagNonPlayer    = "NON_PLAYER"
	FlagManual       = "MANUAL"
)

// Formats and seedings.
const (
	FormatSingleElim = "SINGLE_ELIM"
	SeedingRandom    = "RANDOM"
	SeedingRanked    = "RANKED"
)

// Limits.
const (
	MaxNameLen       = 80
	MaxWeapons       = 40
	MaxArenas        = 8
	MaxPrizes        = 8
	MaxTitleLen      = 40
	MaxPrizePoints   = 1_000_000
	DefaultCheckin   = 30
	DefaultTimer     = 10
	DefaultReady     = 3
	MaxTimerMinutes  = 120
	MaxReadyMinutes  = 60
	MaxCheckinMinute = 1440
)

var (
	ErrNotFound       = errors.New("tournament not found")
	ErrInvalid        = errors.New("invalid tournament")
	ErrWrongStatus    = errors.New("the tournament is not in the right state for that")
	ErrFull           = errors.New("the bracket is full")
	ErrAlreadyEntered = errors.New("already entered")
	ErrNotEntered     = errors.New("not entered")
	ErrPartner        = errors.New("a partner is required for a 2v2 tournament")
	ErrMatchNotFound  = errors.New("match not found")
	ErrEntryNotFound  = errors.New("entry not found")
	ErrNoMatch        = errors.New("no match to call")
)

// Arena is a circle on the map a match is played in.
type Arena struct {
	No     int     `json:"no"`
	Name   string  `json:"name"`
	X      float64 `json:"x"`
	Z      float64 `json:"z"`
	Radius float64 `json:"radius"`
}

// Rules is the tournament's rule set, stored as JSON.
type Rules struct {
	AllowedWeapons    []string `json:"allowedWeapons"`
	Arenas            []Arena  `json:"arenas"`
	MatchTimerMinutes int      `json:"matchTimerMinutes"`
	ReadyMinutes      int      `json:"readyMinutes"`
}

// Prize is what a place wins: Champion Points and, for the winner, a title.
type Prize struct {
	Place  int     `json:"place"`
	Points int64   `json:"points"`
	Title  *string `json:"title"`
}

// EntryPlayer is one player of an entry (one for 1v1, two for 2v2).
type EntryPlayer struct {
	PlayerID      int64
	DiscordUserID string
	Name          string
}

// Entry is one competitor: a player or a team.
type Entry struct {
	ID          int64
	Seed        *int
	TeamNo      int
	CheckedInAt *time.Time
	Status      string
	CreatedAt   time.Time
	Players     []EntryPlayer
}

// CheckedIn reports whether the entry checked in.
func (e Entry) CheckedIn() bool { return e.CheckedInAt != nil }

// Has reports whether playerID plays for the entry.
func (e Entry) Has(playerID int64) bool {
	for _, p := range e.Players {
		if p.PlayerID == playerID {
			return true
		}
	}
	return false
}

// Round is one scored (or flagged) moment of a match: a kill, a non-player death, or an admin
// decision.
type Round struct {
	ID          int64
	MatchID     int64
	N           int
	WinnerEntry *int64
	KillID      *int64
	KillerName  string
	VictimName  string
	Weapon      string
	Distance    *float64
	At          time.Time
	Counted     bool
	Flag        *string
	DecidedBy   *string
}

// Match is one node of the bracket. Position is 1-based within the round. A nil entry is a bye
// (round 1) or a slot not decided yet.
type Match struct {
	ID          int64
	Round       int
	Position    int
	RoundName   string
	EntryA      *int64
	EntryB      *int64
	Status      string
	ArenaNo     *int
	ScoreA      int
	ScoreB      int
	WinnerEntry *int64
	NextMatchID *int64
	// NextPos is the position of the next match (kept so a bracket can be built before ids exist).
	NextPos     int
	CalledAt    *time.Time
	StartedAt   *time.Time
	EndedAt     *time.Time
	TimerEndsAt *time.Time
	Rounds      []Round
}

// Has reports whether the entry plays in the match.
func (m *Match) Has(entryID int64) bool {
	return (m.EntryA != nil && *m.EntryA == entryID) || (m.EntryB != nil && *m.EntryB == entryID)
}

// Open reports whether kills can score in the match (called or live).
func (m *Match) Open() bool { return m.Status == MatchCalled || m.Status == MatchLive }

// Finished reports whether the match has a result.
func (m *Match) Finished() bool { return m.Status == MatchDone || m.Status == MatchForfeit }

// Tournament is the whole aggregate: the tournament row, its entries and its matches with their
// rounds. The engine mutates it in memory; the store writes it back.
type Tournament struct {
	ID                 int64
	InstallationID     int64
	GuildID            int64
	ServerID           int64
	Name               string
	Status             string
	Format             string
	TeamSize           int
	BracketSize        int
	BestOf             int
	Seeding            string
	StartsAt           time.Time
	SignupOpensAt      *time.Time
	CheckinMinutes     int
	StartedAt          *time.Time
	FinishedAt         *time.Time
	Rules              Rules
	Prizes             []Prize
	DiscordChannelID   string
	SignupMessageID    string
	BracketMessageID   string
	CreatedByDiscordID string
	CreatedAt          time.Time
	UpdatedAt          time.Time

	Entries []*Entry
	Matches []*Match
}

// Entry finds an entry by id.
func (t *Tournament) Entry(id int64) *Entry {
	for _, e := range t.Entries {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// EntryOfPlayer finds the entry a player belongs to: the one still holding a slot, else (a
// player who withdrew and never came back) the withdrawn one.
func (t *Tournament) EntryOfPlayer(playerID int64) *Entry {
	var withdrawn *Entry
	for _, e := range t.Entries {
		if !e.Has(playerID) {
			continue
		}
		if e.Status != EntryWithdrawn {
			return e
		}
		withdrawn = e
	}
	return withdrawn
}

// EntryOfDiscordUser is EntryOfPlayer by Discord user.
func (t *Tournament) EntryOfDiscordUser(discordUserID string) *Entry {
	var withdrawn *Entry
	for _, e := range t.Entries {
		for _, p := range e.Players {
			if p.DiscordUserID != discordUserID {
				continue
			}
			if e.Status != EntryWithdrawn {
				return e
			}
			withdrawn = e
		}
	}
	return withdrawn
}

// Match finds a match by id.
func (t *Tournament) Match(id int64) *Match {
	for _, m := range t.Matches {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// MatchAt finds a match by round and position.
func (t *Tournament) MatchAt(round, pos int) *Match {
	for _, m := range t.Matches {
		if m.Round == round && m.Position == pos {
			return m
		}
	}
	return nil
}

// Arena returns the arena of a match (nil when none is assigned or configured).
func (t *Tournament) Arena(m *Match) *Arena {
	if m == nil || m.ArenaNo == nil {
		return nil
	}
	for i := range t.Rules.Arenas {
		if t.Rules.Arenas[i].No == *m.ArenaNo {
			return &t.Rules.Arenas[i]
		}
	}
	return nil
}

// Current is the match in play: the live one, else the called one, else nil.
func (t *Tournament) Current() *Match {
	var called *Match
	for _, m := range t.Matches {
		if m.Status == MatchLive {
			return m
		}
		if m.Status == MatchCalled && called == nil {
			called = m
		}
	}
	return called
}

// NextPending is the next match that can be called: the lowest round, lowest position, pending
// with both entries known.
func (t *Tournament) NextPending() *Match {
	var best *Match
	for _, m := range t.Matches {
		if m.Status != MatchPending || m.EntryA == nil || m.EntryB == nil {
			continue
		}
		if best == nil || m.Round < best.Round || (m.Round == best.Round && m.Position < best.Position) {
			best = m
		}
	}
	return best
}

// Final is the last match.
func (t *Tournament) Final() *Match {
	var last *Match
	for _, m := range t.Matches {
		if last == nil || m.Round > last.Round {
			last = m
		}
	}
	return last
}

// ActiveEntries are the entries still in the running.
func (t *Tournament) ActiveEntries() []*Entry {
	var out []*Entry
	for _, e := range t.Entries {
		if e.Status == EntryActive {
			out = append(out, e)
		}
	}
	return out
}

// Running reports whether the tournament is between its start and its end.
func (t *Tournament) Running() bool { return t.Status == StatusLive || t.Status == StatusPaused }

// Over reports whether the tournament ended.
func (t *Tournament) Over() bool { return t.Status == StatusFinished || t.Status == StatusCancelled }

// RoundsToWin is how many rounds win a best-of match.
func RoundsToWin(bestOf int) int { return int(math.Ceil(float64(bestOf) / 2)) }

// Record is an entry's tally across the tournament.
type Record struct {
	Wins, Losses, RoundsWon, RoundsLost int
}

// RecordOf tallies an entry's finished matches and counted rounds.
func (t *Tournament) RecordOf(entryID int64) Record {
	var r Record
	for _, m := range t.Matches {
		if !m.Has(entryID) || m.EntryA == nil || m.EntryB == nil { // a bye is not a match played
			continue
		}
		if m.Finished() && m.WinnerEntry != nil {
			if *m.WinnerEntry == entryID {
				r.Wins++
			} else {
				r.Losses++
			}
		}
		for _, rd := range m.Rounds {
			if !rd.Counted || rd.WinnerEntry == nil {
				continue
			}
			if *rd.WinnerEntry == entryID {
				r.RoundsWon++
			} else {
				r.RoundsLost++
			}
		}
	}
	return r
}

// --- validation -----------------------------------------------------------------------------------

// Params are the fields an owner sets when creating or editing a tournament.
type Params struct {
	Name           string
	TeamSize       int
	BracketSize    int
	BestOf         int
	Seeding        string
	StartsAt       time.Time
	SignupOpensAt  *time.Time
	CheckinMinutes int
	Rules          Rules
	Prizes         []Prize
}

// NormalizeWeapon makes weapon names comparable the way the kill feed shows them: case,
// spacing, hyphens and underscores do not matter ("M4-A1", "m4a1" and "M4 A1" are one weapon).
func NormalizeWeapon(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch r {
		case ' ', '-', '_', '.':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Validate checks and normalises the parameters.
func (p *Params) Validate() error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len([]rune(p.Name)) > MaxNameLen {
		return fmt.Errorf("%w: name must be 1 to %d characters", ErrInvalid, MaxNameLen)
	}
	if p.TeamSize == 0 {
		p.TeamSize = 1
	}
	if p.TeamSize != 1 && p.TeamSize != 2 {
		return fmt.Errorf("%w: team size must be 1 or 2", ErrInvalid)
	}
	if p.BracketSize == 0 {
		p.BracketSize = 8
	}
	switch p.BracketSize {
	case 4, 8, 16, 32:
	default:
		return fmt.Errorf("%w: bracket size must be 4, 8, 16 or 32", ErrInvalid)
	}
	if p.BestOf == 0 {
		p.BestOf = 1
	}
	switch p.BestOf {
	case 1, 3, 5:
	default:
		return fmt.Errorf("%w: best of must be 1, 3 or 5", ErrInvalid)
	}
	p.Seeding = strings.ToUpper(strings.TrimSpace(p.Seeding))
	if p.Seeding == "" {
		p.Seeding = SeedingRandom
	}
	if p.Seeding != SeedingRandom && p.Seeding != SeedingRanked {
		return fmt.Errorf("%w: seeding must be RANDOM or RANKED", ErrInvalid)
	}
	if p.StartsAt.IsZero() {
		return fmt.Errorf("%w: a start time is required", ErrInvalid)
	}
	p.StartsAt = p.StartsAt.UTC()
	if p.SignupOpensAt != nil {
		if p.SignupOpensAt.After(p.StartsAt) {
			return fmt.Errorf("%w: sign-up must open before the start", ErrInvalid)
		}
		u := p.SignupOpensAt.UTC()
		p.SignupOpensAt = &u
	}
	if p.CheckinMinutes < 0 || p.CheckinMinutes > MaxCheckinMinute {
		return fmt.Errorf("%w: check-in minutes must be 0 to %d", ErrInvalid, MaxCheckinMinute)
	}
	if err := p.Rules.validate(); err != nil {
		return err
	}
	if len(p.Prizes) > MaxPrizes {
		return fmt.Errorf("%w: at most %d prizes", ErrInvalid, MaxPrizes)
	}
	seen := map[int]bool{}
	for i := range p.Prizes {
		pr := &p.Prizes[i]
		if pr.Place < 1 || pr.Place > p.BracketSize || seen[pr.Place] {
			return fmt.Errorf("%w: prize places must be distinct and within the bracket", ErrInvalid)
		}
		seen[pr.Place] = true
		if pr.Points < 0 || pr.Points > MaxPrizePoints {
			return fmt.Errorf("%w: prize points must be 0 to %d", ErrInvalid, MaxPrizePoints)
		}
		if pr.Title != nil {
			t := strings.TrimSpace(*pr.Title)
			if t == "" {
				pr.Title = nil
			} else if len([]rune(t)) > MaxTitleLen {
				return fmt.Errorf("%w: a title is at most %d characters", ErrInvalid, MaxTitleLen)
			} else {
				pr.Title = &t
			}
		}
	}
	sort.SliceStable(p.Prizes, func(i, j int) bool { return p.Prizes[i].Place < p.Prizes[j].Place })
	if p.Prizes == nil {
		p.Prizes = []Prize{}
	}
	return nil
}

func (r *Rules) validate() error {
	if r.MatchTimerMinutes == 0 {
		r.MatchTimerMinutes = DefaultTimer
	}
	if r.ReadyMinutes == 0 {
		r.ReadyMinutes = DefaultReady
	}
	if r.MatchTimerMinutes < 1 || r.MatchTimerMinutes > MaxTimerMinutes {
		return fmt.Errorf("%w: match timer must be 1 to %d minutes", ErrInvalid, MaxTimerMinutes)
	}
	if r.ReadyMinutes < 1 || r.ReadyMinutes > MaxReadyMinutes {
		return fmt.Errorf("%w: ready time must be 1 to %d minutes", ErrInvalid, MaxReadyMinutes)
	}
	if len(r.AllowedWeapons) > MaxWeapons {
		return fmt.Errorf("%w: at most %d allowed weapons", ErrInvalid, MaxWeapons)
	}
	weapons := make([]string, 0, len(r.AllowedWeapons))
	for _, w := range r.AllowedWeapons {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if len([]rune(w)) > 40 {
			return fmt.Errorf("%w: a weapon name is at most 40 characters", ErrInvalid)
		}
		weapons = append(weapons, w)
	}
	r.AllowedWeapons = weapons
	if len(r.Arenas) > MaxArenas {
		return fmt.Errorf("%w: at most %d arenas", ErrInvalid, MaxArenas)
	}
	seen := map[int]bool{}
	for i := range r.Arenas {
		a := &r.Arenas[i]
		if a.No == 0 {
			a.No = i + 1
		}
		if a.No < 1 || seen[a.No] {
			return fmt.Errorf("%w: arena numbers must be distinct and positive", ErrInvalid)
		}
		seen[a.No] = true
		a.Name = strings.TrimSpace(a.Name)
		if a.Name == "" {
			a.Name = fmt.Sprintf("Arena %d", a.No)
		}
		if len([]rune(a.Name)) > 40 {
			return fmt.Errorf("%w: an arena name is at most 40 characters", ErrInvalid)
		}
		if a.Radius <= 0 || a.Radius > 5000 || math.IsNaN(a.X) || math.IsNaN(a.Z) || math.IsInf(a.X, 0) || math.IsInf(a.Z, 0) {
			return fmt.Errorf("%w: an arena needs a position and a radius of 1 to 5000 m", ErrInvalid)
		}
	}
	if r.Arenas == nil {
		r.Arenas = []Arena{}
	}
	return nil
}

// Apply copies validated parameters onto a tournament.
func (t *Tournament) Apply(p Params) {
	t.Name, t.TeamSize, t.BracketSize, t.BestOf, t.Seeding = p.Name, p.TeamSize, p.BracketSize, p.BestOf, p.Seeding
	t.StartsAt, t.SignupOpensAt, t.CheckinMinutes, t.Rules, t.Prizes = p.StartsAt, p.SignupOpensAt, p.CheckinMinutes, p.Rules, p.Prizes
	if t.Format == "" {
		t.Format = FormatSingleElim
	}
}

// CheckinOpensAt is when check-in opens: the start less the check-in window.
func (t *Tournament) CheckinOpensAt() time.Time {
	return t.StartsAt.Add(-time.Duration(t.CheckinMinutes) * time.Minute)
}

// WeaponAllowed reports whether a weapon is allowed by the rules (every weapon is when the list
// is empty).
func (t *Tournament) WeaponAllowed(weapon string) bool {
	if len(t.Rules.AllowedWeapons) == 0 {
		return true
	}
	w := NormalizeWeapon(weapon)
	for _, a := range t.Rules.AllowedWeapons {
		if NormalizeWeapon(a) == w {
			return true
		}
	}
	return false
}

func ptrInt64(v int64) *int64 { return &v }
func ptrTime(v time.Time) *time.Time {
	u := v.UTC()
	return &u
}
func ptrString(v string) *string { return &v }
