package charwipe

import (
	"context"
	"errors"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

const (
	svc        = "19806451"
	rootDir    = "/games/ni1_1"
	missionDir = rootDir + "/noftp/dayzps_missions/dayzOffline.chernarusplus"
	storageDir = missionDir + "/storage_1"
	playersDB  = storageDir + "/players.db"
)

// fake is a Nitrado server with a clock: a stop and a start take time, and the status follows.
type fake struct {
	now   time.Time
	calls []string // every call that changes something, in order

	files map[string]bool // full path -> is a directory

	status      string
	statusErr   bool
	facts       error         // GameserverFacts fails outright (Nitrado unreachable)
	stopTakes   time.Duration // "stopping" for this long, then "stopped"
	stopIgnored bool          // Stop is accepted but the server keeps running
	stopErr     error
	stoppedAt   time.Time
	startTakes  time.Duration // "stopped" for this long after an accepted Restart, then "restarting"
	startAt     time.Time
	restartErrs int  // the first n Restart calls fail
	restartDead bool // Restart is accepted and nothing happens
	deleteErr   error
	deleteNoop  bool // DeleteFile answers ok and deletes nothing
	listErrAt   int  // the n-th listing of the storage folder fails (1-based)
	storageList int

	saved   []string
	saveErr map[string]error
}

func newFake() *fake {
	return &fake{now: time.Date(2026, 10, 4, 11, 55, 0, 0, time.UTC), status: "started", stopTakes: 20 * time.Second, startTakes: 10 * time.Second,
		files: map[string]bool{
			missionDir + "/cfggameplay.json":         false,
			missionDir + "/cfgplayerspawnpoints.xml": false,
			storageDir:                               true,
			playersDB:                                false,
			storageDir + "/data":                     true,
			storageDir + "/data/events.bin":          false,
			storageDir + "/data/vehicles.bin":        false,
		}}
}

func (f *fake) opts() Options {
	return Options{Now: func() time.Time { return f.now }, Sleep: func(_ context.Context, d time.Duration) { f.now = f.now.Add(d) }}
}

func (f *fake) save(_ context.Context, state string) error {
	if err := f.saveErr[state]; err != nil {
		return err
	}
	f.saved = append(f.saved, state)
	return nil
}

func (f *fake) tick() {
	if f.status == "stopping" && !f.now.Before(f.stoppedAt) {
		f.status = "stopped"
	}
	if f.status == "stopped" && !f.startAt.IsZero() && !f.now.Before(f.startAt) {
		f.status = "restarting"
	}
}

func (f *fake) GameserverFacts(context.Context, string) (nitrado.GameserverFacts, error) {
	if f.facts != nil || f.statusErr {
		return nitrado.GameserverFacts{}, errors.New("unavailable")
	}
	f.tick()
	return nitrado.GameserverFacts{Game: "dayzps", Status: f.status, GamePath: rootDir + "/noftp/dayzps", Mission: "dayzOffline.chernarusplus"}, nil
}

func (f *fake) ListEntries(_ context.Context, _, dir string) ([]nitrado.DirEntry, error) {
	if dir == storageDir {
		if f.storageList++; f.storageList == f.listErrAt {
			return nil, errors.New("listing failed")
		}
	}
	var out []nitrado.DirEntry
	for p, isDir := range f.files {
		if path.Dir(p) == dir {
			out = append(out, nitrado.DirEntry{Name: path.Base(p), Path: p, IsDir: isDir})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("directory not found")
	}
	return out, nil
}

func (f *fake) ReadLog(context.Context, string, string) ([]byte, error) {
	panic("clearing characters never downloads a file")
}

func (f *fake) Stop(context.Context, string, string) error {
	f.calls = append(f.calls, "stop")
	if f.stopErr != nil {
		return f.stopErr
	}
	if !f.stopIgnored {
		f.status, f.stoppedAt, f.startAt = "stopping", f.now.Add(f.stopTakes), time.Time{}
	}
	return nil
}

func (f *fake) Restart(context.Context, string, string) error {
	f.calls = append(f.calls, "restart")
	if f.restartErrs > 0 {
		f.restartErrs--
		return errors.New("restart refused")
	}
	f.tick()
	switch {
	case f.restartDead:
	case f.status == "stopped":
		f.startAt = f.now.Add(f.startTakes)
	case f.status == "started":
		f.status = "restarting"
	}
	return nil
}

func (f *fake) DeleteFile(_ context.Context, _, p string) error {
	f.calls = append(f.calls, "delete "+p)
	f.tick()
	if f.status != "stopped" {
		panic("the file was deleted while the server was not stopped")
	}
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if !f.deleteNoop {
		delete(f.files, p)
	}
	return nil
}

func (f *fake) count(call string) int {
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

// untouched fails the test if anything but players.db is missing.
func (f *fake) untouched(t *testing.T) {
	t.Helper()
	for _, p := range []string{missionDir + "/cfggameplay.json", missionDir + "/cfgplayerspawnpoints.xml", storageDir, storageDir + "/data", storageDir + "/data/events.bin", storageDir + "/data/vehicles.bin"} {
		if _, ok := f.files[p]; !ok {
			t.Fatalf("%s is gone; only players.db may ever be deleted", p)
		}
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "delete ") && c != "delete "+playersDB {
			t.Fatalf("deleted something else: %s", c)
		}
	}
}

func TestHappyPath(t *testing.T) {
	f := newFake()
	out := Run(context.Background(), f, svc, f.save, f.opts())
	if !out.Cleared || out.ServerDown || !out.StopRequested || out.Restarts != 1 || out.State() != StateDone {
		t.Fatalf("outcome: %+v", out)
	}
	if want := []string{"stop", "delete " + playersDB, "restart"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls: %v, want %v", f.calls, want)
	}
	if want := []string{StateStopRequested, StateDeleted, StateStartRequested}; !reflect.DeepEqual(f.saved, want) {
		t.Fatalf("states stored: %v, want %v", f.saved, want)
	}
	if _, there := f.files[playersDB]; there || f.status != "restarting" {
		t.Fatalf("players.db there=%v, status %s", there, f.status)
	}
	f.untouched(t)
	if out.Note() != "Saved characters were cleared, so everyone spawns fresh." {
		t.Fatalf("note: %s", out.Note())
	}
}

// Nothing is stopped unless the file is there, the status can be read and the step is recorded.
func TestNothingIsStoppedWithoutTheFile(t *testing.T) {
	cases := []struct {
		name   string
		change func(f *fake)
		reason string
	}{
		{"players.db is not there", func(f *fake) { delete(f.files, playersDB) }, ReasonNotFound},
		{"players.db is a folder", func(f *fake) { f.files[playersDB] = true }, ReasonNotFound},
		{"no storage folder", func(f *fake) {
			for p := range f.files {
				if strings.HasPrefix(p, storageDir) {
					delete(f.files, p)
				}
			}
		}, ReasonNotFound},
		{"the storage folder cannot be listed", func(f *fake) { f.listErrAt = 1 }, ReasonNotFound},
		{"no mission folder", func(f *fake) { delete(f.files, missionDir+"/cfggameplay.json") }, ReasonNotFound},
		{"Nitrado is unreachable", func(f *fake) { f.facts = errors.New("down") }, ReasonUnreachable},
		{"the step cannot be recorded", func(f *fake) { f.saveErr = map[string]error{StateStopRequested: errors.New("db down")} }, ReasonNotRecorded},
	}
	for _, c := range cases {
		f := newFake()
		c.change(f)
		out := Run(context.Background(), f, svc, f.save, f.opts())
		if out.Cleared || out.StopRequested || out.ServerDown || out.Reason != c.reason || out.State() != StateFailed {
			t.Fatalf("%s: outcome %+v, want reason %q", c.name, out, c.reason)
		}
		if len(f.calls) != 0 || len(f.saved) != 0 || f.status != "started" {
			t.Fatalf("%s: the server was touched: calls %v, states %v, status %s", c.name, f.calls, f.saved, f.status)
		}
		if !strings.Contains(out.Note(), "could not be cleared: "+c.reason+".") {
			t.Fatalf("%s: note %q", c.name, out.Note())
		}
	}
	if out := Run(context.Background(), nil, svc, func(context.Context, string) error { return nil }, Options{}); out.Reason != ReasonNotConfigured || out.StopRequested {
		t.Fatalf("no remote: %+v", out)
	}
}

func TestServerNeverStops(t *testing.T) {
	for _, name := range []string{"stop ignored", "stop refused", "stop too slow"} {
		f := newFake()
		switch name {
		case "stop ignored":
			f.stopIgnored = true
		case "stop refused":
			f.stopErr = errors.New("403")
		case "stop too slow":
			f.stopTakes = 150 * time.Second
		}
		began := f.now
		out := Run(context.Background(), f, svc, f.save, f.opts())
		if out.Cleared || out.Reason != ReasonNeverStopped || !out.StopRequested {
			t.Fatalf("%s: outcome %+v", name, out)
		}
		if _, there := f.files[playersDB]; !there || f.count("delete "+playersDB) != 0 {
			t.Fatalf("%s: a server that did not stop must keep its file (calls %v)", name, f.calls)
		}
		// Step 4 ran anyway: the server was asked to start and is running.
		if f.count("restart") == 0 || out.ServerDown || !starting(f.status) {
			t.Fatalf("%s: the server was not started again: calls %v, status %s, outcome %+v", name, f.calls, f.status, out)
		}
		if waited := f.now.Sub(began); waited < 120*time.Second {
			t.Fatalf("%s: gave up waiting for the stop after %s", name, waited)
		}
		if want := []string{StateStopRequested, StateStartRequested}; !reflect.DeepEqual(f.saved, want) {
			t.Fatalf("%s: states %v", name, f.saved)
		}
		f.untouched(t)
	}
}

func TestDeleteFailsOrIsNotConfirmed(t *testing.T) {
	cases := []struct {
		name   string
		change func(f *fake)
		reason string
	}{
		{"the delete is refused", func(f *fake) { f.deleteErr = errors.New("500") }, ReasonNotDeleted},
		{"the delete answers ok but the file is still listed", func(f *fake) { f.deleteNoop = true }, ReasonNotDeleted},
		{"the folder cannot be listed afterwards", func(f *fake) { f.listErrAt = 2 }, ReasonNotConfirmed},
	}
	for _, c := range cases {
		f := newFake()
		c.change(f)
		out := Run(context.Background(), f, svc, f.save, f.opts())
		if out.Cleared || out.Reason != c.reason || out.ServerDown || out.State() != StateFailed {
			t.Fatalf("%s: outcome %+v", c.name, out)
		}
		if f.count("delete "+playersDB) != 1 {
			t.Fatalf("%s: the delete is attempted exactly once: %v", c.name, f.calls)
		}
		if f.count("restart") != 1 || !starting(f.status) || f.calls[len(f.calls)-1] != "restart" {
			t.Fatalf("%s: the server must be started again whatever the delete did: %v, status %s", c.name, f.calls, f.status)
		}
		if want := []string{StateStopRequested, StateStartRequested}; !reflect.DeepEqual(f.saved, want) {
			t.Fatalf("%s: DELETED must only be stored for a verified delete: %v", c.name, f.saved)
		}
		f.untouched(t)
	}
}

func TestRestartFailsThenSucceeds(t *testing.T) {
	// Nitrado refuses the first two requests.
	f := newFake()
	f.restartErrs = 2
	out := Run(context.Background(), f, svc, f.save, f.opts())
	if !out.Cleared || out.ServerDown || out.Restarts != 3 || f.count("restart") != 3 || !starting(f.status) {
		t.Fatalf("refused twice: outcome %+v, calls %v, status %s", out, f.calls, f.status)
	}
	// Nitrado accepts the request and the server stays stopped: it is asked again.
	f = newFake()
	f.restartDead = true
	o := f.opts()
	sleep := o.Sleep
	o.Sleep = func(ctx context.Context, d time.Duration) {
		sleep(ctx, d)
		if f.count("restart") == 2 {
			f.restartDead = false // the third request works
		}
	}
	out = Run(context.Background(), f, svc, f.save, o)
	if !out.Cleared || out.ServerDown || out.Restarts != 3 || !starting(f.status) {
		t.Fatalf("stayed stopped: outcome %+v, calls %v, status %s", out, f.calls, f.status)
	}
	// The status cannot be read for a while after the restart was sent.
	f = newFake()
	o = f.opts()
	sleep = o.Sleep
	o.Sleep = func(ctx context.Context, d time.Duration) {
		sleep(ctx, d)
		f.statusErr = f.count("restart") > 0 && f.now.Sub(f.startAt) < 30*time.Second
	}
	out = Run(context.Background(), f, svc, f.save, o)
	if !out.Cleared || out.ServerDown {
		t.Fatalf("status unreadable for a while: %+v", out)
	}
}

func TestRestartNeverSucceeds(t *testing.T) {
	for _, name := range []string{"accepted, nothing happens", "always refused"} {
		f := newFake()
		if name == "always refused" {
			f.restartErrs = 1000
		} else {
			f.restartDead = true
		}
		afterStop := time.Time{}
		o := f.opts()
		sleep := o.Sleep
		o.Sleep = func(ctx context.Context, d time.Duration) {
			if afterStop.IsZero() && f.count("restart") > 0 {
				afterStop = f.now
			}
			sleep(ctx, d)
		}
		out := Run(context.Background(), f, svc, f.save, o)
		if !out.ServerDown || !out.Cleared || out.State() != StateFailed {
			t.Fatalf("%s: outcome %+v", name, out)
		}
		if n := f.count("restart"); n < 3 || n > 6 {
			t.Fatalf("%s: Restart was sent %d times", name, n)
		}
		if waited := f.now.Sub(afterStop); waited < 120*time.Second || waited > 150*time.Second {
			t.Fatalf("%s: waited %s for the server to start", name, waited)
		}
		if note := out.Note(); !strings.Contains(note, ServerDownMessage) || !strings.HasPrefix(note, "Saved characters were cleared.") {
			t.Fatalf("%s: note %q", name, note)
		}
	}
	// Not cleared and not started: both are said.
	out := Outcome{Reason: ReasonNeverStopped, ServerDown: true, StopRequested: true}
	if note := out.Note(); note != "Saved characters could not be cleared: the server did not stop in time. "+ServerDownMessage {
		t.Fatalf("note: %q", note)
	}
}

// The caller's deadline ends while the server is stopping: the procedure still starts it again.
func TestCallerDeadlineDoesNotLeaveTheServerStopped(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	o := f.opts()
	sleep := o.Sleep
	o.Sleep = func(c context.Context, d time.Duration) {
		cancel() // the pass deadline, as soon as the stop is under way
		sleep(c, d)
	}
	out := Run(ctx, f, svc, f.save, o)
	if out.ServerDown || !out.Cleared || !starting(f.status) || f.count("restart") != 1 {
		t.Fatalf("outcome %+v, calls %v, status %s", out, f.calls, f.status)
	}
	// A context that is already over stops nothing.
	f = newFake()
	out = Run(ctx, f, svc, f.save, f.opts())
	if out.StopRequested || len(f.calls) != 0 {
		t.Fatalf("a finished context must not stop the server: %+v %v", out, f.calls)
	}
}

// After a crash the procedure is resumed from the stored state: never a stop, never a delete, and
// the server is running at the end.
func TestResumeAfterCrashInEachState(t *testing.T) {
	cases := []struct {
		name         string
		state        string
		cleared      bool
		status       string // what the crash left behind
		wantRestarts int
		wantCleared  bool
	}{
		{"STOP_REQUESTED, the stop never went out", StateStopRequested, false, "started", 1, false},
		{"STOP_REQUESTED, the server is stopping", StateStopRequested, false, "stopping", 2, false},
		{"STOP_REQUESTED, the server is stopped", StateStopRequested, false, "stopped", 1, false},
		{"DELETED, the server is stopped", StateDeleted, true, "stopped", 1, true},
		{"START_REQUESTED, the restart never went out", StateStartRequested, true, "stopped", 1, true},
		{"START_REQUESTED, the server is already starting", StateStartRequested, true, "restarting", 0, true},
		{"START_REQUESTED, the server is up", StateStartRequested, false, "started", 0, false},
	}
	for _, c := range cases {
		f := newFake()
		f.status = c.status
		if c.status == "stopping" {
			f.stoppedAt = f.now.Add(20 * time.Second)
		}
		out := Resume(context.Background(), f, svc, c.state, c.cleared, f.save, f.opts())
		if out.ServerDown || !starting(f.status) {
			t.Fatalf("%s: the server is not running: %+v, status %s", c.name, out, f.status)
		}
		if f.count("stop") != 0 || f.count("delete "+playersDB) != 0 {
			t.Fatalf("%s: a resume never stops and never deletes: %v", c.name, f.calls)
		}
		if f.count("restart") != c.wantRestarts || out.Restarts != c.wantRestarts {
			t.Fatalf("%s: %d restarts, want %d (%v)", c.name, f.count("restart"), c.wantRestarts, f.calls)
		}
		if out.Cleared != c.wantCleared || (!c.wantCleared && out.Reason != ReasonInterrupted) {
			t.Fatalf("%s: outcome %+v", c.name, out)
		}
		if _, there := f.files[playersDB]; !there {
			t.Fatalf("%s: the file was deleted on resume", c.name)
		}
	}
	// A resume that cannot start the server says so.
	f := newFake()
	f.status, f.restartDead = "stopped", true
	if out := Resume(context.Background(), f, svc, StateDeleted, true, f.save, f.opts()); !out.ServerDown || !out.Cleared || out.State() != StateFailed {
		t.Fatalf("resume, server stays down: %+v", out)
	}
	// Nothing was in progress: nothing is done.
	for _, state := range []string{StateNone, StateDone, StateFailed, ""} {
		f := newFake()
		if out := Resume(context.Background(), f, svc, state, false, f.save, f.opts()); len(f.calls) != 0 || out.ServerDown || out.StopRequested {
			t.Fatalf("resume in %q: %+v %v", state, out, f.calls)
		}
	}
	if !InProgress(StateStopRequested) || !InProgress(StateDeleted) || !InProgress(StateStartRequested) || InProgress(StateNone) || InProgress(StateDone) || InProgress(StateFailed) ||
		!Closed(StateDone) || !Closed(StateFailed) || Closed(StateNone) || Closed(StateStartRequested) {
		t.Fatal("state classes")
	}
}

// The one thing Champion may ever delete.
func TestOnlyPlayersDBMayBeDeleted(t *testing.T) {
	if err := checkTarget(missionDir, playersDB); err != nil {
		t.Fatalf("the saved-characters file was refused: %v", err)
	}
	for _, bad := range []string{
		"", "/", "players.db", "storage_1/players.db",
		storageDir,                              // the storage folder itself
		storageDir + "/",                        //
		missionDir,                              // the mission folder
		missionDir + "/players.db",              // not in storage_1
		missionDir + "/cfggameplay.json",        //
		missionDir + "/custom/players.db",       //
		storageDir + "/data/players.db",         // deeper
		storageDir + "/data",                    //
		storageDir + "/data/events.bin",         //
		storageDir + "/players.db/",             //
		storageDir + "/players.db.bak",          //
		storageDir + "/Players.db",              //
		storageDir + "/players.dbx",             //
		storageDir + "/xplayers.db",             //
		storageDir + "/../storage_1/players.db", // not clean
		storageDir + "//players.db",             //
		storageDir + "/./players.db",            //
		missionDir + "/storage_2/players.db",    // another storage folder
		missionDir + "/storage_1x/players.db",   //
		missionDir + "/x/storage_1/players.db",  // storage_1 is not directly in the mission folder
		missionDir + "x/storage_1/players.db",   // another mission
		rootDir + "/noftp/dayzps_missions/dayzOffline.enoch/storage_1/players.db", // another mission
		rootDir + "/storage_1/players.db",                                         //
		"/storage_1/players.db",                                                   //
		storageDir + "/players.db\x00",                                            //
		strings.ReplaceAll(playersDB, "/", `\`),                                   //
	} {
		if err := checkTarget(missionDir, bad); !errors.Is(err, ErrNotPlayersDB) {
			t.Errorf("%q was allowed", bad)
		}
	}
	// A mission folder that is not a clean absolute path allows nothing.
	for _, dir := range []string{"", "/", "games/x/mission", missionDir + "/", missionDir + "/../x", "."} {
		if err := checkTarget(dir, dir+"/storage_1/players.db"); err == nil {
			t.Errorf("mission folder %q was accepted", dir)
		}
	}
	// deletePlayersDB sends nothing for a refused path.
	f := newFake()
	f.status = "stopped"
	for _, bad := range []string{storageDir, missionDir + "/cfggameplay.json", storageDir + "/data/events.bin"} {
		if err := deletePlayersDB(context.Background(), f, svc, missionDir, bad); !errors.Is(err, ErrNotPlayersDB) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("a refused path reached Nitrado: %v", f.calls)
	}
	if err := deletePlayersDB(context.Background(), f, svc, missionDir, playersDB); err != nil || len(f.calls) != 1 {
		t.Fatalf("the allowed path: %v %v", err, f.calls)
	}
	f.untouched(t)
}
