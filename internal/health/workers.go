package health

import (
	"sync"
	"time"
)

type WorkerStatus struct {
	Name                       string
	StartedAt, LastHeartbeatAt time.Time
	LastSuccessAt, LastErrorAt *time.Time
	Running                    bool
	State                      State
	LastError                  string
}
type WorkerRegistry struct {
	mu      sync.RWMutex
	workers map[string]WorkerStatus
}

func NewWorkerRegistry() *WorkerRegistry {
	return &WorkerRegistry{workers: make(map[string]WorkerStatus)}
}
func (r *WorkerRegistry) Register(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if _, ok := r.workers[name]; !ok {
		r.workers[name] = WorkerStatus{Name: name, StartedAt: time.Now(), LastHeartbeatAt: time.Now(), Running: true, State: Healthy}
	}
	r.mu.Unlock()
}
func (r *WorkerRegistry) Heartbeat(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	w := r.workers[name]
	w.Name = name
	w.LastHeartbeatAt = time.Now()
	w.Running = true
	w.State = Healthy
	r.workers[name] = w
	r.mu.Unlock()
}
func (r *WorkerRegistry) Error(name string, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	w := r.workers[name]
	now := time.Now()
	w.Name = name
	w.LastErrorAt = &now
	w.LastError = err.Error()
	w.State = Degraded
	r.workers[name] = w
	r.mu.Unlock()
}
func (r *WorkerRegistry) Snapshot(stale time.Duration) []WorkerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	out := make([]WorkerStatus, 0, len(r.workers))
	for name, w := range r.workers {
		if now.Sub(w.LastHeartbeatAt) > stale {
			w.State = Unhealthy
			w.Running = false
		}
		out = append(out, w)
		r.workers[name] = w
	}
	return out
}
