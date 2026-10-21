package tournament

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// EventKind is what happened in one transition, for the Notifier.
type EventKind string

const (
	EvSignupChanged EventKind = "SIGNUP_CHANGED" // entries joined, left or checked in
	EvOpened        EventKind = "OPENED"         // sign-up opened
	EvCheckin       EventKind = "CHECKIN"        // check-in opened
	EvStarted       EventKind = "STARTED"        // the bracket is drawn and the tournament is live
	EvPaused        EventKind = "PAUSED"
	EvResumed       EventKind = "RESUMED"
	EvCancelled     EventKind = "CANCELLED"
	EvFinished      EventKind = "FINISHED"
	EvMatchCalled   EventKind = "MATCH_CALLED"
	EvMatchStarted  EventKind = "MATCH_STARTED"
	EvRound         EventKind = "ROUND"
	EvMatchDone     EventKind = "MATCH_DONE"
	EvAdminPing     EventKind = "ADMIN_PING"
	EvBracket       EventKind = "BRACKET" // the bracket changed in a way no other event covers
)

// Event is one thing the Notifier announces.
type Event struct {
	Kind  EventKind
	Match *Match
	Round *Round
	Entry *Entry
	// Text is a plain-English reason for pings, cancellations and manual decisions.
	Text string
}

// --- sign-up --------------------------------------------------------------------------------------

// OpenSignup moves a draft to SIGNUP.
func (t *Tournament) OpenSignup(now time.Time) ([]Event, error) {
	if t.Status != StatusDraft {
		return nil, ErrWrongStatus
	}
	t.Status = StatusSignup
	if t.SignupOpensAt == nil || t.SignupOpensAt.After(now) {
		t.SignupOpensAt = ptrTime(now)
	}
	return []Event{{Kind: EvOpened}}, nil
}

// OpenCheckin moves SIGNUP to CHECKIN.
func (t *Tournament) OpenCheckin(now time.Time) ([]Event, error) {
	if t.Status != StatusSignup {
		return nil, ErrWrongStatus
	}
	t.Status = StatusCheckin
	return []Event{{Kind: EvCheckin}}, nil
}

// entered counts the entries that hold a slot (not withdrawn, not disqualified).
func (t *Tournament) entered() int {
	n := 0
	for _, e := range t.Entries {
		if e.Status == EntryActive {
			n++
		}
	}
	return n
}

// Join enters a player (1v1) or a team (2v2). During check-in the entry is checked in at once.
func (t *Tournament) Join(now time.Time, players []EntryPlayer) (*Entry, []Event, error) {
	if t.Status != StatusSignup && t.Status != StatusCheckin {
		return nil, nil, ErrWrongStatus
	}
	if len(players) != t.TeamSize {
		if t.TeamSize == 2 {
			return nil, nil, ErrPartner
		}
		return nil, nil, fmt.Errorf("%w: a 1v1 entry is one player", ErrInvalid)
	}
	if t.TeamSize == 2 && players[0].PlayerID == players[1].PlayerID {
		return nil, nil, fmt.Errorf("%w: a partner must be another player", ErrInvalid)
	}
	for _, p := range players {
		if e := t.EntryOfPlayer(p.PlayerID); e != nil && e.Status != EntryWithdrawn {
			return nil, nil, ErrAlreadyEntered
		}
	}
	if t.entered() >= t.BracketSize {
		return nil, nil, ErrFull
	}
	teamNo := 0
	for _, e := range t.Entries {
		if e.TeamNo > teamNo {
			teamNo = e.TeamNo
		}
	}
	e := &Entry{TeamNo: teamNo + 1, Status: EntryActive, CreatedAt: now.UTC(), Players: append([]EntryPlayer{}, players...)}
	if t.Status == StatusCheckin {
		e.CheckedInAt = ptrTime(now)
	}
	t.Entries = append(t.Entries, e)
	return e, []Event{{Kind: EvSignupChanged, Entry: e}}, nil
}

// Leave withdraws the entry of a Discord user (both players of a team).
func (t *Tournament) Leave(now time.Time, discordUserID string) (*Entry, []Event, error) {
	if t.Status != StatusSignup && t.Status != StatusCheckin {
		return nil, nil, ErrWrongStatus
	}
	e := t.EntryOfDiscordUser(discordUserID)
	if e == nil || e.Status != EntryActive {
		return nil, nil, ErrNotEntered
	}
	e.Status = EntryWithdrawn
	e.CheckedInAt = nil
	return e, []Event{{Kind: EvSignupChanged, Entry: e}}, nil
}

