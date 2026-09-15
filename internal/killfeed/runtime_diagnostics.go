package killfeed

import (
	"fmt"
	"sync"
	"time"
)

type RuntimeDiagnosticSnapshot struct {
	ServerID                int64
	WorkerRunning           bool
	SelectedADM             string
	NewestADM               string
	SelectionMatch          bool
	SelectionReason         string
	LastMetadataCheck       time.Time
	LastMetadataChanged     bool
	RemoteSize              int64
	RemoteModified          time.Time
	LastDownloadAttempt     time.Time
	LastDownloadSuccess     time.Time
	DownloadedBytes         int64
	NewBytes                int64
	LastIncrementalParse    time.Time
	CompleteLines           int
	PartialLineBuffered     bool
	LastParsedEventType     string
	LastParsedEventAt       time.Time
	LastPersistenceEvent    string
	LastPersistenceResult   string
	LastPersistenceAt       time.Time
	CheckpointOffset        int64
	CheckpointRemoteSize    int64
	CheckpointLastSaved     time.Time
	TrackerCount            int
	LastConnectAt           time.Time
	LastDisconnectAt        time.Time
	DesiredVoiceCount       int
	LastVoicePublishedCount int
	ActualDiscordVoiceCount int
	LastKillParsedAt        time.Time
	LastKillPersistedAt     time.Time
	LastKillPublishedAt     time.Time
	LastErrorStage          string
	LastErrorClass          string
	LastErrorAt             time.Time
	RecentEvents            []string
	CandidateCount          int

	LastVoicePublishResult string
}

type RuntimeDiagnostics struct {
	mu       sync.RWMutex
	snapshot RuntimeDiagnosticSnapshot
	events   []string
}

func NewRuntimeDiagnostics(serverID int64) *RuntimeDiagnostics {
	return &RuntimeDiagnostics{snapshot: RuntimeDiagnosticSnapshot{ServerID: serverID}}
}
func (d *RuntimeDiagnostics) Update(fn func(*RuntimeDiagnosticSnapshot)) {
	if d == nil || fn == nil {
		return
	}
	d.mu.Lock()
	fn(&d.snapshot)
	d.mu.Unlock()
}
func (d *RuntimeDiagnostics) Event(message string) {
	if d == nil || message == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, fmt.Sprintf("%s %s", time.Now().UTC().Format("15:04:05"), message))
	if len(d.events) > 25 {
		d.events = d.events[len(d.events)-25:]
	}
}
func (d *RuntimeDiagnostics) Snapshot() RuntimeDiagnosticSnapshot {
	if d == nil {
		return RuntimeDiagnosticSnapshot{}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := d.snapshot
	out.RecentEvents = append([]string(nil), d.events...)
	return out
}
func (d *RuntimeDiagnostics) Reset() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.snapshot.RecentEvents = nil
	d.events = nil
	d.mu.Unlock()
}
func (s RuntimeDiagnosticSnapshot) Classification() string {
	switch {
	case s.LastErrorStage != "":
		return s.LastErrorStage
	case s.LastMetadataChanged && s.LastDownloadAttempt.IsZero():
		return "ADM_DOWNLOAD_FAILURE"
	case s.DownloadedBytes > 0 && s.CompleteLines > 0 && s.LastParsedEventType == "":
		return "PARSER_FAILURE"
	case s.LastParsedEventType != "" && s.LastPersistenceResult == "FAILURE":
		return "PERSISTENCE_FAILURE"
	case s.LastKillParsedAt.After(s.LastKillPersistedAt) && !s.LastKillPersistedAt.IsZero():
		return "KILL_NOT_PUBLISHED"
	default:
		return "HEALTHY"
	}
}
