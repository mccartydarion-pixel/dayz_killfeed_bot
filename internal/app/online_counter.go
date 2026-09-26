package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Online-players voice counter: ONE authority and fallback policy
// (docs/ONLINE_COUNTER_AND_LINK_CHECK.md).
//
// The counter shows CURRENTLY CONNECTED players of the public server, which
// is a different fact from player activity history. Sources, in order:
//
//  1. Nitrado's live gameserver query (query.player_current / player_max),
//     only when this evaluation's read succeeded AND the response names the
//     requested service (GameserverLive.BelongsTo). A stopped server is a
//     known 0; a restarting one (no query) is not a count.
//  2. The ADM presence tracker, only with evidence that it is complete: a
//     COMPLETE player list reconciled it within onlineCounterADMTrust, or a
//     verified restart (BOOT_RESET) was read from the new boot's start and the
//     pipeline is still reading (onlineCounterPipelineFresh). Connect and
//     disconnect lines alone are never a live count.
//  3. Otherwise UNKNOWN: the channel keeps its last value for
//     onlineCounterUnknownGrace, then shows "?" - never a manufactured 0.
//
// Disagreement: when both 1 and 2 are available and differ, Nitrado is shown
// (it is what the Nitrado panel and players see) and the disagreement is
// recorded with the time it started; one lasting longer than
// onlineCounterADMTrust degrades health (runtime_health.go).
//
// Only one source is ever used per evaluation, so a player is never counted
// twice; the tracker itself is keyed by DayZ ID.
//
// All Discord renames happen on this loop's goroutine or the counter's own
// timers - never on the ADM pipeline, which only pokes this loop. Renames are
// spaced by discord.DefaultCounterMinRenameInterval (Discord allows 2 channel
// renames per 10 minutes) and a 429 is scheduled, not slept through.

const (
	onlineCounterPollInterval = 60 * time.Second
	// onlineCounterMinEval bounds how often a presence change can trigger an
	// extra Nitrado read.
	onlineCounterMinEval = 15 * time.Second
	// onlineCounterADMTrust is how long a complete ADM player list keeps the
	// tracker trustworthy (DayZ writes one every 5 minutes).
	onlineCounterADMTrust = 11 * time.Minute
	// onlineCounterPipelineFresh is how recently the ADM worker must have
	// checked its source for a BOOT_RESET tracker to still be complete.
	onlineCounterPipelineFresh = 2 * time.Minute
	// onlineCounterUnknownGrace holds the last known value through a brief
	// outage before the channel is switched to the unknown state.
	onlineCounterUnknownGrace = 10 * time.Minute
	onlineCounterLiveTimeout  = 10 * time.Second
)

// liveStatusReader is the Nitrado read the counter needs (*nitrado.Client).
type liveStatusReader interface {
	GameserverLive(ctx context.Context, serviceID string) (nitrado.GameserverLive, error)
}

// counterSource is what a running server worker exposes to the counter.
type counterSource struct {
	serviceID string
	live      liveStatusReader
	engine    *killfeed.Engine
}

// onlineReadingInput is everything resolveOnlineReading decides from.
type onlineReadingInput struct {
	ServiceID  string // the service the Nitrado read was for; "" skips the ownership check
	Live       nitrado.GameserverLive
	LiveErr    error
	LiveRead   bool // a Nitrado read was attempted in this evaluation
	Tracker    int
	ADMProven  time.Time // last complete ADM player list reconciliation
	ADMState   string    // killfeed.PresenceEvidence state
	PipelineAt time.Time // last ADM source check by the worker
	KnownSlots int       // capacity remembered from an earlier read
	Now        time.Time
}

// Counter reading sources, reported in diagnostics and logs.
const (
	counterSourceNitrado        = "NITRADO_QUERY"
	counterSourceNitradoStopped = "NITRADO_SERVER_STOPPED"
	counterSourceADMPlayerList  = "ADM_PLAYER_LIST"
	counterSourceADMBootReset   = "ADM_BOOT_RESET"
	counterSourceUnknown        = "UNKNOWN"
)

// nitradoTrusted reports whether this evaluation's Nitrado read may be used.
func nitradoTrusted(in onlineReadingInput) bool {
	if !in.LiveRead || in.LiveErr != nil {
		return false
	}
	return in.ServiceID == "" || in.Live.BelongsTo(in.ServiceID)
}

// admEvidence reports whether the ADM tracker is currently complete, and why.
func admEvidence(in onlineReadingInput) (bool, string) {
	if !in.ADMProven.IsZero() && in.Now.Sub(in.ADMProven) <= onlineCounterADMTrust {
		return true, counterSourceADMPlayerList
	}
	if in.ADMState == killfeed.PresenceBootReset && !in.PipelineAt.IsZero() && in.Now.Sub(in.PipelineAt) <= onlineCounterPipelineFresh {
		return true, counterSourceADMBootReset
	}
	return false, ""
}

