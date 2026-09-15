package killfeed

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
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

const (
	ADMRemoteMetadataPollInterval = 10 * time.Second
	ADMMonitorRefreshInterval     = 5 * time.Minute
)

type DurableCheckpoint struct {
	Filename           string
	RemoteModifiedAt   time.Time
	RemoteSize         int64
	ProcessedOffset    int64
	PendingPartialLine string
}

type CheckpointStore interface {
	LoadADMCheckpoint(context.Context, int64, int64) (*DurableCheckpoint, error)
	SaveADMCheckpoint(context.Context, int64, int64, string, DurableCheckpoint) error
}

// StateSink receives sanitized engine progress updates for the status endpoint.
type StateSink interface {
	SetLogSource(filename, path string, size int64, modified time.Time)
	SetPollStats(lastPoll, lastLogChange time.Time, interval time.Duration, bytesRead, lines int64)
	SetDiscovery(state string, dirsVisited, filesDiscovered int)
	SetMetrics(m map[string]int64, lastKill time.Time)
	SetSelectedLogActive(active bool)
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

// Metrics counts parser and publisher activity for the status endpoint.
type Metrics struct {
	ADMLinesProcessed      int64
	EventsParsed           int64
	EventsIgnored          int64
	HitsParsed             int64
	ExplicitKillsParsed    int64
	DeathsParsed           int64
	ConnectsParsed         int64
	DisconnectsParsed      int64
	DuplicateEventsDropped int64
	DiscordKillsPublished  int64
	DiscordPublishErrors   int64
	LastKillTime           time.Time
}

type PresenceSnapshot struct {
	ServerID               int64
	OnlineCount            int
	TrackedEntries         int
	LastEventType          string
	LastConnectAt          time.Time
	LastDisconnectAt       time.Time
	LastPersistenceResult  string
	LastVoicePublishCount  int
	LastVoicePublishAt     time.Time
	LastVoicePublishResult string
}

// KillPublisher is the consumer for authoritative PLAYER_KILL events.
// Implementations must not propagate errors that would stop log processing.
type KillPublisher interface {
	PublishKill(ev *Event) error
}

// Engine orchestrates log discovery, selection, incremental polling, and parsing.
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

	dedupe      *Deduplicator
	publisher   KillPublisher
	metrics     Metrics
	persistence *PersistenceQueue

	players   *PlayerTracker
	onPlayers func(count int) // optional hook when the online player set changes

	previousFileName         string
	lastRotationAt           time.Time
	lastConnectAt            time.Time
	lastDisconnectAt         time.Time
	lastPresenceEvent        string
	lastPersistenceResult    string
	lastVoicePublishCount    int
	lastVoicePublishAt       time.Time
	lastVoicePublishResult   string
	presenceMu               sync.RWMutex
	lastDownloadAt           time.Time
	rotationPending          bool
	newestDiscoveredFile     string
	newestDiscoveredModified time.Time
	candidateCount           int
	selectionReason          string

	// onAdmSnapshot fires once per poll cycle from this engine's own goroutine
	// (never concurrently), so the ADM monitor can read a consistent snapshot
	// without needing its own synchronization on Engine's internal fields.
	onAdmSnapshot func(AdmSnapshot)
	onDownload    func(DownloadReport)

	// startAtTail, when set before the first log selection, seeds the checkpoint
	// at the current end of file instead of byte 0 so pre-existing log history
	// (from before this server was connected) is never replayed as new events.
	startAtTail      bool
	guildID          int64
	serverID         int64
	checkpointStore  CheckpointStore
	checkpointLoaded bool
}

// StartAtLogTail marks this engine to begin at the end of the log on its first
// selection (safe first-connect behavior), instead of replaying the file's
// existing history from byte 0. Has no effect after the first log is selected.
func (e *Engine) StartAtLogTail() {
	if e == nil {
		return
	}
	e.startAtTail = true
}

func (e *Engine) SetDurableCheckpoint(store CheckpointStore, guildID, serverID int64) {
	if e == nil {
		return
	}
	e.checkpointStore = store
	e.guildID = guildID
	e.serverID = serverID
}

