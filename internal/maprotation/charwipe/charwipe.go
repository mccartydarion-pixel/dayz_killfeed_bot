// Package charwipe clears the saved characters of a Nitrado DayZ server after a map switch, so
// that everyone spawns fresh on the new map (docs/MAP_ROTATION.md, "Fresh characters on every map
// switch"). DayZ keeps every character in <mission folder>/storage_1/players.db and only reads it
// when the server starts, so the procedure is: stop the server, delete that one file, start the
// server again.
//
// This is the only code in Champion that deletes anything on a game server, and the only thing it
// can delete is that one file: deletePlayersDB is the single caller of the Nitrado delete call and
// refuses every other path. The isolation test in internal/shop/missionwrite allows the delete
// call in this package alone, and only internal/app/map_rotation_worker.go may import it.
//
// The rule above all others: a server Champion stopped is started again. Once Stop has been
// requested, Restart is sent whatever else happened, on a context that outlives the caller's
// deadline, and it is sent again while the server stays down. The caller stores each state before
// the step it names (StateStopRequested before Stop), so that after a crash Resume can finish the
// job: it never stops and never deletes, it only makes sure the server is running. If the server
// cannot be seen starting, Outcome.ServerDown tells the caller to stop the rotation and alert
// staff.
//
// The stop, delete and restart calls have not been exercised against a live Nitrado service. The
// procedure therefore trusts no answer to those calls: whether the server stopped, whether the
// file is gone and whether the server is starting are each judged by a read afterwards.
package charwipe

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/maprotation"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Remote is every Nitrado call the procedure makes: the server's status and mission (GameserverFacts),
// a folder listing (ListEntries), stop, restart and the delete. ReadLog comes with
// maprotation.Reader and is never called here. *nitrado.Client satisfies it.
type Remote interface {
	maprotation.Reader
	Stop(ctx context.Context, serviceID, message string) error
	Restart(ctx context.Context, serviceID, message string) error
	DeleteFile(ctx context.Context, serviceID, path string) error
}

// How far clearing the characters got. The caller stores the state on the switch.
const (
	StateNone           = "NONE"
	StateStopRequested  = "STOP_REQUESTED"  // stored before Stop is sent
	StateDeleted        = "DELETED"         // the file is verified gone
	StateStartRequested = "START_REQUESTED" // stored before Restart is sent
	StateDone           = "DONE"            // characters cleared, server seen starting
	StateFailed         = "FAILED"          // closed without that: see the note
)

// InProgress reports whether state is one a crash can leave behind with the server possibly
// stopped. A switch in such a state is handed to Resume.
func InProgress(state string) bool {
	return state == StateStopRequested || state == StateDeleted || state == StateStartRequested
}

// Closed reports whether the procedure ran to an end for the switch (it never runs twice).
func Closed(state string) bool { return state == StateDone || state == StateFailed }

// ServerDownMessage is what the owner and staff read when a stopped server could not be seen
// starting again.
const ServerDownMessage = "The server was stopped to clear characters and could not be started again. Start it in Nitrado."

// Reasons the characters were not cleared, in plain language.
const (
	ReasonNotFound      = "the saved-characters file was not found"
	ReasonUnreachable   = "Nitrado could not be reached"
	ReasonNoStatus      = "Nitrado did not report whether the server is running"
	ReasonNotRecorded   = "Champion could not record the step, so the server was left running"
	ReasonNeverStopped  = "the server did not stop in time"
	ReasonNotDeleted    = "Nitrado did not delete the saved-characters file"
	ReasonNotConfirmed  = "it could not be confirmed that the saved-characters file was deleted"
	ReasonInterrupted   = "Champion was interrupted before the saved-characters file was confirmed deleted"
	ReasonNotConfigured = "the server connection is missing"
)

// SaveFunc stores a state durably and returns nil only when it is stored.
type SaveFunc func(ctx context.Context, state string) error

// Options are the waits. The zero value means the defaults; tests replace Sleep and Now.
type Options struct {
	Poll        time.Duration // between two status reads (5 s)
	StopWait    time.Duration // how long the server may take to report "stopped" (120 s)
	StartWait   time.Duration // how long it may take to be seen starting (120 s)
	Resend      time.Duration // Restart is sent again when the server is not starting after this (40 s)
	MaxRestarts int           // Restart requests Nitrado accepted, at most (3)
	Call        time.Duration // one Nitrado call (20 s)
	Total       time.Duration // everything after Stop was requested (7 min)
	Sleep       func(ctx context.Context, d time.Duration)
	Now         func() time.Time
}

