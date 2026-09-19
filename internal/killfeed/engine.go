package killfeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

	// staleProbeAfter is how long the selected ADM may look unchanged before
	// Champion stops trusting directory metadata and reads the file directly.
	staleProbeAfter = 2 * time.Minute
	// staleProbeInterval bounds forced probes so a stale source never causes a
	// download storm.
	staleProbeInterval = 60 * time.Second

	// staleGiveUpAfter is how long the selected ADM may show no read progress
	// before Champion gives up on it and forces full rediscovery. A trusted
	// external DayZ server operator (2026-09-17) advised that even a fully
	// healthy noftp ADM source can naturally go 3-6 minutes between writes;
	// the previous 5-minute threshold sat inside that normal range and could
	// occasionally abandon a live source mid-cycle. Widened with margin above
	// the reported range so normal delayed updates are tolerated without
	// hiding a genuinely dead source. probeStaleSource's cheap direct-read
	// safety net (staleProbeAfter/staleProbeInterval, well below this
	// threshold) keeps actively checking the source the whole time this
	// threshold is pending, so this only changes how long Champion waits
	// before the expensive full-discovery fallback, not how quickly it
	// notices real activity.
	staleGiveUpAfter = 8 * time.Minute
)

// persistEnqueueTimeout bounds how long processLine waits for a persistence
// ack. A var (not const) so tests can shrink it instead of waiting for real
// seconds to prove the timeout actually bounds a hung downstream call.
var persistEnqueueTimeout = 30 * time.Second

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

// DeathPublisher is the consumer for authoritative PLAYER_DEATH/SUICIDE_ACTION
// events. Implementations must not propagate errors that would stop log processing.
type DeathPublisher interface {
	PublishDeath(ev *Event) error
}

// HitPublisher is the consumer for parsed PLAYER_HIT events (the HITFEED).
// PublishHit runs on the polling goroutine for every non-duplicate hit line, so
// implementations MUST NOT block (no network, no database) and have no error to
// return: hits are not persisted, and a failing consumer must never stop log
// processing, killfeed publishing or checkpointing. Panics are recovered by the
// engine as a last resort.
type HitPublisher interface {
	PublishHit(ev *Event)
}

// ConnectionKind is what a ConnectionNotice reports. Only the two states the
// ADM log states explicitly exist: there is deliberately no "reconnect" kind,
// because Champion never infers one.
type ConnectionKind string

const (
	ConnectionConnected    ConnectionKind = "CONNECTED"
	ConnectionDisconnected ConnectionKind = "DISCONNECTED"
)

// ConnectionNotice is what the CONNECTIONS feed receives. It carries only what
// is safe and useful to show: the display name, the kind, and (for a
// disconnect) the observed session length. The ADM player id and position are
// deliberately not passed on, so the feed cannot leak them.
type ConnectionNotice struct {
	Kind    ConnectionKind
	Name    string
	Session time.Duration // 0 = unknown
}