// rescanInterval is how often, while polling a selected log, we do a lightweight
// directory check for a newer ADM file (DayZ creates a new timestamped ADM on
// restart). This is deliberately far slower than the 2s selected-file poll.
const rescanInterval = ADMRemoteMetadataPollInterval

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
	interval := envPollInterval("NITRADO_POLL_INTERVAL", ADMRemoteMetadataPollInterval)
	if parser == nil {
		parser = NewADMParser()
	}
	return &Engine{
		parser:       parser,
		client:       client,
		serviceID:    serviceID,
		pollInterval: interval,
		tracker:      NewTracker(serviceID),
		state:        StateDiscovery,
		dedupe:       NewDeduplicator(90*time.Second, 8192),
		players:      NewPlayerTracker(),
	}
}

// SetKillPublisher attaches the consumer for authoritative kill events.
func (e *Engine) SetKillPublisher(p KillPublisher) {
	if e == nil {
		return
	}
	e.publisher = p
}

// SetPersistence attaches the durable persistence queue and wires Discord
// publish to happen only after a successful non-duplicate durable insert.
func (e *Engine) SetPersistence(q *PersistenceQueue) {
	if e == nil || q == nil {
		return
	}
	e.persistence = q
	q.SetKillPersistedHook(func(ev *Event) {
		if e.publisher == nil {
			return
		}
		if err := e.publisher.PublishKill(ev); err != nil {
			e.metrics.DiscordPublishErrors++
		} else {
			e.metrics.DiscordKillsPublished++
			e.metrics.LastKillTime = time.Now()
		}
	})
}

// PlayerTracker exposes the engine's online player tracker.
func (e *Engine) PlayerTracker() *PlayerTracker {
	if e == nil {
		return nil
	}
	return e.players
}

// OnPlayersChanged registers a hook fired when the online player set changes.
func (e *Engine) OnPlayersChanged(fn func(count int)) {
	if e == nil {
		return
	}
	e.onPlayers = fn
}

func (e *Engine) RecordVoicePublish(count int, result string) {
	if e == nil {
		return
	}
	e.presenceMu.Lock()
	e.lastVoicePublishCount = count
	e.lastVoicePublishAt = time.Now()
	e.lastVoicePublishResult = result
	e.presenceMu.Unlock()
}

func (e *Engine) PresenceSnapshot() PresenceSnapshot {
	if e == nil {
		return PresenceSnapshot{}
	}
	e.presenceMu.RLock()
	snapshot := PresenceSnapshot{ServerID: e.serverID, LastEventType: e.lastPresenceEvent, LastConnectAt: e.lastConnectAt, LastDisconnectAt: e.lastDisconnectAt, LastPersistenceResult: e.lastPersistenceResult, LastVoicePublishCount: e.lastVoicePublishCount, LastVoicePublishAt: e.lastVoicePublishAt, LastVoicePublishResult: e.lastVoicePublishResult}
	e.presenceMu.RUnlock()
	if e.players != nil {
		snapshot.OnlineCount = e.players.OnlineCount()
		snapshot.TrackedEntries = len(e.players.GetOnlinePlayers())
	}
	return snapshot
}

// OnAdmSnapshot registers a hook fired once per poll cycle with a sanitized
// snapshot of ADM/presence state, for the private admin monitor.
func (e *Engine) OnAdmSnapshot(fn func(AdmSnapshot)) {
	if e == nil {
		return
	}
	e.onAdmSnapshot = fn
}

func (e *Engine) OnDownload(fn func(DownloadReport)) {
	if e == nil {
		return
	}
	e.onDownload = fn
}

// Metrics returns a copy of the parser/publisher counters.
func (e *Engine) Metrics() Metrics {
	if e == nil {
		return Metrics{}
	}
	return e.metrics
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
	e.recordCandidates(logs)
	// Discovery produced real candidates; clear the backoff counter.
	e.discoverFails = 0

	// Phase 2.8: select the best candidate immediately. Ranking (ListLogs already
	// sorts newest-modified first): prefer a non-trivial file (has content) over a
	// brand-new empty ADM, but do not require active growth — an inactive ADM with
	// real gameplay events is a valid source. ListLogs returns newest-first, so the
	// first candidate with meaningful size wins; fall back to logs[0] if all are tiny.
	candidate := selectBestCandidate(logs)
	e.selectionReason = "newest_remote_modified"
	if len(logs) > 1 && logs[0].Modified.Equal(logs[1].Modified) {
		e.selectionReason = "newest_filename_timestamp"
	}
	slog.Info("component=adm_discovery", "event", "selection_decision", "selected", candidate.Name, "reason", e.selectionReason)
	e.selectLog(candidate)
	e.reportPoll()
	return nil
}