func (o Options) withDefaults() Options {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Poll, 5*time.Second)
	def(&o.StopWait, 120*time.Second)
	def(&o.StartWait, 120*time.Second)
	def(&o.Resend, 40*time.Second)
	def(&o.Call, 20*time.Second)
	def(&o.Total, 7*time.Minute)
	if o.MaxRestarts <= 0 {
		o.MaxRestarts = 3
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Outcome is what happened.
type Outcome struct {
	// Cleared: players.db was deleted and a listing afterwards no longer shows it.
	Cleared bool
	// Reason says why not, when Cleared is false.
	Reason string
	// StopRequested: the procedure got as far as asking Nitrado to stop the server (so it also
	// asked for a start afterwards).
	StopRequested bool
	// ServerDown: the server was stopped and could not be seen starting again. The caller stops the
	// rotation and alerts staff.
	ServerDown bool
	// Restarts is how many Restart requests were sent.
	Restarts int
}

// State is the closing state for the outcome.
func (o Outcome) State() string {
	if o.Cleared && !o.ServerDown {
		return StateDone
	}
	return StateFailed
}

// Note is the outcome as a sentence for the switch's message.
func (o Outcome) Note() string {
	note := "Saved characters were cleared, so everyone spawns fresh."
	if !o.Cleared {
		reason := o.Reason
		if reason == "" {
			reason = ReasonNotConfirmed
		}
		note = "Saved characters could not be cleared: " + reason + "."
	}
	if o.ServerDown {
		if o.Cleared {
			note = "Saved characters were cleared."
		}
		note += " " + ServerDownMessage
	}
	return note
}

// ErrNotPlayersDB: the path is not the one file Champion may delete.
var ErrNotPlayersDB = errors.New("charwipe: only <mission folder>/storage_1/players.db may be deleted")

// checkTarget allows exactly one path: the file named players.db, directly inside a folder named
// storage_1, directly inside the located mission folder. Everything else is refused: another file
// name, another folder, a deeper or shallower path, the storage folder itself, a relative or
// unclean path.
func checkTarget(missionDir, full string) error {
	if missionDir == "" || missionDir == "/" || !strings.HasPrefix(missionDir, "/") || path.Clean(missionDir) != missionDir ||
		strings.ContainsAny(full, "\\\x00") || path.Clean(full) != full {
		return ErrNotPlayersDB
	}
	storage := path.Dir(full)
	if path.Base(full) != maprotation.PlayersDBFile || path.Base(storage) != maprotation.StorageDir || path.Dir(storage) != missionDir {
		return ErrNotPlayersDB
	}
	if full != missionDir+"/"+maprotation.StorageDir+"/"+maprotation.PlayersDBFile {
		return ErrNotPlayersDB
	}
	return nil
}

// deletePlayersDB is the only place Champion deletes a file on a game server. It deletes full only
// if checkTarget allows it.
func deletePlayersDB(ctx context.Context, rm Remote, serviceID, missionDir, full string) error {
	if err := checkTarget(missionDir, full); err != nil {
		return err
	}
	return rm.DeleteFile(ctx, serviceID, full)
}

type run struct {
	rm   Remote
	svc  string
	save SaveFunc
	o    Options
}

func (r *run) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, r.o.Call)
}

// status is Nitrado's word for the server: "started", "stopped", "stopping", "restarting", ...
func (r *run) status(ctx context.Context) (string, error) {
	cctx, cancel := r.call(ctx)
	defer cancel()
	gs, err := r.rm.GameserverFacts(cctx, r.svc)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(gs.Status)), nil
}

// starting: Nitrado says the server is up or on its way up.
func starting(status string) bool { return status == "started" || status == "restarting" }

// listed reports whether players.db is a file in the storage folder.
func (r *run) listed(ctx context.Context, storageDir string) (bool, error) {
	cctx, cancel := r.call(ctx)
	defer cancel()
	entries, err := r.rm.ListEntries(cctx, r.svc, storageDir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name == maprotation.PlayersDBFile && !e.IsDir {
			return true, nil
		}
	}
	return false, nil
}

// waitStopped polls until Nitrado reports the server stopped, for at most StopWait.
func (r *run) waitStopped(ctx context.Context) bool {
	began := r.o.Now()
	for {
		if st, err := r.status(ctx); err == nil && st == "stopped" {
			return true
		}
		if r.o.Now().Sub(began) >= r.o.StopWait || ctx.Err() != nil {
			return false
		}
		r.o.Sleep(ctx, r.o.Poll)
	}
}

// ensureStarted asks Nitrado to start the server and polls until it is seen starting, for at most
// StartWait. Restart is sent again while the server is not starting: after Resend when Nitrado
// accepted the request, at the next poll when it did not. It returns whether the server was seen
// starting and how many requests were sent.
func (r *run) ensureStarted(ctx context.Context) (bool, int) {
	// Stored first: the caller records the time of this restart for the restart count, and after a
	// crash the state says a start may already be under way.
	_ = r.save(ctx, StateStartRequested)
	began := r.o.Now()
	accepted, sent := 0, 0
	var nextSend time.Time
	for {
		if now := r.o.Now(); accepted < r.o.MaxRestarts && sent < 2*r.o.MaxRestarts && !now.Before(nextSend) {
			cctx, cancel := r.call(ctx)
			err := r.rm.Restart(cctx, r.svc, "Champion: starting the server after clearing saved characters")
			cancel()
			sent++
			nextSend = now.Add(r.o.Resend)
			if err == nil {
				accepted++
			} else {
				nextSend = now // not accepted: asked again at the next poll
			}
		}
		r.o.Sleep(ctx, r.o.Poll)
		if st, err := r.status(ctx); err == nil && starting(st) {
			return true, sent
		}
		if r.o.Now().Sub(began) >= r.o.StartWait || ctx.Err() != nil {
			return false, sent
		}
	}
}

