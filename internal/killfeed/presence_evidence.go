package killfeed

import (
	"log/slog"
	"time"
)

// Presence evidence states. The tracker's count is only published as fact
// once one of the known states is reached: a fresh process (or a resume from
// a checkpoint/log tail) has no idea who was already online, and must never
// report that ignorance as "0 players".
const (
	// PresenceUnknown: no evidence yet about who was online before this
	// engine started reading. The tracker holds at most a lower bound.
	PresenceUnknown = "UNKNOWN"
	// PresenceSnapshotConfirmed: a complete ADM PlayerList snapshot
	// reconciled the tracker (authoritative).
	PresenceSnapshotConfirmed = "SNAPSHOT_CONFIRMED"
	// PresenceBootReset: a verified newer boot (server restart) was selected
	// and read from its start; everyone from the previous session was
	// disconnected by the restart, so empty is known-correct.
	PresenceBootReset = "BOOT_RESET"
	// PresenceEventDerived: the unknown window expired without a snapshot
	// (servers without adminLogPlayerList). The count is built from
	// connect/disconnect events only - best effort, labelled as such.
	PresenceEventDerived = "EVENT_DERIVED"
)

// presenceUnknownWindow is how long presence may stay UNKNOWN before falling
// back to event-derived counting: one DayZ PlayerList interval (5 minutes)
// plus margin, so a server with snapshots enabled always confirms first.
var presenceUnknownWindow = 6 * time.Minute

// PresenceEvidence describes how trustworthy the current online count is.
type PresenceEvidence struct {
	State        string    `json:"state"`
	Known        bool      `json:"known"`
	EvidenceAt   time.Time `json:"evidence_at,omitempty"`
	UnknownSince time.Time `json:"unknown_since,omitempty"`
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
	return PresenceEvidence{State: state, Known: state != PresenceUnknown, EvidenceAt: e.presenceEvidenceAt, UnknownSince: e.presenceUnknownSince}
}

// startPresenceClock starts the unknown window at the first source selection.
func (e *Engine) startPresenceClock(now time.Time) {
	e.presenceMu.Lock()
	if e.presenceUnknownSince.IsZero() && e.presenceState == "" {
		e.presenceUnknownSince = now
	}
	e.presenceMu.Unlock()
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
// verified newer boot (server restart) is selected. Called only from
// acceptBoot for a boot strictly newer than an already-accepted one, after
// drainRotationTail has consumed the old file, so no old-session line can
// arrive afterwards and re-add a player.
func (e *Engine) resetPresenceForNewBoot(boot time.Time) {
	if e.players == nil {
		return
	}
	previous := e.players.OnlineCount()
	e.players.Reset()
	e.setPresenceEvidence(PresenceBootReset, time.Now())
	slog.Info("component=presence", "event", "boot_reset", "server_id", e.serverID, "boot", boot.Format("2006-01-02T15:04:05"), "cleared_players", previous)
	e.firePlayersChanged()
}

// checkPresenceWindow promotes UNKNOWN to EVENT_DERIVED once the unknown
// window expires without a snapshot, firing the players hook once so the
// (now labelled best-effort) count is published. Called every poll cycle.
func (e *Engine) checkPresenceWindow(now time.Time) {
	e.presenceMu.RLock()
	expired := (e.presenceState == "" || e.presenceState == PresenceUnknown) &&
		!e.presenceUnknownSince.IsZero() && now.Sub(e.presenceUnknownSince) >= presenceUnknownWindow
	e.presenceMu.RUnlock()
	if !expired {
		return
	}
	e.setPresenceEvidence(PresenceEventDerived, now)
	slog.Warn("component=presence", "event", "presence_event_derived", "server_id", e.serverID,
		"reason", "no complete PlayerList snapshot within window; enable adminLogPlayerList for authoritative presence",
		"window", presenceUnknownWindow.String())
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
