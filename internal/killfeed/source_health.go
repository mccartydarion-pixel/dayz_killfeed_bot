package killfeed

import (
	"time"
)

// Champion Live Sync phase 2.1: ADM worker liveness and source health, kept separate so a quiet
// server never looks like a dead worker and a real failure is never reported as quiet.

// ADM source health states.
const (
	ADMHealthy        = "HEALTHY"         // polling works and the ADM changed recently
	ADMQuiet          = "QUIET"           // polling works; the current boot's ADM has simply not changed
	ADMSourceLagging  = "SOURCE_LAGGING"  // a newer boot is listed but not yet accepted, or players are online and the ADM is not advancing
	ADMTransportError = "TRANSPORT_ERROR" // consecutive Nitrado list/read failures
	ADMWorkerStalled  = "WORKER_STALLED"  // no poll cycle has completed recently
)

const (
	admQuietAfter          = 3 * time.Minute // no ADM change for this long, with working polls = QUIET
	admActiveStallAfter    = 5 * time.Minute // players online and no change for this long = SOURCE_LAGGING
	admWorkerStallAfter    = 2 * time.Minute // no completed poll cycle for this long = WORKER_STALLED
	admTransportErrorAfter = 3               // consecutive failed list/read calls = TRANSPORT_ERROR
	admNewBootGrace        = 30 * time.Second
)

// PollOutcome is reported after every completed poll cycle.
type PollOutcome struct {
	At              time.Time
	Err             error // an error PollOnce returned (discovery/list failure)
	TransportStreak int   // consecutive failed Nitrado calls, including ones PollOnce retries silently
	LastErrorClass  string
}

// OnPollCycle registers a callback run on the engine goroutine after every poll cycle.
func (e *Engine) OnPollCycle(fn func(PollOutcome)) { e.onPollCycle = fn }

// ADMSourceHealth is a point-in-time copy of what the classification needs, written by the engine
// goroutine at the end of each poll cycle and read by the health refresher.
type ADMSourceHealth struct {
	LastCycleAt       time.Time
	LastChangeAt      time.Time
	TransportStreak   int
	LastErrorClass    string
	SelectedFile      string
	OnlinePlayers     int
	AcceptedFile      string
	NewestListedFile  string
	NewestListedSince time.Time
}

func (e *Engine) noteTransportFailure(class string) {
	e.transportStreak++
	e.lastTransportClass = class
}

func (e *Engine) noteTransportSuccess() { e.transportStreak = 0 }

func (e *Engine) firePollCycle(err error) {
	now := time.Now()
	snap := ADMSourceHealth{LastCycleAt: now, LastChangeAt: e.lastLogChange, TransportStreak: e.transportStreak,
		LastErrorClass: e.lastTransportClass, OnlinePlayers: e.players.OnlineCount()}
	if e.selected != nil {
		snap.SelectedFile = canonicalADMID(e.selected.Path)
	}
	e.presenceMu.Lock()
	snap.AcceptedFile = e.bootStats.AcceptedFile
	snap.NewestListedFile, snap.NewestListedSince = e.bootStats.LastNewBootFile, e.bootStats.LastNewBootSeenAt
	e.sourceHealth = snap
	e.presenceMu.Unlock()
	if e.onPollCycle != nil {
		e.onPollCycle(PollOutcome{At: now, Err: err, TransportStreak: e.transportStreak, LastErrorClass: e.lastTransportClass})
	}
}

// SourceHealth returns the latest ADM source-health snapshot.
func (e *Engine) SourceHealth() ADMSourceHealth {
	e.presenceMu.RLock()
	defer e.presenceMu.RUnlock()
	return e.sourceHealth
}

// ClassifyADMSourceHealth maps a snapshot to a state and a short reason. Failures win over quiet.
func ClassifyADMSourceHealth(h ADMSourceHealth, now time.Time) (state, reason string) {
	switch {
	case h.LastCycleAt.IsZero() || now.Sub(h.LastCycleAt) > admWorkerStallAfter:
		return ADMWorkerStalled, "no ADM poll cycle completed recently"
	case h.TransportStreak >= admTransportErrorAfter:
		return ADMTransportError, "consecutive Nitrado failures: " + h.LastErrorClass
	case h.NewestListedFile != "" && h.NewestListedFile != h.AcceptedFile && now.Sub(h.NewestListedSince) > admNewBootGrace:
		return ADMSourceLagging, "a newer boot is listed but not accepted"
	case h.OnlinePlayers > 0 && !h.LastChangeAt.IsZero() && now.Sub(h.LastChangeAt) > admActiveStallAfter:
		return ADMSourceLagging, "players online and the ADM is not advancing"
	case h.LastChangeAt.IsZero() || now.Sub(h.LastChangeAt) > admQuietAfter:
		return ADMQuiet, "polling healthy; the current boot's ADM has not changed"
	}
	return ADMHealthy, "polling healthy"
}