// resolveOnlineReading picks the authoritative current player count, or
// reports it unknown. It never derives a count from activity history.
func resolveOnlineReading(in onlineReadingInput) (discord.CounterReading, string) {
	slots := in.KnownSlots
	if nitradoTrusted(in) {
		if c := in.Live.Capacity(); c > 0 {
			slots = c
		}
		switch {
		case in.Live.Running() && in.Live.PlayerCurrent != nil:
			return discord.CounterReading{Count: *in.Live.PlayerCurrent, Slots: slots, Known: true}, counterSourceNitrado
		case in.Live.Stopped():
			return discord.CounterReading{Count: 0, Slots: slots, Known: true}, counterSourceNitradoStopped
		}
	}
	if ok, source := admEvidence(in); ok {
		return discord.CounterReading{Count: in.Tracker, Slots: slots, Known: true}, source
	}
	return discord.CounterReading{Slots: slots}, counterSourceUnknown
}

// presenceDisagreement records Nitrado and the proven ADM tracker disagreeing.
type presenceDisagreement struct {
	NitradoCount int       `json:"nitrado_count"`
	ADMCount     int       `json:"adm_count"`
	Since        time.Time `json:"since"`
}

// onlineCounterStatus is the latest evaluation, for diagnostics.
type onlineCounterStatus struct {
	ServerID            int64
	Reading             discord.CounterReading
	Source              string
	NitradoStatus       string
	NitradoCount        *int
	NitradoError        string
	NitradoWrongService bool
	TrackerCount        int
	ADMState            string
	Disagreement        *presenceDisagreement
	EvaluatedAt         time.Time
	LastKnownAt         time.Time
	Held                bool // unknown, but the last known value is still displayed
}

type onlineCounterLoop struct {
	mu           sync.Mutex
	sources      map[int64]counterSource
	status       onlineCounterStatus
	slots        map[int64]int
	lastKnown    map[int64]time.Time
	disagreement map[int64]*presenceDisagreement
	lastEval     time.Time
	// startedAt starts the unknown grace at process start too, so a restart
	// of the bot (workers not registered yet, first Nitrado read failing)
	// never flashes the channel to "?".
	startedAt time.Time
	poke      chan struct{}
}

func (a *App) counterLoop() *onlineCounterLoop {
	a.onlineLoopOnce.Do(func() {
		a.onlineLoop = &onlineCounterLoop{
			sources:      map[int64]counterSource{},
			slots:        map[int64]int{},
			lastKnown:    map[int64]time.Time{},
			disagreement: map[int64]*presenceDisagreement{},
			startedAt:    time.Now(),
			poke:         make(chan struct{}, 1),
		}
	})
	return a.onlineLoop
}

func (a *App) registerCounterSource(serverID int64, src counterSource) {
	l := a.counterLoop()
	l.mu.Lock()
	l.sources[serverID] = src
	l.mu.Unlock()
	a.pokeOnlineCounter()
}

func (a *App) unregisterCounterSource(serverID int64) {
	l := a.counterLoop()
	l.mu.Lock()
	delete(l.sources, serverID)
	l.mu.Unlock()
}

// pokeOnlineCounter asks the loop to re-evaluate soon. Never blocks, so it is
// safe to call from the ADM pipeline.
func (a *App) pokeOnlineCounter() {
	select {
	case a.counterLoop().poke <- struct{}{}:
	default:
	}
}

// OnlineCounterStatus returns the latest counter evaluation.
func (a *App) OnlineCounterStatus() onlineCounterStatus {
	l := a.counterLoop()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

// runOnlineCounter drives the voice counter until ctx ends. It evaluates once
// at start (startup/restart recovery: the channel's actual name is read and
// reconciled before any rename).
func (a *App) runOnlineCounter(ctx context.Context, counter *discord.VoiceChannelCounter) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=voice_counter", "event", "panic_recovered", "err", fmt.Sprint(r))
		}
	}()
	l := a.counterLoop()
	ticker := time.NewTicker(onlineCounterPollInterval)
	defer ticker.Stop()
	a.evaluateOnlineCounter(ctx, counter)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.evaluateOnlineCounter(ctx, counter)
		case <-l.poke:
			l.mu.Lock()
			wait := onlineCounterMinEval - time.Since(l.lastEval)
			l.mu.Unlock()
			if wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
			a.evaluateOnlineCounter(ctx, counter)
		}
	}
}

