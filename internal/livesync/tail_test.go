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
	growFirst string // appended to the file right before each partial read (the log keeps writing)
	tailCalls atomic.Int64
	pastEnd   atomic.Int64 // reads that asked for bytes past the end of the file
}

// ReadLogRange behaves like Nitrado's seek: asking for bytes past the end of the file fails.
func (t *tailRemote) ReadLogRange(_ context.Context, _ string, p string, offset, length int64) (*nitrado.PartialReadResult, bool) {
	if n := t.tailCalls.Add(1); t.growFirst != "" && n%2 == 1 {
		t.appendTo(p, t.growFirst)
	}
	t.mu.Lock()
	c := append([]byte(nil), t.files[p]...)
	t.mu.Unlock()
	if offset > int64(len(c)) || offset+length > int64(len(c)) {
		t.pastEnd.Add(1)
		return nil, false
	}
	data := c[offset : offset+length]
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
	waitFor(t, "service trusted", func() bool { return nitrado.TailTrust()["svc-tail-ok"].Trusted })
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
	if n := remote.pastEnd.Load(); n != 0 {
		t.Fatalf("%d partial reads asked for bytes past the end of the file", n)
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
	st := nitrado.TailTrust()["svc-tail-bad"]
	if !st.Disabled || st.Trusted {
		t.Fatalf("a mismatching partial read must disable tail reads: %+v", st)
	}
}

// RPT logs are written constantly: the file often grows between the full download and the partial
// read. That is not a mismatch and must not disable tail reads.
func TestGrowthBetweenReadsIsNotAMismatch(t *testing.T) {
	remote, store := &tailRemote{fakeRemote: newFakeRemote(), growFirst: " 5:30:00.000 Localization not present: STR_X\n"}, newMemStore()
	remote.set(admA, "AdminLog started on 2026-09-24 at 04:15:07\n")
	remote.set(rptA, rptHeader+" 4:15:07.592 Localization not present: STR_DATE_FORMAT_SHORT\n")
	cfg := testConfig()
	cfg.ServiceID = "svc-tail-growing"
	sup := NewSupervisor(cfg, remote, store, nil)
	stop := startSupervisor(t, sup)
	defer stop()
	for i := 1; i <= 5; i++ {
		remote.appendTo(rptA, " 5:21:14.888 [Server] :: termination in: 5\n")
		want := i
		waitFor(t, "countdown arrives", func() bool { return countdowns(store) == want })
	}
	waitFor(t, "trusted despite growth", func() bool { return nitrado.TailTrust()["svc-tail-growing"].Trusted })
	if nitrado.TailTrust()["svc-tail-growing"].Disabled {
		t.Fatal("growth between reads must never disable tail reads")
	}
}
