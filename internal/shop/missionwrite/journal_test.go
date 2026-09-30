package missionwrite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type jpaths struct{ journal, anchor string }

func newPaths(t *testing.T) jpaths {
	d := t.TempDir()
	return jpaths{filepath.Join(d, "cfg", "journal.jsonl"), filepath.Join(d, "home", "anchor.json")}
}

func TestJournalPathsMustBeAbsoluteAndSeparate(t *testing.T) {
	p := newPaths(t)
	for _, c := range [][2]string{
		{"journal.jsonl", p.anchor},
		{p.journal, "anchor.json"},
		{p.journal, filepath.Join(filepath.Dir(p.journal), "anchor.json")},
	} {
		if _, err := InitJournal(c[0], c[1]); !errors.Is(err, ErrJournalPath) {
			t.Errorf("init %v: %v", c, err)
		}
		if _, err := OpenJournal(c[0], c[1]); !errors.Is(err, ErrJournalPath) {
			t.Errorf("open %v: %v", c, err)
		}
	}
}

// Repeated invocations and different working directories see the same journal: the path is
// absolute, and reopening (a new process) verifies the same chain.
func TestJournalSurvivesRestartAndWorkingDirectory(t *testing.T) {
	p := newPaths(t)
	j, err := InitJournal(p.journal, p.anchor)
	if err != nil {
		t.Fatal(err)
	}
	_ = j.Append(Entry{PlanID: "mw-1", Service: "s", Path: "x.json", Status: "STARTED"})
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	for i := 0; i < 3; i++ {
		_ = os.Chdir(t.TempDir())
		j2, err := OpenJournal(p.journal, p.anchor)
		if err != nil {
			t.Fatal(err)
		}
		if used, _ := j2.Used("mw-1"); !used {
			t.Fatal("reopened journal lost the authorization")
		}
		if open, _ := j2.Outstanding("s", "x.json"); !open {
			t.Fatal("reopened journal lost the outstanding write")
		}
	}
	if _, err := InitJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalExists) {
		t.Fatalf("re-init must refuse: %v", err)
	}
}

func TestMissingJournalCannotResetAuthorization(t *testing.T) {
	p := newPaths(t)
	j, _ := InitJournal(p.journal, p.anchor)
	_ = j.Append(Entry{PlanID: "mw-1", Service: "s", Path: "x.json", Status: StatusUncertain})
	if err := os.Remove(p.journal); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalAnchor) {
		t.Fatalf("deleted journal: %v", err)
	}
	if _, err := InitJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalExists) {
		t.Fatalf("a new journal over a live anchor: %v", err)
	}
	// No journal and no anchor at all: nothing silently created; init is explicit.
	q := newPaths(t)
	if _, err := OpenJournal(q.journal, q.anchor); !errors.Is(err, ErrJournalMissing) {
		t.Fatalf("fresh: %v", err)
	}
	if _, err := os.Stat(q.journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("OpenJournal must not create a journal")
	}
}

func TestReplacedTruncatedEditedOrTornJournalIsRefused(t *testing.T) {
	setup := func(t *testing.T) (jpaths, []byte) {
		p := newPaths(t)
		j, _ := InitJournal(p.journal, p.anchor)
		_ = j.Append(Entry{PlanID: "mw-1", Service: "s", Path: "x.json", Status: "STARTED"})
		_ = j.Append(Entry{PlanID: "mw-1", Service: "s", Path: "x.json", Status: StatusUncertain})
		raw, _ := os.ReadFile(p.journal)
		return p, raw
	}
	t.Run("replaced", func(t *testing.T) {
		p, _ := setup(t)
		other := newPaths(t)
		_, _ = InitJournal(other.journal, other.anchor)
		fresh, _ := os.ReadFile(other.journal)
		_ = os.WriteFile(p.journal, fresh, 0o600)
		if _, err := OpenJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalAnchor) {
			t.Fatal(err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		p, raw := setup(t)
		lines := bytes.SplitAfter(raw, []byte("\n"))
		_ = os.WriteFile(p.journal, bytes.Join(lines[:2], nil), 0o600) // drop the UNCERTAIN line
		if _, err := OpenJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalAnchor) {
			t.Fatal(err)
		}
	})
	t.Run("edited middle", func(t *testing.T) {
		p, raw := setup(t)
		_ = os.WriteFile(p.journal, bytes.Replace(raw, []byte(`"STARTED"`), []byte(`"RESOLVED"`), 1), 0o600)
		if _, err := OpenJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalCorrupt) {
			t.Fatal(err)
		}
	})
	t.Run("edited last", func(t *testing.T) {
		p, raw := setup(t)
		_ = os.WriteFile(p.journal, bytes.Replace(raw, []byte(`"UNCERTAIN"`), []byte(`"NOT_WRITTEN"`), 1), 0o600)
		if _, err := OpenJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalAnchor) {
			t.Fatal(err)
		}
	})
	t.Run("torn line", func(t *testing.T) {
		p, raw := setup(t)
		_ = os.WriteFile(p.journal, append(raw, []byte(`{"seq":3,"prev":"ab`)...), 0o600)
		if _, err := OpenJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalCorrupt) {
			t.Fatal(err)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		p, _ := setup(t)
		_ = os.WriteFile(p.journal, []byte("not json\n"), 0o600)
		if _, err := OpenJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalCorrupt) {
			t.Fatal(err)
		}
	})
	t.Run("anchor deleted", func(t *testing.T) {
		p, _ := setup(t)
		_ = os.Remove(p.anchor)
		if _, err := OpenJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalAnchor) {
			t.Fatal(err)
		}
		// Explicit adoption re-anchors but changes nothing: the write stays outstanding.
		j, err := AdoptJournal(p.journal, p.anchor)
		if err != nil {
			t.Fatal(err)
		}
		if open, _ := j.Outstanding("s", "x.json"); !open {
			t.Fatal("adoption must not clear an outstanding write")
		}
		if _, err := AdoptJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalExists) {
			t.Fatal("adopt over an existing anchor")
		}
	})
}

