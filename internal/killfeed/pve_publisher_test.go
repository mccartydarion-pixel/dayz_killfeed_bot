package killfeed

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

const (
	suicideLine = `16:20:00 | Player "Alice" (id=a001 pos=<1234.5, 6789.5, 3.0>) performed EmoteSuicide with Fists`
	// A generic death line: no "killed by", no cause stated.
	genericDeathLine = `16:35:00 | Player "Ceiyxe" (DEAD) (id=c003 pos=<7000.0, 1200.0, 8.0>) died. Stats> Water: 100 Energy: 80`
	pvpKillLine      = `16:40:12 | Player "ookylianoo" (DEAD) (id=v9 pos=<1.0, 2.0, 3.0>) killed by Player "MmeyAFK_7" (id=k9 pos=<4.0, 5.0, 6.0>) with M4-A1 from 62.1978 meters`
)

// --- classification ---------------------------------------------------------------

func TestPveCauseClassificationRules(t *testing.T) {
	player := &PlayerRef{Name: "Alice", ID: "a001"}
	other := &PlayerRef{Name: "Bob", ID: "b002"}
	for _, tc := range []struct {
		name  string
		ev    *Event
		cause DeathCause
		ok    bool
	}{
		{"nil", nil, "", false},
		// Rule 1: an explicit player attacker is PvP, never PvE.
		{"PvP kill", &Event{Type: EventPlayerKill, Killer: other, Victim: player}, "", false},
		{"kill even with a cause set", &Event{Type: EventPlayerKill, Killer: other, Victim: player, Cause: DeathCauseInfected}, "", false},
		{"death with an attacker", &Event{Type: EventPlayerDeath, Player: player, Attacker: other, Cause: DeathCauseAnimal}, "", false},
		{"suicide with an attacker", &Event{Type: EventSuicideAction, Player: player, Killer: other}, "", false},
		// Rule 2: explicit suicide.
		{"suicide", &Event{Type: EventSuicideAction, Player: player, Cause: DeathCauseSuicide}, DeathCauseSuicide, true},
		{"suicide without a cause value", &Event{Type: EventSuicideAction, Player: player}, DeathCauseSuicide, true},
		// Rules 3/4: an explicit non-player source.
		{"infected", &Event{Type: EventPlayerDeath, Player: player, Cause: DeathCauseInfected}, DeathCauseInfected, true},
		{"animal", &Event{Type: EventPlayerDeath, Player: player, Cause: DeathCauseAnimal}, DeathCauseAnimal, true},
		{"environment", &Event{Type: EventPlayerDeath, Player: player, Cause: DeathCauseEnvironment}, DeathCauseEnvironment, true},
		// Rule 5: ambiguous - no guessed cause, current behaviour preserved.
		{"generic death", &Event{Type: EventPlayerDeath, Player: player}, "", false},
		{"generic death with Dead marker and a weapon string", &Event{Type: EventPlayerDeath, Player: player, Dead: true, Weapon: "Zmb_something"}, "", false},
		{"unknown cause value", &Event{Type: EventPlayerDeath, Player: player, Cause: "FALL"}, "", false},
		{"hit", &Event{Type: EventPlayerHit, Attacker: other, Victim: player}, "", false},
		{"connect", &Event{Type: EventPlayerConnect, Player: player}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause, ok := PveCause(tc.ev)
			if cause != tc.cause || ok != tc.ok {
				t.Fatalf("PveCause = (%q, %v), want (%q, %v)", cause, ok, tc.cause, tc.ok)
			}
		})
	}
}

// What the parser can and cannot prove: only the suicide line carries a cause.
func TestParserProvesOnlySuicideCause(t *testing.T) {
	p := NewADMParser()
	for _, tc := range []struct {
		line  string
		typ   EventType
		cause DeathCause
	}{
		{suicideLine, EventSuicideAction, DeathCauseSuicide},
		{genericDeathLine, EventPlayerDeath, ""},
		{pvpKillLine, EventPlayerKill, ""},
	} {
		ev, err := p.ParseLine(tc.line)
		if err != nil || ev == nil {
			t.Fatalf("expected %s to parse, got %v %v", tc.typ, ev, err)
		}
		if ev.Type != tc.typ || ev.Cause != tc.cause {
			t.Fatalf("line parsed as %s cause=%q, want %s cause=%q", ev.Type, ev.Cause, tc.typ, tc.cause)
		}
	}
	// A "killed by <non-player>" line is not understood at all today: no event,
	// so no cause can be claimed from it.
	if ev, _ := p.ParseLine(`16:50:00 | Player "Alice" (DEAD) (id=a001 pos=<1.0, 2.0, 3.0>) killed by SomeNonPlayerThing`); ev != nil {
		t.Fatalf("a non-player 'killed by' line must not be guessed into an event, got %+v", ev)
	}
}

// --- fakes ----------------------------------------------------------------------------

type recordingPveDeathPublisher struct {
	mu      sync.Mutex
	claim   bool
	notices []PveDeathNotice
}

