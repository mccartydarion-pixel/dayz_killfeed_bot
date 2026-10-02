package livesync

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// tailRemote adds partial reads to fakeRemote. corrupt makes every partial read return wrong bytes.
type tailRemote struct {
	*fakeRemote
	corrupt   bool
	tailCalls atomic.Int64
}

func (t *tailRemote) ReadLogFrom(_ context.Context, _ string, p string, offset int64, _ nitrado.DeltaMode) (*nitrado.PartialReadResult, bool) {
	t.tailCalls.Add(1)
	t.mu.Lock()
	c := append([]byte(nil), t.files[p]...)
	t.mu.Unlock()
	if offset > int64(len(c)) {
		return nil, false
	}
	data := c[offset:]
	if len(data) > 64 {
		data = data[:64] // force several chunks
	}
	if t.corrupt && len(data) > 0 {
		data = append([]byte("X"), data[1:]...)
	}
	return &nitrado.PartialReadResult{Data: data, RequestedOffset: offset, StartOffset: offset, EndOffset: offset + int64(len(data)), Method: "SEEK_SUPPORTED"}, true
}

func countdowns(store *memStore) int { return len(store.find(category(CategoryShutdownCountdown))) }

func TestTailReadsReplaceFullDownloadsOnceVerified(t *testing.T) {
	remote, store := &tailRemote{fakeRemote: newFakeRemote()}, newMemStore()
	remote.set(admA, "AdminLog started on 2026-09-24 at 04:15:07\n")
	remote.set(rptA, rptHeader+" 4:15:07.592 Localization not present: STR_DATE_FORMAT_SHORT\n")
	cfg := testConfig()
	cfg.ServiceID = "svc-tail-ok"
	sup := NewSupervisor(cfg, remote, store, nil)
	stop := startSupervisor(t, sup)
	defer stop()
	waitFor(t, "initial RPT read", func() bool { return len(store.find(category(CategoryLocalization))) == 1 })

	// Each appended countdown line must arrive exactly once, whether read in full or as a tail.
	for i := 1; i <= 8; i++ {
		remote.appendTo(rptA, " 5:21:14.888 [Server] :: termination in: 5 and some padding to cross chunk sizes\n")
		want := i
		waitFor(t, "countdown arrives", func() bool { return countdowns(store) == want })
	}
	waitFor(t, "service trusted", func() bool {
		tailTrust.mu.Lock()
		defer tailTrust.mu.Unlock()
		return tailTrust.state("svc-tail-ok").trusted
	})
	before := remote.readCount(rptA)
	for i := 9; i <= 12; i++ {
		remote.appendTo(rptA, " 5:22:00.000 [Server] :: termination in: 4\n")
		want := i
		waitFor(t, "countdown via tail", func() bool { return countdowns(store) == want })
	}
	if full := remote.readCount(rptA) - before; full > 2 {
		t.Fatalf("a trusted service must stop downloading the whole file (allowing a periodic recheck), got %d full reads", full)
	}
	if remote.tailCalls.Load() == 0 {
		t.Fatal("tail reads were never used")
	}
}

func TestBrokenPartialReadsAreDisabledAndNothingIsLost(t *testing.T) {
	remote, store := &tailRemote{fakeRemote: newFakeRemote(), corrupt: true}, newMemStore()
	remote.set(admA, "AdminLog started on 2026-09-24 at 04:15:07\n")
	remote.set(rptA, rptHeader+" 4:15:07.592 Localization not present: STR_DATE_FORMAT_SHORT\n")
	cfg := testConfig()
	cfg.ServiceID = "svc-tail-bad"
	sup := NewSupervisor(cfg, remote, store, nil)
	stop := startSupervisor(t, sup)
	defer stop()
	waitFor(t, "initial RPT read", func() bool { return len(store.find(category(CategoryLocalization))) == 1 })
	for i := 1; i <= 4; i++ {
		remote.appendTo(rptA, " 5:21:14.888 [Server] :: termination in: 5\n")
		want := i
		waitFor(t, "countdown arrives through full reads", func() bool { return countdowns(store) == want })
	}
	tailTrust.mu.Lock()
	st := *tailTrust.state("svc-tail-bad")
	tailTrust.mu.Unlock()
	if !st.disabled || st.trusted {
		t.Fatalf("a mismatching partial read must disable tail reads: %+v", st)
	}
}

func TestTailMatches(t *testing.T) {
	full := []byte("abcdef")
	if !tailMatches(full, 2, []byte("cdef")) || tailMatches(full, 2, []byte("cdeX")) || tailMatches(full, 9, nil) {
		t.Fatal("tailMatches")
	}
}
