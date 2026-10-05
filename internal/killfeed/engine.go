package killfeed

import (
	"context"
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

// DeltaSource is an optional capability for partial ADM reads (Champion Performance Phase 1.5,
// docs/NITRADO_DELTA_READS.md). *nitrado.Client implements it; a fake LogSource in a test that
// doesn't implement it simply never offers delta reads, and the engine behaves exactly as if
// NITRADO_DELTA_READ_MODE were "off" - the same optional-capability pattern StatSource already
// uses above.
type DeltaSource interface {
	ReadDelta(ctx context.Context, serviceID, path string, fromOffset, targetSize int64, mode nitrado.DeltaMode) (*nitrado.PartialReadResult, bool)
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
	// Presence is how the online count is backed (presence_evidence.go);
	// OnlineCount is only a fact when Presence.Known.
	Presence PresenceEvidence
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

// PveDeathNotice is what the PVE_FEED receives. Like ConnectionNotice it carries
// only the display name and the proven cause - never the ADM id or position.
type PveDeathNotice struct {
	Cause DeathCause
	Name  string
}

// PveDeathPublisher is the consumer for provably non-PvP deaths (the PVE_FEED).
//
// PublishPveDeath runs on the persistence queue's goroutine, AFTER the death was
// durably persisted as a non-duplicate, so it MUST NOT block. It returns whether
// it CLAIMED the death: true means the PVE_FEED owns it and the legacy death feed
// must not post it as well; false (no route configured, lookup failed) leaves it
// to the legacy death feed exactly as before. The decision is made once per
// event, so a death is never published to both.
type PveDeathPublisher interface {
	PublishPveDeath(n PveDeathNotice) (claimed bool)
}

// PveCause classifies a death/suicide event for the PVE_FEED. ok=false means the
// event is not the PVE_FEED's and stays wherever it is today. The ordered rules:
//
//  1. an explicit player attacker (PLAYER_KILL, or any Killer/Attacker) is PvP
//     and belongs to the KILLFEED - never PvE;
//  2. an explicit suicide is PvE;
//  3. a death whose parser-proven Cause is a non-player source
//     (infected/animal/environment) is PvE;
//  4. everything else - notably a generic "died." line, which states no cause -
//     is ambiguous: no guess is made and current behaviour is preserved.
func PveCause(ev *Event) (cause DeathCause, ok bool) {
	if ev == nil || ev.Type == EventPlayerKill || ev.Killer != nil || ev.Attacker != nil {
		return "", false
	}
	switch ev.Type {
	case EventSuicideAction:
		return DeathCauseSuicide, true
	case EventPlayerDeath:
		switch ev.Cause {
		case DeathCauseInfected, DeathCauseAnimal, DeathCauseEnvironment:
			return ev.Cause, true
		}
	}
	return "", false
}

// Engine orchestrates log discovery, selection, incremental polling, and parsing.
type Engine struct {
	parser       Parser
	tracker      *Tracker
	client       LogSource
	sink         StateSink
	serviceID    string
	pollInterval time.Duration
	// fastInterval is the poll rate while the server is busy and the token has headroom (see
	// pollingInterval); 0 disables it. NITRADO_POLL_INTERVAL_FAST, default 3s.
	fastInterval    time.Duration
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
	// growthReadSize is the listed size that last triggered a growth-only
	// read (same modified second; see pollSelected). One read per distinct
	// listed size, so a listing that overstates the file never causes a
	// download every poll.
	growthReadSize int64
	sampleCaptured bool // whether we've logged the gameplay sample for the selected log

	dedupe         *Deduplicator
	publisher      KillPublisher
	deathPublisher DeathPublisher
	hitPublisher   HitPublisher
	buildPublisher BuildPublisher
	connPublisher  ConnectionPublisher
	pvePublisher   PveDeathPublisher
	metrics        Metrics
	persistence    *PersistenceQueue
	// locationQueue is the optional Phase 3 location-history pipeline (docs/PLAYER_INTELLIGENCE.md)
	// - nil-safe throughout (EnqueueEvent is a no-op on a nil queue), so an engine that never had
	// one attached behaves exactly as before this feature existed.
	locationQueue *LocationQueue
	// Optional opt-in C.A.S.E. evidence sink, source-addressed and durable.
	evidenceStore EvidenceStore
	// Build observations require a second explicit allowlist; the existing
	// collector opt-in alone must not increase live ADM checkpoint writes.
	buildEvidenceEnabled bool
	// Live Sync phase 1 (engine_observations.go): player-list snapshots, per-file server-local
	// clocks and the current ADM boot session.
	playerLists     playerListAssembler
	admClocks       map[string]*admClock
	sessionStore    ADMSessionStore
	sessionFile     string
	playerListStats PlayerListStats
	// Live Sync phase 2.1 (boot_authority.go): the accepted current boot and the directories the
	// boot scan lists. acceptedBoot only ever moves forward.
	acceptedBoot    time.Time
	acceptedFile    *nitrado.LogFile
	admDirs         map[string]bool
	verifiedBoots   map[string]bool
	lastBootScan    time.Time
	lastNewBootFile string
	bootStats       BootAuthorityStats
	// Final-hit correlation (final_hit.go): the previous processed line and the last lethal hit.
	lastLineFile string
	lastLineEnd  int64
	lastLethal   *lethalHit
	// onPollCycle is told the outcome of every completed poll cycle (worker liveness).
	onPollCycle        func(PollOutcome)
	transportStreak    int
	lastTransportClass string
	// lastFailureOverload: the latest failed Nitrado call was a 429, a 5xx or a network failure.
	lastFailureOverload bool
	sourceHealth        ADMSourceHealth

	players   *PlayerTracker
	onPlayers func(count int) // optional hook when the online player set changes
	// onNewBoot is told when a verified newer boot is selected (a DayZ server
	// restart), after the previous boot's presence was cleared.
	onNewBoot func(cleared int)

	previousFileName       string
	lastRotationAt         time.Time
	lastConnectAt          time.Time
	lastDisconnectAt       time.Time
	lastPresenceEvent      string
	lastPersistenceResult  string
	lastVoicePublishCount  int
	lastVoicePublishAt     time.Time
	lastVoicePublishResult string
	presenceMu             sync.RWMutex
	// presenceState/presenceEvidenceAt/presenceUnknownSince back the
	// presence evidence model (presence_evidence.go). Guarded by presenceMu.
	presenceState      string
	presenceEvidenceAt time.Time
	lastDownloadAt     time.Time
	rotationPending    bool
	lastStaleProbeAt   time.Time
	// lastAltProbeAt/altProbeHistory/directSizeHint back the direct-read
	// probing of non-selected candidates (adm_alt_probe.go).
	lastAltProbeAt            time.Time
	altProbeHistory           map[string]directProbeObservation
	directSizeHint            directSizeHint
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
	// staleWarnedFile is the last file selected_stale was logged at warn level for.
	staleWarnedFile string

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

	// deltaMode gates the partial-read path (Champion Performance Phase 1.5). Default
	// nitrado.DeltaModeOff (see NewEngine) - the full-download path (ReadLog) is completely
	// unaffected unless an operator explicitly sets NITRADO_DELTA_READ_MODE.
	deltaMode nitrado.DeltaMode
	// admLast* remember the byte just before the checkpoint (file, offset, value), so a verified
	// tail read (nitrado/tail_trust.go) can prove it continues exactly where the last read ended.
	admLastPath   string
	admLastOffset int64
	admLastByte   byte
	// deltaBytesReceived/fullReadBytesAvoided are cumulative, process-lifetime counters for the
	// admin performance snapshot (task section 30) - never reset, never used for any decision.
	deltaBytesReceived   int64
	fullReadBytesAvoided int64

	// Latency measurement only (feed_latency.go). batchReadAt is when the bytes now being parsed
	// were downloaded; utcOffset reports the server clock's offset from UTC in minutes, when known.
	batchReadAt time.Time
	utcOffset   func() (minutes int, known bool)
}

// SetServerUTCOffset attaches the source of the game server clock's UTC offset, used only to
// express a line's own time in UTC for latency measurement. Optional: without it (or while it
// reports unknown) no line time is converted.
func (e *Engine) SetServerUTCOffset(fn func() (minutes int, known bool)) {
	if e != nil {
		e.utcOffset = fn
	}
}

// loggedAtUTC converts a line's server-local time to UTC, or returns zero when the offset is unknown.
func (e *Engine) loggedAtUTC(local *time.Time) time.Time {
	if e.utcOffset == nil || local == nil {
		return time.Time{}
	}
	minutes, known := e.utcOffset()
	if !known {
		return time.Time{}
	}
	return local.Add(-time.Duration(minutes) * time.Minute)
}

// markBatchRead notes that the bytes about to be parsed have just been downloaded.
func (e *Engine) markBatchRead() { e.batchReadAt = time.Now() }

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
		fastInterval: envFastPollInterval(interval),
		tracker:      NewTracker(serviceID),
		state:        StateDiscovery,
		dedupe:       NewDeduplicator(90*time.Second, 8192),
		players:      NewPlayerTracker(),
		diagnostics:  NewRuntimeDiagnostics(0),
		deltaMode:    nitrado.ParseDeltaMode(os.Getenv("NITRADO_DELTA_READ_MODE")),
	}
}

