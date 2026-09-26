package killfeed

import (
	"context"
	"log/slog"
	"time"
)

// Champion Live Sync phase 1 (docs/CHAMPION_LIVE_SYNC.md): structured ADM observations that are
// not feed events - player-list snapshots and the ADM file header - plus the source identity every
// location observation now carries.

// ADMSessionStore records which ADM file is the server's current boot session. The file identity
// (canonicalADMID) is the server-session epoch: current-session queries only trust observations from
// it. *repository.LocationRepository implements it.
//
// RecordADMSession reports whether the database ACCEPTED the file as the current session: it refuses
// a boot older than the recorded one (Live Sync phase 2.1), and the engine must not claim otherwise.
type ADMSessionStore interface {
	RecordADMSession(ctx context.Context, guildID, serverID int64, admFile string, localStart *time.Time) (accepted bool, err error)
}

// PlayerListStats are the player-list counters exposed for diagnostics.
type PlayerListStats struct {
	Snapshots           int64
	CompleteSnapshots   int64
	IncompleteSnapshots int64
	Entries             int64
	LastSnapshotID      string
	LastSnapshotPlayers int
	LastSnapshotAt      time.Time // when Champion processed it
	// LastCompleteSnapshotAt is when Champion last reconciled presence against
	// a COMPLETE player list - the moment the tracker was last proven exact.
	LastCompleteSnapshotAt time.Time
	PresenceAdded          int64 // players a complete snapshot proved online without a seen connect
	PresenceRemoved        int64 // players a complete snapshot proved gone without a seen disconnect
}

// SetADMSessionStore attaches the current-session recorder (optional; nil-safe).
func (e *Engine) SetADMSessionStore(store ADMSessionStore) { e.sessionStore = store }

// PlayerListStats returns a snapshot of the player-list counters.
func (e *Engine) PlayerListStats() PlayerListStats {
	e.presenceMu.RLock()
	defer e.presenceMu.RUnlock()
	return e.playerListStats
}

// clockFor returns the server-local clock of one canonical ADM file, created from its filename.
func (e *Engine) clockFor(file string) *admClock {
	if file == "" {
		return nil
	}
	if e.admClocks == nil {
		e.admClocks = map[string]*admClock{}
	}
	if c, ok := e.admClocks[file]; ok {
		return c
	}
	c, ok := newADMClock(file)
	if !ok {
		return nil
	}
	if len(e.admClocks) > 16 { // only a handful of files are ever live at once
		e.admClocks = map[string]*admClock{}
	}
	e.admClocks[file] = c
	return c
}

// locationSource builds an observation's physical source. sourcePath "" (tests, legacy callers)
// yields an empty source, which keeps the old dedupe key.
func (e *Engine) locationSource(ev *Event, sourcePath string, endOffset int64, snapshot string) LocationSource {
	if sourcePath == "" || endOffset < 0 {
		return LocationSource{}
	}
	file := canonicalADMID(sourcePath)
	return LocationSource{File: file, Offset: endOffset, LocalTime: e.clockFor(file).at(ev.TimeOfDay), Snapshot: snapshot}
}

