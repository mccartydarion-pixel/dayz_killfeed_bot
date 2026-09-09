package servers

import (
	"context"
	"testing"
	"time"
)

func TestWorkerManagerIsolation(t *testing.T) {
	m := NewWorkerManager(func(ctx context.Context, id int64) error { <-ctx.Done(); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx, 1); err == nil {
		t.Fatal("duplicate worker")
	}
	if err := m.Start(ctx, 2); err != nil {
		t.Fatal(err)
	}
	m.Stop(1)
	m.StopAll()
	if Jitter(time.Second, -time.Millisecond) != time.Second+time.Millisecond {
		t.Fatal("jitter")
	}
}