// Checkin marks the entry of a Discord user present.
func (t *Tournament) Checkin(now time.Time, discordUserID string) (*Entry, []Event, error) {
	if t.Status != StatusCheckin {
		if t.Status == StatusSignup {
			return nil, nil, fmt.Errorf("%w: check-in has not opened yet", ErrWrongStatus)
		}
		return nil, nil, ErrWrongStatus
	}
	e := t.EntryOfDiscordUser(discordUserID)
	if e == nil || e.Status != EntryActive {
		return nil, nil, ErrNotEntered
	}
	if e.CheckedIn() {
		return e, nil, nil
	}
	e.CheckedInAt = ptrTime(now)
	return e, []Event{{Kind: EvSignupChanged, Entry: e}}, nil
}

// --- start and the bracket --------------------------------------------------------------------------

// ErrNotEnough is a start with fewer than two checked-in entries.
var ErrNotEnough = fmt.Errorf("%w: at least two checked-in entries are needed", ErrWrongStatus)

// Start draws the bracket and goes live. From SIGNUP (an admin starting early) or with no
// check-in window every active entry counts as present; otherwise entries that did not check in
// are withdrawn. The first match is called at once.
func (t *Tournament) Start(now time.Time, ranks []Rank, rng *rand.Rand) ([]Event, error) {
	if t.Status != StatusSignup && t.Status != StatusCheckin {
		return nil, ErrWrongStatus
	}
	for _, e := range t.Entries {
		if e.Status != EntryActive {
			continue
		}
		if t.Status == StatusSignup || t.CheckinMinutes == 0 {
			if !e.CheckedIn() {
				e.CheckedInAt = ptrTime(now)
			}
		} else if !e.CheckedIn() {
			e.Status = EntryWithdrawn
		}
	}
	if len(t.ActiveEntries()) < 2 {
		return nil, ErrNotEnough
	}
	AssignSeeds(t, ranks, rng)
	BuildBracket(t, now)
	t.Status = StatusLive
	t.StartedAt = ptrTime(now)
	events := []Event{{Kind: EvStarted}}
	m, more, err := t.CallNext(now)
	if err == nil && m != nil {
		events = append(events, more...)
	}
	events = append(events, t.finishIfOver(now)...)
	return events, nil
}

// CallNext calls the next pending match, or re-calls the match in play (its ready timer starts
// again when it was waiting for an admin). ErrNoMatch when nothing can be called.
func (t *Tournament) CallNext(now time.Time) (*Match, []Event, error) {
	if t.Status != StatusLive {
		return nil, nil, ErrWrongStatus
	}
	if m := t.Current(); m != nil {
		if m.Status == MatchCalled {
			m.TimerEndsAt = ptrTime(now.Add(time.Duration(t.Rules.ReadyMinutes) * time.Minute))
		}
		return m, []Event{{Kind: EvMatchCalled, Match: m}}, nil
	}
	m := t.NextPending()
	if m == nil {
		return nil, nil, ErrNoMatch
	}
	t.call(m, now)
	return m, []Event{{Kind: EvMatchCalled, Match: m}}, nil
}

// call marks a match CALLED with the next arena in rotation and the ready timer.
func (t *Tournament) call(m *Match, now time.Time) {
	m.Status = MatchCalled
	m.CalledAt = ptrTime(now)
	m.TimerEndsAt = ptrTime(now.Add(time.Duration(t.Rules.ReadyMinutes) * time.Minute))
	if n := len(t.Rules.Arenas); n > 0 {
		called := 0
		for _, o := range t.Matches {
			if o != m && o.CalledAt != nil {
				called++
			}
		}
		no := t.Rules.Arenas[called%n].No
		m.ArenaNo = &no
	}
}

// StartMatch moves a called match to LIVE and starts the match timer.
func (t *Tournament) StartMatch(m *Match, now time.Time) ([]Event, error) {
	if t.Status != StatusLive || m == nil || m.Status != MatchCalled {
		return nil, ErrWrongStatus
	}
	m.Status = MatchLive
	m.StartedAt = ptrTime(now)
	m.TimerEndsAt = ptrTime(now.Add(time.Duration(t.Rules.MatchTimerMinutes) * time.Minute))
	return []Event{{Kind: EvMatchStarted, Match: m}}, nil
}