func (r *recordingPveDeathPublisher) PublishPveDeath(n PveDeathNotice) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, n)
	return r.claim
}

func (r *recordingPveDeathPublisher) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notices)
}

type panickingPveDeathPublisher struct{}

func (panickingPveDeathPublisher) PublishPveDeath(PveDeathNotice) bool { panic("consumer bug") }

type recordingDeathFeed struct {
	mu     sync.Mutex
	events []*Event
}

func (r *recordingDeathFeed) PublishDeath(ev *Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingDeathFeed) types() []EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []EventType
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

// failingDeathStore fails InsertDeath while fail is set.
type failingDeathStore struct {
	*fakePersistenceStore
	mu   sync.Mutex
	fail bool
}

func (s *failingDeathStore) InsertDeath(ctx context.Context, d repository.DeathRecord) error {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return errors.New("database unavailable")
	}
	return s.fakePersistenceStore.InsertDeath(ctx, d)
}

func (s *failingDeathStore) setFail(v bool) {
	s.mu.Lock()
	s.fail = v
	s.mu.Unlock()
}

type pveRig struct {
	engine *Engine
	pve    *recordingPveDeathPublisher
	legacy *recordingDeathFeed
	kills  *recordingPublisher
	store  *failingDeathStore
	queue  *PersistenceQueue
}

// newPveRig wires an engine over a real PersistenceQueue (fake store), so the
// death hooks run exactly as in production: only after a durable insert.
func newPveRig(t *testing.T, claim bool) *pveRig {
	t.Helper()
	store := &failingDeathStore{fakePersistenceStore: newFakePersistenceStore()}
	queue := NewPersistenceQueueWithServerID(store, 1, 2, "session")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go queue.Run(ctx)

	r := &pveRig{
		engine: NewEngine(nil, "svc", NewADMParser()),
		pve:    &recordingPveDeathPublisher{claim: claim},
		legacy: &recordingDeathFeed{},
		kills:  &recordingPublisher{},
		store:  store,
		queue:  queue,
	}
	r.engine.SetPersistence(queue)
	r.engine.SetPveDeathPublisher(r.pve)
	r.engine.SetDeathPublisher(r.legacy)
	r.engine.SetKillPublisher(r.kills)
	return r
}

// --- A: PvP stays on KILLFEED -------------------------------------------------------------

func TestEnginePvPKillGoesToKillfeedOnlyNeverPveFeed(t *testing.T) {
	r := newPveRig(t, true)
	r.engine.processLines([]string{pvpKillLine})
	r.queue.Close()

	if len(r.kills.kills) != 1 {
		t.Fatalf("expected the PvP kill on the KILLFEED, got %d", len(r.kills.kills))
	}
	if r.pve.calls() != 0 {
		t.Fatalf("a PvP kill must never be offered to the PVE_FEED, got %d calls", r.pve.calls())
	}
	if len(r.legacy.types()) != 0 {
		t.Fatalf("a PvP kill must never reach the death feed, got %v", r.legacy.types())
	}
}

// --- B: suicide -> PVE_FEED only ----------------------------------------------------------

func TestEngineSuicideIsClaimedByPveFeedAndSkipsLegacyDeathFeed(t *testing.T) {
	r := newPveRig(t, true)
	r.engine.processLines([]string{suicideLine})
	r.queue.Close()

	if r.pve.calls() != 1 || r.pve.notices[0].Cause != DeathCauseSuicide || r.pve.notices[0].Name != "Alice" {
		t.Fatalf("expected one suicide notice for Alice, got %+v", r.pve.notices)
	}
	if got := r.legacy.types(); len(got) != 0 {
		t.Fatalf("a claimed suicide must NOT also go to the legacy death feed, got %v", got)
	}
	if len(r.kills.kills) != 0 {
		t.Fatal("a suicide is not a kill")
	}
}

// Not claimed (no route, lookup failure): behaviour is exactly the legacy one.
func TestEngineUnclaimedSuicideStillReachesLegacyDeathFeed(t *testing.T) {
	r := newPveRig(t, false)
	r.engine.processLines([]string{suicideLine})
	r.queue.Close()
	if got := r.legacy.types(); len(got) != 1 || got[0] != EventSuicideAction {
		t.Fatalf("an unclaimed suicide must reach the legacy death feed as before, got %v", got)
	}
}

// --- F: ambiguous death: no guessed cause, current behaviour preserved ---------------------

func TestEngineGenericDeathIsNotGuessedAndStaysOnLegacyDeathFeed(t *testing.T) {
	r := newPveRig(t, true)
	r.engine.processLines([]string{genericDeathLine})
	r.queue.Close()

	if r.pve.calls() != 0 {
		t.Fatalf("a death with no proven cause must not be offered to the PVE_FEED, got %+v", r.pve.notices)
	}
	if got := r.legacy.types(); len(got) != 1 || got[0] != EventPlayerDeath {
		t.Fatalf("expected the generic death on the legacy death feed as before, got %v", got)
	}
}

