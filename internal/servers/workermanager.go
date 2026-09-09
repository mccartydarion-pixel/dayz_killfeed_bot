package servers

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

type WorkerFactory func(context.Context, int64) error
type WorkerManager struct {
	mu      sync.Mutex
	workers map[int64]context.CancelFunc
	factory WorkerFactory
}

func NewWorkerManager(factory WorkerFactory) *WorkerManager {
	return &WorkerManager{workers: make(map[int64]context.CancelFunc), factory: factory}
}
func (m *WorkerManager) Start(parent context.Context, serverID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.workers[serverID]; ok {
		return fmt.Errorf("worker %d already running", serverID)
	}
	ctx, cancel := context.WithCancel(parent)
	m.workers[serverID] = cancel
	go m.run(ctx, serverID)
	return nil
}

// run executes the worker factory inside a panic-recovery boundary so a fault on
// one server (panic, or any other failure) can never take down other workers or
// the process. This is what makes WorkerManager the safe, sole owner of workers.
func (m *WorkerManager) run(ctx context.Context, serverID int64) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=servers", "msg", "worker panic recovered; other servers unaffected", "server_id", serverID, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
		m.mu.Lock()
		delete(m.workers, serverID)
		m.mu.Unlock()
	}()
	if m.factory == nil {
		return
	}
	if err := m.factory(ctx, serverID); err != nil {
		slog.Error("component=servers", "msg", "worker exited with error", "server_id", serverID, "err", err.Error())
	}
}
func (m *WorkerManager) Stop(serverID int64) {
	m.mu.Lock()
	if cancel := m.workers[serverID]; cancel != nil {
		cancel()
	}
	m.mu.Unlock()
}
func (m *WorkerManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cancel := range m.workers {
		cancel()
	}
	m.workers = make(map[int64]context.CancelFunc)
}
func (m *WorkerManager) Running(serverID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.workers[serverID]
	return ok
}
func Jitter(base time.Duration, offset time.Duration) time.Duration {
	if offset < 0 {
		offset = -offset
	}
	return base + offset
}