// Run clears the saved characters of the server. It is called once per switch, after the switch's
// two files are verified in place. save is called with StateStopRequested before Stop is sent (if
// that cannot be stored, the server is not stopped), with StateDeleted once the file is verified
// gone and with StateStartRequested before Restart is sent. The caller stores the closing state
// (Outcome.State) itself.
//
// Until Stop is requested the caller's context applies and nothing has been changed. From then on
// the work continues on a context of its own, so that a deadline or a shutdown never leaves the
// server stopped.
func Run(ctx context.Context, rm Remote, serviceID string, save SaveFunc, opt Options) Outcome {
	if rm == nil || serviceID == "" || save == nil {
		return Outcome{Reason: ReasonNotConfigured}
	}
	r := &run{rm: rm, svc: serviceID, save: save, o: opt.withDefaults()}

	// 1. The file must be there before anything is stopped.
	paths, err := maprotation.Locate(ctx, rm, serviceID)
	if err != nil {
		if errors.Is(err, maprotation.ErrServerLookup) {
			return Outcome{Reason: ReasonUnreachable}
		}
		return Outcome{Reason: ReasonNotFound}
	}
	storageDir, file, err := paths.PlayersDB()
	if err != nil || checkTarget(paths.MissionDir, file) != nil {
		return Outcome{Reason: ReasonNotFound}
	}
	if found, err := r.listed(ctx, storageDir); err != nil || !found {
		return Outcome{Reason: ReasonNotFound}
	}
	// The status is what every later step is judged by: if it cannot be read now, nothing starts.
	if _, err := r.status(ctx); err != nil {
		return Outcome{Reason: ReasonNoStatus}
	}
	if ctx.Err() != nil {
		return Outcome{Reason: ReasonUnreachable}
	}

	// 2. Remember that a stop is coming, then stop.
	if err := save(ctx, StateStopRequested); err != nil {
		return Outcome{Reason: ReasonNotRecorded}
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.o.Total)
	defer cancel()
	out := Outcome{StopRequested: true}
	cctx, ccancel := r.call(wctx)
	_ = rm.Stop(cctx, serviceID, "Champion: clearing saved characters for the new map") // the status decides, not this answer
	ccancel()

	if !r.waitStopped(wctx) {
		out.Reason = ReasonNeverStopped
	} else {
		// 3. Delete the one file, once, and judge it by listing the folder again.
		dctx, dcancel := r.call(wctx)
		_ = deletePlayersDB(dctx, rm, serviceID, paths.MissionDir, file)
		dcancel()
		switch found, err := r.listed(wctx, storageDir); {
		case err != nil:
			out.Reason = ReasonNotConfirmed
		case found:
			out.Reason = ReasonNotDeleted
		default:
			out.Cleared = true
			_ = save(wctx, StateDeleted)
		}
	}

	// 4. Always: start the server again.
	ok, sent := r.ensureStarted(wctx)
	out.Restarts, out.ServerDown = sent, !ok
	return out
}

// Resume finishes a procedure a crash interrupted in state (one of the InProgress states). It
// never stops the server and never deletes: it makes sure the server is running, and reports the
// characters as cleared only if that was verified before the crash (cleared).
//
// A procedure interrupted in StateStartRequested may already have sent its Restart, so the status
// is read first and a server that is starting is left alone. In the two earlier states Restart is
// always sent: the status may not show yet that a stop is under way.
func Resume(ctx context.Context, rm Remote, serviceID, state string, cleared bool, save SaveFunc, opt Options) Outcome {
	out := Outcome{Cleared: cleared, StopRequested: true}
	if !cleared {
		out.Reason = ReasonInterrupted
	}
	if !InProgress(state) {
		out.StopRequested = false
		return out
	}
	if rm == nil || serviceID == "" || save == nil {
		out.ServerDown = true
		return out
	}
	r := &run{rm: rm, svc: serviceID, save: save, o: opt.withDefaults()}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.o.Total)
	defer cancel()
	if state == StateStartRequested {
		if st, err := r.status(wctx); err == nil && starting(st) {
			return out
		}
	}
	ok, sent := r.ensureStarted(wctx)
	out.Restarts, out.ServerDown = sent, !ok
	return out
}

var _ Remote = (*nitrado.Client)(nil)