// firstPollDelay is the wait before a worker's first cycle. It used to be a whole poll interval
// (10s by default), which every restart and every newly connected server spent doing nothing.
const firstPollDelay = time.Second

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
	timer := time.NewTimer(firstPollDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			started := time.Now()
			err := e.PollOnce(ctx)
			if err != nil {
				slog.Warn("component=killfeed", "msg", "poll failed", "err", err.Error())
				e.apiFailures++
			}
			e.firePollCycle(err)
			timer.Reset(e.nextDelay(time.Since(started)))
		}
	}
}

// nextInterval returns how long to wait before the next cycle based on state.
func (e *Engine) nextInterval() time.Duration {
	if e.state == StatePolling {
		return e.pollingInterval(time.Now())
	}
	// Discovery walks the whole file tree, so it never retries faster than the failure backoff
	// while Nitrado is answering 429/5xx.
	wait := discoveryBackoff(e.discoverFails)
	if fb := e.failureBackoff(); fb > wait {
		wait = fb
	}
	return wait
}

// minPollGap is the shortest pause between two selected-log polls, so a slow poll is never
// followed immediately by another one.
const minPollGap = 250 * time.Millisecond

// nextDelay is how long to wait after a cycle that took elapsed. While polling the selected log,
// the poll interval is measured from the start of one poll to the start of the next, so a 2s
// interval really polls every 2s instead of every 2s plus however long the poll took. Discovery
// backoff and the failure backoff are still a full wait after each attempt.
func (e *Engine) nextDelay(elapsed time.Duration) time.Duration {
	next := e.nextInterval()
	if e.state != StatePolling || e.failureBackoff() > 0 {
		// A backoff is a full wait: a cycle that spent seconds retrying a 429 must not be
		// followed almost at once by the next one.
		return next
	}
	if next -= elapsed; next < minPollGap {
		next = minPollGap
	}
	return next
}