// --- C/D/E: explicit non-player sources (the parser cannot produce them yet, so the
// event is built with the explicit cause a future parser would set) ------------------------

func TestEngineExplicitNonPlayerCausesAreClaimed(t *testing.T) {
	for _, cause := range []DeathCause{DeathCauseInfected, DeathCauseAnimal, DeathCauseEnvironment} {
		t.Run(string(cause), func(t *testing.T) {
			r := newPveRig(t, true)
			r.engine.parser = fixedParser{ev: &Event{Type: EventPlayerDeath, TimeOfDay: "16:36:00", Player: &PlayerRef{Name: "Carol", ID: "c9"}, Dead: true, Cause: cause}}
			r.engine.processLines([]string{"anything"})
			r.queue.Close()
			if r.pve.calls() != 1 || r.pve.notices[0].Cause != cause || r.pve.notices[0].Name != "Carol" {
				t.Fatalf("expected the %s death claimed, got %+v", cause, r.pve.notices)
			}
			if len(r.legacy.types()) != 0 {
				t.Fatal("a claimed death must not also reach the legacy death feed")
			}
		})
	}
}

type fixedParser struct{ ev *Event }

func (p fixedParser) ParseLine(string) (*Event, error) {
	cp := *p.ev
	return &cp, nil
}

// --- L: replay -------------------------------------------------------------------------------

// A replayed suicide (same engine: ADM dedupe; fresh engine over the same durable
// store: the durable fingerprint) is published once.
func TestEngineReplayedSuicideIsPublishedOnce(t *testing.T) {
	r := newPveRig(t, true)
	r.engine.processLines([]string{suicideLine})
	r.engine.processLines([]string{suicideLine}) // in-memory ADM dedupe
	if r.pve.calls() != 1 {
		t.Fatalf("in-process replay: expected 1 publish, got %d", r.pve.calls())
	}

	// A process restart: new engine (empty dedupe), same durable store.
	restarted := NewEngine(nil, "svc", NewADMParser())
	restarted.SetPersistence(r.queue)
	restarted.SetPveDeathPublisher(r.pve)
	restarted.SetDeathPublisher(r.legacy)
	restarted.processLines([]string{suicideLine})
	r.queue.Close()
	if r.pve.calls() != 1 {
		t.Fatalf("post-restart replay: the durable dedupe must prevent a second publish, got %d", r.pve.calls())
	}
	if len(r.legacy.types()) != 0 {
		t.Fatalf("a claimed, replayed suicide must never reach the legacy feed either, got %v", r.legacy.types())
	}
}

// --- persistence ordering -----------------------------------------------------------------------

// Nothing is published while the durable insert fails; once it succeeds the
// (retried) death is published exactly once.
func TestEnginePveDeathPublishedOnlyAfterPersistenceSucceeds(t *testing.T) {
	r := newPveRig(t, true)
	r.store.setFail(true)
	r.engine.processLines([]string{suicideLine})
	if r.pve.calls() != 0 || len(r.legacy.types()) != 0 {
		t.Fatalf("a failed persist must publish nothing, pve=%d legacy=%v", r.pve.calls(), r.legacy.types())
	}

	r.store.setFail(false)
	r.engine.processLines([]string{suicideLine}) // the retry on the next poll
	r.queue.Close()
	if r.pve.calls() != 1 {
		t.Fatalf("expected exactly one publish after recovery, got %d", r.pve.calls())
	}
}

// --- K: consumer failure --------------------------------------------------------------------------

// A panicking PVE consumer neither stops persistence nor swallows the death:
// it is treated as unclaimed and continues to the legacy death feed; kills and
// the other events are unaffected.
func TestEngineSurvivesPanickingPveFeed(t *testing.T) {
	r := newPveRig(t, true)
	r.engine.SetPveDeathPublisher(panickingPveDeathPublisher{})

	r.engine.processLines([]string{suicideLine, pvpKillLine})
	r.queue.Close()

	if got := r.legacy.types(); len(got) != 1 || got[0] != EventSuicideAction {
		t.Fatalf("an unclaimed (panicked) death must fall through to the legacy feed, got %v", got)
	}
	if len(r.kills.kills) != 1 {
		t.Fatalf("kill processing must be unaffected, got %d", len(r.kills.kills))
	}
	if len(r.store.deaths) != 1 {
		t.Fatalf("the death must still be persisted, got %d", len(r.store.deaths))
	}
}

// With no PVE publisher attached every death goes to the legacy feed as before.
func TestEngineWithoutPveFeedIsUnchanged(t *testing.T) {
	r := newPveRig(t, true)
	r.engine.SetPveDeathPublisher(nil)
	r.engine.processLines([]string{suicideLine, genericDeathLine})
	r.queue.Close()
	got := r.legacy.types()
	if len(got) != 2 || got[0] != EventSuicideAction || got[1] != EventPlayerDeath {
		t.Fatalf("expected both deaths on the legacy feed, got %v", got)
	}
}
