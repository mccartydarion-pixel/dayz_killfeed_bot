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

// EngineState is the current phase of the log pipeline.
type EngineState string

const (
	// StateDiscovery is searching the file server for a gameplay log.
	StateDiscovery EngineState = "DISCOVERY"
	// StateLogSelected means a candidate log was chosen and is being confirmed.
	StateLogSelected EngineState = "LOG_SELECTED"
	// StatePolling means the engine polls only the selected log each interval.
	StatePolling EngineState = "POLL_SELECTED_LOG"
)

// LogSource is the minimal log access contract the engine depends on.
// *nitrado.Client satisfies it in production; tests use fakes.
type LogSource interface {
	ListLogs(ctx context.Context, serviceID string) ([]nitrado.LogFile, error)
	ReadLog(ctx context.Context, serviceID string, path string) ([]byte, error)
}

// StatSource is an optional capability for cheap per-file change detection
// without re-running full discovery. *nitrado.Client implements it.
type StatSource interface {
	StatFile(ctx context.Context, serviceID, path string) (*nitrado.LogFile, error)
}

// StateSink receives sanitized engine progress updates for the status endpoint.
type StateSink interface {
	SetLogSource(filename, path string, size int64, modified time.Time)
	SetPollStats(lastPoll, lastLogChange time.Time, interval time.Duration, bytesRead, lines int64)
}

// EngineStats is a point-in-time copy of the engine counters.
type EngineStats struct {
	State           EngineState
	LastPoll        time.Time
	LastLogChange   time.Time
	PollInterval    time.Duration
	BytesProcessed  int64
	LinesDiscovered int64
	APIFailures     int
	LogSourceFound  bool
	SelectedPath    string
}

// Engine orchestrates log discovery, selection, and incremental polling.
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

	state          EngineState
	selected       *nitrado.LogFile
	confirmPending *nitrado.LogFile // awaiting a second sample to detect growth
	consecFailures int
	discoverFails  int // consecutive empty/failed discovery passes, drives backoff
}

// maxConsecFailures forces rediscovery after this many consecutive read/stat
// failures on the selected log (file moved, rotated, or server restarted).
const maxConsecFailures = 3

// discoveryBackoff caps how often failed discovery retries, so we do not hammer
// Nitrado every 2 seconds when no log is found. Sequence: 5s, 10s, 20s, 30s max.
const discoveryBackoffMax = 30 * time.Second

// discoveryBackoff returns the wait before the next discovery attempt.
func discoveryBackoff(fails int) time.Duration {
	if fails < 1 {
		fails = 1
	}
	d := time.Duration(5*fails) * time.Second
	if d > discoveryBackoffMax {
		return discoveryBackoffMax
	}
	return d
}