// ConnectionPublisher is the consumer for authoritative connect/disconnect
// state changes (the CONNECTIONS feed). PublishConnection runs on the polling
// goroutine, after the event passed dedupe and durable persistence, so
// implementations MUST NOT block and have no error to return; a failing
// consumer must never stop log processing. Panics are recovered by the engine.
type ConnectionPublisher interface {
	PublishConnection(n ConnectionNotice)
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

	dedupe         *Deduplicator
	publisher      KillPublisher
	deathPublisher DeathPublisher
	hitPublisher   HitPublisher
	connPublisher  ConnectionPublisher
	metrics        Metrics
	persistence    *PersistenceQueue

	players   *PlayerTracker
	onPlayers func(count int) // optional hook when the online player set changes

	previousFileName          string
	lastRotationAt            time.Time
	lastConnectAt             time.Time
	lastDisconnectAt          time.Time
	lastPresenceEvent         string
	lastPersistenceResult     string
	lastVoicePublishCount     int
	lastVoicePublishAt        time.Time
	lastVoicePublishResult    string
	presenceMu                sync.RWMutex
	lastDownloadAt            time.Time
	rotationPending           bool
	lastStaleProbeAt          time.Time
	lastStaleProbeFingerprint string
	newestDiscoveredFile      string
	newestDiscoveredModified  time.Time
	candidateCount            int
	selectionReason           string

	// candidateHistory/staleMarks/lastStaleRediscoveryAt back the
	// activity-aware source selection in source_selection.go: they replace
	// "pick whichever candidate Nitrado reports as newest-modified" with
	// "pick whichever candidate has actual evidence of being alive,"
	// demoting (never permanently blacklisting) a source just proven stale.
	candidateHistory       map[string]candidateObservation
	staleMarks             map[string]staleMark
	lastStaleRediscoveryAt time.Time

	// noftpMemory backs the bounded-retry mount preference in
	// canonical_source.go: it remembers each canonical ADM source's last
	// known noftp representation and how many consecutive passes it has been
	// missing from the live listing, so a transient Nitrado listing gap for
	// the noftp mount doesn't immediately flap selection onto ftproot.
	noftpMemory map[string]*noftpAliasMemory

	// onAdmSnapshot fires once per poll cycle from this engine's own goroutine
	// (never concurrently), so the ADM monitor can read a consistent snapshot
	// without needing its own synchronization on Engine's internal fields.
	onAdmSnapshot func(AdmSnapshot)
	onDownload    func(DownloadReport)
	diagnostics   *RuntimeDiagnostics
	onDiagnostics func(*RuntimeDiagnostics)

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
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) { s.ServerID = serverID; s.WorkerRunning = true })
	}
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
		diagnostics:  NewRuntimeDiagnostics(0),
	}
}

func (e *Engine) SetDiagnostics(d *RuntimeDiagnostics) {
	if e != nil {
		e.diagnostics = d
	}
}
func (e *Engine) Diagnostics() *RuntimeDiagnostics {
	if e == nil {
		return nil
	}
	return e.diagnostics
}
func (e *Engine) OnDiagnostics(fn func(*RuntimeDiagnostics)) {
	if e != nil {
		e.onDiagnostics = fn
	}
}
func (e *Engine) reportDiagnostics() {
	if e != nil && e.onDiagnostics != nil {
		e.onDiagnostics(e.diagnostics)
	}
}

func nameOfSelected(e *Engine) string {
	if e == nil || e.selected == nil {
		return ""
	}
	return e.selected.Name
}

// SelectedName returns the basename of the currently selected ADM, or "" if
// none is selected yet.
func (e *Engine) SelectedName() string {
	return nameOfSelected(e)
}

// LogSource exposes the engine's Nitrado client so diagnostics tooling (such
// as the live ADM source scan) can reuse the exact same authenticated client.
func (e *Engine) LogSource() LogSource {
	if e == nil {
		return nil
	}
	return e.client
}

// ServiceID returns the Nitrado service ID this engine polls.
func (e *Engine) ServiceID() string {
	if e == nil {
		return ""
	}
	return e.serviceID
}

// SetKillPublisher attaches the consumer for authoritative kill events.
func (e *Engine) SetKillPublisher(p KillPublisher) {
	if e == nil {
		return
	}
	e.publisher = p
}

// SetDeathPublisher attaches the consumer for authoritative death/suicide events.
func (e *Engine) SetDeathPublisher(p DeathPublisher) {
	if e == nil {
		return
	}
	e.deathPublisher = p
}

// SetHitPublisher attaches the consumer for parsed hit events. Optional: with
// none attached hits are only counted, exactly as before the HITFEED existed.
func (e *Engine) SetHitPublisher(p HitPublisher) {
	if e == nil {
		return
	}
	e.hitPublisher = p
}

// publishHit hands a hit to the attached HitPublisher. It is deliberately
// fire-and-forget and panic-safe: a broken hit consumer must not be able to
// stop the polling loop (which also carries kills, deaths and checkpoints).
func (e *Engine) publishHit(ev *Event) {
	if e.hitPublisher == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=killfeed", "msg", "hit publisher panic recovered", "server_id", e.serverID, "panic", fmt.Sprint(r))
		}
	}()
	e.hitPublisher.PublishHit(ev)
}

// SetConnectionPublisher attaches the consumer for connect/disconnect state
// changes. Optional: with none attached presence is tracked exactly as before.
func (e *Engine) SetConnectionPublisher(p ConnectionPublisher) {
	if e == nil {
		return
	}
	e.connPublisher = p
}

