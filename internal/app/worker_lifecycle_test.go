package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/servers"
)

// TestDisconnectThenRepairReallyRestartsWorker covers the owner "restart
// worker" / self-heal sequence: DisconnectServer must not return until the
// worker goroutine is gone, so the RepairServer right after it starts a new
// worker instead of seeing the stale one and doing nothing.
func TestDisconnectThenRepairReallyRestartsWorker(t *testing.T) {
	var starts atomic.Int32
	a := &App{}
	a.WorkerManager = servers.NewWorkerManager(func(ctx context.Context, id int64) error {
		starts.Add(1)
		<-ctx.Done()
		time.Sleep(30 * time.Millisecond) // the real worker drains queues after cancellation
		return nil
	})
	t.Cleanup(a.WorkerManager.StopAll)
	if err := a.RepairServer(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	for starts.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	a.DisconnectServer(5)
	if err := a.RepairServer(context.Background(), 5); err != nil {
		t.Fatalf("repair after disconnect: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for starts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if starts.Load() != 2 {
		t.Fatalf("worker was not restarted: %d start(s)", starts.Load())
	}
	if !a.WorkerManager.Running(5) {
		t.Fatal("restarted worker not running")
	}
}

// TestWorkerRegistriesForgetStoppedQueues: the per-worker queue/feed
// registries used by health and shutdown are not append-only - removing a
// queue also drops its health component.
func TestWorkerRegistriesForgetStoppedQueues(t *testing.T) {
	a := &App{HealthRegistry: health.NewRegistry()}
	pq1 := killfeed.NewPersistenceQueueWithServerID(nil, 1, 11, "s1")
	pq2 := killfeed.NewPersistenceQueueWithServerID(nil, 1, 12, "s2")
	a.addPersistQueue(pq1)
	a.addPersistQueue(pq2)
	a.HealthRegistry.Set(health.Component{Name: "persistence_queue_11", State: health.Unhealthy, Critical: true})
	a.HealthRegistry.Set(health.Component{Name: "persistence_queue_12", State: health.Healthy})
	a.removePersistQueue(pq1)
	a.removePersistQueue(nil)
	if got := a.allPersistQueues(); len(got) != 1 || got[0] != pq2 {
		t.Fatalf("expected only pq2 to remain, got %d entries", len(got))
	}
	if _, ok := a.HealthRegistry.Get("persistence_queue_11"); ok {
		t.Fatal("stopped worker's queue still reported by health")
	}
	if _, ok := a.HealthRegistry.Get("persistence_queue_12"); !ok {
		t.Fatal("running worker's queue was dropped from health")
	}
	if a.HealthRegistry.Snapshot().Overall != health.Healthy {
		t.Fatal("dead queue still degrades overall health")
	}

	lq1 := killfeed.NewLocationQueue(nil, 1, 11)
	lq2 := killfeed.NewLocationQueue(nil, 1, 12)
	a.addLocationQueue(lq1)
	a.addLocationQueue(lq2)
	a.removeLocationQueue(lq2)
	if got := a.allLocationQueues(); len(got) != 1 || got[0] != lq1 {
		t.Fatalf("expected only lq1 to remain, got %d entries", len(got))
	}
}
