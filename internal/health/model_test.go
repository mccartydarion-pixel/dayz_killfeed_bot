package health

import (
	"errors"
	"testing"
	"time"
)

func TestRegistrySeverity(t *testing.T) {
	r := NewRegistry()
	r.Set(Component{Name: "leaderboard", State: Degraded})
	if r.Snapshot().Overall != Degraded {
		t.Fatal("noncritical degradation should surface as degraded")
	}
	r.Set(Component{Name: "database", State: Unhealthy, Critical: true})
	if r.Snapshot().Overall != Unhealthy {
		t.Fatal("critical failure should be unhealthy")
	}
}

func TestWorkerRegistryMarksExitedWorkers(t *testing.T) {
	r := NewWorkerRegistry()
	r.Register("adm_worker_1")
	r.Error("adm_worker_1", errors.New("worker failed"))

	workers := r.Snapshot(time.Hour)
	if len(workers) != 1 || workers[0].Running {
		t.Fatalf("expected failed worker to be marked stopped: %+v", workers)
	}
}
