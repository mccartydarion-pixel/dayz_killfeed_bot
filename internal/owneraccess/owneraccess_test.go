package owneraccess

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu    sync.Mutex
	orgs  []int64
	insts []int64
	err   error
	loads int
	asked []string
}

func (m *memStore) PlatformOwnerScope(_ context.Context, ids []string) ([]int64, []int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loads++
	m.asked = append([]string(nil), ids...)
	return m.orgs, m.insts, m.err
}

func (m *memStore) set(orgs, insts []int64, err error) {
	m.mu.Lock()
	m.orgs, m.insts, m.err = orgs, insts, err
	m.mu.Unlock()
}

func (m *memStore) loadCount() int { m.mu.Lock(); defer m.mu.Unlock(); return m.loads }

func TestAnswersFromTheSnapshot(t *testing.T) {
	store := &memStore{orgs: []int64{3}, insts: []int64{30, 31}}
	r := New(store, []string{"900000000000000001"}, time.Hour)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.Organization(3) || r.Organization(4) || r.Organization(0) || r.Organization(-3) {
		t.Fatal("organization answers are wrong")
	}
	if !r.Installation(30) || !r.Installation(31) || r.Installation(32) || r.Installation(0) {
		t.Fatal("installation answers are wrong")
	}
	if len(store.asked) != 1 || store.asked[0] != "900000000000000001" {
		t.Fatalf("the store was asked about %v", store.asked)
	}
	for i := 0; i < 100; i++ {
		r.Organization(3)
		r.Installation(30)
	}
	if store.loadCount() != 1 {
		t.Fatalf("a fresh snapshot must not be reloaded, loads=%d", store.loadCount())
	}
}

func TestNoOwnersOrNoStoreMeansNobody(t *testing.T) {
	store := &memStore{orgs: []int64{3}, insts: []int64{30}}
	for _, r := range []*Resolver{nil, New(store, nil, time.Hour), New(nil, []string{"900000000000000001"}, time.Hour)} {
		if err := r.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r.Organization(3) || r.Installation(30) {
			t.Fatal("nobody may have owner access")
		}
	}
	if store.loadCount() != 0 {
		t.Fatal("the store must not be queried when there are no platform owners")
	}
}

func TestStaleSnapshotReloadsInBackground(t *testing.T) {
	store := &memStore{}
	r := New(store, []string{"900000000000000001"}, 10*time.Millisecond)
	_ = r.Refresh(context.Background())
	store.set([]int64{5}, []int64{50}, nil)
	time.Sleep(20 * time.Millisecond)
	if r.Organization(5) {
		t.Fatal("the first stale read answers from the old snapshot without blocking")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !r.Organization(5) || !r.Installation(50) {
		if time.Now().After(deadline) {
			t.Fatal("background reload never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Ownership taken away is seen after the next reload too.
	store.set(nil, nil, nil)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Organization(5) || r.Installation(50) {
		t.Fatal("a reload must drop organizations that are no longer owned")
	}
}

func TestFailedReloadKeepsThePreviousSnapshot(t *testing.T) {
	store := &memStore{orgs: []int64{3}, insts: []int64{30}}
	r := New(store, []string{"900000000000000001"}, time.Hour)
	_ = r.Refresh(context.Background())
	store.set(nil, nil, errors.New("database is down"))
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected the error")
	}
	if !r.Organization(3) || !r.Installation(30) {
		t.Fatal("a failed reload must keep the previous answer")
	}
}
