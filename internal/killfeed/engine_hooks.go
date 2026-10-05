package killfeed

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// DeltaStats is a point-in-time snapshot of this engine's partial-read counters (task section 30),
// exposed through the internal admin performance view - never a customer-facing surface.
type DeltaStats struct {
	Mode                 string
	BytesReceived        int64
	FullReadBytesAvoided int64
}

// DeltaStats returns the engine's cumulative delta-read counters. Safe on a nil Engine.
func (e *Engine) DeltaStats() DeltaStats {
	if e == nil {
		return DeltaStats{Mode: string(nitrado.DeltaModeOff)}
	}
	return DeltaStats{Mode: string(e.deltaMode), BytesReceived: e.deltaBytesReceived, FullReadBytesAvoided: e.fullReadBytesAvoided}
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

// BuildPublisher consumes parsed build/placement actions (BUILD_FEED).
type BuildPublisher interface {
	PublishBuild(ev *Event)
}

// SetBuildPublisher attaches the consumer for build actions. Optional: with
// none attached build lines are parsed and dropped.
func (e *Engine) SetBuildPublisher(p BuildPublisher) {
	if e == nil {
		return
	}
	e.buildPublisher = p
}

// publishBuild is fire-and-forget and panic-safe, like publishHit.
func (e *Engine) publishBuild(ev *Event) {
	if e.buildPublisher == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=killfeed", "msg", "build publisher panic recovered", "server_id", e.serverID, "panic", fmt.Sprint(r))
		}
	}()
	e.buildPublisher.PublishBuild(ev)
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

// SetPveDeathPublisher attaches the consumer for provably non-PvP deaths.
// Optional: with none attached every death goes to the legacy death feed as before.
func (e *Engine) SetPveDeathPublisher(p PveDeathPublisher) {
	if e == nil {
		return
	}
	e.pvePublisher = p
}

// claimByPveFeed offers a persisted death to the PVE_FEED and reports whether the
// PVE_FEED took it. Panic-safe: a broken consumer must not stop persistence, and
// an unclaimed event simply continues to the legacy death feed.
func (e *Engine) claimByPveFeed(ev *Event) (claimed bool) {
	if e.pvePublisher == nil {
		return false
	}
	cause, ok := PveCause(ev)
	if !ok || ev.Player == nil {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			claimed = false
			slog.Error("component=killfeed", "msg", "pve publisher panic recovered", "server_id", e.serverID, "panic", fmt.Sprint(r))
		}
	}()
	return e.pvePublisher.PublishPveDeath(PveDeathNotice{Cause: cause, Name: ev.Player.Name})
}

// SetLocationQueue attaches the optional Phase 3 location-history pipeline
// (docs/PLAYER_INTELLIGENCE.md). Unset by default - an engine with no location queue attached
// simply never enqueues location candidates, at zero cost to the existing hot path.
func (e *Engine) SetLocationQueue(q *LocationQueue) {
	if e == nil {
		return
	}
	e.locationQueue = q
}

// LocationQueueHealth returns the attached location queue's observability snapshot, or a zero
// value if none is attached.
func (e *Engine) LocationQueueHealth() LocationQueueHealth {
	if e == nil {
		return LocationQueueHealth{}
	}
	return e.locationQueue.Health()
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
		// Runs only after a durable, non-duplicate insert. A death the PVE_FEED
		// claims is not also posted to the legacy death feed.
		if e.claimByPveFeed(ev) {
			return
		}
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

// OnNewBoot registers a hook fired when the engine switches to a verified newer
// server boot (a DayZ server restart). cleared is how many players the
// previous boot still had tracked as online. It runs on the polling goroutine
// before any line of the new boot is processed, so it must be quick and must
// never call Discord.
func (e *Engine) OnNewBoot(fn func(cleared int)) {
	if e == nil {
		return
	}
	e.onNewBoot = fn
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
	snapshot.Presence = e.PresenceEvidence()
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