// A crash between the journal fsync and the anchor update leaves the anchor exactly one entry
// behind: accepted and rolled forward. Two behind is a truncation or tampering: refused.
func TestAnchorOneBehindIsRecoveredTwoBehindRefused(t *testing.T) {
	p := newPaths(t)
	j, _ := InitJournal(p.journal, p.anchor)
	a0, _ := os.ReadFile(p.anchor)
	_ = j.Append(Entry{PlanID: "mw-1", Service: "s", Path: "x.json", Status: "STARTED"})
	_ = os.WriteFile(p.anchor, a0, 0o600)
	j2, err := OpenJournal(p.journal, p.anchor)
	if err != nil {
		t.Fatalf("one behind: %v", err)
	}
	if used, _ := j2.Used("mw-1"); !used {
		t.Fatal("recovered entry lost")
	}
	_ = j2.Append(Entry{PlanID: "mw-1", Service: "s", Path: "x.json", Status: StatusUncertain})
	_ = j2.Append(Entry{PlanID: "mw-1", Service: "s", Path: "x.json", Status: "RESOLVED", Detail: "n"})
	_ = os.WriteFile(p.anchor, a0, 0o600)
	if _, err := OpenJournal(p.journal, p.anchor); !errors.Is(err, ErrJournalAnchor) {
		t.Fatalf("three behind: %v", err)
	}
}

func TestConcurrentRunsAreSerializedAndStaleLocksKept(t *testing.T) {
	s := newStandIn(t)
	r, j := gateA(t), journal(t)
	p := plan(t, s, r)
	unlock, err := j.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), s.client(), r, p.ID, j); !errors.Is(err, ErrJournalLocked) {
		t.Fatalf("second run while locked: %v", err)
	}
	if len(s.uploadCalls)+len(s.mkdirCalls) != 0 {
		t.Fatal("a locked journal must stop before any write")
	}
	if _, err := j.Lock(); !errors.Is(err, ErrJournalLocked) {
		t.Fatal("a held lock is never taken over")
	}
	unlock()
	if o, err := Execute(context.Background(), s.client(), r, p.ID, j); err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("after unlock: %v %+v", err, o)
	}
	if _, err := os.Stat(j.path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Execute releases its lock")
	}
}

// After an UNCERTAIN outcome the authorization is dead - across a restart and even after the
// owner's resolution - and the destination stays blocked until resolved.
func TestUncertainAuthorizationNeverReusableAfterRestart(t *testing.T) {
	s := newStandIn(t)
	s.dirs[missionDir+"/champion"] = true // folder present, so the state after the run is identical
	s.claimNoStore = true                 // server says 2xx, file absent: the same plan ID is recomputed
	r, j := gateA(t), journal(t)
	p := plan(t, s, r)
	o, err := Execute(context.Background(), s.client(), r, p.ID, j)
	if err != nil || o.Status != StatusUncertain {
		t.Fatalf("%v %+v", err, o)
	}
	s.claimNoStore = false
	j2, err := OpenJournal(j.path, j.anchor) // "restarted" tool
	if err != nil {
		t.Fatal(err)
	}
	if p2 := plan(t, s, r); p2.ID != p.ID {
		t.Fatal("the unchanged state must give the same plan ID (the case this test covers)")
	}
	if _, err := Execute(context.Background(), s.client(), r, p.ID, j2); !errors.Is(err, ErrUncertainOutstanding) {
		t.Fatalf("after restart: %v", err)
	}
	if err := j2.Resolve(p.ID, testService, r.Path, "owner inspected: file absent"); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), s.client(), r, p.ID, j2); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("after resolution the old authorization stays used: %v", err)
	}
	if len(s.transfers) != 1 {
		t.Fatalf("exactly one transfer ever: %d", len(s.transfers))
	}
}

func TestFailureSideEffectsAreReported(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		setup    func(s *standIn)
		status   string
		dir      string
		tokenReq int
	}{
		{"mkdir refused, folder absent", func(s *standIn) { s.mkdirStatus = 403 }, StatusNotWritten, DirNotCreated, 0},
		{"mkdir error but folder created", func(s *standIn) { s.mkdirThenFail = true }, StatusNotWritten, DirCreatedDespiteErr, 0},
		{"mkdir ok, token refused", func(s *standIn) { s.tokenStatus = 403 }, StatusNotWritten, DirCreated, 1},
		{"token refused but a file appeared", func(s *standIn) {
			s.tokenStatus = 500
			s.tokenSideEffect = func(s *standIn) { s.files[champFile] = []byte{} }
		}, StatusUncertain, DirCreated, 1},
		{"transfer refused", func(s *standIn) { s.transferStatus = 507 }, StatusNotWritten, DirCreated, 1},
	}
	for _, c := range cases {
		s := newStandIn(t)
		c.setup(s)
		r, j := gateA(t), journal(t)
		o, err := Execute(ctx, s.client(), r, plan(t, s, r).ID, j)
		if err != nil || o.Status != c.status || o.Directory != c.dir || len(s.uploadCalls) != c.tokenReq {
			t.Errorf("%s: %v status=%s dir=%q tokens=%d checks=%+v", c.name, err, o.Status, o.Directory, len(s.uploadCalls), o.Checks)
			continue
		}
		es, _ := j.Entries()
		last := es[len(es)-1]
		if last.Status != c.status || !strings.Contains(last.Detail, "directory: "+c.dir) {
			t.Errorf("%s: journal %+v", c.name, last)
		}
	}
}