func (e *Engine) recordCandidates(logs []nitrado.LogFile) {
	e.candidateCount = len(logs)
	if len(logs) == 0 {
		return
	}
	e.newestDiscoveredFile = logs[0].Name
	e.newestDiscoveredModified = logs[0].Modified
	for _, candidate := range logs {
		slog.Info("component=adm_discovery", "event", "candidate", "file", candidate.Name, "modified_at", candidate.Modified.UTC().Format(time.RFC3339), "size", candidate.Size, "filename_timestamp", filenameTimestamp(candidate.Name))
	}
}

func filenameTimestamp(name string) string {
	for _, layout := range []string{"2006-01-02_15-04-05", "2006-01-02_15-04"} {
		for start := 0; start+len(layout) <= len(name); start++ {
			if parsed, err := time.ParseInLocation(layout, name[start:start+len(layout)], time.UTC); err == nil {
				return parsed.Format(time.RFC3339)
			}
		}
	}
	return ""
}

// selectBestCandidate picks the current gameplay log: the newest-modified ADM.
// ListLogs already sorts candidates newest-first; file size must never decide
// this, since an old, already-rotated-out ADM can be far larger than the
// current one and would otherwise wrongly win.
func selectBestCandidate(logs []nitrado.LogFile) nitrado.LogFile {
	if len(logs) == 0 {
		return nitrado.LogFile{}
	}
	return logs[0]
}