// busyWindow is how recently the selected log must have changed for the server to count as busy.
const busyWindow = 5 * time.Minute

// playersBusyWindow is how long players being online keeps the server busy without a log change.
// DayZ writes the player list every 5 minutes while anyone is online, so with players on the log
// changes about once per busyWindow and the fast rate would lapse right when the next write is
// due. Three missed player lists mean the presence figure is stale or the source is stuck, which
// the stale probe handles; the fast rate stops then.
const playersBusyWindow = 15 * time.Minute

// lowBudgetInterval is the slowest a selected log is polled while its token's budget is low.
const lowBudgetInterval = 30 * time.Second

// pollFailureBackoffMax caps the wait after consecutive Nitrado 429/5xx/network failures.
const pollFailureBackoffMax = 60 * time.Second

// failureBackoff is the wait Nitrado's own failures impose: after a cycle that ended in a 429, a
// 5xx or a network error the next poll waits the base interval, doubling with each further
// consecutive failure up to pollFailureBackoffMax. 0 when the last call succeeded or failed for
// another reason (a missing file is a rotation, not an overloaded API; see overloadFailure).
func (e *Engine) failureBackoff() time.Duration {
	if e.transportStreak <= 0 || !e.lastFailureOverload {
		return 0
	}
	wait := e.pollInterval
	for i := 1; i < e.transportStreak && wait < pollFailureBackoffMax; i++ {
		wait *= 2
	}
	if wait > pollFailureBackoffMax {
		wait = pollFailureBackoffMax
	}
	return wait
}

