package killfeed

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// LogSource is the minimal log access contract the engine depends on.
// *nitrado.Client satisfies it in production; tests use fakes.
type LogSource interface {
	ListLogs(ctx context.Context, serviceID string) ([]nitrado.LogFile, error)
	ReadLog(ctx context.Context, serviceID string, path string) ([]byte, error)
}

// StateSink receives sanitized engine progress updates for the status endpoint.
type StateSink interface {
	SetLogSource(filename, path string, size int64, modified time.Time)
	SetPollStats(lastPoll, lastLogChange time.Time, interval time.Duration, bytesRead, lines int64)
}

// EngineStats is a point-in-time copy of the engine counters.
type EngineStats struct {
	LastPoll        time.Time
	LastLogChange   time.Time
	PollInterval    time.Duration
	BytesProcessed  int64
	LinesDiscovered int64
	APIFailures     int
	LogSourceFound  bool
}

// Engine is the future orchestrator for the log ingestion pipeline.
type Engine struct {
	parser          Parser
	tracker         *Tracker
	client          LogSource
	sink            StateSink
	serviceID       string
	pollInterval    time.Duration
	lastPoll        time.Time
	lastLogChange   time.Time
	bytesProcessed  int64
	linesDiscovered int64
	apiFailures     int
	logSourceFound  bool
	noSourceLogged  bool
}

// NewEngine creates the killfeed engine skeleton.
func NewEngine(client LogSource, serviceID string, parser Parser) *Engine {
	interval := envPollInterval("NITRADO_POLL_INTERVAL", 2*time.Second)
	return &Engine{
		parser:       parser,
		client:       client,
		serviceID:    serviceID,
		pollInterval: interval,
		tracker:      NewTracker(serviceID),
	}
}

// SetStateSink attaches a sanitized status reporter.
func (e *Engine) SetStateSink(sink StateSink) {
	if e == nil {
		return
	}
	e.sink = sink
}

// Stats returns a copy of the current engine counters.
func (e *Engine) Stats() EngineStats {
	if e == nil {
		return EngineStats{}
	}
	return EngineStats{
		LastPoll:        e.lastPoll,
		LastLogChange:   e.lastLogChange,
		PollInterval:    e.pollInterval,
		BytesProcessed:  e.bytesProcessed,
		LinesDiscovered: e.linesDiscovered,
		APIFailures:     e.apiFailures,
		LogSourceFound:  e.logSourceFound,
	}
}

// Start begins the polling cycle with a safe time-based tick.
// Recoverable failures are logged and retried; the engine never crashes on them.
func (e *Engine) Start(ctx context.Context) error {
	if e == nil {
		return nil
	}
	if e.pollInterval < time.Second {
		e.pollInterval = time.Second
	}
	if e.tracker == nil {
		e.tracker = NewTracker(e.serviceID)
	}

	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := e.PollOnce(ctx); err != nil {
				// Recoverable failures are logged once-per-change and retried; the
				// engine must never crash on them (e.g. no log source found yet).
				slog.Warn("component=killfeed", "msg", "poll failed", "err", err.Error())
				e.apiFailures++
			}
		}
	}
}

