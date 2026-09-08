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
	SetDiscovery(state string, dirsVisited, filesDiscovered int)
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
	discoverFails  int       // consecutive empty/failed discovery passes, drives backoff
	lastRescan     time.Time // last time we checked for a newer ADM file
	sampleCaptured bool      // whether we've logged the gameplay sample for the selected log
}

// rescanInterval is how often, while polling a selected log, we do a lightweight
// directory check for a newer ADM file (DayZ creates a new timestamped ADM on
// restart). This is deliberately far slower than the 2s selected-file poll.
const rescanInterval = 45 * time.Second

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

	// Phase 2.8: select the best candidate immediately. Ranking (ListLogs already
	// sorts newest-modified first): prefer a non-trivial file (has content) over a
	// brand-new empty ADM, but do not require active growth — an inactive ADM with
	// real gameplay events is a valid source. ListLogs returns newest-first, so the
	// first candidate with meaningful size wins; fall back to logs[0] if all are tiny.
	candidate := selectBestCandidate(logs)
	e.selectLog(candidate)
	e.reportPoll()
	return nil
}

// selectBestCandidate picks the newest ADM that actually has content; if every
// candidate is empty (e.g. a freshly-created ADM after restart), it falls back to
// selectBestCandidate picks the newest ADM that actually has content. A fresh ADM
// created after a restart is just a small header, while an ADM with real gameplay
// events is much larger. We prefer the newest candidate whose size is a meaningful
// fraction of the largest ADM seen; if all are tiny, we track the newest one that
// will grow. Input is newest-modified-first.
func selectBestCandidate(logs []nitrado.LogFile) nitrado.LogFile {
	if len(logs) == 0 {
		return nitrado.LogFile{}
	}

	// Find the largest ADM to establish what "has content" means for this server.
	var maxSize int64
	for _, lf := range logs {
		if lf.Size > maxSize {
			maxSize = lf.Size
		}
	}

	// A candidate counts as having content if it's at least a fraction of the
	// largest ADM. This makes a fresh ~472-byte header lose to a 100KB log but
	// still win when nothing bigger exists.
	const contentFraction = 0.10 // 10% of the largest ADM
	threshold := int64(float64(maxSize) * contentFraction)
	if threshold < 200 {
		threshold = 200 // absolute floor so a totally fresh set still selects something
	}

	for _, lf := range logs {
		if lf.Size >= threshold {
			return lf
		}
	}
	return logs[0]
}

// selectLog locks in the active gameplay log and switches to polling only it.
func (e *Engine) selectLog(lf nitrado.LogFile) {
	candidate := lf
	e.selected = &candidate
	e.confirmPending = nil
	e.state = StatePolling
	e.consecFailures = 0
	e.sampleCaptured = false

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
		e.sink.SetDiscovery(string(StatePolling), 0, 0)
	}
	e.lastRescan = time.Now()
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

	// Periodically check for a newer ADM file (post-restart) on a slow cadence,
	// separate from the per-2s selected-file poll.
	if time.Since(e.lastRescan) >= rescanInterval {
		e.lastRescan = time.Now()
		e.checkForNewerLog(ctx)
		if e.state != StatePolling {
			e.reportPoll()
			return nil
		}
	}

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

	// Capture a representative real gameplay sample once per selected log for
	// parser design. Only IPs are redacted; gameplay syntax is preserved verbatim.
	if !e.sampleCaptured && len(content) > 0 {
		sample := SelectSampleLines(string(content), 45)
		if len(sample) > 0 {
			e.sampleCaptured = true
			slog.Info("component=killfeed", "msg", "REAL DAYZ ADM SAMPLE", "file", current.Name, "path", current.Path, "lines", len(sample))
			for _, line := range sample {
				slog.Info("component=killfeed_adm_sample", "line", line)
			}
		}
	}

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
	if e.sink != nil {
		e.sink.SetDiscovery(string(StateDiscovery), 0, 0)
	}
}

// checkForNewerLog does a lightweight scan of the selected file's own directory
// for a newer ADM (DayZ writes a new timestamped ADM after restart). If a strictly
// newer ADM exists, it switches selection. Runs on rescanInterval; never rescans
// the whole ftproot tree.
func (e *Engine) checkForNewerLog(ctx context.Context) {
	if e.selected == nil {
		return
	}
	var logs []nitrado.LogFile
	var err error
	if scoped, ok := e.client.(interface {
		ListLogsInDir(ctx context.Context, serviceID, dir string) ([]nitrado.LogFile, error)
	}); ok {
		logs, err = scoped.ListLogsInDir(ctx, e.serviceID, e.selected.Directory)
	} else {
		logs, err = e.client.ListLogs(ctx, e.serviceID)
	}
	if err != nil || len(logs) == 0 {
		return
	}
	newest := logs[0] // newest-first
	if newest.Path != e.selected.Path && newest.Modified.After(e.selected.Modified) {
		slog.Info("component=killfeed", "msg", "newer ADM detected; switching",
			"previous", e.selected.Path, "file", newest.Path, "modified", newest.Modified.UTC().Format(time.RFC3339))
		e.selectLog(newest)
	}
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