// publishConnection hands a state change to the ConnectionPublisher. Like
// publishHit it is fire-and-forget and panic-safe.
func (e *Engine) publishConnection(n ConnectionNotice) {
	if e.connPublisher == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=killfeed", "msg", "connection publisher panic recovered", "server_id", e.serverID, "panic", fmt.Sprint(r))
		}
	}()
	e.connPublisher.PublishConnection(n)
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
	q.SetDeathPersistedHook(func(ev *Event) {
		if e.deathPublisher == nil {
			return
		}
		if err := e.deathPublisher.PublishDeath(ev); err != nil {
			slog.Warn("component=killfeed", "msg", "death feed publish failed", "err", err.Error())
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

	// One-time transport/mount diagnostics (section 3/6 of the noftp API
	// audit). All ADM discovery and download traffic goes through the
	// Nitrado REST API (internal/nitrado) - there is no FTP client anywhere
	// in this runtime - and noftp is the preferred mount representation
	// whenever it is available (see canonical_source.go).
	slog.Info("component=nitrado", "event", "log_transport", "transport", "api", "preferred_mount", "noftp")
	// Nitrado's Gameserver Details API (settings.general/settings.config) was
	// checked live and exposes several granular ADM verbosity flags (nolog,
	// adminLogPlayerHitsOnly, adminLogBuildActions, adminLogPlacement) but no
	// field unambiguously documented as "Reduced Log Output" - guessing which
	// one that panel toggle maps to would be inventing support that isn't
	// actually confirmed, so this stays a manual check.
	slog.Info("component=nitrado", "event", "reduced_log_output", "status", "manual_check_required")

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
	// Collapse ftproot/noftp mount aliases of the same logical ADM into one
	// candidate before anything else sees the list - ranking, history, and
	// selection all operate on logical sources from this point on, so a pure
	// mount-representation change can never look like a rotation.
	logs = e.deduplicateCandidates(logs)
	e.recordCandidates(logs)
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.SelectedADM = nameOfSelected(e)
			s.NewestADM = logs[0].Name
			s.SelectionMatch = nameOfSelected(e) == logs[0].Name
			s.SelectionReason = e.selectionReason
			s.CandidateCount = len(logs)
		})
	}
	// Discovery produced real candidates; clear the backoff counter.
	e.discoverFails = 0

	// Rank candidates by evidence of real activity (growth/modified-advance
	// across discovery passes, demoting anything just proven stale) rather
	// than raw "newest modified timestamp" alone - see source_selection.go.
	// History must be read by ranking BEFORE this pass's observations
	// overwrite it.
	now := time.Now()
	ranked := e.rankCandidates(logs, now)
	previousPath := ""
	if e.selected != nil {
		previousPath = e.selected.Path
	}
	for _, r := range ranked {
		slog.Debug("component=adm_discovery", "event", "candidate_ranked",
			"candidate_path", r.File.Path, "metadata_size", r.File.Size, "modified_at", r.File.Modified.UTC().Format(time.RFC3339),
			"candidate_state", string(r.State), "reason", r.Reason, "score", r.Score)
	}
	e.updateCandidateHistory(logs, now)

	best := ranked[0]
	candidate := best.File
	e.selectionReason = best.Reason

	// No candidate anywhere shows real evidence of life (ACTIVE), and the
	// best alternative is not even a brand-new/never-seen file (UNKNOWN) -
	// it is just another proven-or-passively-stale candidate. Retain the
	// current source rather than walking backward to an equally dead
	// historical file (section 7/9): reselect the SAME path with its freshest
	// known metadata so state correctly returns to POLL_SELECTED_LOG instead
	// of spamming full rediscovery every poll.
	if e.selected != nil && candidate.Path != e.selected.Path && best.State != candidateActive && best.State != candidateUnknown {
		slog.Info("component=adm_discovery", "event", "no_active_adm_candidate",
			"current_path", e.selected.Path, "best_alternative_path", candidate.Path,
			"best_alternative_state", string(best.State), "candidate_count", len(logs))
		for _, r := range ranked {
			if r.File.Path == e.selected.Path {
				candidate = r.File
				break
			}
		}
		e.selectionReason = "no_active_candidate_retain_current"
	} else if best.Reason == "newest_remote_modified" && len(logs) > 1 && logs[0].Modified.Equal(logs[1].Modified) {
		e.selectionReason = "newest_filename_timestamp"
	}

	logicalChanged := previousPath != "" && canonicalADMID(previousPath) != canonicalADMID(candidate.Path)
	slog.Info("component=adm_discovery", "event", "selection_decision",
		"selected_path", candidate.Path, "previous_path", previousPath, "selection_reason", e.selectionReason,
		"source_switched", logicalChanged, "physical_path_changed", previousPath != "" && previousPath != candidate.Path,
		"logical_source_changed", logicalChanged, "candidate_state", string(best.State))
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
	// One line per candidate at DEBUG only - at INFO this was 100+ log lines
	// per discovery pass on a server with a long ADM history (section 10).
	for _, candidate := range logs {
		slog.Debug("component=adm_discovery", "event", "candidate", "file", candidate.Name, "modified_at", candidate.Modified.UTC().Format(time.RFC3339), "size", candidate.Size, "filename_timestamp", filenameTimestamp(candidate.Name))
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

// selectBestCandidate picks the newest-modified ADM with no other evidence to
// go on. ListLogs already sorts candidates newest-first; file size must never
// decide this, since an old, already-rotated-out ADM can be far larger than
// the current one and would otherwise wrongly win. This is now only the
// bottom-tier fallback inside rankCandidates' scoring (source_selection.go) -
// discoverOnce no longer calls it directly, since "newest modified" alone is
// exactly the signal that caused Champion to keep reselecting a known-dead
// file. Kept as its own function because it is still the correct rule once
// no candidate has any stronger activity evidence.
func selectBestCandidate(logs []nitrado.LogFile) nitrado.LogFile {
	if len(logs) == 0 {
		return nitrado.LogFile{}
	}
	return logs[0]
}

// selectLog locks in the active gameplay log and switches to polling only it.
func (e *Engine) selectLog(lf nitrado.LogFile) {
	previousName := ""
	previousPath := ""
	if e.selected != nil {
		previousName = e.selected.Name
		previousPath = e.selected.Path
	}
	candidate := lf
	e.selected = &candidate
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.SelectedADM = candidate.Name
			s.SelectionMatch = e.newestDiscoveredFile == "" || candidate.Name == e.newestDiscoveredFile
			s.SelectionReason = e.selectionReason
		})
	}
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
		} else if !e.logSourceFound && !e.startAtTail {
			e.startAtTail = true
			slog.Info("component=adm", "event", "cold_start_baseline_required", "server_id", e.serverID, "file", candidate.Name)
		}
		e.checkpointLoaded = true
	}

	if e.tracker == nil {
		e.tracker = NewTracker(e.serviceID)
	}
	if e.tracker.CurrentLogFile != "" && e.tracker.CurrentLogFile != candidate.Path {
		// If this exact path was already polled earlier in this process's
		// lifetime (e.g. it grew stale, got demoted, and later became
		// eligible again - see source_selection.go), resume from its own
		// previously tracked offset instead of discarding it. Only a path
		// genuinely new to this tracker gets reset to 0. This is what makes
		// switching sources safe: a re-selected file is never replayed from
		// the start, and its bytes are never confused with another file's.
		if existing, ok := e.tracker.Checkpoints[candidate.Path]; ok {
			e.tracker.CurrentLogFile = candidate.Path
			e.tracker.LastByteOffset = existing.Offset
			e.tracker.LineBuffer = ""
		} else if alias, ok := e.checkpointForCanonicalAlias(canonicalADMID(candidate.Path)); ok {
			// A different mount's copy of this exact logical ADM was already
			// being tracked (e.g. the same file previously read as noftp/X.ADM
			// is now represented as ftproot/X.ADM) - resume from that offset
			// instead of replaying from byte 0 just because the physical
			// representation changed (section 4).
			e.tracker.CurrentLogFile = candidate.Path
			e.tracker.LastByteOffset = alias.Offset
			e.tracker.LineBuffer = ""
			slog.Debug("component=adm", "event", "alias_checkpoint_reused", "canonical_source_id", canonicalADMID(candidate.Path), "physical_path", candidate.Path, "offset", alias.Offset)
		} else {
			e.tracker.ResetForRotation(candidate.Path)
		}
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
		if e.diagnostics != nil {
			e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
				s.ColdStartBaseline = true
				s.ColdStartBaselineOffset = candidate.Size
				s.CheckpointOffset = candidate.Size
				s.CheckpointRemoteSize = candidate.Size
				s.CheckpointLastSaved = time.Now()
			})
		}
		slog.Info("component=killfeed", "msg", "first connect: starting at log tail, existing history skipped", "file", candidate.Name, "size", candidate.Size)
	}

	// ADM session != player session: switching which file Champion reads (first
	// selection or later rotation) must never clear live presence. Only
	// authoritative PLAYER_CONNECT/PLAYER_DISCONNECT events change who is online.
	//
	// Rotation is judged by LOGICAL source identity, not raw path: switching
	// between mount representations of the exact same ADM (e.g.
	// noftp/X.ADM -> ftproot/X.ADM) must never emit a rotation or presence
	// event, reset the staleness clock, or otherwise look like anything
	// happened (section 5). A real rotation still does all of that exactly
	// as before.
	if e.logSourceFound {
		logicalChanged := previousPath != "" && canonicalADMID(previousPath) != canonicalADMID(candidate.Path)
		if logicalChanged {
			e.previousFileName = previousName
			e.rotationPending = true
			e.lastRotationAt = time.Now()
			// A switch to a genuinely different source starts its own fresh
			// staleness clock: without this, a source just proven stale (which
			// is why we are switching away from it at all) would leave
			// lastLogChange already past staleGiveUpAfter, so the very next poll of
			// the newly selected source would immediately re-trigger the
			// give-up path before it ever got a normal read - even though it
			// may be perfectly live.
			e.lastLogChange = time.Now()
			slog.Info("component=adm", "event", "rotation", "previous", previousName, "current", candidate.Name, "presence_retained", true)
			slog.Info("component=presence", "event", "rotation", "presence_retained", true, "online_count", e.players.OnlineCount())
		}
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
	if !e.lastLogChange.IsZero() && time.Since(e.lastLogChange) > staleGiveUpAfter {
		// Directory-listing metadata can lag behind the file Nitrado is actually
		// writing (see internal/killfeed/adm_source_scan.go). Force a direct read
		// before giving up: on success this updates lastLogChange, so a genuinely
		// live file resumes normal polling instead of re-triggering this branch
		// every tick once discovery reselects the same newest file.
		e.probeStaleSource(ctx, e.selected)
		if time.Since(e.lastLogChange) > staleGiveUpAfter {
			// Bound how often a still-stale selection re-runs the (expensive,
			// full-tree) discovery walk: without this, every poll tick while
			// stuck (as often as every couple seconds) would call ListLogs
			// again even though nothing has changed. Reuses staleProbeInterval
			// so there is one consistent cadence for "how often do we check a
			// quiet source again," not a second magic number.
			if !e.lastStaleRediscoveryAt.IsZero() && time.Since(e.lastStaleRediscoveryAt) < staleProbeInterval {
				e.reportPoll()
				return nil
			}
			e.lastStaleRediscoveryAt = time.Now()
			e.markSelectedStale(e.selected)
			slog.Warn("component=adm", "event", "selected_stale", "file", e.selected.Name)
			e.state = StateDiscovery
			return e.discoverOnce(ctx)
		}
		// The probe found and processed genuinely new content directly, using
		// its own checkpoint-aware read. Stop here rather than falling through
		// into the metadata-comparison poll below, which would re-derive
		// changed/truncated state from the same (still stale) directory
		// metadata that caused this branch to fire and could misread it as a
		// truncation, double-processing bytes the probe already consumed.
		e.reportPoll()
		return nil
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
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.LastMetadataCheck = time.Now()
			s.LastMetadataChanged = changed
			s.RemoteSize = current.Size
			s.RemoteModified = current.Modified
		})
	}
	slog.Debug("component=adm", "event", "metadata_checked", "changed", changed, "file", current.Name, "size", current.Size)
	if !changed {
		e.probeStaleSource(ctx, current)
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
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) { s.LastDownloadAttempt = time.Now() })
		e.diagnostics.Event("download started")
	}
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
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.LastDownloadSuccess = e.lastDownloadAt
			s.DownloadedBytes = int64(len(content))
			s.NewBytes = int64(len(content)) - oldOffset
		})
	}
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
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.CheckpointOffset = newOffset
			s.CheckpointRemoteSize = current.Size
			s.CheckpointLastSaved = time.Now()
			s.LastIncrementalParse = time.Now()
			s.CompleteLines = len(lineChunks)
			s.PartialLineBuffered = len(e.tracker.LineBuffer) > 0
			s.TrackerCount = e.players.OnlineCount()
		})
	}
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