// PollOnce executes a single incremental discovery/read cycle. A missing log
// source is recoverable (returns nil) so the engine keeps polling rather than
// crash-looping; genuine transport errors are returned for the caller to count.
func (e *Engine) PollOnce(ctx context.Context) error {
	if e == nil || e.client == nil {
		return nil
	}

	logs, err := e.client.ListLogs(ctx, e.serviceID)
	if err != nil {
		// "No log files discovered" is a recoverable empty state, not a crash.
		var reqErr *nitrado.RequestError
		if !errors.As(err, &reqErr) && strings.Contains(err.Error(), "no log files discovered") {
			if !e.noSourceLogged {
				e.noSourceLogged = true
				slog.Info("component=killfeed", "msg", "no gameplay log source discovered yet; will keep polling")
			}
			e.reportPoll()
			return nil
		}
		e.noSourceLogged = false
		return err
	}
	e.noSourceLogged = false
	if len(logs) == 0 {
		e.reportPoll()
		return nil
	}

	candidate := logs[0]
	if candidate.Path == "" {
		e.reportPoll()
		return nil
	}
	if e.tracker == nil {
		e.tracker = NewTracker(e.serviceID)
	}

	e.lastPoll = time.Now()
	if !e.logSourceFound {
		e.logSourceFound = true
		slog.Info("component=killfeed", "msg", "log source identified",
			"file", candidate.Name,
			"path", candidate.Path,
			"size", candidate.Size,
			"modified", candidate.Modified.UTC().Format(time.RFC3339),
		)
	}
	if e.sink != nil {
		e.sink.SetLogSource(candidate.Name, candidate.Path, candidate.Size, candidate.Modified)
	}

	if !e.tracker.ShouldReadAgain(candidate.Path, candidate.Size, candidate.Modified) {
		e.reportPoll()
		return nil
	}

	// Rotation: a different file became the newest candidate.
	if e.tracker.CurrentLogFile != "" && e.tracker.CurrentLogFile != candidate.Path {
		slog.Debug("component=killfeed", "msg", "log rotation detected",
			"previous_file", e.tracker.CurrentLogFile,
			"file", candidate.Path,
		)
		e.tracker.ResetForRotation(candidate.Path)
	}
	// Truncation: the same file shrank below our offset.
	if candidate.Path == e.tracker.CurrentLogFile && candidate.Size < e.tracker.LastByteOffset {
		slog.Debug("component=killfeed", "msg", "log truncation detected",
			"file", candidate.Path,
			"old_offset", e.tracker.LastByteOffset,
			"current_size", candidate.Size,
		)
		e.tracker.ResetForRotation(candidate.Path)
	}

	oldOffset := e.tracker.LastByteOffset
	oldSize := e.tracker.CurrentSize

	content, err := e.client.ReadLog(ctx, e.serviceID, candidate.Path)
	if err != nil {
		return err
	}

	readOffset := oldOffset
	if readOffset > int64(len(content)) || readOffset < 0 {
		// File changed between stat and read; restart from the beginning safely.
		readOffset = 0
		e.tracker.ResetForRotation(candidate.Path)
	}

	newBytes := content[readOffset:]
	e.tracker.AppendPartialLine(string(newBytes))
	lines := e.tracker.DrainCompleteLines()

	// Only complete lines advance the durable offset; the trailing partial
	// line stays buffered until its newline arrives in a later poll.
	newOffset := int64(len(content)) - int64(len(e.tracker.LineBuffer))
	bytesConsumed := newOffset - oldOffset

	e.bytesProcessed += bytesConsumed
	e.linesDiscovered += int64(len(lines))
	e.lastLogChange = e.lastPoll
	e.tracker.UpdateCheckpoint(e.serviceID, candidate.Path, int64(len(content)), candidate.Modified, newOffset)

	slog.Debug("component=killfeed",
		"file", candidate.Name,
		"old_size", oldSize,
		"new_size", int64(len(content)),
		"old_offset", oldOffset,
		"new_offset", newOffset,
		"bytes", bytesConsumed,
		"lines", len(lines),
	)

	e.reportPoll()
	return nil
}

func (e *Engine) reportPoll() {
	if e == nil || e.sink == nil {
		return
	}
	e.sink.SetPollStats(e.lastPoll, e.lastLogChange, e.pollInterval, e.bytesProcessed, e.linesDiscovered)
}

func envPollInterval(name string, fallback time.Duration) time.Duration {
	value := stringsFromEnv(name)
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	if d < time.Second {
		return time.Second
	}
	return d
}

func stringsFromEnv(name string) string {
	return os.Getenv(name)
}