// serverActive reports whether the server counts as busy: its log changed within busyWindow, or
// players are online and it changed within playersBusyWindow.
func (e *Engine) serverActive(now time.Time) bool {
	if e.lastLogChange.IsZero() {
		return false
	}
	quiet := now.Sub(e.lastLogChange)
	if quiet <= busyWindow {
		return true
	}
	return quiet <= playersBusyWindow && e.players != nil && e.players.OnlineCount() > 0
}

// pollingInterval picks the selected-log poll rate (docs/NITRADO_POLLING.md), first match wins:
//   - the last Nitrado call failed with a 429, a 5xx or a network error -> failureBackoff (never
//     faster than the base interval);
//   - the token's Nitrado budget is low (under 20% left)  -> max(3x base, 30s) until it resets;
//   - the server is active (serverActive) and the budget is known and at least half left -> fast;
//   - otherwise -> the base interval (NITRADO_POLL_INTERVAL).
//
// The fast rate is only used once Nitrado's own rate-limit headers have been seen, so an unknown
// budget is never spent faster than before.
func (e *Engine) pollingInterval(now time.Time) time.Duration {
	base := e.pollInterval
	failWait := e.failureBackoff()
	src, ok := e.client.(interface{ RateBudget() nitrado.Budget })
	if !ok {
		if failWait > base {
			return failWait
		}
		return base
	}
	budget := src.RateBudget()
	if budget.Low(now) {
		slow := 3 * base
		if slow < lowBudgetInterval {
			slow = lowBudgetInterval
		}
		if failWait > slow {
			return failWait
		}
		return slow
	}
	if failWait > 0 {
		return failWait
	}
	if e.fastInterval > 0 && e.fastInterval < base && budget.Known && budget.Healthy(now) && e.serverActive(now) {
		return e.fastInterval
	}
	return base
}

// PollOnce executes one cycle of the state machine. In DISCOVERY it scans for a
// log; once selected it polls only that file each interval.
func (e *Engine) PollOnce(ctx context.Context) error {
	if e == nil || e.client == nil {
		return nil
	}
	e.batchReadAt = time.Time{} // set again by whichever read this cycle makes

	switch e.state {
	case StateDiscovery, StateLogSelected:
		return e.discoverOnce(ctx)
	default:
		return e.pollSelected(ctx)
	}
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
	e.sink.SetPollStats(e.lastPoll, e.lastLogChange, e.pollingInterval(time.Now()), e.bytesProcessed, e.linesDiscovered)
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

// envFastPollInterval reads NITRADO_POLL_INTERVAL_FAST (default 3s; "off" or "0" disables). It is
// never below 1s and only used when faster than the base interval.
func envFastPollInterval(base time.Duration) time.Duration {
	raw := stringsFromEnv("NITRADO_POLL_INTERVAL_FAST")
	if raw == "off" || raw == "0" {
		return 0
	}
	fast := envPollInterval("NITRADO_POLL_INTERVAL_FAST", 3*time.Second)
	if fast >= base {
		return 0
	}
	return fast
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