// probeStaleSource downloads the selected ADM even when directory metadata
// claims it is unchanged, because Nitrado listings can go stale while the file
// is still being written. It is rate limited and only reads the unread tail.
func (e *Engine) probeStaleSource(ctx context.Context, current *nitrado.LogFile) {
	if e == nil || current == nil || e.tracker == nil || e.client == nil {
		return
	}
	if e.lastLogChange.IsZero() || time.Since(e.lastLogChange) < staleProbeAfter {
		return
	}
	if !e.lastStaleProbeAt.IsZero() && time.Since(e.lastStaleProbeAt) < staleProbeInterval {
		return
	}
	e.lastStaleProbeAt = time.Now()

	content, err := e.client.ReadLog(ctx, e.serviceID, current.Path)
	if err != nil {
		if e.diagnostics != nil {
			e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
				s.LastProbeAt = e.lastStaleProbeAt
				s.ProbeResult = "FAILURE"
				s.ProbeClassification = "LIVE_SOURCE_STALE"
			})
		}
		slog.Warn("component=adm", "event", "stale_probe_failed", "server_id", e.serverID, "file", current.Name, "error_class", safeDownloadErrorClass(err))
		return
	}

	directSize := int64(len(content))
	sum := sha256.Sum256(content)
	fingerprint := hex.EncodeToString(sum[:])
	contentChanged := e.lastStaleProbeFingerprint != "" && e.lastStaleProbeFingerprint != fingerprint
	e.lastStaleProbeFingerprint = fingerprint

	checkpointOffset := e.tracker.LastByteOffset
	unread := directSize - checkpointOffset
	if unread < 0 {
		unread = 0
	}
	classification := "WRONG_OR_INACTIVE_ADM_SOURCE"
	if directSize > current.Size || contentChanged || unread > 0 {
		classification = "NITRADO_METADATA_STALE"
	}

	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.LastProbeAt = e.lastStaleProbeAt
			s.ProbeResult = "SUCCESS"
			s.ProbeMetadataSize = current.Size
			s.ProbeDirectSize = directSize
			s.ProbeContentChanged = contentChanged
			s.ProbeUnreadBytes = unread
			s.ProbeClassification = classification
		})
		e.diagnostics.Event("stale probe " + classification)
	}
	slog.Info("component=adm", "event", "stale_probe", "server_id", e.serverID, "file", current.Name, "metadata_size", current.Size, "direct_size", directSize, "checkpoint", checkpointOffset, "unread_bytes", unread, "content_changed", contentChanged, "classification", classification)

	if unread > 0 {
		e.processProbeTail(ctx, current, content, checkpointOffset)
	}
}

