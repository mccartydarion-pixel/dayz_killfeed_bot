package tournament

import (
	"errors"
	"math/rand"
	"testing"
	"time"
)

// started is a fixture with the bracket drawn and the first match called.
func started(t *testing.T, n, size int, bestOf int) *Tournament {
	t.Helper()
	tr := fixture(n, size, SeedingRanked, bestOf)
	var ranks []Rank
	for _, e := range tr.Entries {
		ranks = append(ranks, Rank{EntryID: e.ID, RP: 1000 - e.ID})
	}
	ev, err := tr.Start(t0, ranks, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	assignIDs(tr)
	if tr.Status != StatusLive || tr.StartedAt == nil || len(ev) < 2 || ev[0].Kind != EvStarted || ev[1].Kind != EvMatchCalled {
		t.Fatalf("start: status %s events %v", tr.Status, kinds(ev))
	}
	return tr
}

func kinds(ev []Event) []EventKind {
	out := make([]EventKind, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.Kind)
	}
	return out
}

func has(ev []Event, k EventKind) bool {
	for _, e := range ev {
		if e.Kind == k {
			return true
		}
	}
	return false
}

func kill(id int64, killer, victim *Entry, weapon string, at time.Time) Kill {
	return Kill{KillID: id, KillerPlayerID: killer.Players[0].PlayerID, VictimPlayerID: victim.Players[0].PlayerID,
		KillerName: killer.Players[0].Name, VictimName: victim.Players[0].Name, Weapon: weapon, At: at}
}