// evaluateOnlineCounter resolves the public server's current count and
// publishes it to the voice counter.
func (a *App) evaluateOnlineCounter(ctx context.Context, counter *discord.VoiceChannelCounter) {
	l := a.counterLoop()
	now := time.Now()
	a.counterOwnerMu.RLock()
	serverID := a.publicCounterServerID
	a.counterOwnerMu.RUnlock()

	l.mu.Lock()
	l.lastEval = now
	src, haveSource := l.sources[serverID]
	in := onlineReadingInput{KnownSlots: l.slots[serverID], Now: now}
	l.mu.Unlock()
	if serverID == 0 {
		return // no installation selected: leave the channel alone
	}

	status := onlineCounterStatus{ServerID: serverID, EvaluatedAt: now, ADMState: killfeed.PresenceUnknown}
	if haveSource {
		if src.live != nil && src.serviceID != "" {
			liveCtx, cancel := context.WithTimeout(ctx, onlineCounterLiveTimeout)
			in.Live, in.LiveErr = src.live.GameserverLive(liveCtx, src.serviceID)
			cancel()
			in.LiveRead, in.ServiceID = true, src.serviceID
			if in.LiveErr != nil {
				status.NitradoError = in.LiveErr.Error()
			} else if !in.Live.BelongsTo(src.serviceID) {
				status.NitradoWrongService = true
				status.NitradoError = "response does not name the requested service"
			} else {
				status.NitradoStatus = in.Live.Status
				status.NitradoCount = in.Live.PlayerCurrent
			}
		}
		if src.engine != nil {
			in.Tracker = src.engine.PlayerTracker().OnlineCount()
			in.ADMProven = src.engine.PlayerListStats().LastCompleteSnapshotAt
			in.ADMState = src.engine.PresenceEvidence().State
			if d := src.engine.Diagnostics(); d != nil {
				in.PipelineAt = d.Snapshot().LastMetadataCheck
			}
			status.ADMState = in.ADMState
		}
	}
	reading, source := resolveOnlineReading(in)
	status.Reading, status.Source, status.TrackerCount = reading, source, in.Tracker

	// Explicit disagreement between two trusted sources.
	var disagreement *presenceDisagreement
	if admOK, _ := admEvidence(in); admOK && source == counterSourceNitrado && reading.Count != in.Tracker {
		disagreement = &presenceDisagreement{NitradoCount: reading.Count, ADMCount: in.Tracker, Since: now}
	}

	l.mu.Lock()
	if reading.Slots > 0 {
		l.slots[serverID] = reading.Slots
	}
	if reading.Known {
		l.lastKnown[serverID] = now
	}
	if disagreement != nil {
		if prev := l.disagreement[serverID]; prev != nil {
			disagreement.Since = prev.Since
		}
		l.disagreement[serverID] = disagreement
	} else {
		delete(l.disagreement, serverID)
	}
	status.Disagreement = disagreement
	status.LastKnownAt = l.lastKnown[serverID]
	graceFrom := status.LastKnownAt
	if l.startedAt.After(graceFrom) {
		graceFrom = l.startedAt
	}
	status.Held = !reading.Known && now.Sub(graceFrom) < onlineCounterUnknownGrace
	previous := l.status
	l.status = status
	l.mu.Unlock()

	if previous.Source != source || previous.Reading != reading {
		slog.Info("component=voice_counter", "event", "reading", "server_id", serverID, "source", source,
			"known", reading.Known, "count", reading.Count, "slots", reading.Slots, "tracker_count", in.Tracker,
			"adm_state", in.ADMState, "nitrado_status", status.NitradoStatus, "nitrado_error", status.NitradoError != "")
	}
	if disagreement != nil && (previous.Disagreement == nil || *previous.Disagreement != *disagreement) {
		slog.Warn("component=presence", "event", "presence_disagreement", "server_id", serverID,
			"nitrado_count", disagreement.NitradoCount, "adm_count", disagreement.ADMCount, "since", disagreement.Since.UTC().Format(time.RFC3339), "shown", "nitrado")
	}
	if reading.Known && a.State != nil {
		a.State.SetOnlinePlayers(reading.Count)
	}
	if counter == nil || status.Held {
		return
	}
	a.bindCounterChannel(ctx, counter)
	if counter.LastName() == "" {
		// First publish to this channel: its current name is not known yet
		// (fresh process, new route) - read it so an identical name is never
		// renamed, a stale one is corrected, and ownership is validated.
		if err := counter.Reconcile(reading); err != nil {
			slog.Warn("component=voice_counter", "event", "reconcile_failed", "err", err.Error())
		}
	} else {
		counter.Publish(reading)
	}
	if a.State != nil {
		a.State.SetOnlineCounter(counter.LastPublished(), counter.UpdateErrors(), counter.PermissionBlocked())
	}
}

// knownPresenceCount returns the count the counter would show for serverID
// (its latest known reading), for health output. known is false while the
// count is unknown or the server is not the counter's public server.
func (a *App) knownPresenceCount(serverID int64) (int, bool) {
	st := a.OnlineCounterStatus()
	if st.ServerID != serverID || !st.Reading.Known {
		return 0, false
	}
	return st.Reading.Count, true
}
