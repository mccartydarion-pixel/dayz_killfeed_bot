package killfeed

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/nitrado/nitradofixture"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Restart safety (docs/PERFORMANCE.md section 22). Each test runs the production engine, the real
// Nitrado client and the real persistence queue against the staging Nitrado fixture, with one
// shared "database" (kills and deaths unique by fingerprint, one durable checkpoint) and one
// testBot per bot process. A restart is a new testBot: nothing in memory survives, only the
// database does. restart_safety_integration_test.go runs the same scenarios on PostgreSQL.

// sharedCheckpoints is the adm_checkpoints row both processes read and write.
type sharedCheckpoints struct {
	mu sync.Mutex
	cp *DurableCheckpoint
}

func (s *sharedCheckpoints) LoadADMCheckpoint(context.Context, int64, int64) (*DurableCheckpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cp == nil {
		return nil, nil
	}
	cp := *s.cp
	return &cp, nil
}

func (s *sharedCheckpoints) SaveADMCheckpoint(_ context.Context, _ int64, _ int64, _ string, cp DurableCheckpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cp = &cp
	return nil
}

// restartDB is the database the processes of one scenario share: the guild and server rows they
// work for, the persistence store each process uses (one per process - on PostgreSQL each has its
// own connection pool) and the checkpoint store.
type restartDB struct {
	guildID, serverID int64
	stores            [2]PersistenceStore
	checkpoints       CheckpointStore
}

func fakeRestartDB() restartDB {
	store := newFakePersistenceStore()
	return restartDB{guildID: 1, serverID: 7, stores: [2]PersistenceStore{store, store}, checkpoints: &sharedCheckpoints{}}
}

// testBot is one bot process: its own engine (tracker, in-memory dedupe), persistence queue and
// publisher. Only store and checkpoints outlive it.
type testBot struct {
	engine *Engine
	pub    *fixturePublisher
	stop   context.CancelFunc
	apiURL string
}

// poll runs one engine cycle. The Nitrado client reuses a directory listing for 750 ms, which in
// production is far shorter than a poll interval; here polls follow each other within
// milliseconds, so each one gets a client with an empty listing cache.
func (b *testBot) poll() error {
	b.engine.client = nitrado.NewClient(b.apiURL, "fixture-token-not-a-credential", nil)
	return b.engine.PollOnce(context.Background())
}

func startTestBot(t *testing.T, apiURL, serviceID string, db restartDB, store PersistenceStore, checkpoints CheckpointStore) *testBot {
	t.Helper()
	client := nitrado.NewClient(apiURL, "fixture-token-not-a-credential", nil)
	e := NewEngine(client, serviceID, NewADMParser())
	e.SetDurableCheckpoint(checkpoints, db.guildID, db.serverID)
	pub := &fixturePublisher{}
	e.SetKillPublisher(pub)
	e.SetDeathPublisher(pub)
	pq := NewPersistenceQueueWithServerID(store, db.guildID, db.serverID, serviceID)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go pq.Run(ctx)
	e.SetPersistence(pq)
	return &testBot{engine: e, pub: pub, stop: cancel, apiURL: apiURL}
}

func (b *testBot) killVictims() []string {
	b.pub.mu.Lock()
	defer b.pub.mu.Unlock()
	out := make([]string, 0, len(b.pub.kills))
	for _, ev := range b.pub.kills {
		out = append(out, ev.Victim.Name)
	}
	return out
}