// processProbeTail processes only the unread tail discovered by a stale probe,
// using the same acknowledged persistence path as normal polling.
func (e *Engine) processProbeTail(ctx context.Context, current *nitrado.LogFile, content []byte, startOffset int64) {
	if startOffset < 0 || startOffset > int64(len(content)) {
		return
	}
	e.tracker.LineBuffer = string(content[startOffset:])
	lineChunks := e.tracker.DrainCompleteLinesWithOffsets(startOffset)
	safeOffset := startOffset
	eventsParsed := 0
	for _, chunk := range lineChunks {
		parsed, processErr := e.processLine(chunk.Text)
		if parsed {
			eventsParsed++
		}
		if processErr != nil {
			break
		}
		safeOffset = chunk.EndOffset
	}
	e.tracker.LineBuffer = string(content[safeOffset:])
	e.bytesProcessed += safeOffset - startOffset
	e.linesDiscovered += int64(len(lineChunks))
	e.lastLogChange = time.Now()
	e.lastDownloadAt = time.Now()
	e.tracker.UpdateCheckpoint(e.serviceID, current.Path, int64(len(content)), current.Modified, safeOffset)
	checkpointOK := e.saveDurableCheckpoint(ctx, current, safeOffset)
	e.emitDownloadReport(DownloadReport{ServerID: e.serverID, File: current.Name, RemoteSize: current.Size, DownloadedBytes: int64(len(content)), PreviousOffset: startOffset, NewOffset: safeOffset, NewBytes: safeOffset - startOffset, EventsParsed: eventsParsed, Result: "success", CheckpointCurrent: checkpointOK, At: time.Now()})
	slog.Info("component=adm", "event", "stale_probe_processed", "server_id", e.serverID, "file", current.Name, "previous_offset", startOffset, "new_offset", safeOffset, "events_parsed", eventsParsed)
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
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.LastParsedEventType = string(ev.Type)
			s.LastParsedEventAt = time.Now()
			if ev.Type == EventPlayerKill {
				s.LastKillParsedAt = time.Now()
			}
		})
		e.diagnostics.Event("parser " + string(ev.Type))
	}
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
		// Bounded, not context.Background(): a single slow/hung downstream
		// call (DB, Discord) inside the persistence queue's consumer must
		// never freeze this engine's entire poll loop indefinitely - observed
		// live as a ~40 minute stall with no logged error, blocking every
		// later ADM poll and kill/death publish behind it. On timeout the
		// checkpoint does not advance past this event, so it is retried on
		// the next poll (existing persistence-failure path); durable dedupe
		// (kill/death fingerprints, connect/disconnect upserts) makes a
		// retry safe even if the original call eventually completes.
		persistCtx, cancel := context.WithTimeout(context.Background(), persistEnqueueTimeout)
		err := e.persistence.EnqueueAndWait(persistCtx, ev)
		cancel()
		if err != nil {
			e.presenceMu.Lock()
			e.lastPersistenceResult = "FAILURE"
			e.presenceMu.Unlock()
			if e.diagnostics != nil {
				e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
					s.LastPersistenceEvent = string(ev.Type)
					s.LastPersistenceResult = "FAILURE"
					s.LastPersistenceAt = time.Now()
					s.LastErrorStage = "PERSISTENCE_FAILURE"
					s.LastErrorAt = time.Now()
				})
			}
			return true, err
		}
		e.presenceMu.Lock()
		e.lastPersistenceResult = "SUCCESS"
		e.presenceMu.Unlock()
		if e.diagnostics != nil {
			e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
				s.LastPersistenceEvent = string(ev.Type)
				s.LastPersistenceResult = "SUCCESS"
				s.LastPersistenceAt = time.Now()
				if ev.Type == EventPlayerKill {
					s.LastKillPersistedAt = time.Now()
				}
			})
			e.diagnostics.Event("persistence success")
		}
	} else if ev.Type == EventPlayerKill && e.publisher != nil {
		if err := e.publisher.PublishKill(ev); err != nil {
			e.metrics.DiscordPublishErrors++
		} else {
			e.metrics.DiscordKillsPublished++
			e.metrics.LastKillTime = time.Now()
		}
	}
	e.dedupe.Remember(ev)
	if ev.Type == EventPlayerHit {
		// Hits are not persisted. Only a non-duplicate hit reaches here, so a
		// replayed line (retry after a later persistence failure, rotation
		// overlap) is never fed to the HITFEED twice.
		e.publishHit(ev)
	}
	if e.players != nil {
		switch ev.Type {
		case EventPlayerConnect:
			if e.players.PlayerConnected(ev.Player) {
				connectAt := time.Now()
				e.presenceMu.Lock()
				e.lastPresenceEvent = "PLAYER_CONNECT"
				e.lastConnectAt = connectAt
				e.presenceMu.Unlock()
				if e.diagnostics != nil {
					e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
						s.LastConnectAt = connectAt
						s.TrackerCount = e.players.OnlineCount()
					})
				}
				slog.Info("component=presence", "event", "connect_committed", "server_id", e.serverID, "online_count", e.players.OnlineCount())
				e.firePlayersChanged()
				// Published only here: after dedupe, after durable persistence, and
				// only when the player was genuinely not online yet - a repeated
				// "is connected" for an online player is a refresh, not a new
				// connection.
				e.publishConnection(ConnectionNotice{Kind: ConnectionConnected, Name: ev.Player.Name})
			}
		case EventPlayerDisconnect:
			if session, removed := e.players.DisconnectSession(ev.Player); removed {
				disconnectAt := time.Now()
				e.presenceMu.Lock()
				e.lastPresenceEvent = "PLAYER_DISCONNECT"
				e.lastDisconnectAt = disconnectAt
				e.presenceMu.Unlock()
				if e.diagnostics != nil {
					e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
						s.LastDisconnectAt = disconnectAt
						s.TrackerCount = e.players.OnlineCount()
					})
				}
				slog.Info("component=presence", "event", "disconnect_committed", "server_id", e.serverID, "online_count", e.players.OnlineCount())
				e.firePlayersChanged()
				e.publishConnection(ConnectionNotice{Kind: ConnectionDisconnected, Name: ev.Player.Name, Session: session})
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