// NewEngine creates the killfeed engine.
func NewEngine(client LogSource, serviceID string, parser Parser) *Engine {
	interval := envPollInterval("NITRADO_POLL_INTERVAL", 2*time.Second)
	return &Engine{
		parser:       parser,
		client:       client,
		serviceID:    serviceID,
		pollInterval: interval,
		tracker:      NewTracker(serviceID),
		state:        StateDiscovery,
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
	selected := ""
	if e.selected != nil {
		selected = e.selected.Path
	}
	return EngineStats{
		State:           e.state,
		LastPoll:        e.lastPoll,
		LastLogChange:   e.lastLogChange,
		PollInterval:    e.pollInterval,
		BytesProcessed:  e.bytesProcessed,
		LinesDiscovered: e.linesDiscovered,
		APIFailures:     e.apiFailures,
		LogSourceFound:  e.logSourceFound,
		SelectedPath:    selected,
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
	if e.state == "" {
		e.state = StateDiscovery
	}

	slog.Info("component=killfeed", "state", string(e.state))

	// State-aware scheduler: poll the selected log at pollInterval, but back off
	// during failed discovery to avoid hammering Nitrado.
	timer := time.NewTimer(e.pollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			if err := e.PollOnce(ctx); err != nil {
				slog.Warn("component=killfeed", "msg", "poll failed", "err", err.Error())
				e.apiFailures++
			}
			timer.Reset(e.nextInterval())
		}
	}
}

// nextInterval returns how long to wait before the next cycle based on state.
func (e *Engine) nextInterval() time.Duration {
	if e.state == StatePolling {
		return e.pollInterval
	}
	return discoveryBackoff(e.discoverFails)
}

// PollOnce executes one cycle of the state machine. In DISCOVERY it scans for a
// log; once selected it polls only that file each interval.
func (e *Engine) PollOnce(ctx context.Context) error {
	if e == nil || e.client == nil {
		return nil
	}

	switch e.state {
	case StateDiscovery, StateLogSelected:
		return e.discoverOnce(ctx)
	default:
		return e.pollSelected(ctx)
	}
}

// discoverOnce runs directory discovery until a candidate is confirmed as the
// active gameplay log, then transitions to polling only that file.
func (e *Engine) discoverOnce(ctx context.Context) error {
	logs, err := e.client.ListLogs(ctx, e.serviceID)
	if err != nil {
		var reqErr *nitrado.RequestError
		if !errors.As(err, &reqErr) && strings.Contains(err.Error(), "no log files discovered") {
			e.discoverFails++
			if !e.noSourceLogged {
				e.noSourceLogged = true
				slog.Warn("component=killfeed", "state", string(StateDiscovery), "msg", "no gameplay log discovered yet; backing off", "retry_in", discoveryBackoff(e.discoverFails).String())
			}
			e.reportPoll()
			return nil
		}
		e.noSourceLogged = false
		e.discoverFails++
		return err
	}
	e.noSourceLogged = false
	if len(logs) == 0 {
		e.discoverFails++
		e.reportPoll()
		return nil
	}
	// Discovery produced real candidates; clear the backoff counter.
	e.discoverFails = 0

	// If a candidate is pending confirmation, compare its size across the two
	// passes to detect an actively-growing gameplay log.
	if e.confirmPending != nil {
		prev := e.confirmPending
		for _, cur := range logs {
			if cur.Path != prev.Path {
				continue
			}
			changed := cur.Size != prev.Size
			slog.Info("component=killfeed", "msg", "candidate activity check",
				"candidate", cur.Path, "old_size", prev.Size, "new_size", cur.Size, "changed", changed)
			e.selectLog(cur)
			e.confirmPending = nil
			e.reportPoll()
			return nil
		}
		// Pending candidate disappeared; fall through to re-select.
		e.confirmPending = nil
	}

	// First pass: choose the most recently modified candidate. If only one
	// exists, select it immediately; otherwise confirm growth on the next pass.
	candidate := logs[0]
	if len(logs) == 1 {
		e.selectLog(candidate)
		e.reportPoll()
		return nil
	}
	e.confirmPending = &candidate
	e.state = StateLogSelected
	slog.Info("component=killfeed", "state", string(StateLogSelected), "path", candidate.Path, "msg", "candidate pending confirmation")
	e.reportPoll()
	return nil
}

// selectLog locks in the active gameplay log and switches to polling only it.
func (e *Engine) selectLog(lf nitrado.LogFile) {
	candidate := lf
	e.selected = &candidate
	e.confirmPending = nil
	e.state = StatePolling
	e.consecFailures = 0

	if e.tracker == nil {
		e.tracker = NewTracker(e.serviceID)
	}
	if e.tracker.CurrentLogFile != "" && e.tracker.CurrentLogFile != candidate.Path {
		e.tracker.ResetForRotation(candidate.Path)
	}

	if !e.logSourceFound {
		e.logSourceFound = true
		e.lastLogChange = time.Now()
	}
	slog.Info("component=killfeed", "state", string(StatePolling),
		"msg", "log source selected",
		"file", candidate.Name,
		"path", candidate.Path,
		"size", candidate.Size,
		"modified", candidate.Modified.UTC().Format(time.RFC3339),
	)
	if e.sink != nil {
		e.sink.SetLogSource(candidate.Name, candidate.Path, candidate.Size, candidate.Modified)
	}
}

// pollSelected checks only the selected log for changes and reads new bytes.
// No directory discovery happens here.
func (e *Engine) pollSelected(ctx context.Context) error {
	if e.selected == nil || e.selected.Path == "" {
		e.enterDiscovery()
		return nil
	}
	if e.tracker == nil {
		e.tracker = NewTracker(e.serviceID)
	}

	e.lastPoll = time.Now()

	current, err := e.currentMeta(ctx)
	if err != nil {
		return e.handleSelectedFailure(ctx, err)
	}
	e.consecFailures = 0

	if !e.tracker.ShouldReadAgain(current.Path, current.Size, current.Modified) {
		e.reportPoll()
		return nil
	}

	if current.Path != e.tracker.CurrentLogFile && e.tracker.CurrentLogFile != "" {
		slog.Debug("component=killfeed", "msg", "log rotation detected", "previous_file", e.tracker.CurrentLogFile, "file", current.Path)
		e.tracker.ResetForRotation(current.Path)
	}
	if current.Path == e.tracker.CurrentLogFile && current.Size < e.tracker.LastByteOffset {
		slog.Debug("component=killfeed", "msg", "log truncation detected", "file", current.Path, "old_offset", e.tracker.LastByteOffset, "current_size", current.Size)
		e.tracker.ResetForRotation(current.Path)
	}

	oldOffset := e.tracker.LastByteOffset
	oldSize := e.tracker.CurrentSize

	content, err := e.client.ReadLog(ctx, e.serviceID, current.Path)
	if err != nil {
		return e.handleSelectedFailure(ctx, err)
	}
	e.consecFailures = 0

	readOffset := oldOffset
	if readOffset > int64(len(content)) || readOffset < 0 {
		readOffset = 0
		e.tracker.ResetForRotation(current.Path)
	}

	newBytes := content[readOffset:]
	e.tracker.AppendPartialLine(string(newBytes))
	lines := e.tracker.DrainCompleteLines()

	newOffset := int64(len(content)) - int64(len(e.tracker.LineBuffer))
	bytesConsumed := newOffset - oldOffset

	e.bytesProcessed += bytesConsumed
	e.linesDiscovered += int64(len(lines))
	e.lastLogChange = e.lastPoll
	e.tracker.UpdateCheckpoint(e.serviceID, current.Path, int64(len(content)), current.Modified, newOffset)

	slog.Debug("component=killfeed", "state", "POLLING",
		"file", current.Name,
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

// currentMeta returns fresh metadata for the selected log, preferring the cheap
// per-file stat when the client supports it to avoid full directory scans.
func (e *Engine) currentMeta(ctx context.Context) (*nitrado.LogFile, error) {
	if stat, ok := e.client.(StatSource); ok {
		return stat.StatFile(ctx, e.serviceID, e.selected.Path)
	}

	logs, err := e.client.ListLogs(ctx, e.serviceID)
	if err != nil {
		return nil, err
	}
	for _, lf := range logs {
		if lf.Path == e.selected.Path {
			current := lf
			return &current, nil
		}
	}
	return nil, &nitrado.RequestError{Op: "stat file", Kind: nitrado.KindNotFound, Message: "selected log no longer present", StatusCode: 404}
}

// handleSelectedFailure counts consecutive failures on the selected log and
// re-enters discovery only after the file is confirmed repeatedly unreachable.
func (e *Engine) handleSelectedFailure(ctx context.Context, err error) error {
	e.consecFailures++

	var reqErr *nitrado.RequestError
	isNotFound := errors.As(err, &reqErr) && reqErr.Kind == nitrado.KindNotFound

	if isNotFound || e.consecFailures >= maxConsecFailures {
		slog.Warn("component=killfeed", "msg", "selected log lost; re-entering discovery", "path", e.selected.Path, "err", err.Error())
		e.enterDiscovery()
		e.reportPoll()
		return nil
	}

	slog.Debug("component=killfeed", "state", "POLLING", "msg", "transient read/stat failure; will retry", "path", e.selected.Path, "failures", e.consecFailures, "err", err.Error())
	e.reportPoll()
	return nil
}

// enterDiscovery resets selection so the next poll re-runs discovery.
func (e *Engine) enterDiscovery() {
	e.state = StateDiscovery
	e.selected = nil
	e.confirmPending = nil
	e.consecFailures = 0
	slog.Info("component=killfeed", "state", string(StateDiscovery))
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
