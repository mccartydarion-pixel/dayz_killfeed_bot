package killfeed

import "time"

// AdmHealth classifies the ADM pipeline's observed liveness. It is a
// diagnostic signal only; STALE does not mean the DayZ server is offline.
type AdmHealth string

const (
	AdmHealthy   AdmHealth = "HEALTHY"
	AdmIdle      AdmHealth = "IDLE"
	AdmStale     AdmHealth = "STALE"
	AdmSwitching AdmHealth = "SWITCHING"
	AdmError     AdmHealth = "ERROR"
)

// AdmSnapshot is a sanitized, point-in-time view of one Engine's ADM state for
// the private admin monitor. It never includes raw ADM lines, player IDs, or
// private filesystem paths (CurrentFile/PreviousFile are basenames only).
type AdmSnapshot struct {
	State              EngineState
	CurrentFile        string
	PreviousFile       string
	FileSize           int64
	Modified           time.Time
	LastPoll           time.Time
	LastLogChange      time.Time
	LastRotationAt     time.Time
	PollInterval       time.Duration
	BytesProcessed     int64
	ProcessedOffset    int64
	PendingPartialLine string
	LastDownload       time.Time
	APIFailures        int
	OnlineCount        int
	LastConnectAt      time.Time
	LastDisconnectAt   time.Time
}

// AdmSnapshot returns a sanitized snapshot of this engine's current ADM and
// presence state, safe to render in the admin monitor.
func (e *Engine) AdmSnapshot() AdmSnapshot {
	if e == nil {
		return AdmSnapshot{}
	}
	snap := AdmSnapshot{
		State:            e.state,
		PreviousFile:     e.previousFileName,
		LastPoll:         e.lastPoll,
		LastLogChange:    e.lastLogChange,
		LastRotationAt:   e.lastRotationAt,
		PollInterval:     e.pollInterval,
		BytesProcessed:   e.bytesProcessed,
		LastDownload:     e.lastDownloadAt,
		APIFailures:      e.apiFailures,
		LastConnectAt:    e.lastConnectAt,
		LastDisconnectAt: e.lastDisconnectAt,
	}
	if e.selected != nil {
		snap.CurrentFile = e.selected.Name
		snap.FileSize = e.selected.Size
		snap.Modified = e.selected.Modified
	}
	if e.players != nil {
		snap.OnlineCount = e.players.OnlineCount()
	}
	if e.tracker != nil {
		snap.ProcessedOffset = e.tracker.LastByteOffset
		snap.PendingPartialLine = e.tracker.LineBuffer
	}
	return snap
}

// Health classifies the snapshot's liveness. staleAfter is the configured
// threshold since the last observed log change before a healthy-but-quiet
// file is reported as STALE rather than HEALTHY.
func (s AdmSnapshot) Health(now time.Time, staleAfter time.Duration) AdmHealth {
	if s.State != StatePolling || s.CurrentFile == "" {
		return AdmSwitching
	}
	if s.LastLogChange.IsZero() {
		return AdmIdle
	}
	if staleAfter > 0 && now.Sub(s.LastLogChange) > staleAfter {
		return AdmStale
	}
	return AdmHealthy
}