// selectLog locks in the active gameplay log and switches to polling only it.
func (e *Engine) selectLog(lf nitrado.LogFile) {
	previousName := ""
	if e.selected != nil {
		previousName = e.selected.Name
	}
	candidate := lf
	e.selected = &candidate
	e.confirmPending = nil
	e.state = StatePolling
	e.consecFailures = 0
	e.sampleCaptured = false
	if !e.checkpointLoaded && e.checkpointStore != nil && e.guildID > 0 && e.serverID > 0 {
		checkpoint, err := e.checkpointStore.LoadADMCheckpoint(context.Background(), e.guildID, e.serverID)
		if err != nil {
			slog.Warn("component=adm", "event", "checkpoint_load_failed", "server_id", e.serverID, "err", err.Error())
		} else if checkpoint != nil && checkpoint.Filename == candidate.Path {
			e.tracker.UpdateCheckpoint(e.serviceID, candidate.Path, checkpoint.RemoteSize, checkpoint.RemoteModifiedAt, checkpoint.ProcessedOffset)
			e.tracker.LineBuffer = checkpoint.PendingPartialLine
			slog.Info("component=adm", "event", "checkpoint_loaded", "server_id", e.serverID, "offset", checkpoint.ProcessedOffset)
		} else if checkpoint == nil && !e.startAtTail {
			e.startAtTail = true
		}
		e.checkpointLoaded = true
	}

	if e.tracker == nil {
		e.tracker = NewTracker(e.serviceID)
	}
	if e.tracker.CurrentLogFile != "" && e.tracker.CurrentLogFile != candidate.Path {
		e.tracker.ResetForRotation(candidate.Path)
		if e.checkpointStore != nil && e.guildID > 0 && e.serverID > 0 {
			if checkpoint, err := e.checkpointStore.LoadADMCheckpoint(context.Background(), e.guildID, e.serverID); err == nil && checkpoint != nil && checkpoint.Filename == candidate.Path {
				e.tracker.UpdateCheckpoint(e.serviceID, candidate.Path, checkpoint.RemoteSize, checkpoint.RemoteModifiedAt, checkpoint.ProcessedOffset)
				e.tracker.LineBuffer = checkpoint.PendingPartialLine
			}
		}
	}

	// Safe first-connect behavior: skip any history already in the log by
	// seeding the checkpoint at the current tail. Only applies to the very
	// first selection of this engine instance (never on rotation/restart).
	if e.startAtTail && !e.logSourceFound {
		e.tracker.UpdateCheckpoint(e.serviceID, candidate.Path, candidate.Size, candidate.Modified, candidate.Size)
		e.saveDurableCheckpoint(context.Background(), &candidate, candidate.Size)
		slog.Info("component=killfeed", "msg", "first connect: starting at log tail, existing history skipped", "file", candidate.Name, "size", candidate.Size)
	}

	// ADM session != player session: switching which file Champion reads (first
	// selection or later rotation) must never clear live presence. Only
	// authoritative PLAYER_CONNECT/PLAYER_DISCONNECT events change who is online.
	if e.logSourceFound {
		if previousName != "" && previousName != candidate.Name {
			e.previousFileName = previousName
			e.rotationPending = true
			e.lastRotationAt = time.Now()
			slog.Info("component=adm", "event", "rotation", "previous", previousName, "current", candidate.Name, "presence_retained", true)
		}
		slog.Info("component=presence", "event", "rotation", "presence_retained", true, "online_count", e.players.OnlineCount())
	}
	if !e.logSourceFound {
		e.logSourceFound = true
		e.lastLogChange = time.Now()
		slog.Info("component=adm", "event", "current_selected", "file", candidate.Name)
	}
	slog.Info("component=killfeed", "state", string(StatePolling),
		"msg", "log source selected",
		"file", candidate.Name,
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
	if !e.lastLogChange.IsZero() && time.Since(e.lastLogChange) > 5*time.Minute {
		slog.Warn("component=adm", "event", "selected_stale", "file", e.selected.Name)
		e.state = StateDiscovery
		return e.discoverOnce(ctx)
	}

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

	changed := e.tracker.ShouldReadAgain(current.Path, current.Size, current.Modified)
	slog.Debug("component=adm", "event", "metadata_checked", "changed", changed, "file", current.Name, "size", current.Size)
	if !changed {
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
	previousFile := e.previousFileName
	rotation := e.rotationPending
	if !rotation {
		previousFile = ""
	}
	downloadStarted := time.Now()

	slog.Info("component=adm", "event", "download_started", "server_id", e.serverID, "file", current.Name, "remote_size", current.Size)
	content, err := e.client.ReadLog(ctx, e.serviceID, current.Path)
	if err != nil {
		report := DownloadReport{ServerID: e.serverID, File: current.Name, PreviousFile: previousFile, RemoteSize: current.Size, PreviousOffset: oldOffset, Duration: time.Since(downloadStarted), Result: "failure", ErrorClass: safeDownloadErrorClass(err), At: time.Now()}
		slog.Warn("component=adm", "event", "download_failed", "server_id", e.serverID, "file", current.Name, "error_class", report.ErrorClass, "result", report.Result, "timestamp", report.At.UTC().Format(time.RFC3339))
		if e.onDownload != nil {
			e.onDownload(report)
		}
		return e.handleSelectedFailure(ctx, err)
	}
	e.consecFailures = 0
	e.lastDownloadAt = time.Now()
	downloadDuration := time.Since(downloadStarted)

	readOffset := oldOffset
	if readOffset > int64(len(content)) || readOffset < 0 {
		slog.Warn("component=adm", "event", "file_truncated", "file", current.Name, "old_offset", oldOffset, "size", len(content))
		e.tracker.ResetForRotation(current.Path)
		e.tracker.UpdateCheckpoint(e.serviceID, current.Path, current.Size, current.Modified, int64(len(content)))
		checkpointOK := e.saveDurableCheckpoint(ctx, current, int64(len(content)))
		result := "success_no_new_events"
		if !checkpointOK {
			result = "checkpoint_failed"
		}
		report := DownloadReport{ServerID: e.serverID, File: current.Name, PreviousFile: previousFile, RemoteSize: current.Size, DownloadedBytes: int64(len(content)), PreviousOffset: oldOffset, NewOffset: int64(len(content)), NewBytes: int64(len(content)) - oldOffset, Duration: downloadDuration, Result: result, Truncated: true, Rotation: rotation, CheckpointCurrent: checkpointOK, At: time.Now()}
		e.emitDownloadReport(report)
		slog.Info("component=adm", "event", "download_complete", "server_id", e.serverID, "file", current.Name, "remote_size", current.Size, "downloaded_bytes", len(content), "previous_offset", oldOffset, "new_offset", len(content), "new_bytes", int64(len(content))-oldOffset, "events_parsed", 0, "duration_ms", downloadDuration.Milliseconds(), "result", result, "timestamp", report.At.UTC().Format(time.RFC3339))
		e.rotationPending = false
		e.reportPoll()
		return nil
	}

	e.tracker.LineBuffer = string(content[readOffset:])
	lineChunks := e.tracker.DrainCompleteLinesWithOffsets(readOffset)
	eventsParsed := 0
	newOffset := oldOffset
	for _, chunk := range lineChunks {
		parsed, processErr := e.processLine(chunk.Text)
		if parsed {
			eventsParsed++
		}
		if processErr != nil {
			e.tracker.LineBuffer = string(content[newOffset:])
			e.tracker.UpdateCheckpoint(e.serviceID, current.Path, int64(len(content)), current.Modified, newOffset)
			checkpointOK := e.saveDurableCheckpoint(ctx, current, newOffset)
			report := DownloadReport{ServerID: e.serverID, File: current.Name, PreviousFile: previousFile, RemoteSize: current.Size, DownloadedBytes: int64(len(content)), PreviousOffset: oldOffset, NewOffset: newOffset, NewBytes: newOffset - oldOffset, EventsParsed: eventsParsed, Duration: downloadDuration, Result: "persistence_failed", Rotation: rotation, CheckpointCurrent: checkpointOK, At: time.Now()}
			e.emitDownloadReport(report)
			e.rotationPending = false
			e.reportPoll()
			return nil
		}
		newOffset = chunk.EndOffset
	}
	if len(lineChunks) == 0 {
		newOffset = int64(len(content)) - int64(len(e.tracker.LineBuffer))
	}
	bytesConsumed := newOffset - oldOffset

	e.bytesProcessed += bytesConsumed
	e.linesDiscovered += int64(len(lineChunks))
	e.lastLogChange = e.lastPoll
	e.tracker.UpdateCheckpoint(e.serviceID, current.Path, int64(len(content)), current.Modified, newOffset)
	checkpointOK := e.saveDurableCheckpoint(ctx, current, newOffset)
	result := "success"
	if eventsParsed == 0 {
		result = "success_no_new_events"
	}
	if !checkpointOK {
		result = "checkpoint_failed"
	}
	report := DownloadReport{ServerID: e.serverID, File: current.Name, PreviousFile: previousFile, RemoteSize: current.Size, DownloadedBytes: int64(len(content)), PreviousOffset: oldOffset, NewOffset: newOffset, NewBytes: int64(len(content)) - oldOffset, EventsParsed: eventsParsed, Duration: downloadDuration, Result: result, Rotation: rotation, CheckpointCurrent: checkpointOK, At: time.Now()}
	e.emitDownloadReport(report)
	e.rotationPending = false
	slog.Info("component=adm", "event", "download_complete", "server_id", e.serverID, "file", current.Name, "remote_size", current.Size, "downloaded_bytes", len(content), "previous_offset", oldOffset, "new_offset", newOffset, "new_bytes", int64(len(content))-oldOffset, "events_parsed", eventsParsed, "duration_ms", downloadDuration.Milliseconds(), "result", result, "timestamp", report.At.UTC().Format(time.RFC3339))

	// ADM sample capture is disabled by default in production. Enable with
	// ADM_SAMPLE_DEBUG=true for parser diagnostics; even then it logs at DEBUG.
	if !e.sampleCaptured && len(content) > 0 && admSampleDebugEnabled() {
		sample := SelectSampleLines(string(content), 45)
		if len(sample) > 0 {
			e.sampleCaptured = true
			slog.Debug("component=killfeed", "msg", "ADM sample captured", "file", current.Name, "lines", len(sample))
			for _, line := range sample {
				slog.Debug("component=killfeed_adm_sample", "line", line)
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
		"lines", len(lineChunks),
	)

	e.reportPoll()
	return nil
}

func (e *Engine) saveDurableCheckpoint(ctx context.Context, current *nitrado.LogFile, offset int64) bool {
	if e == nil || e.checkpointStore == nil || current == nil || e.guildID == 0 || e.serverID == 0 {
		return true
	}
	checkpoint := DurableCheckpoint{Filename: current.Path, RemoteModifiedAt: current.Modified, RemoteSize: current.Size, ProcessedOffset: offset}
	if e.tracker != nil {
		checkpoint.PendingPartialLine = e.tracker.LineBuffer
	}
	if err := e.checkpointStore.SaveADMCheckpoint(ctx, e.guildID, e.serverID, e.serviceID, checkpoint); err != nil {
		slog.Warn("component=adm", "event", "checkpoint_failed", "server_id", e.serverID, "err", err.Error())
		return false
	}
	slog.Debug("component=adm", "event", "checkpoint_saved", "server_id", e.serverID, "offset", offset)
	return true
}

func (e *Engine) emitDownloadReport(report DownloadReport) {
	if e != nil && e.onDownload != nil {
		e.onDownload(report)
	}
}

func safeDownloadErrorClass(err error) string {
	var reqErr *nitrado.RequestError
	if errors.As(err, &reqErr) {
		return string(reqErr.Kind)
	}
	return "download_error"
}

// processLines runs the ordered pipeline for complete ADM lines:
// parse -> dedupe -> (PLAYER_KILL only) publish. Runs sequentially on the
// polling goroutine; line order is preserved and no per-line goroutines spawn.
func (e *Engine) processLines(lines []string) int {
	if e == nil || len(lines) == 0 {
		return 0
	}
	if e.dedupe == nil {
		e.dedupe = NewDeduplicator(90*time.Second, 8192)
	}
	parsedCount := 0
	for _, line := range lines {
		parsed, err := e.processLine(line)
		if parsed {
			parsedCount++
		}
		if err != nil {
			break
		}
	}
	return parsedCount
}

func (e *Engine) processLine(line string) (bool, error) {
	e.metrics.ADMLinesProcessed++
	ev, err := e.parser.ParseLine(line)
	if err != nil {
		e.metrics.EventsIgnored++
		return false, nil
	}
	if ev == nil {
		e.metrics.EventsIgnored++
		return false, nil
	}
	e.metrics.EventsParsed++
	if ev.Type == EventPlayerDisconnect {
		slog.Info("component=presence", "event", "disconnect_parsed", "matched", true)
	}
	switch ev.Type {
	case EventPlayerHit:
		e.metrics.HitsParsed++
	case EventPlayerKill:
		e.metrics.ExplicitKillsParsed++
	case EventPlayerDeath:
		e.metrics.DeathsParsed++
	case EventPlayerConnect:
		e.metrics.ConnectsParsed++
	case EventPlayerDisconnect:
		e.metrics.DisconnectsParsed++
	}
	if e.dedupe == nil {
		e.dedupe = NewDeduplicator(90*time.Second, 8192)
	}
	if e.dedupe.Contains(ev) {
		e.metrics.DuplicateEventsDropped++
		return true, nil
	}
	if e.persistence != nil && (ev.Type == EventPlayerConnect || ev.Type == EventPlayerDisconnect || ev.Type == EventPlayerDeath || ev.Type == EventSuicideAction || ev.Type == EventPlayerKill) {
		if err := e.persistence.EnqueueAndWait(context.Background(), ev); err != nil {
			e.presenceMu.Lock()
			e.lastPersistenceResult = "FAILURE"
			e.presenceMu.Unlock()
			return true, err
		}
		e.presenceMu.Lock()
		e.lastPersistenceResult = "SUCCESS"
		e.presenceMu.Unlock()
	} else if ev.Type == EventPlayerKill && e.publisher != nil {
		if err := e.publisher.PublishKill(ev); err != nil {
			e.metrics.DiscordPublishErrors++
		} else {
			e.metrics.DiscordKillsPublished++
			e.metrics.LastKillTime = time.Now()
		}
	}
	e.dedupe.Remember(ev)
	if e.players != nil {
		switch ev.Type {
		case EventPlayerConnect:
			if e.players.PlayerConnected(ev.Player) {
				e.presenceMu.Lock()
				e.lastPresenceEvent = "PLAYER_CONNECT"
				e.lastConnectAt = time.Now()
				e.presenceMu.Unlock()
				slog.Info("component=presence", "event", "connect_committed", "server_id", e.serverID, "online_count", e.players.OnlineCount())
				e.firePlayersChanged()
			}
		case EventPlayerDisconnect:
			if e.players.PlayerDisconnected(ev.Player) {
				e.presenceMu.Lock()
				e.lastPresenceEvent = "PLAYER_DISCONNECT"
				e.lastDisconnectAt = time.Now()
				e.presenceMu.Unlock()
				slog.Info("component=presence", "event", "disconnect_committed", "server_id", e.serverID, "online_count", e.players.OnlineCount())
				e.firePlayersChanged()
			}
		}
	}
	return true, nil
}

// firePlayersChanged invokes the registered hook after the online set changes.
func (e *Engine) firePlayersChanged() {
	if e == nil || e.onPlayers == nil || e.players == nil {
		return
	}
	e.onPlayers(e.players.OnlineCount())
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
		slog.Warn("component=killfeed", "msg", "selected log lost; re-entering discovery", "file", e.selected.Name, "err", err.Error())
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
		e.drainRotationTail(ctx)
		slog.Info("component=killfeed", "msg", "newer ADM detected; switching",
			"previous", e.selected.Name, "file", newest.Name, "modified", newest.Modified.UTC().Format(time.RFC3339))
		e.selectLog(newest)
	}
}

func (e *Engine) drainRotationTail(ctx context.Context) {
	if e == nil || e.selected == nil || e.tracker == nil {
		return
	}
	old := *e.selected
	meta, err := e.currentMeta(ctx)
	if err != nil || meta.Size <= e.tracker.LastByteOffset {
		return
	}
	started := time.Now()
	content, err := e.client.ReadLog(ctx, e.serviceID, old.Path)
	if err != nil {
		slog.Warn("component=adm", "event", "rotation_tail_incomplete", "server_id", e.serverID, "file", old.Name, "error_class", safeDownloadErrorClass(err))
		return
	}
	readOffset := e.tracker.LastByteOffset
	if readOffset < 0 || readOffset > int64(len(content)) {
		return
	}
	e.tracker.LineBuffer = string(content[readOffset:])
	chunks := e.tracker.DrainCompleteLinesWithOffsets(readOffset)
	safeOffset := readOffset
	parsed := 0
	for _, chunk := range chunks {
		ok, processErr := e.processLine(chunk.Text)
		if processErr != nil {
			slog.Warn("component=adm", "event", "rotation_tail_incomplete", "server_id", e.serverID, "file", old.Name, "error_class", safeDownloadErrorClass(processErr))
			break
		}
		if ok {
			parsed++
		}
		safeOffset = chunk.EndOffset
	}
	e.tracker.LineBuffer = string(content[safeOffset:])
	e.tracker.UpdateCheckpoint(e.serviceID, old.Path, int64(len(content)), old.Modified, safeOffset)
	checkpointOK := e.saveDurableCheckpoint(ctx, &old, safeOffset)
	e.emitDownloadReport(DownloadReport{ServerID: e.serverID, File: old.Name, DownloadedBytes: int64(len(content)), RemoteSize: meta.Size, PreviousOffset: readOffset, NewOffset: safeOffset, NewBytes: safeOffset - readOffset, EventsParsed: parsed, Duration: time.Since(started), Result: "success", CheckpointCurrent: checkpointOK, At: time.Now()})
}

func (e *Engine) reportPoll() {
	if e == nil {
		return
	}
	if e.onAdmSnapshot != nil {
		e.onAdmSnapshot(e.AdmSnapshot())
	}
	if e.sink == nil {
		return
	}
	e.sink.SetPollStats(e.lastPoll, e.lastLogChange, e.pollInterval, e.bytesProcessed, e.linesDiscovered)
	// The selected log is "active" if it changed within roughly two poll cycles.
	active := !e.lastLogChange.IsZero() && time.Since(e.lastLogChange) <= 2*e.pollInterval
	e.sink.SetSelectedLogActive(active)
	e.sink.SetMetrics(map[string]int64{
		"adm_lines_processed":      e.metrics.ADMLinesProcessed,
		"events_parsed":            e.metrics.EventsParsed,
		"events_ignored":           e.metrics.EventsIgnored,
		"hits_parsed":              e.metrics.HitsParsed,
		"explicit_kills_parsed":    e.metrics.ExplicitKillsParsed,
		"deaths_parsed":            e.metrics.DeathsParsed,
		"connects_parsed":          e.metrics.ConnectsParsed,
		"disconnects_parsed":       e.metrics.DisconnectsParsed,
		"duplicate_events_dropped": e.metrics.DuplicateEventsDropped,
		"discord_kills_published":  e.metrics.DiscordKillsPublished,
		"discord_publish_errors":   e.metrics.DiscordPublishErrors,
	}, e.metrics.LastKillTime)
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

// admSampleDebugEnabled reports whether ADM sample dumping is enabled. Default off.
func admSampleDebugEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("ADM_SAMPLE_DEBUG")), "true")
}
