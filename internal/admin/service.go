package admin

import (
	"context"
	"time"

	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/server"
)

type Service struct {
	state    *server.State
	registry *health.Registry
	workers  *health.WorkerRegistry
	started  time.Time
}

func NewService(state *server.State, registry *health.Registry) *Service {
	return &Service{state: state, registry: registry, started: time.Now()}
}
func (s *Service) SetWorkers(w *health.WorkerRegistry) { s.workers = w }
func (s *Service) Status(ctx context.Context) map[string]any {
	out := map[string]any{"uptime": time.Since(s.started).String()}
	if s.state != nil {
		out["runtime"] = s.state.Snapshot()
	}
	if s.registry != nil {
		out["health"] = s.registry.Snapshot()
	}
	if s.workers != nil {
		out["workers"] = s.workers.Snapshot(30 * time.Second)
	}
	return out
}
func (s *Service) Diagnostics(ctx context.Context) map[string]any { return s.Status(ctx) }
