package killfeed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const (
	connectingAlice = `16:16:09 | Player "Alice" (id=a001) is connecting`
	connectAlice    = `16:16:10 | Player "Alice" (id=a001 pos=<1234.5, 6789.5, 3.0>) is connected`
	connectAliceLat = `16:20:44 | Player "Alice" (id=a001 pos=<1234.5, 6789.5, 3.0>) is connected`
	connectBob      = `16:16:10 | Player "Bob" (id=b002) is connected`
	disconnectAlice = `16:58:12 | Player "Alice" (id=a001 pos=<1.0, 2.0, 3.0>) has been disconnected`
	disconnectBob   = `16:58:12 | Player "Bob" (id=b002) has been disconnected`
)

type recordingConnectionPublisher struct{ notices []ConnectionNotice }

func (r *recordingConnectionPublisher) PublishConnection(n ConnectionNotice) {
	r.notices = append(r.notices, n)
}

type panickingConnectionPublisher struct{}

func (panickingConnectionPublisher) PublishConnection(ConnectionNotice) { panic("consumer bug") }

type trackerClock struct{ t time.Time }

func (c *trackerClock) now() time.Time { return c.t }

func connectionEngine() (*Engine, *recordingConnectionPublisher, *trackerClock) {
	e := newPresenceEngine()
	pub := &recordingConnectionPublisher{}
	e.SetConnectionPublisher(pub)
	clock := &trackerClock{t: time.Date(2026, 9, 18, 16, 16, 10, 0, time.UTC)}
	e.players.now = clock.now
	return e, pub, clock
}

// --- tracker ------------------------------------------------------------------