// handleObservationLine consumes player-list and ADM-header lines. handled=true means the line
// was fully processed here and must not reach dedupe, persistence, evidence or any publisher: a
// routine player-list entry is never a killfeed or connection event.
func (e *Engine) handleObservationLine(ev *Event, sourcePath string, endOffset int64) (handled bool) {
	switch ev.Type {
	case EventAdminLogStarted:
		if file := canonicalADMID(sourcePath); file != "" {
			if _, ok := newADMClock(file); !ok { // a file whose name has no timestamp: use the header
				if t, err := time.Parse("2006-01-02 15:04:05", ev.AdminLogStart); err == nil {
					if e.admClocks == nil {
						e.admClocks = map[string]*admClock{}
					}
					e.admClocks[file] = &admClock{file: file, base: t, lastSec: t.Hour()*3600 + t.Minute()*60 + t.Second()}
				}
			}
		}
		slog.Info("component=livesync", "event", "adm_session_header", "server_id", e.serverID, "local_start", ev.AdminLogStart)
		return true
	case EventPlayerListHeader:
		if abandoned := e.playerLists.header(canonicalADMID(sourcePath), endOffset, ev); abandoned != nil {
			e.finishSnapshot(abandoned)
		}
		return true
	case EventPlayerListEntry:
		file := canonicalADMID(sourcePath)
		snapshot := e.playerLists.entry(file, ev)
		e.presenceMu.Lock()
		e.playerListStats.Entries++
		e.presenceMu.Unlock()
		src := e.locationSource(ev, sourcePath, endOffset, snapshot)
		observed := time.Now().UTC()
		e.locationQueue.EnqueueObservation(ev.Player, LocationEventPlayerList, observed, src)
		return true
	case EventPlayerListFooter:
		if s := e.playerLists.footer(canonicalADMID(sourcePath), ev); s != nil {
			e.finishSnapshot(s)
		}
		return true
	}
	return false
}

// finishSnapshot records a closed snapshot and, only when it is complete, reconciles the in-memory
// presence tracker with it. An incomplete snapshot changes nobody's presence.
func (e *Engine) finishSnapshot(s *PlayerListSnapshot) {
	e.presenceMu.Lock()
	e.playerListStats.Snapshots++
	if s.Complete {
		e.playerListStats.CompleteSnapshots++
	} else {
		e.playerListStats.IncompleteSnapshots++
	}
	e.playerListStats.LastSnapshotID = s.ID()
	e.playerListStats.LastSnapshotPlayers = len(s.Entries)
	e.playerListStats.LastSnapshotAt = time.Now()
	if s.Complete {
		e.playerListStats.LastCompleteSnapshotAt = e.playerListStats.LastSnapshotAt
	}
	e.presenceMu.Unlock()
	slog.Info("component=livesync", "event", "player_list_snapshot", "server_id", e.serverID, "snapshot", s.ID(),
		"declared", s.Declared, "entries", len(s.Entries), "complete", s.Complete)
	if !s.Complete || e.players == nil {
		return
	}
	added, removed := e.players.ReconcileSnapshot(s.Entries)
	if added == 0 && removed == 0 {
		return
	}
	e.presenceMu.Lock()
	e.playerListStats.PresenceAdded += int64(added)
	e.playerListStats.PresenceRemoved += int64(removed)
	e.presenceMu.Unlock()
	slog.Info("component=presence", "event", "player_list_reconciled", "server_id", e.serverID, "added", added, "removed", removed, "online_count", e.players.OnlineCount())
	e.firePlayersChanged()
}

// noteADMSession records the selected file as the server's current boot session when it changes.
// Best-effort: a failure is logged and retried at the next selection.
func (e *Engine) noteADMSession(path string) {
	file := canonicalADMID(path)
	if file == "" || file == e.sessionFile {
		return
	}
	if abandoned := e.playerLists.abandon(); abandoned != nil {
		e.finishSnapshot(abandoned)
	}
	if e.sessionStore == nil || e.guildID <= 0 || e.serverID <= 0 {
		e.sessionFile = file
		return
	}
	var start *time.Time
	if c := e.clockFor(file); c != nil {
		t := c.base
		start = &t
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted, err := e.sessionStore.RecordADMSession(ctx, e.guildID, e.serverID, file, start)
	if err != nil {
		slog.Warn("component=livesync", "event", "adm_session_record_failed", "server_id", e.serverID, "err", err.Error())
		return
	}
	e.sessionFile = file
	if !accepted {
		// The database kept a newer boot: this file is NOT the current session, and the log must
		// not say it is. Only the canonical file identity is logged.
		slog.Warn("component=livesync", "event", "session_rejected", "server_id", e.serverID, "file", file, "reason", "older_than_recorded_session")
		return
	}
	slog.Info("component=livesync", "event", "adm_session_current", "server_id", e.serverID, "file", file)
}