// checkForNewerLog does a lightweight scan of the selected file's own
// directory (DayZ writes a new timestamped ADM after restart) on
// rescanInterval; never rescans the whole ftproot tree. It feeds the result
// through the SAME activity-aware ranking discoverOnce uses
// (rankCandidates/staleMarks/candidateHistory) - there is exactly one
// candidate-selection policy, not a second "newer filename wins" rule here.
// This closes a real production regression: a file just proven stale (see
// markSelectedStale) kept winning this fast path back solely because Nitrado
// still reported it as the newest by modified time, undoing the recovery
// discoverOnce had just performed.
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
	// Collapse mount aliases before ranking - same reasoning as discoverOnce.
	logs = e.deduplicateCandidates(logs)

	now := time.Now()
	ranked := e.rankCandidates(logs, now)
	// Running this scan far more often than full discovery (rescanInterval vs
	// the staleGiveUpAfter give-up cycle) means genuine new evidence for a demoted
	// source, or a freshly rotated file, is picked up quickly - see section
	// 5/B of the rotation fast-path fix.
	e.updateCandidateHistory(logs, now)

	currentState := candidateUnknown
	for _, r := range ranked {
		if r.File.Path == e.selected.Path {
			currentState = r.State
			break
		}
	}

	best := ranked[0]
	if best.File.Path == e.selected.Path {
		// Nothing beat the current source. Log only when something with a
		// strictly newer Modified timestamp existed but lost on the evidence
		// model, so the rejection (the exact regression this guards against)
		// is still visible without implying a switch happened.
		for _, r := range ranked {
			if r.File.Path != e.selected.Path && r.File.Modified.After(e.selected.Modified) {
				slog.Debug("component=killfeed", "event", "rotation_check",
					"current_path", e.selected.Path, "candidate_path", r.File.Path,
					"current_state", string(currentState), "candidate_state", string(r.State),
					"selection_reason", r.Reason, "switch", false)
				break
			}
		}
		return
	}

	if best.State != candidateActive && best.State != candidateUnknown {
		// Nothing shows real evidence of life; do not hop to an equally dead
		// alternative via the fast path either (section 7).
		slog.Debug("component=killfeed", "event", "no_active_adm_candidate",
			"current_path", e.selected.Path, "best_alternative_path", best.File.Path,
			"best_alternative_state", string(best.State))
		return
	}

	logicalChanged := canonicalADMID(e.selected.Path) != canonicalADMID(best.File.Path)
	slog.Info("component=killfeed", "event", "rotation_check",
		"current_path", e.selected.Path, "candidate_path", best.File.Path,
		"current_state", string(currentState), "candidate_state", string(best.State),
		"selection_reason", best.Reason, "switch", logicalChanged,
		"physical_path_changed", true, "logical_source_changed", logicalChanged)
	e.drainRotationTail(ctx)
	e.selectLog(best.File)
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
