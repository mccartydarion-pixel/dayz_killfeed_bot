package servers

import (
	"context"
	"fmt"
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
	go func() {
		if m.factory != nil {
			_ = m.factory(ctx, serverID)
		}
		m.mu.Lock()
		delete(m.workers, serverID)
		m.mu.Unlock()
	}()
	return nil
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
