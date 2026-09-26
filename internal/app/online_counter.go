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

// Online-players voice counter.
//
// The counter shows CURRENTLY CONNECTED players, which is a different fact
// from player activity history. Its sources, in order of authority:
//
//  1. Nitrado's gameserver query (GET /services/:id/gameservers,
//     query.player_current / player_max) - what the Nitrado panel itself
//     shows. A stopped server is a known 0.
//  2. The ADM presence tracker, but only while a COMPLETE ADM player list
//     has reconciled it recently (adminLogPlayerList, every ~5 minutes).
//     Connect/disconnect lines alone are not trusted as a live count: a bot
//     restart misses players who joined earlier, and a server restart writes
//     no disconnects.
//  3. Otherwise the count is UNKNOWN. The channel keeps its last value for
//     onlineCounterUnknownGrace, then shows "?" - never a manufactured 0.
//
// All Discord renames happen on this loop's goroutine (never on the ADM
// pipeline), through VoiceChannelCounter's rate-limit spacing.

const (
	onlineCounterPollInterval = 60 * time.Second
	// onlineCounterMinEval bounds how often a presence change can trigger an
	// extra Nitrado read.
	onlineCounterMinEval = 15 * time.Second
	// onlineCounterADMTrust is how long a complete ADM player list keeps the
	// tracker trustworthy (DayZ writes one every 5 minutes).
	onlineCounterADMTrust = 11 * time.Minute
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
	Live       nitrado.GameserverLive
	LiveErr    error
	LiveRead   bool // a Nitrado read was attempted
	Tracker    int
	ADMProven  time.Time // last complete ADM player list reconciliation
	KnownSlots int       // capacity remembered from an earlier read
	Now        time.Time
}

// Counter reading sources, reported in diagnostics and logs.
const (
	counterSourceNitrado        = "NITRADO_QUERY"
	counterSourceNitradoStopped = "NITRADO_SERVER_STOPPED"
	counterSourceADMPlayerList  = "ADM_PLAYER_LIST"
	counterSourceUnknown        = "UNKNOWN"
)

// resolveOnlineReading picks the authoritative current player count, or
// reports it unknown. It never derives a count from activity history.
func resolveOnlineReading(in onlineReadingInput) (discord.CounterReading, string) {
	slots := in.KnownSlots
	if in.LiveRead && in.LiveErr == nil {
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
	if !in.ADMProven.IsZero() && in.Now.Sub(in.ADMProven) <= onlineCounterADMTrust {
		return discord.CounterReading{Count: in.Tracker, Slots: slots, Known: true}, counterSourceADMPlayerList
	}
	return discord.CounterReading{Slots: slots}, counterSourceUnknown
}

// onlineCounterStatus is the latest evaluation, for diagnostics.
type onlineCounterStatus struct {
	ServerID      int64
	Reading       discord.CounterReading
	Source        string
	NitradoStatus string
	NitradoCount  *int
	NitradoError  string
	TrackerCount  int
	EvaluatedAt   time.Time
	LastKnownAt   time.Time
	Held          bool // unknown, but the last known value is still displayed
}

type onlineCounterLoop struct {
	mu        sync.Mutex
	sources   map[int64]counterSource
	status    onlineCounterStatus
	slots     map[int64]int
	lastKnown map[int64]time.Time
	lastEval  time.Time
	// startedAt starts the unknown grace at process start too, so a restart
	// of the bot (workers not registered yet, first Nitrado read failing)
	// never flashes the channel to "?".
	startedAt time.Time
	poke      chan struct{}
}

func (a *App) counterLoop() *onlineCounterLoop {
	a.onlineLoopOnce.Do(func() {
		a.onlineLoop = &onlineCounterLoop{
			sources:   map[int64]counterSource{},
			slots:     map[int64]int{},
			lastKnown: map[int64]time.Time{},
			startedAt: time.Now(),
			poke:      make(chan struct{}, 1),
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

// pokeOnlineCounter asks the loop to re-evaluate soon. Never blocks.
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

// runOnlineCounter drives the voice counter until ctx ends.
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

	status := onlineCounterStatus{ServerID: serverID, EvaluatedAt: now}
	if haveSource {
		if src.live != nil && src.serviceID != "" {
			liveCtx, cancel := context.WithTimeout(ctx, onlineCounterLiveTimeout)
			in.Live, in.LiveErr = src.live.GameserverLive(liveCtx, src.serviceID)
			cancel()
			in.LiveRead = true
			if in.LiveErr != nil {
				status.NitradoError = in.LiveErr.Error()
			} else {
				status.NitradoStatus = in.Live.Status
				status.NitradoCount = in.Live.PlayerCurrent
			}
		}
		if src.engine != nil {
			in.Tracker = src.engine.PlayerTracker().OnlineCount()
			in.ADMProven = src.engine.PlayerListStats().LastCompleteSnapshotAt
		}
	}
	reading, source := resolveOnlineReading(in)
	status.Reading, status.Source, status.TrackerCount = reading, source, in.Tracker

	l.mu.Lock()
	if reading.Slots > 0 {
		l.slots[serverID] = reading.Slots
	}
	if reading.Known {
		l.lastKnown[serverID] = now
	}
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
			"nitrado_status", status.NitradoStatus, "nitrado_error", status.NitradoError != "")
	}
	if reading.Known && source == counterSourceNitrado && in.Tracker != reading.Count {
		slog.Debug("component=presence", "event", "tracker_differs_from_nitrado", "server_id", serverID, "tracker_count", in.Tracker, "nitrado_count", reading.Count)
	}
	if reading.Known && a.State != nil {
		a.State.SetOnlinePlayers(reading.Count)
	}
	if counter == nil || status.Held {
		return
	}
	// The ONLINE_COUNTER route wins; the legacy GuildSetup channel is only
	// the fallback (re-binding the legacy channel over a routed one made the
	// counter alternate between two channels).
	if channelID, ok := a.onlineCounterRouteChannel(ctx); ok {
		counter.SetChannelID(channelID)
	} else if a.setupStore != nil && a.Config != nil {
		bindOnlineCounter(a.setupStore, a.Config.DiscordGuildID, counter)
	}
	if counter.LastName() == "" {
		// First publish to this channel: its current name is not known yet
		// (fresh process, new route) - read it so an identical name is never
		// renamed and a stale one is corrected.
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