// pollUntilKills polls like the worker does until the bot has published want kills, and fails if
// it publishes more or the deadline passes.
func (b *testBot) pollUntilKills(t *testing.T, what string, want int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		if err := b.poll(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		got := len(b.killVictims())
		if got == want {
			return
		}
		if got > want || time.Now().After(deadline) {
			t.Fatalf("%s: published %d kills, want %d", what, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle polls a few more times to prove nothing further is published.
func (b *testBot) settle(t *testing.T, what string, want int) {
	t.Helper()
	for i := 0; i < 3; i++ {
		b.pollUntilKills(t, what, want)
	}
}

func sameNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func newRestartFixture(t *testing.T, serviceID int64) (fx *nitradofixture.Server, apiURL, svc string) {
	t.Helper()
	fx = nitradofixture.New(serviceID, "")
	srv := httptest.NewServer(fx)
	t.Cleanup(srv.Close)
	return fx, srv.URL, strconv.FormatInt(serviceID, 10)
}

// Kills that land while the bot is down are posted after it comes back: all of them, in the
// order the game wrote them, and nothing the previous process already posted is posted again.
func runKillsWhileDownArePostedInOrder(t *testing.T, serviceID int64, db restartDB) {
	fx, apiURL, svc := newRestartFixture(t, serviceID)

	first := startTestBot(t, apiURL, svc, db, db.stores[0], db.checkpoints)
	first.pollUntilKills(t, "first start", 0)
	before := fx.AddKills(3)
	first.pollUntilKills(t, "before the restart", 3)
	if got := first.killVictims(); !sameNames(got, before) {
		t.Fatalf("before the restart: %v, want %v", got, before)
	}
	first.stop() // the process is gone

	missed := fx.AddKills(5) // the game keeps writing while no bot is running
	fx.AddDeaths(1)
	missed = append(missed, fx.AddKills(2)...)

	second := startTestBot(t, apiURL, svc, db, db.stores[1], db.checkpoints)
	second.pollUntilKills(t, "after the restart", len(missed))
	if got := second.killVictims(); !sameNames(got, missed) {
		t.Fatalf("after the restart the new process posted %v, want exactly the missed kills in order %v", got, missed)
	}
	second.settle(t, "nothing is posted twice", len(missed))
	if _, deaths := second.pub.counts(); deaths != 1 {
		t.Fatalf("the death written while down was posted %d times, want 1", deaths)
	}
}

func TestRestartKillsWhileDownArePostedInOrder(t *testing.T) {
	t.Parallel()
	runKillsWhileDownArePostedInOrder(t, 90000101, fakeRestartDB())
}

// dyingStore is a database connection whose process is killed right after its Nth kill insert
// commits: that kill is stored (and its card handed to the feed), and nothing after it - no later
// event, no checkpoint - is ever written by this process.
type dyingStore struct {
	PersistenceStore
	mu        sync.Mutex
	killsLeft int
	dead      bool
}

var errProcessDied = errors.New("process died")

func (d *dyingStore) isDead() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dead
}

func (d *dyingStore) UpsertPlayer(ctx context.Context, guildID int64, dayzID, name string, at time.Time) (int64, error) {
	if d.isDead() {
		return 0, errProcessDied
	}
	return d.PersistenceStore.UpsertPlayer(ctx, guildID, dayzID, name, at)
}

func (d *dyingStore) InsertKill(ctx context.Context, k repository.KillRecord) error {
	if d.isDead() {
		return errProcessDied
	}
	err := d.PersistenceStore.InsertKill(ctx, k)
	if err == nil {
		d.mu.Lock()
		if d.killsLeft--; d.killsLeft <= 0 {
			d.dead = true
		}
		d.mu.Unlock()
	}
	return err
}

func (d *dyingStore) InsertDeath(ctx context.Context, r repository.DeathRecord) error {
	if d.isDead() {
		return errProcessDied
	}
	return d.PersistenceStore.InsertDeath(ctx, r)
}

// dyingCheckpoints refuses every save once the process is dead.
type dyingCheckpoints struct {
	CheckpointStore
	dead func() bool
}

func (d dyingCheckpoints) SaveADMCheckpoint(ctx context.Context, guildID, serverID int64, session string, cp DurableCheckpoint) error {
	if d.dead() {
		return errProcessDied
	}
	return d.CheckpointStore.SaveADMCheckpoint(ctx, guildID, serverID, session, cp)
}

// A process killed in the middle of a batch (some kills stored, the checkpoint not yet moved): the
// next process reads the whole batch again, does not post the kills that were already stored a
// second time, and posts the rest in order.
func runRestartMidBatchPostsEachKillOnce(t *testing.T, serviceID int64, db restartDB) {
	fx, apiURL, svc := newRestartFixture(t, serviceID)

	dying := &dyingStore{PersistenceStore: db.stores[0], killsLeft: 1 << 30}
	first := startTestBot(t, apiURL, svc, db, dying, dyingCheckpoints{CheckpointStore: db.checkpoints, dead: dying.isDead})
	first.pollUntilKills(t, "first start", 0)

	dying.mu.Lock()
	dying.killsLeft = 3
	dying.mu.Unlock()
	batch := fx.AddKills(6) // one download holds all six
	first.pollUntilKills(t, "until the process dies", 3)
	if !dying.isDead() {
		t.Fatal("the first process was meant to die after its third kill")
	}
	if got := first.killVictims(); !sameNames(got, batch[:3]) {
		t.Fatalf("the first process posted %v, want %v", got, batch[:3])
	}
	first.stop()

	second := startTestBot(t, apiURL, svc, db, db.stores[1], db.checkpoints)
	second.pollUntilKills(t, "after the restart", 3)
	if got := second.killVictims(); !sameNames(got, batch[3:]) {
		t.Fatalf("after the restart the new process posted %v, want only the unposted rest %v", got, batch[3:])
	}
	second.settle(t, "nothing is posted twice", 3)
}

func TestRestartMidBatchPostsEachKillOnce(t *testing.T) {
	t.Parallel()
	runRestartMidBatchPostsEachKillOnce(t, 90000102, fakeRestartDB())
}

// A deploy overlaps the old and the new process for some seconds. Both read the same log and both
// try to store every kill; only the one whose insert wins posts it. Across both processes every
// kill is posted exactly once, and once the old process is gone the new one carries on alone.
func runOverlappingProcessesPostEachKillOnce(t *testing.T, serviceID int64, db restartDB) {
	fx, apiURL, svc := newRestartFixture(t, serviceID)

	old := startTestBot(t, apiURL, svc, db, db.stores[0], db.checkpoints)
	old.pollUntilKills(t, "old process start", 0)
	warm := fx.AddKills(2)
	old.pollUntilKills(t, "old process alone", 2)

	fresh := startTestBot(t, apiURL, svc, db, db.stores[1], db.checkpoints) // the deploy's new process
	fresh.pollUntilKills(t, "new process start", 0)

	// Both processes poll at the same time while the game keeps writing.
	var wg sync.WaitGroup
	stopPolling := make(chan struct{})
	for _, b := range []*testBot{old, fresh} {
		wg.Add(1)
		go func(b *testBot) {
			defer wg.Done()
			for {
				select {
				case <-stopPolling:
					return
				default:
				}
				if err := b.poll(); err != nil {
					t.Error(err)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}(b)
	}
	var during []string
	for burst := 0; burst < 4; burst++ {
		during = append(during, fx.AddKills(5)...)
		time.Sleep(120 * time.Millisecond) // several polls of each process per burst
	}
	deadline := time.Now().Add(8 * time.Second)
	for len(old.killVictims())+len(fresh.killVictims()) < len(warm)+len(during) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // a few more polls each: a double post would show up now
	close(stopPolling)
	wg.Wait()

	posted := map[string]int{}
	for _, name := range append(old.killVictims(), fresh.killVictims()...) {
		posted[name]++
	}
	for _, name := range append(append([]string{}, warm...), during...) {
		if posted[name] != 1 {
			t.Fatalf("kill of %s was posted %d times across the two processes, want exactly 1 (old posted %d, new posted %d)",
				name, posted[name], len(old.killVictims()), len(fresh.killVictims()))
		}
	}
	if len(posted) != len(warm)+len(during) {
		t.Fatalf("%d distinct kills posted, want %d", len(posted), len(warm)+len(during))
	}
	// Within one process the order is the log's order.
	for _, b := range []*testBot{old, fresh} {
		last := ""
		for _, name := range b.killVictims() {
			if name <= last { // fixture victim names are zero-padded and increasing
				t.Fatalf("a process posted out of order: %v", b.killVictims())
			}
			last = name
		}
	}

	old.stop() // the overlap ends
	alreadyByNew := len(fresh.killVictims())
	after := fx.AddKills(3)
	fresh.pollUntilKills(t, "new process alone", alreadyByNew+len(after))
	got := fresh.killVictims()
	if !sameNames(got[alreadyByNew:], after) {
		t.Fatalf("after the overlap the new process posted %v, want %v", got[alreadyByNew:], after)
	}
	fresh.settle(t, "nothing is posted twice", alreadyByNew+len(after))
}

func TestOverlappingProcessesPostEachKillOnce(t *testing.T) {
	t.Parallel()
	runOverlappingProcessesPostEachKillOnce(t, 90000103, fakeRestartDB())
}
