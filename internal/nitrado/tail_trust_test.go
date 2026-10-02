package nitrado

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTailMatchesToleratesGrowthBetweenReads(t *testing.T) {
	full := []byte("abcdef")
	if ok, n := TailMatches(full, 2, []byte("cdef")); !ok || n != 3 {
		t.Fatalf("exact: %v %d", ok, n)
	}
	if ok, n := TailMatches(full, 2, []byte("cdefGH")); !ok || n != 3 {
		t.Fatalf("file grew after the full download: %v %d", ok, n)
	}
	if ok, _ := TailMatches(full, 2, []byte("cdXf")); ok {
		t.Fatal("different bytes must not match")
	}
	if ok, _ := TailMatches(full, 9, []byte("x")); ok {
		t.Fatal("offset past the file")
	}
}

func TestTailTrustLifecycle(t *testing.T) {
	svc := "trust-lifecycle"
	if tailOnly, verify := TailPlan(svc); tailOnly || !verify {
		t.Fatal("a new service verifies")
	}
	TailVerified(svc, true, 0, "SEEK", "test") // no new bytes: does not count
	for i := 0; i < TailTrustAfter; i++ {
		TailVerified(svc, true, 10, "SEEK", "test")
	}
	if !TailTrust()[svc].Trusted {
		t.Fatal("trusted after enough matches")
	}
	tailOnlyReads, verifyReads := 0, 0
	for i := 0; i < TailRecheckEvery; i++ {
		tailOnly, verify := TailPlan(svc)
		if tailOnly {
			tailOnlyReads++
		}
		if verify {
			verifyReads++
		}
	}
	if verifyReads != 1 || tailOnlyReads != TailRecheckEvery-1 {
		t.Fatalf("periodic recheck: tail=%d verify=%d", tailOnlyReads, verifyReads)
	}
	TailFailed(svc)
	if TailTrust()[svc].Trusted {
		t.Fatal("a failed tail read goes back to verifying")
	}
	TailVerified(svc, false, 5, "SEEK", "test")
	if st := TailTrust()[svc]; !st.Disabled {
		t.Fatal("a mismatch disables")
	}
	if tailOnly, verify := TailPlan(svc); tailOnly || verify {
		t.Fatal("disabled services only do full downloads")
	}
}

type memTrustStore struct {
	mu    sync.Mutex
	saved map[string]TailTrustState
	wrote chan string
}

func (m *memTrustStore) LoadTailTrust(context.Context) (map[string]TailTrustState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]TailTrustState{}
	for k, v := range m.saved {
		out[k] = v
	}
	return out, nil
}

func (m *memTrustStore) SaveTailTrust(_ context.Context, id string, s TailTrustState) error {
	m.mu.Lock()
	m.saved[id] = s
	m.mu.Unlock()
	m.wrote <- id
	return nil
}

func TestTailTrustSurvivesRestart(t *testing.T) {
	svc := "trust-restart"
	store := &memTrustStore{saved: map[string]TailTrustState{svc: {Matches: TailTrustAfter, Trusted: true}}, wrote: make(chan string, 16)}
	t.Cleanup(func() {
		tailTrust.mu.Lock()
		tailTrust.store = nil
		delete(tailTrust.services, svc)
		tailTrust.mu.Unlock()
	})
	RestoreTailTrust(context.Background(), store)
	if !TailTrust()[svc].Trusted {
		t.Fatal("saved trust is restored")
	}
	if tailOnly, verify := TailPlan(svc); tailOnly || !verify {
		t.Fatal("the first read after a restart verifies again")
	}
	if tailOnly, _ := TailPlan(svc); !tailOnly {
		t.Fatal("then reads are tail only")
	}
	TailVerified(svc, false, 5, "SEEK", "test")
	select {
	case <-store.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("a mismatch is saved")
	}
	store.mu.Lock()
	got := store.saved[svc]
	store.mu.Unlock()
	if got.Trusted || got.Matches != 0 || got.Disabled {
		t.Fatalf("a disabled service restarts as verifying: %+v", got)
	}
}

type lengthRecorder struct {
	file    []byte
	lengths []int64
}

func (l *lengthRecorder) ReadLogRange(_ context.Context, _ string, _ string, offset, length int64) (*PartialReadResult, bool) {
	l.lengths = append(l.lengths, length)
	if offset+length > int64(len(l.file)) {
		return nil, false
	}
	if length > 4 {
		length = 4 // short reads force several chunks
	}
	data := l.file[offset : offset+length]
	return &PartialReadResult{Data: data, StartOffset: offset, EndOffset: offset + int64(len(data)), Method: "SEEK_SUPPORTED"}, true
}

func TestReadTailNeverAsksPastTheKnownSize(t *testing.T) {
	r := &lengthRecorder{file: []byte("0123456789")}
	data, _, ok := ReadTail(context.Background(), r, "svc", "/x", 3, 10)
	if !ok || string(data) != "3456789" {
		t.Fatalf("tail = %v %q", ok, data)
	}
	if want := []int64{7, 3}; len(r.lengths) != 2 || r.lengths[0] != want[0] || r.lengths[1] != want[1] {
		t.Fatalf("requested lengths = %v, want %v", r.lengths, want)
	}
}