func TestSignupJoinLeaveCheckin(t *testing.T) {
	tr := &Tournament{Status: StatusDraft, TeamSize: 1, BracketSize: 4, BestOf: 1, Seeding: SeedingRandom, StartsAt: t0, CheckinMinutes: 30, Rules: Rules{MatchTimerMinutes: 10, ReadyMinutes: 2}}
	if _, _, err := tr.Join(t0, []EntryPlayer{{PlayerID: 1, DiscordUserID: "a", Name: "A"}}); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("join a draft: %v", err)
	}
	if _, err := tr.OpenSignup(t0.Add(-2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	players := []EntryPlayer{{PlayerID: 1, DiscordUserID: "a", Name: "A"}}
	if _, _, err := tr.Join(t0, players); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.Join(t0, players); !errors.Is(err, ErrAlreadyEntered) {
		t.Fatalf("double join: %v", err)
	}
	for i := int64(2); i <= 4; i++ {
		if _, _, err := tr.Join(t0, []EntryPlayer{{PlayerID: i, DiscordUserID: string(rune('a' + i)), Name: "X"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := tr.Join(t0, []EntryPlayer{{PlayerID: 9, DiscordUserID: "z", Name: "Z"}}); !errors.Is(err, ErrFull) {
		t.Fatalf("full: %v", err)
	}
	if _, _, err := tr.Checkin(t0, "a"); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("check-in before it opens: %v", err)
	}
	if _, _, err := tr.Leave(t0, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.Leave(t0, "a"); !errors.Is(err, ErrNotEntered) {
		t.Fatalf("leave twice: %v", err)
	}
	// A withdrawn slot frees up, and the player can come back.
	if _, _, err := tr.Join(t0, []EntryPlayer{{PlayerID: 9, DiscordUserID: "z", Name: "Z"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.Join(t0, players); !errors.Is(err, ErrFull) {
		t.Fatalf("full again: %v", err)
	}
	if _, err := tr.OpenCheckin(t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.Checkin(t0, "a"); !errors.Is(err, ErrNotEntered) {
		t.Fatalf("withdrawn check-in: %v", err)
	}
	e, ev, err := tr.Checkin(t0, "z")
	if err != nil || !e.CheckedIn() || len(ev) != 1 {
		t.Fatalf("check-in: %v %v", err, ev)
	}
	if _, ev, err := tr.Checkin(t0, "z"); err != nil || len(ev) != 0 {
		t.Fatalf("second check-in must be quiet: %v %v", err, ev)
	}
	// Only one checked in: the start fails.
	if _, err := tr.Start(t0, nil, nil); !errors.Is(err, ErrNotEnough) {
		t.Fatalf("start with one: %v", err)
	}
}

func TestJoinTwoVTwo(t *testing.T) {
	tr := &Tournament{Status: StatusSignup, TeamSize: 2, BracketSize: 4, BestOf: 1, StartsAt: t0, Rules: Rules{MatchTimerMinutes: 10, ReadyMinutes: 2}}
	if _, _, err := tr.Join(t0, []EntryPlayer{{PlayerID: 1, DiscordUserID: "a"}}); !errors.Is(err, ErrPartner) {
		t.Fatalf("solo join of a 2v2: %v", err)
	}
	if _, _, err := tr.Join(t0, []EntryPlayer{{PlayerID: 1, DiscordUserID: "a"}, {PlayerID: 1, DiscordUserID: "a"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self partner: %v", err)
	}
	e, _, err := tr.Join(t0, []EntryPlayer{{PlayerID: 1, DiscordUserID: "a"}, {PlayerID: 2, DiscordUserID: "b"}})
	if err != nil || len(e.Players) != 2 {
		t.Fatal(err)
	}
	if _, _, err := tr.Join(t0, []EntryPlayer{{PlayerID: 3, DiscordUserID: "c"}, {PlayerID: 2, DiscordUserID: "b"}}); !errors.Is(err, ErrAlreadyEntered) {
		t.Fatalf("partner already entered: %v", err)
	}
	// Either partner leaving withdraws the team.
	if _, _, err := tr.Leave(t0, "b"); err != nil {
		t.Fatal(err)
	}
	if tr.EntryOfDiscordUser("a").Status != EntryWithdrawn {
		t.Fatal("the team must be withdrawn")
	}
}

func TestMatchFlowBestOfThree(t *testing.T) {
	tr := started(t, 4, 4, 3)
	m := tr.Current()
	if m == nil || m.Status != MatchCalled || m.Round != 1 || m.Position != 1 || m.ArenaNo == nil || *m.ArenaNo != 1 || m.TimerEndsAt == nil || !m.TimerEndsAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("current: %+v", m)
	}
	a, b := tr.Entry(*m.EntryA), tr.Entry(*m.EntryB)
	if a.ID != 1 || b.ID != 4 {
		t.Fatalf("1 must meet 4: %d v %d", a.ID, b.ID)
	}
	// The first counted kill starts the match (LIVE, match timer).
	d := tr.Attribute(kill(1, a, b, "M4-A1", t0.Add(time.Minute)))
	if d == nil || d.Match != m || !d.Round.Counted || d.Ping != "" {
		t.Fatalf("decision: %+v", d)
	}
	ev, err := tr.RecordRound(m, d.Round, t0.Add(time.Minute))
	if err != nil || !has(ev, EvMatchStarted) || !has(ev, EvRound) || has(ev, EvMatchDone) {
		t.Fatalf("round 1: %v %v", err, kinds(ev))
	}
	if m.Status != MatchLive || m.ScoreA != 1 || m.ScoreB != 0 || m.StartedAt == nil || !m.TimerEndsAt.Equal(t0.Add(11*time.Minute)) {
		t.Fatalf("after round 1: %+v", m)
	}
	// B wins one back, then A takes the match 2-1.
	d = tr.Attribute(kill(2, b, a, "M4-A1", t0.Add(2*time.Minute)))
	if _, err := tr.RecordRound(m, d.Round, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	d = tr.Attribute(kill(3, a, b, "M4-A1", t0.Add(3*time.Minute)))
	ev, err = tr.RecordRound(m, d.Round, t0.Add(3*time.Minute))
	if err != nil || !has(ev, EvMatchDone) || !has(ev, EvMatchCalled) {
		t.Fatalf("deciding round: %v %v", err, kinds(ev))
	}
	if m.Status != MatchDone || m.WinnerEntry == nil || *m.WinnerEntry != a.ID || m.ScoreA != 2 || m.ScoreB != 1 || m.EndedAt == nil || m.TimerEndsAt != nil {
		t.Fatalf("done: %+v", m)
	}
	if b.Status != EntryEliminated || a.Status != EntryActive {
		t.Fatalf("entries: a %s b %s", a.Status, b.Status)
	}
	final := tr.Final()
	if final.EntryA == nil || *final.EntryA != a.ID || final.EntryB != nil {
		t.Fatalf("final slot: %+v", final)
	}
	// The second semi-final is called next, on the second arena.
	next := tr.Current()
	if next == nil || next.Round != 1 || next.Position != 2 || next.Status != MatchCalled || *next.ArenaNo != 2 {
		t.Fatalf("next: %+v", next)
	}
	// A kill from the finished match is ignored, and a kill is attributed once.
	if d := tr.Attribute(kill(4, b, a, "M4-A1", t0.Add(4*time.Minute))); d != nil {
		t.Fatalf("a kill between players not in an open match must be ignored: %+v", d)
	}
	if d := tr.Attribute(kill(3, a, b, "M4-A1", t0.Add(3*time.Minute))); d != nil {
		t.Fatal("the same kill id must not be attributed twice")
	}
	if rec := tr.RecordOf(a.ID); rec.Wins != 1 || rec.RoundsWon != 2 || rec.RoundsLost != 1 {
		t.Fatalf("record: %+v", rec)
	}
}

func TestKillFlags(t *testing.T) {
	tr := started(t, 4, 4, 1)
	tr.Rules.AllowedWeapons = []string{"M4-A1"}
	m := tr.Current()
	a, b := tr.Entry(*m.EntryA), tr.Entry(*m.EntryB)
	// Weapon not allowed: a round, not counted, with a ping.
	d := tr.Attribute(kill(1, a, b, "KA-M", t0))
	if d == nil || d.Round.Counted || d.Round.Flag == nil || *d.Round.Flag != FlagWeapon || d.Ping == "" {
		t.Fatalf("weapon: %+v", d)
	}
	if _, err := tr.RecordRound(m, d.Round, t0); err != nil {
		t.Fatal(err)
	}
	if m.Status != MatchCalled || m.ScoreA != 0 || len(m.Rounds) != 1 {
		t.Fatalf("a flagged round must not score or start the match: %+v", m)
	}
	// The allowed weapon, spelt differently, counts.
	if d := tr.Attribute(kill(2, a, b, "m4a1", t0)); d == nil || !d.Round.Counted {
		t.Fatalf("normalised weapon: %+v", d)
	}
	// Outside the arena (arena 1 at 1000,1000 r=150): flagged; inside counts; unknown counts.
	x, z := 1300.0, 1000.0
	k := kill(3, a, b, "M4-A1", t0)
	k.KillerX, k.KillerZ = &x, &z
	if d := tr.Attribute(k); d == nil || d.Round.Counted || *d.Round.Flag != FlagOutsideArena {
		t.Fatalf("outside: %+v", d)
	}
	x = 1100
	if d := tr.Attribute(k); d == nil || !d.Round.Counted {
		t.Fatalf("inside: %+v", d)
	}
	// Interference: a player of the match killed by someone outside it, and the reverse.
	c := tr.Entry(2)
	d = tr.Attribute(kill(4, c, a, "M4-A1", t0))
	if d == nil || d.Match != m || d.Round.Counted || *d.Round.Flag != FlagInterference || d.Round.WinnerEntry != nil || d.Ping == "" {
		t.Fatalf("interference: %+v", d)
	}
	if d := tr.Attribute(kill(5, a, c, "M4-A1", t0)); d == nil || *d.Round.Flag != FlagInterference {
		t.Fatalf("interference by a match player: %+v", d)
	}
	// Two players outside the match: nothing.
	if d := tr.Attribute(kill(6, c, tr.Entry(3), "M4-A1", t0)); d != nil {
		t.Fatalf("outsiders: %+v", d)
	}
	// A non-player death of a match player: NON_PLAYER, not counted, with a ping.
	dd := tr.AttributeDeath(Death{PlayerID: a.Players[0].PlayerID, Name: "P1", Cause: "SUICIDE", At: t0})
	if dd == nil || dd.Match != m || dd.Round.Counted || *dd.Round.Flag != FlagNonPlayer || dd.Ping == "" {
		t.Fatalf("death: %+v", dd)
	}
	if dd := tr.AttributeDeath(Death{PlayerID: c.Players[0].PlayerID, At: t0}); dd != nil {
		t.Fatal("a death outside the match is nothing")
	}
	// Nothing is attributed while paused.
	if _, err := tr.Pause(t0); err != nil {
		t.Fatal(err)
	}
	if d := tr.Attribute(kill(7, a, b, "M4-A1", t0)); d != nil {
		t.Fatal("paused tournaments do not score")
	}
}

func TestTwoVTwoAttribution(t *testing.T) {
	tr := &Tournament{Status: StatusCheckin, TeamSize: 2, BracketSize: 4, BestOf: 3, Seeding: SeedingRandom, StartsAt: t0, Rules: Rules{MatchTimerMinutes: 10, ReadyMinutes: 2}}
	for i := int64(1); i <= 2; i++ {
		at := t0
		tr.Entries = append(tr.Entries, &Entry{ID: i, TeamNo: int(i), Status: EntryActive, CheckedInAt: &at,
			Players: []EntryPlayer{{PlayerID: i*10 + 1, Name: "A"}, {PlayerID: i*10 + 2, Name: "B"}}})
	}
	if _, err := tr.Start(t0, nil, rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	assignIDs(tr)
	m := tr.Current()
	// Either team-mate's kill scores for the team.
	d := tr.Attribute(Kill{KillID: 1, KillerPlayerID: 12, VictimPlayerID: 21, Weapon: "M4-A1", At: t0})
	if d == nil || !d.Round.Counted || *d.Round.WinnerEntry != 1 {
		t.Fatalf("team kill: %+v", d)
	}
	// A team kill is interference, not a point.
	d = tr.Attribute(Kill{KillID: 2, KillerPlayerID: 11, VictimPlayerID: 12, Weapon: "M4-A1", At: t0})
	if d == nil || d.Round.Counted || *d.Round.Flag != FlagInterference {
		t.Fatalf("team-mate kill: %+v", d)
	}
	_ = m
}

func TestForfeitAndTimers(t *testing.T) {
	tr := started(t, 4, 4, 1)
	m := tr.Current()
	// Before the ready timer nothing happens.
	if ev := tr.Tick(t0.Add(time.Minute), nil, nil, nil); len(ev) != 0 {
		t.Fatalf("early tick: %v", kinds(ev))
	}
	// Ready timer over, both absent: a ping and the match waits (timer cleared, still CALLED).
	ev := tr.Tick(t0.Add(3*time.Minute), nil, nil, map[int64]bool{})
	if !has(ev, EvAdminPing) || m.Status != MatchCalled || m.TimerEndsAt != nil {
		t.Fatalf("both absent: %v %+v", kinds(ev), m)
	}
	if ev := tr.Tick(t0.Add(4*time.Minute), nil, nil, map[int64]bool{}); len(ev) != 0 {
		t.Fatal("a waiting match is not pinged again")
	}
	// Re-called: the timer runs again; only B present when it ends: B wins by forfeit.
	if _, ev, err := tr.CallNext(t0.Add(4 * time.Minute)); err != nil || !has(ev, EvMatchCalled) || m.TimerEndsAt == nil {
		t.Fatalf("re-call: %v %v", err, kinds(ev))
	}
	b := tr.Entry(*m.EntryB)
	ev = tr.Tick(t0.Add(7*time.Minute), nil, nil, map[int64]bool{b.Players[0].PlayerID: true})
	if !has(ev, EvMatchDone) || m.Status != MatchForfeit || m.WinnerEntry == nil || *m.WinnerEntry != b.ID {
		t.Fatalf("forfeit: %v %+v", kinds(ev), m)
	}
	if tr.Entry(*m.EntryA).Status != EntryEliminated {
		t.Fatal("the absent side is out")
	}
	// The next match was called; both present at the ready timeout: it starts; the match timer
	// then runs out: a ping, no decision.
	next := tr.Current()
	if next == nil || next.Status != MatchCalled {
		t.Fatalf("next: %+v", next)
	}
	present := map[int64]bool{tr.Entry(*next.EntryA).Players[0].PlayerID: true, tr.Entry(*next.EntryB).Players[0].PlayerID: true}
	ev = tr.Tick(t0.Add(10*time.Minute), nil, nil, present)
	if !has(ev, EvMatchStarted) || next.Status != MatchLive {
		t.Fatalf("both present: %v", kinds(ev))
	}
	ev = tr.Tick(t0.Add(21*time.Minute), nil, nil, nil)
	if !has(ev, EvAdminPing) || next.Status != MatchLive || next.WinnerEntry != nil || next.TimerEndsAt != nil {
		t.Fatalf("match timer: %v %+v", kinds(ev), next)
	}
	// An admin decides it; the final is called; the admin decides the final; the tournament ends.
	ev, err := tr.AdminResult(next.ID, *next.EntryA, "admin", "B left", t0.Add(22*time.Minute))
	if err != nil || !has(ev, EvMatchDone) || next.Status != MatchDone || len(next.Rounds) != 1 || *next.Rounds[0].Flag != FlagManual || !next.Rounds[0].Counted {
		t.Fatalf("admin result: %v %v", err, kinds(ev))
	}
	final := tr.Current()
	if final == nil || final.Round != 2 || final.Status != MatchCalled {
		t.Fatalf("final: %+v", final)
	}
	ev, err = tr.AdminResult(final.ID, *final.EntryB, "admin", "", t0.Add(30*time.Minute))
	if err != nil || !has(ev, EvFinished) || tr.Status != StatusFinished || tr.FinishedAt == nil {
		t.Fatalf("finish: %v %v %s", err, kinds(ev), tr.Status)
	}
	champ := tr.Champion()
	if champ == nil || champ.ID != *final.EntryB || tr.Place(champ.ID) != 1 || tr.Place(*final.EntryA) != 2 || tr.Place(1) != 3 || tr.Place(3) != 3 {
		t.Fatalf("places: champion %v", champ)
	}
	for _, e := range tr.Entries {
		if e.Status == EntryActive {
			t.Fatalf("entry %d still active after the final", e.ID)
		}
	}
	if _, err := tr.Cancel(t0, "x"); !errors.Is(err, ErrWrongStatus) {
		t.Fatal("a finished tournament cannot be cancelled")
	}
}

func TestPauseResumeAndReplay(t *testing.T) {
	tr := started(t, 4, 4, 1)
	m := tr.Current()
	a, b := tr.Entry(*m.EntryA), tr.Entry(*m.EntryB)
	if _, err := tr.Pause(t0); err != nil || tr.Status != StatusPaused || m.TimerEndsAt != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := tr.Resume(t0.Add(time.Minute)); err != nil || tr.Status != StatusLive || m.TimerEndsAt == nil {
		t.Fatalf("resume: %v", err)
	}
	// A wins; the admin orders a replay: the round stops counting, the match is called again and
	// the final slot is cleared.
	d := tr.Attribute(kill(1, a, b, "M4-A1", t0))
	if _, err := tr.RecordRound(m, d.Round, t0.Add(2*time.Minute)); err != nil || m.Status != MatchDone {
		t.Fatal(err)
	}
	ev, err := tr.Replay(m.ID, "admin", t0.Add(3*time.Minute))
	if err != nil || !has(ev, EvBracket) {
		t.Fatalf("replay while another match is in play: %v %v", err, kinds(ev))
	}
	if m.Status != MatchPending || m.ScoreA != 0 || m.WinnerEntry != nil || m.Rounds[0].Counted || len(m.Rounds) != 2 || b.Status != EntryActive {
		t.Fatalf("replayed: %+v", m)
	}
	if final := tr.Final(); final.EntryA != nil {
		t.Fatal("the final slot must be cleared")
	}
	// The other semi-final decides; then the replayed match is called again.
	other := tr.Current()
	if other == nil || other == m {
		t.Fatalf("other: %+v", other)
	}
	if _, err := tr.AdminResult(other.ID, *other.EntryA, "admin", "", t0.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if cur := tr.Current(); cur != m || m.Status != MatchCalled {
		t.Fatalf("the replay must be called: %+v", cur)
	}
	// Once the final is called, neither semi-final can be replayed.
	if _, err := tr.AdminResult(m.ID, *m.EntryA, "admin", "", t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if tr.Current() != tr.Final() || tr.Final().Status != MatchCalled {
		t.Fatalf("the final must be called: %+v", tr.Current())
	}
	if _, err := tr.Replay(other.ID, "admin", t0); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("cannot replay once the next match is called: %v", err)
	}
}

func TestDQ(t *testing.T) {
	// Before the draw: the entry leaves the field.
	tr := fixture(4, 4, SeedingRanked, 1)
	if _, err := tr.DQ(3, "admin", "cheating", t0); err != nil || tr.Entry(3).Status != EntryDQ {
		t.Fatal(err)
	}
	if _, err := tr.Start(t0, nil, rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	assignIDs(tr)
	if len(tr.ActiveEntries()) != 3 {
		t.Fatalf("active: %d", len(tr.ActiveEntries()))
	}
	// During play: the DQ'd side of the match in play forfeits it.
	m := tr.Current()
	loser, winner := *m.EntryA, *m.EntryB
	ev, err := tr.DQ(loser, "admin", "cheating", t0)
	if err != nil || !has(ev, EvMatchDone) || m.Status != MatchForfeit || *m.WinnerEntry != winner || tr.Entry(loser).Status != EntryDQ {
		t.Fatalf("dq in play: %v %v %+v", err, kinds(ev), m)
	}
	// A player holding a later slot alone (a bye) is disqualified: the slot clears and the
	// opponent walks over when they arrive.
	tr2 := started(t, 3, 4, 1)
	final := tr2.Final()
	byeHolder := *final.EntryA
	ev, err = tr2.DQ(byeHolder, "admin", "", t0)
	if err != nil || final.EntryA != nil {
		t.Fatalf("dq bye holder: %v %+v", err, final)
	}
	cur := tr2.Current()
	if _, err := tr2.AdminResult(cur.ID, *cur.EntryA, "admin", "", t0); err != nil {
		t.Fatal(err)
	}
	if tr2.Status != StatusFinished || tr2.Champion() == nil || tr2.Champion().ID != *cur.EntryA {
		t.Fatalf("walkover to the title: %s", tr2.Status)
	}
}

func TestTickOpensAndStartsAndCancels(t *testing.T) {
	opens := t0.Add(-2 * time.Hour)
	tr := &Tournament{Status: StatusDraft, TeamSize: 1, BracketSize: 4, BestOf: 1, Seeding: SeedingRandom, StartsAt: t0, SignupOpensAt: &opens, CheckinMinutes: 30, Rules: Rules{MatchTimerMinutes: 10, ReadyMinutes: 2}}
	if ev := tr.Tick(opens.Add(-time.Second), nil, nil, nil); len(ev) != 0 || tr.Status != StatusDraft {
		t.Fatalf("before opening: %v", kinds(ev))
	}
	if ev := tr.Tick(opens, nil, nil, nil); !has(ev, EvOpened) || tr.Status != StatusSignup {
		t.Fatalf("opening: %v", kinds(ev))
	}
	for i := int64(1); i <= 2; i++ {
		if _, _, err := tr.Join(opens, []EntryPlayer{{PlayerID: i, DiscordUserID: string(rune('a' + i))}}); err != nil {
			t.Fatal(err)
		}
	}
	if ev := tr.Tick(t0.Add(-30*time.Minute), nil, nil, nil); !has(ev, EvCheckin) || tr.Status != StatusCheckin {
		t.Fatalf("check-in: %v", kinds(ev))
	}
	// Nobody checks in: cancelled at the start.
	if ev := tr.Tick(t0, nil, rand.New(rand.NewSource(1)), nil); !has(ev, EvCancelled) || tr.Status != StatusCancelled {
		t.Fatalf("cancel: %v %s", kinds(ev), tr.Status)
	}
	// A draft that opens and starts in one tick (a late scheduler): no check-in window, everyone
	// counts as present.
	tr2 := &Tournament{Status: StatusDraft, TeamSize: 1, BracketSize: 4, BestOf: 1, Seeding: SeedingRandom, StartsAt: t0, SignupOpensAt: &opens, CheckinMinutes: 0, Rules: Rules{MatchTimerMinutes: 10, ReadyMinutes: 2}}
	tr2.Status = StatusSignup
	for i := int64(1); i <= 2; i++ {
		tr2.Entries = append(tr2.Entries, &Entry{ID: i, TeamNo: int(i), Status: EntryActive, Players: []EntryPlayer{{PlayerID: i}}})
	}
	if ev := tr2.Tick(t0, nil, rand.New(rand.NewSource(1)), nil); !has(ev, EvStarted) || tr2.Status != StatusLive {
		t.Fatalf("start: %v %s", kinds(ev), tr2.Status)
	}
}

func TestParamsValidate(t *testing.T) {
	good := Params{Name: " Friday Night ", StartsAt: t0, Prizes: []Prize{{Place: 2, Points: 10}, {Place: 1, Points: 100, Title: ptrString(" Champ ")}}, Rules: Rules{AllowedWeapons: []string{"M4-A1", ""}, Arenas: []Arena{{X: 1, Z: 2, Radius: 50}}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if good.Name != "Friday Night" || good.TeamSize != 1 || good.BracketSize != 8 || good.BestOf != 1 || good.Seeding != SeedingRandom || good.CheckinMinutes != 0 ||
		good.Rules.MatchTimerMinutes != DefaultTimer || good.Rules.ReadyMinutes != DefaultReady || len(good.Rules.AllowedWeapons) != 1 ||
		good.Rules.Arenas[0].No != 1 || good.Rules.Arenas[0].Name != "Arena 1" || good.Prizes[0].Place != 1 || *good.Prizes[0].Title != "Champ" {
		t.Fatalf("defaults: %+v", good)
	}
	bad := []Params{
		{Name: "", StartsAt: t0},
		{Name: "x", StartsAt: t0, TeamSize: 3},
		{Name: "x", StartsAt: t0, BracketSize: 6},
		{Name: "x", StartsAt: t0, BestOf: 2},
		{Name: "x", StartsAt: t0, Seeding: "BEST"},
		{Name: "x"},
		{Name: "x", StartsAt: t0, SignupOpensAt: ptrTime(t0.Add(time.Hour))},
		{Name: "x", StartsAt: t0, Prizes: []Prize{{Place: 1}, {Place: 1}}},
		{Name: "x", StartsAt: t0, Prizes: []Prize{{Place: 9}}},
		{Name: "x", StartsAt: t0, Rules: Rules{Arenas: []Arena{{Radius: 0}}}},
		{Name: "x", StartsAt: t0, Rules: Rules{MatchTimerMinutes: 500}},
	}
	for i, p := range bad {
		if err := p.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if NormalizeWeapon(" M4-A1 ") != "m4a1" || NormalizeWeapon("KA_M") != "kam" {
		t.Fatal("normalisation")
	}
}
