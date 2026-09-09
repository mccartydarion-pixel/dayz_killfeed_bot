package health

import (
	"sync"
	"time"
)

type State string

const (
	Healthy   State = "HEALTHY"
	Degraded  State = "DEGRADED"
	Unhealthy State = "UNHEALTHY"
	Unknown   State = "UNKNOWN"
)

type Component struct {
	Name                         string
	State                        State
	Message                      string
	LastSuccessAt, LastFailureAt *time.Time
	ConsecutiveFailures          int
	Critical                     bool
}
type Snapshot struct {
	Overall     State
	Components  []Component
	GeneratedAt time.Time
}
type Registry struct {
	mu         sync.RWMutex
	components map[string]Component
	started    time.Time
}

func NewRegistry() *Registry {
	return &Registry{components: make(map[string]Component), started: time.Now()}
}
func (r *Registry) Set(c Component) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.components[c.Name] = c
	r.mu.Unlock()
}
func (r *Registry) Get(name string) (Component, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.components[name]
	return c, ok
}
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := Snapshot{Overall: Healthy, GeneratedAt: time.Now()}
	for _, c := range r.components {
		out.Components = append(out.Components, c)
		if c.State == Unhealthy && c.Critical {
			out.Overall = Unhealthy
		} else if c.State == Degraded && out.Overall == Healthy {
			out.Overall = Degraded
		}
	}
	return out
}
func (r *Registry) Uptime() time.Duration {
	if r == nil {
		return 0
	}
	return time.Since(r.started)
}