// StartCurrent is an admin starting the match in play before its first kill.
func (t *Tournament) StartCurrent(now time.Time) ([]Event, error) {
	m := t.Current()
	if m == nil {
		return nil, ErrNoMatch
	}
	return t.StartMatch(m, now)
}

// RecordRound appends a round to an open match. A counted round with a winner scores; the match
// ends at RoundsToWin, its winner advances, the next match is called and the tournament finishes
// after the final. A first counted round starts a called match.
func (t *Tournament) RecordRound(m *Match, r Round, now time.Time) ([]Event, error) {
	if t.Status != StatusLive || m == nil || !m.Open() {
		return nil, ErrWrongStatus
	}
	var events []Event
	if r.Counted && m.Status == MatchCalled {
		more, _ := t.StartMatch(m, now)
		events = append(events, more...)
	}
	r.MatchID = m.ID
	r.N = len(m.Rounds) + 1
	if r.At.IsZero() {
		r.At = now.UTC()
	}
	m.Rounds = append(m.Rounds, r)
	stored := &m.Rounds[len(m.Rounds)-1]
	events = append(events, Event{Kind: EvRound, Match: m, Round: stored})
	if !r.Counted || r.WinnerEntry == nil {
		return events, nil
	}
	if m.EntryA != nil && *m.EntryA == *r.WinnerEntry {
		m.ScoreA++
	} else if m.EntryB != nil && *m.EntryB == *r.WinnerEntry {
		m.ScoreB++
	}
	need := RoundsToWin(t.BestOf)
	if m.ScoreA >= need || m.ScoreB >= need {
		events = append(events, t.finishMatch(m, now)...)
	}
	return events, nil
}

// finishMatch closes a decided match, advances the winner, calls the next match and finishes the
// tournament after the final.
func (t *Tournament) finishMatch(m *Match, now time.Time) []Event {
	if m.ScoreA > m.ScoreB {
		m.WinnerEntry = m.EntryA
	} else if m.ScoreB > m.ScoreA {
		m.WinnerEntry = m.EntryB
	}
	m.Status = MatchDone
	m.EndedAt = ptrTime(now)
	m.TimerEndsAt = nil
	t.advance(m)
	if loser := t.loserOf(m); loser != nil && loser.Status == EntryActive {
		loser.Status = EntryEliminated
	}
	events := []Event{{Kind: EvMatchDone, Match: m}}
	events = append(events, t.settle(now)...)
	if fin := t.finishIfOver(now); len(fin) > 0 {
		return append(events, fin...)
	}
	if next, more, err := t.CallNext(now); err == nil && next != nil {
		events = append(events, more...)
	}
	return events
}

// loserOf is the entry that lost a decided match (nil for a bye or an undecided match).
func (t *Tournament) loserOf(m *Match) *Entry {
	if m.WinnerEntry == nil || m.EntryA == nil || m.EntryB == nil {
		return nil
	}
	if *m.WinnerEntry == *m.EntryA {
		return t.Entry(*m.EntryB)
	}
	return t.Entry(*m.EntryA)
}

// Forfeit ends a match without play: the winner (nil when nobody) advances.
func (t *Tournament) Forfeit(m *Match, winner *int64, now time.Time, why string) ([]Event, error) {
	if t.Status != StatusLive || m == nil || m.Finished() {
		return nil, ErrWrongStatus
	}
	m.Status = MatchForfeit
	m.WinnerEntry = winner
	m.EndedAt = ptrTime(now)
	m.TimerEndsAt = nil
	t.advance(m)
	if loser := t.loserOf(m); loser != nil && loser.Status == EntryActive {
		loser.Status = EntryEliminated
	}
	events := []Event{{Kind: EvMatchDone, Match: m, Text: why}}
	events = append(events, t.settle(now)...)
	if fin := t.finishIfOver(now); len(fin) > 0 {
		return append(events, fin...), nil
	}
	if next, more, err := t.CallNext(now); err == nil && next != nil {
		events = append(events, more...)
	}
	return events, nil
}

