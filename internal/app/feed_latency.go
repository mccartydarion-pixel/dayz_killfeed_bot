package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// serverClockRefreshEvery is how often a worker re-reads its server's learned UTC offset.
const serverClockRefreshEvery = 5 * time.Minute

// serverClock is one game server's UTC offset as Live Sync learned it from restart.log
// (live_sync_server_clock). The ADM engine reads it on its own goroutine to express a log line's
// server-local time in UTC for the latency statistics (docs/PERFORMANCE.md section 19); the
// database is only ever asked from refresh's goroutine, never from the engine's.
type serverClock struct {
	minutes atomic.Int64
	known   atomic.Bool
}

func (c *serverClock) offset() (int, bool) {
	if c == nil || !c.known.Load() {
		return 0, false
	}
	return int(c.minutes.Load()), true
}

func (c *serverClock) set(minutes *int) {
	if minutes == nil {
		return // not learned yet: keep whatever was known, never invent one
	}
	c.minutes.Store(int64(*minutes))
	c.known.Store(true)
}

// serverUTCOffsetSource returns the offset reader for row's worker and keeps it fresh until ctx
// ends. Without a database it always reports unknown.
func (a *App) serverUTCOffsetSource(ctx context.Context, row repository.GameServer) func() (int, bool) {
	clock := &serverClock{}
	if a.DB == nil || a.DB.Pool == nil {
		return clock.offset
	}
	repo := repository.NewCaseEvidenceRepository(a.DB.Pool)
	refresh := func() {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		minutes, err := repo.CaseServerUTCOffset(readCtx, row.GuildID, row.ID)
		if err != nil {
			if ctx.Err() == nil {
				slog.Debug("component=killfeed", "event", "server_clock_read_failed", "server_id", row.ID, "err", err.Error())
			}
			return
		}
		clock.set(minutes)
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("component=servers", "msg", "server clock refresh panic recovered", "server_id", row.ID)
			}
		}()
		refresh()
		ticker := time.NewTicker(serverClockRefreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
	return clock.offset
}
