package killfeed

import (
	"log/slog"
	"time"
)

// Presence evidence states. The tracker's count is only ADM evidence once one
// of the known states is reached: a fresh process (or a resume from a
// checkpoint/log tail) has no idea who was already online, and must never
// report that ignorance as "0 players". Connect/disconnect lines alone never
// make presence known. The online counter's authority and fallback policy
// (Nitrado live query first, then this evidence) is in
// internal/app/online_counter.go and docs/ONLINE_COUNTER_AND_LINK_CHECK.md.
const (
	// PresenceUnknown: no evidence yet about who was online before this
	// engine started reading. The tracker holds at most a lower bound.
	PresenceUnknown = "UNKNOWN"
	// PresenceSnapshotConfirmed: a complete ADM PlayerList snapshot
	// reconciled the tracker (authoritative at that moment).
	PresenceSnapshotConfirmed = "SNAPSHOT_CONFIRMED"
	// PresenceBootReset: a verified newer boot (server restart) was selected
	// and read from its start; everyone from the previous session was
	// disconnected by the restart, so every player since is tracked.
	PresenceBootReset = "BOOT_RESET"
)

// PresenceEvidence describes how trustworthy the ADM tracker's count is.
type PresenceEvidence struct {
	State      string    `json:"state"`
	Known      bool      `json:"known"`
	EvidenceAt time.Time `json:"evidence_at,omitempty"`
}

// PresenceEvidence returns the current presence evidence state.
func (e *Engine) PresenceEvidence() PresenceEvidence {
	if e == nil {
		return PresenceEvidence{State: PresenceUnknown}
	}
	e.presenceMu.RLock()
	defer e.presenceMu.RUnlock()
	state := e.presenceState
	if state == "" {
		state = PresenceUnknown
	}
	return PresenceEvidence{State: state, Known: state != PresenceUnknown, EvidenceAt: e.presenceEvidenceAt}
}

// setPresenceEvidence records a state transition and reports whether presence
// just became known (so the count should be published even if it did not
// change numerically).
func (e *Engine) setPresenceEvidence(state string, at time.Time) (becameKnown bool) {
	e.presenceMu.Lock()
	wasKnown := e.presenceState != "" && e.presenceState != PresenceUnknown
	e.presenceState = state
	e.presenceEvidenceAt = at
	e.presenceMu.Unlock()
	if !wasKnown {
		slog.Info("component=presence", "event", "presence_known", "server_id", e.serverID, "state", state)
	}
	return !wasKnown
}

// notePresenceEvent refreshes the evidence time for a committed
// connect/disconnect without changing the state.
func (e *Engine) notePresenceEvent(at time.Time) {
	e.presenceMu.Lock()
	if e.presenceState != "" && e.presenceState != PresenceUnknown {
		e.presenceEvidenceAt = at
	}
	e.presenceMu.Unlock()
}

// resetPresenceForNewBoot clears the previous session's players when a
// verified newer boot (server restart) is selected: DayZ writes no disconnect
// lines on shutdown. Called only from acceptBoot for a boot strictly newer
// than an already-accepted one, after drainRotationTail has consumed the old
// file, so no old-session line can arrive afterwards and re-add a player.
// onNewBoot then closes the previous boot's open activity sessions so phantom
// sessions stop accruing link playtime.
func (e *Engine) resetPresenceForNewBoot(boot time.Time) {
	if e.players == nil {
		return
	}
	previous := e.players.OnlineCount()
	e.players.Reset()
	// No player list of the new boot has proven the (now empty) tracker yet;
	// the boot itself is the evidence until one does.
	e.presenceMu.Lock()
	e.playerListStats.LastCompleteSnapshotAt = time.Time{}
	e.presenceMu.Unlock()
	e.setPresenceEvidence(PresenceBootReset, time.Now())
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) { s.TrackerCount = 0 })
	}
	slog.Info("component=presence", "event", "server_restart_reset", "server_id", e.serverID, "boot", boot.Format("2006-01-02T15:04:05"), "cleared", previous)
	if e.onNewBoot != nil {
		e.onNewBoot(previous)
	}
	e.firePlayersChanged()
}

// PendingEvents is the durable persistence queue's backlog for health output.
type PendingEvents struct {
	Depth        int           `json:"depth"`
	Capacity     int           `json:"capacity"`
	Dropped      int64         `json:"dropped"`
	OldestAge    time.Duration `json:"-"`
	OldestAgeSec float64       `json:"oldest_age_seconds"`
}

// PendingEvents returns the persistence backlog; ok is false when this engine
// has no persistence queue attached.
func (e *Engine) PendingEvents() (PendingEvents, bool) {
	if e == nil || e.persistence == nil {
		return PendingEvents{}, false
	}
	depth, capacity, _, dropped, oldest := e.persistence.QueueHealth()
	return PendingEvents{Depth: depth, Capacity: capacity, Dropped: dropped, OldestAge: oldest, OldestAgeSec: oldest.Seconds()}, true
}