// settle closes the pending matches that can no longer be played: feeders done and at most one
// entry (a walkover), in bracket order so a walkover can cascade.
func (t *Tournament) settle(now time.Time) []Event {
	var events []Event
	rounds := Rounds(t.BracketSize)
	for r := 1; r <= rounds; r++ {
		for p := 1; p <= t.BracketSize>>r; p++ {
			m := t.MatchAt(r, p)
			if m == nil || m.Status != MatchPending {
				continue
			}
			if r > 1 {
				fa, fb := t.MatchAt(r-1, 2*p-1), t.MatchAt(r-1, 2*p)
				if fa == nil || fb == nil || !fa.Finished() || !fb.Finished() {
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
			events = append(events, Event{Kind: EvBracket, Match: m})
		}
	}
	return events
}

// finishIfOver ends the tournament once the final has a result.
func (t *Tournament) finishIfOver(now time.Time) []Event {
	if t.Status != StatusLive {
		return nil
	}
	final := t.Final()
	if final == nil || !final.Finished() {
		return nil
	}
	t.Status = StatusFinished
	t.FinishedAt = ptrTime(now)
	for _, m := range t.Matches {
		if m.Open() {
			m.Status = MatchDone
			m.EndedAt = ptrTime(now)
			m.TimerEndsAt = nil
		}
	}
	var champion *Entry
	if final.WinnerEntry != nil {
		champion = t.Entry(*final.WinnerEntry)
	}
	for _, e := range t.Entries {
		switch {
		case champion != nil && e.ID == champion.ID:
			e.Status = EntryWinner
		case e.Status == EntryActive:
			e.Status = EntryEliminated
		}
	}
	return []Event{{Kind: EvFinished, Entry: champion}}
}

// Champion is the winning entry of a finished tournament.
func (t *Tournament) Champion() *Entry {
	for _, e := range t.Entries {
		if e.Status == EntryWinner {
			return e
		}
	}
	return nil
}

// Place is an entry's final place: 1 for the champion, 2 for the finalist, 3 for both semi-final
// losers, 5 for the quarter-final losers, 9, 17 (the usual tied placings). 0 when the entry never
// played a decided match (withdrawn before the draw, disqualified before the first match).
func (t *Tournament) Place(entryID int64) int {
	e := t.Entry(entryID)
	if e == nil {
		return 0
	}
	if e.Status == EntryWinner {
		return 1
	}
	lostIn := 0
	for _, m := range t.Matches {
		if m.Has(entryID) && m.Finished() && m.WinnerEntry != nil && *m.WinnerEntry != entryID && m.Round > lostIn {
			lostIn = m.Round
		}
	}
	if lostIn == 0 {
		return 0
	}
	return (t.BracketSize >> lostIn) + 1
}

// --- pause, resume, cancel ----------------------------------------------------------------------------

// Pause stops scoring; timers are cleared and set again on resume.
func (t *Tournament) Pause(now time.Time) ([]Event, error) {
	if t.Status != StatusLive {
		return nil, ErrWrongStatus
	}
	t.Status = StatusPaused
	for _, m := range t.Matches {
		if m.Open() {
			m.TimerEndsAt = nil
		}
	}
	return []Event{{Kind: EvPaused}}, nil
}

// Resume continues a paused tournament; the match in play gets a fresh timer.
func (t *Tournament) Resume(now time.Time) ([]Event, error) {
	if t.Status != StatusPaused {
		return nil, ErrWrongStatus
	}
	t.Status = StatusLive
	events := []Event{{Kind: EvResumed}}
	if m := t.Current(); m != nil {
		switch m.Status {
		case MatchCalled:
			m.TimerEndsAt = ptrTime(now.Add(time.Duration(t.Rules.ReadyMinutes) * time.Minute))
		case MatchLive:
			m.TimerEndsAt = ptrTime(now.Add(time.Duration(t.Rules.MatchTimerMinutes) * time.Minute))
		}
		events = append(events, Event{Kind: EvMatchCalled, Match: m})
	} else if next, more, err := t.CallNext(now); err == nil && next != nil {
		events = append(events, more...)
	}
	return events, nil
}

// Cancel ends a tournament that is not finished.
func (t *Tournament) Cancel(now time.Time, why string) ([]Event, error) {
	if t.Over() {
		return nil, ErrWrongStatus
	}
	t.Status = StatusCancelled
	t.FinishedAt = ptrTime(now)
	for _, m := range t.Matches {
		if m.Open() {
			m.TimerEndsAt = nil
		}
	}
	return []Event{{Kind: EvCancelled, Text: why}}, nil
}

// --- admin decisions -----------------------------------------------------------------------------------

// AdminResult decides a match: a MANUAL round for the winner that settles it outright.
func (t *Tournament) AdminResult(matchID, winnerEntryID int64, by, note string, now time.Time) ([]Event, error) {
	if t.Status != StatusLive {
		return nil, ErrWrongStatus
	}
	m := t.Match(matchID)
	if m == nil {
		return nil, ErrMatchNotFound
	}
	if m.Finished() || !m.Has(winnerEntryID) || m.EntryA == nil || m.EntryB == nil {
		return nil, ErrWrongStatus
	}
	if m.Status == MatchPending {
		t.call(m, now)
	}
	if m.Status == MatchCalled {
		m.Status = MatchLive
		m.StartedAt = ptrTime(now)
	}
	r := Round{MatchID: m.ID, N: len(m.Rounds) + 1, WinnerEntry: ptrInt64(winnerEntryID), At: now.UTC(), Counted: true, Flag: ptrString(FlagManual), DecidedBy: ptrString(by), Weapon: note}
	m.Rounds = append(m.Rounds, r)
	need := RoundsToWin(t.BestOf)
	if *m.EntryA == winnerEntryID {
		m.ScoreA = need
	} else {
		m.ScoreB = need
	}
	events := []Event{{Kind: EvRound, Match: m, Round: &m.Rounds[len(m.Rounds)-1], Text: note}}
	return append(events, t.finishMatch(m, now)...), nil
}

// Replay restarts a match: its rounds no longer count, the scores are cleared and it is called
// again. A decided match can be replayed only while the match its winner went to has not begun.
func (t *Tournament) Replay(matchID int64, by string, now time.Time) ([]Event, error) {
	if t.Status != StatusLive {
		return nil, ErrWrongStatus
	}
	m := t.Match(matchID)
	if m == nil {
		return nil, ErrMatchNotFound
	}
	if m.EntryA == nil || m.EntryB == nil {
		return nil, ErrWrongStatus
	}
	if m.Finished() {
		if m.NextPos > 0 {
			next := t.MatchAt(m.Round+1, m.NextPos)
			if next != nil {
				if next.Status != MatchPending {
					return nil, fmt.Errorf("%w: the next match has already begun", ErrWrongStatus)
				}
				if m.Position%2 == 1 {
					next.EntryA = nil
				} else {
					next.EntryB = nil
				}
			}
		}
		if loser := t.loserOf(m); loser != nil && loser.Status == EntryEliminated {
			loser.Status = EntryActive
		}
	} else if cur := t.Current(); cur != nil && cur != m {
		return nil, fmt.Errorf("%w: another match is in play", ErrWrongStatus)
	}
	for i := range m.Rounds {
		m.Rounds[i].Counted = false
	}
	m.Rounds = append(m.Rounds, Round{MatchID: m.ID, N: len(m.Rounds) + 1, At: now.UTC(), Flag: ptrString(FlagManual), DecidedBy: ptrString(by), Weapon: "replay"})
	m.ScoreA, m.ScoreB, m.WinnerEntry, m.StartedAt, m.EndedAt = 0, 0, nil, nil, nil
	if cur := t.Current(); cur != nil && cur != m {
		// Another match is in play: this one waits its turn.
		m.Status, m.CalledAt, m.TimerEndsAt, m.ArenaNo = MatchPending, nil, nil, nil
		return []Event{{Kind: EvBracket, Match: m, Text: "replay"}}, nil
	}
	t.call(m, now)
	return []Event{{Kind: EvMatchCalled, Match: m, Text: "replay"}}, nil
}

// DQ disqualifies an entry. Before the draw it just leaves the field; during play its current
// match is forfeited to the opponent and any later slot it held is cleared.
func (t *Tournament) DQ(entryID int64, by, why string, now time.Time) ([]Event, error) {
	if t.Over() {
		return nil, ErrWrongStatus
	}
	e := t.Entry(entryID)
	if e == nil {
		return nil, ErrEntryNotFound
	}
	if e.Status == EntryDQ {
		return nil, nil
	}
	e.Status = EntryDQ
	e.CheckedInAt = nil
	events := []Event{{Kind: EvSignupChanged, Entry: e, Text: why}}
	if !t.Running() {
		return events, nil
	}
	for _, m := range t.Matches {
		if !m.Has(entryID) || m.Finished() {
			continue
		}
		var other *int64
		if m.EntryA != nil && *m.EntryA != entryID {
			other = m.EntryA
		} else if m.EntryB != nil && *m.EntryB != entryID {
			other = m.EntryB
		}
		if m.Open() || other != nil {
			m.Rounds = append(m.Rounds, Round{MatchID: m.ID, N: len(m.Rounds) + 1, WinnerEntry: other, At: now.UTC(), Flag: ptrString(FlagManual), DecidedBy: ptrString(by), Weapon: "disqualified"})
			if t.Status == StatusLive {
				more, _ := t.Forfeit(m, other, now, "disqualified")
				events = append(events, more...)
			} else {
				m.Status, m.WinnerEntry, m.EndedAt, m.TimerEndsAt = MatchForfeit, other, ptrTime(now), nil
				t.advance(m)
			}
			break
		}
		// The slot was held but the opponent is not known yet: clear it; settle fills the walkover.
		if m.EntryA != nil && *m.EntryA == entryID {
			m.EntryA = nil
		} else {
			m.EntryB = nil
		}
		events = append(events, t.settle(now)...)
		break
	}
	return events, nil
}

// --- kills -------------------------------------------------------------------------------------------

// Kill is a persisted kill as the feed hands it over.
type Kill struct {
	KillID         int64
	KillerPlayerID int64
	VictimPlayerID int64
	KillerName     string
	VictimName     string
	Weapon         string
	Distance       *float64
	// KillerX/KillerZ are the killer's map position when the log had one.
	KillerX, KillerZ *float64
	At               time.Time
}

// Decision is what a kill means for the tournament.
type Decision struct {
	Match *Match
	Round Round
	// Ping is a plain-English reason an admin must look (empty when the round simply counts).
	Ping string
}

// Attribute decides which match a kill belongs to and whether it counts. nil when the kill has
// nothing to do with the tournament. A kill already attributed (same kill id) is nil too.
func (t *Tournament) Attribute(k Kill) *Decision {
	if t.Status != StatusLive {
		return nil
	}
	for _, m := range t.Matches {
		for _, r := range m.Rounds {
			if r.KillID != nil && *r.KillID == k.KillID {
				return nil
			}
		}
	}
	ke, ve := t.EntryOfPlayer(k.KillerPlayerID), t.EntryOfPlayer(k.VictimPlayerID)
	base := Round{KillID: ptrInt64(k.KillID), KillerName: k.KillerName, VictimName: k.VictimName, Weapon: k.Weapon, Distance: k.Distance, At: k.At.UTC()}
	var open []*Match
	for _, m := range t.Matches {
		if m.Open() {
			open = append(open, m)
		}
	}
	if ke != nil && ve != nil && ke.ID != ve.ID {
		for _, m := range open {
			if m.Has(ke.ID) && m.Has(ve.ID) {
				r := base
				r.WinnerEntry = ptrInt64(ke.ID)
				r.Counted = true
				d := &Decision{Match: m}
				if !t.WeaponAllowed(k.Weapon) {
					r.Counted, r.Flag = false, ptrString(FlagWeapon)
					d.Ping = fmt.Sprintf("%s killed %s with %s, which is not an allowed weapon. Rule on it with /tournament result or /tournament replay.", k.KillerName, k.VictimName, k.Weapon)
				} else if a := t.Arena(m); a != nil && k.KillerX != nil && k.KillerZ != nil {
					if dist := math.Hypot(*k.KillerX-a.X, *k.KillerZ-a.Z); dist > a.Radius {
						r.Counted, r.Flag = false, ptrString(FlagOutsideArena)
						d.Ping = fmt.Sprintf("%s killed %s %.0f m outside %s. Rule on it with /tournament result or /tournament replay.", k.KillerName, k.VictimName, dist-a.Radius, a.Name)
					}
				}
				d.Round = r
				return d
			}
		}
	}
	// One side is in a live match and the other is not (or they are team-mates): interference.
	for _, m := range open {
		if (ke != nil && m.Has(ke.ID)) || (ve != nil && m.Has(ve.ID)) {
			r := base
			r.Flag = ptrString(FlagInterference)
			who := k.KillerName
			if ke == nil || !m.Has(ke.ID) {
				who = k.VictimName
			}
			return &Decision{Match: m, Round: r, Ping: fmt.Sprintf("%s was killed by %s during the match: someone outside the match is interfering (%s is in it).", k.VictimName, k.KillerName, who)}
		}
	}
	return nil
}

// Death is a non-player death (the player died with no killer) as the feed hands it over.
type Death struct {
	PlayerID int64
	Name     string
	Cause    string
	At       time.Time
}

// AttributeDeath records a match player dying to something other than a player: the round is not
// counted and the match should be replayed or ruled on.
func (t *Tournament) AttributeDeath(d Death) *Decision {
	if t.Status != StatusLive {
		return nil
	}
	e := t.EntryOfPlayer(d.PlayerID)
	if e == nil {
		return nil
	}
	for _, m := range t.Matches {
		if m.Open() && m.Has(e.ID) {
			cause := d.Cause
			if cause == "" {
				cause = "something other than a player"
			}
			return &Decision{Match: m, Round: Round{VictimName: d.Name, Weapon: cause, At: d.At.UTC(), Flag: ptrString(FlagNonPlayer)},
				Ping: fmt.Sprintf("%s died to %s during the match. The round does not count; replay it with /tournament replay or decide it with /tournament result.", d.Name, cause)}
		}
	}
	return nil
}

// --- the clock -----------------------------------------------------------------------------------------

// Tick applies everything that is due at now. present says, for the players of the match in play,
// who is on the server (a missing player counts as absent); it is only read for a ready timeout.
func (t *Tournament) Tick(now time.Time, ranks []Rank, rng *rand.Rand, present map[int64]bool) []Event {
	var events []Event
	switch t.Status {
	case StatusDraft:
		if t.SignupOpensAt != nil && !now.Before(*t.SignupOpensAt) {
			more, _ := t.OpenSignup(now)
			events = append(events, more...)
		} else {
			return nil
		}
		fallthrough
	case StatusSignup:
		if t.Status == StatusSignup && t.CheckinMinutes > 0 && !now.Before(t.CheckinOpensAt()) && now.Before(t.StartsAt) {
			more, _ := t.OpenCheckin(now)
			events = append(events, more...)
		}
		fallthrough
	case StatusCheckin:
		if (t.Status == StatusSignup || t.Status == StatusCheckin) && !now.Before(t.StartsAt) {
			more, err := t.Start(now, ranks, rng)
			if err != nil {
				more, _ = t.Cancel(now, "Fewer than two entries checked in, so the tournament was cancelled.")
			}
			events = append(events, more...)
		}
	case StatusLive:
		m := t.Current()
		if m == nil || m.TimerEndsAt == nil || now.Before(*m.TimerEndsAt) {
			return nil
		}
		switch m.Status {
		case MatchCalled:
			a, b := t.entryPresent(m.EntryA, present), t.entryPresent(m.EntryB, present)
			switch {
			case a && b:
				// Both are on the server but nobody fought: the match starts and the timer runs.
				more, _ := t.StartMatch(m, now)
				events = append(events, more...)
			case a:
				more, _ := t.Forfeit(m, m.EntryA, now, "ready time ran out")
				events = append(events, more...)
			case b:
				more, _ := t.Forfeit(m, m.EntryB, now, "ready time ran out")
				events = append(events, more...)
			default:
				m.TimerEndsAt = nil
				events = append(events, Event{Kind: EvAdminPing, Match: m, Text: "Neither side showed up within the ready time. The match is waiting: call it again with /tournament call, decide it with /tournament result, or disqualify with /tournament dq."})
			}
		case MatchLive:
			m.TimerEndsAt = nil
			events = append(events, Event{Kind: EvAdminPing, Match: m, Text: "The match timer ran out without a result. Decide it with /tournament result or replay it with /tournament replay."})
		}
	}
	return events
}

// entryPresent reports whether every player of an entry is present.
func (t *Tournament) entryPresent(id *int64, present map[int64]bool) bool {
	if id == nil {
		return false
	}
	e := t.Entry(*id)
	if e == nil {
		return false
	}
	for _, p := range e.Players {
		if !present[p.PlayerID] {
			return false
		}
	}
	return true
}