func TestTrackerDisconnectSessionReportsObservedTime(t *testing.T) {
	tr := NewPlayerTracker()
	clock := &trackerClock{t: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
	tr.now = clock.now
	p := &PlayerRef{ID: "a001", Name: "Alice"}

	tr.PlayerConnected(p)
	clock.t = clock.t.Add(42 * time.Minute)
	session, removed := tr.DisconnectSession(p)
	if !removed || session != 42*time.Minute {
		t.Fatalf("expected removal with a 42m session, got %v removed=%v", session, removed)
	}
	if _, removed := tr.DisconnectSession(p); removed {
		t.Fatal("a second disconnect for the same player must not remove anything")
	}
}

// A duplicate "is connected" refreshes the record but must not restart the
// session clock.
func TestTrackerDuplicateConnectKeepsSessionStart(t *testing.T) {
	tr := NewPlayerTracker()
	clock := &trackerClock{t: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
	tr.now = clock.now
	p := &PlayerRef{ID: "a001", Name: "Alice"}

	if !tr.PlayerConnected(p) {
		t.Fatal("first connect must be new")
	}
	clock.t = clock.t.Add(10 * time.Minute)
	if tr.PlayerConnected(p) {
		t.Fatal("a duplicate connect must not count as a new connection")
	}
	clock.t = clock.t.Add(5 * time.Minute)
	if session, _ := tr.DisconnectSession(p); session != 15*time.Minute {
		t.Fatalf("expected the session to be measured from the FIRST connect (15m), got %v", session)
	}
}

// --- engine: A/B connect + disconnect --------------------------------------------

func TestEnginePublishesConnectWithOnlyNameAndKind(t *testing.T) {
	e, pub, _ := connectionEngine()
	e.processLines([]string{connectAlice})
	if len(pub.notices) != 1 {
		t.Fatalf("expected 1 notice, got %d", len(pub.notices))
	}
	if n := pub.notices[0]; n.Kind != ConnectionConnected || n.Name != "Alice" || n.Session != 0 {
		t.Fatalf("unexpected notice: %+v", n)
	}
}

func TestEnginePublishesDisconnectWithObservedSession(t *testing.T) {
	e, pub, clock := connectionEngine()
	e.processLines([]string{connectAlice})
	clock.t = clock.t.Add(42 * time.Minute)
	e.processLines([]string{disconnectAlice})
	if len(pub.notices) != 2 {
		t.Fatalf("expected connect + disconnect notices, got %d", len(pub.notices))
	}
	if n := pub.notices[1]; n.Kind != ConnectionDisconnected || n.Name != "Alice" || n.Session != 42*time.Minute {
		t.Fatalf("unexpected disconnect notice: %+v", n)
	}
}

// A player who joined before Champion tracked them still gets a disconnect, but
// no invented duration - only a tracked player can be announced leaving.
func TestEngineDisconnectOfUntrackedPlayerIsNotPublished(t *testing.T) {
	e, pub, _ := connectionEngine()
	e.processLines([]string{disconnectAlice})
	if len(pub.notices) != 0 {
		t.Fatalf("a disconnect for a player Champion never saw connect must not be announced, got %+v", pub.notices)
	}
}

// --- F: replay / duplicates --------------------------------------------------------

func TestEngineReplayedConnectionEventsAreNotPublishedTwice(t *testing.T) {
	e, pub, _ := connectionEngine()
	e.processLines([]string{connectAlice})
	e.processLines([]string{connectAlice}) // replay (checkpoint retry, rotation overlap)
	e.processLines([]string{disconnectAlice})
	e.processLines([]string{disconnectAlice})
	if len(pub.notices) != 2 {
		t.Fatalf("expected exactly one connect and one disconnect, got %+v", pub.notices)
	}
}

// A repeated "is connected" for a player who is already online (a later second,
// so it is not a byte-identical replay) is a refresh, not a new connection.
func TestEngineDuplicateConnectWhileOnlineIsNotAnnouncedAgain(t *testing.T) {
	e, pub, clock := connectionEngine()
	e.processLines([]string{connectAlice})
	clock.t = clock.t.Add(4 * time.Minute)
	e.processLines([]string{connectAliceLat})
	clock.t = clock.t.Add(10 * time.Minute)
	e.processLines([]string{disconnectAlice})

	if len(pub.notices) != 2 || pub.notices[0].Kind != ConnectionConnected || pub.notices[1].Kind != ConnectionDisconnected {
		t.Fatalf("expected exactly connect then disconnect, got %+v", pub.notices)
	}
	if pub.notices[1].Session != 14*time.Minute {
		t.Fatalf("expected the session measured from the first connect (14m), got %v", pub.notices[1].Session)
	}
}

// The regression the feed depends on: two DIFFERENT players connecting in the
// same second (a server-restart burst) must both be tracked and announced.
func TestEngineSameSecondConnectsOfDifferentPlayersAreBothHandled(t *testing.T) {
	e, pub, _ := connectionEngine()
	e.processLines([]string{connectAlice, connectBob})
	if e.players.OnlineCount() != 2 {
		t.Fatalf("expected both players online, got %d", e.players.OnlineCount())
	}
	if len(pub.notices) != 2 || pub.notices[0].Name != "Alice" || pub.notices[1].Name != "Bob" {
		t.Fatalf("expected both announced as separate players, got %+v", pub.notices)
	}
	e.processLines([]string{disconnectAlice, disconnectBob})
	if e.players.OnlineCount() != 0 || len(pub.notices) != 4 {
		t.Fatalf("expected both disconnects handled, online=%d notices=%d", e.players.OnlineCount(), len(pub.notices))
	}
}

// --- K: reconnect -------------------------------------------------------------------

// Champion has no explicit reconnect event, so none is inferred: a disconnect
// followed by a connect is exactly Disconnected then Connected.
func TestEngineReconnectIsNeverInferred(t *testing.T) {
	e, pub, clock := connectionEngine()
	e.processLines([]string{connectAlice})
	clock.t = clock.t.Add(2 * time.Minute)
	e.processLines([]string{disconnectAlice})
	clock.t = clock.t.Add(5 * time.Second)
	e.processLines([]string{connectAliceLat})

	want := []ConnectionKind{ConnectionConnected, ConnectionDisconnected, ConnectionConnected}
	if len(pub.notices) != len(want) {
		t.Fatalf("expected %d notices, got %+v", len(want), pub.notices)
	}
	for i, k := range want {
		if pub.notices[i].Kind != k {
			t.Fatalf("notice %d: expected %s, got %s", i, k, pub.notices[i].Kind)
		}
	}
}

// "is connecting" is parsed but is not a connection state change.
func TestEngineConnectingIsNotAConnection(t *testing.T) {
	e, pub, _ := connectionEngine()
	e.processLines([]string{connectingAlice})
	if len(pub.notices) != 0 {
		t.Fatalf("PLAYER_CONNECTING must not be announced, got %+v", pub.notices)
	}
}

// --- persistence ordering -----------------------------------------------------------

type failingConnectStore struct {
	*fakePersistenceStore
	mu   sync.Mutex
	fail bool
}

func (s *failingConnectStore) RecordConnect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail {
		return errors.New("database unavailable")
	}
	return s.fakePersistenceStore.RecordConnect(ctx, guildID, serverID, playerID, at)
}

func (s *failingConnectStore) setFail(v bool) {
	s.mu.Lock()
	s.fail = v
	s.mu.Unlock()
}

// The notice is published only after the durable persistence succeeded: while it
// fails nothing is announced, and once it recovers the event is announced
// exactly once (the failed attempt was not remembered by the dedupe).
func TestEngineConnectionPublishedOnlyAfterPersistenceSucceeds(t *testing.T) {
	store := &failingConnectStore{fakePersistenceStore: newFakePersistenceStore(), fail: true}
	queue := NewPersistenceQueueWithServerID(store, 1, 2, "session")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go queue.Run(ctx)

	e := newPresenceEngine()
	pub := &recordingConnectionPublisher{}
	e.SetConnectionPublisher(pub)
	e.SetPersistence(queue)

	e.processLines([]string{connectAlice})
	if len(pub.notices) != 0 || e.players.OnlineCount() != 0 {
		t.Fatalf("a failed persist must publish nothing and not track, notices=%d online=%d", len(pub.notices), e.players.OnlineCount())
	}

	store.setFail(false)
	e.processLines([]string{connectAlice}) // the retry on the next poll
	queue.Close()
	if len(pub.notices) != 1 || pub.notices[0].Kind != ConnectionConnected {
		t.Fatalf("expected exactly one notice after recovery, got %+v", pub.notices)
	}
}

// --- H: consumer failure -------------------------------------------------------------

func TestEngineSurvivesPanickingConnectionPublisher(t *testing.T) {
	e := newPresenceEngine()
	e.SetConnectionPublisher(panickingConnectionPublisher{})
	hits := &recordingHitPublisher{}
	e.SetHitPublisher(hits)

	parsed := e.processLines([]string{connectAlice, hitLineTorso, disconnectAlice})

	if parsed != 3 {
		t.Fatalf("expected all lines processed despite the panic, got %d", parsed)
	}
	if e.players.OnlineCount() != 0 {
		t.Fatalf("presence tracking must be unaffected, online=%d", e.players.OnlineCount())
	}
	if len(hits.hits) != 1 {
		t.Fatalf("the hit feed must be unaffected, got %d hits", len(hits.hits))
	}
}

func TestEngineWithoutConnectionPublisherTracksPresenceAsBefore(t *testing.T) {
	e := newPresenceEngine()
	e.processLines([]string{connectAlice, connectBob})
	if e.players.OnlineCount() != 2 {
		t.Fatalf("expected 2 online, got %d", e.players.OnlineCount())
	}
}
