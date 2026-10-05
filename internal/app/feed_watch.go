package app

import (
	"context"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/ownerops"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The feed watch (docs/OWNER_OPS.md "Silent feeds", docs/SERVER_STATUS.md): a small sampler that
// remembers, per server and in memory only, since when players have been online, since when the
// log source has been failing and when the last log line arrived. The silent-feed incident and
// the owner-facing server status both read it. Nothing is stored: after a restart every clock
// starts again, which is also what keeps a deploy from raising an alert.

const feedWatchInterval = 30 * time.Second

// feedSample is one observation of one server's worker, gathered from memory.
type feedSample struct {
	WorkerRunning  bool
	PlayersKnown   bool
	PlayersOnline  int
	PlayersSource  string
	ServerStopped  bool
	SourceState    string
	LastLogLineAt  time.Time // last time new log bytes were read
	LastLogCheckAt time.Time // last completed poll cycle
	LastPlayerList time.Time // last player list read (position lines)
}

// feedWatchEntry is what the watch remembers for one server.
type feedWatchEntry struct {
	watchingSince  time.Time
	playersSince   time.Time // zero while nobody is (known to be) online
	sourceBadSince time.Time // zero while reads work
	// lastLogLineAt and lastPlayerList are the newest seen over the entry's life: a worker that
	// is restarted (self-healing) starts with empty counters, and that must not look like a
	// server that never wrote a player list or like fresh silence.
	lastLogLineAt  time.Time
	lastPlayerList time.Time
	last           feedSample
	lastAt         time.Time
}

type feedWatch struct {
	mu      sync.Mutex
	entries map[int64]*feedWatchEntry
}

// observe folds one sample into the entry for serverID.
func (w *feedWatch) observe(serverID int64, s feedSample, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.entries == nil {
		w.entries = map[int64]*feedWatchEntry{}
	}
	if !s.WorkerRunning {
		// No worker: there is nothing to time, and a restarted worker starts fresh.
		delete(w.entries, serverID)
		return
	}
	e := w.entries[serverID]
	if e == nil {
		e = &feedWatchEntry{watchingSince: now}
		w.entries[serverID] = e
	}
	switch {
	case s.PlayersKnown && s.PlayersOnline > 0:
		if e.playersSince.IsZero() {
			e.playersSince = now
		}
	case s.PlayersKnown:
		e.playersSince = time.Time{}
	}
	// An unknown count neither starts nor stops the clock: one failed Nitrado read must not
	// reset an hour of evidence, and must not invent players either.
	if s.SourceState == killfeed.ADMTransportError {
		if e.sourceBadSince.IsZero() {
			e.sourceBadSince = now
		}
	} else {
		e.sourceBadSince = time.Time{}
	}
	if s.LastLogLineAt.After(e.lastLogLineAt) {
		e.lastLogLineAt = s.LastLogLineAt
	}
	if s.LastPlayerList.After(e.lastPlayerList) {
		e.lastPlayerList = s.LastPlayerList
	}
	s.LastLogLineAt, s.LastPlayerList = e.lastLogLineAt, e.lastPlayerList
	e.last, e.lastAt = s, now
}

// feedWatchView is the watch's answer for one server at one moment.
type feedWatchView struct {
	Watching         bool
	Sample           feedSample
	PlayersOnlineFor time.Duration
	LogSilentFor     time.Duration
	SourceBadFor     time.Duration
	PlayerListSeen   bool
}

func (w *feedWatch) view(serverID int64, now time.Time) feedWatchView {
	w.mu.Lock()
	defer w.mu.Unlock()
	e := w.entries[serverID]
	if e == nil {
		return feedWatchView{}
	}
	v := feedWatchView{Watching: true, Sample: e.last, PlayerListSeen: !e.last.LastPlayerList.IsZero()}
	if !e.playersSince.IsZero() {
		v.PlayersOnlineFor = now.Sub(e.playersSince)
	}
	if !e.sourceBadSince.IsZero() {
		v.SourceBadFor = now.Sub(e.sourceBadSince)
	}
	// Silence is counted from the last line this process read, or from when it began watching
	// when it has read none - never from before the process existed.
	from := e.watchingSince
	if e.last.LastLogLineAt.After(from) {
		from = e.last.LastLogLineAt
	}
	v.LogSilentFor = now.Sub(from)
	return v
}

// sampleServer reads one server's worker state from memory. No database, no Nitrado.
func (a *App) sampleServer(serverID int64, now time.Time) feedSample {
	var s feedSample
	if serverID <= 0 || a.WorkerManager == nil || !a.WorkerManager.Running(serverID) {
		return s
	}
	a.presenceMu.Lock()
	eng := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if eng == nil {
		return s
	}
	s.WorkerRunning = true
	h := eng.SourceHealth()
	s.SourceState, _ = killfeed.ClassifyADMSourceHealth(h, now)
	s.LastLogCheckAt = h.LastCycleAt
	if d := eng.Diagnostics(); d != nil {
		s.LastLogLineAt = d.Snapshot().LastSourceGrowthAt
	}
	s.LastPlayerList = eng.PlayerListStats().LastSnapshotAt
	s.PlayersSource = counterSourceUnknown
	// One authority for the count: the online counter's reading when it watches this server
	// (Nitrado first), else this worker's own tracker once a player list or a boot proved it.
	if st := a.OnlineCounterStatus(); st.ServerID == serverID && !st.EvaluatedAt.IsZero() && now.Sub(st.EvaluatedAt) < 5*time.Minute {
		if st.Reading.Known {
			s.PlayersKnown, s.PlayersOnline, s.PlayersSource = true, st.Reading.Count, st.Source
		}
		s.ServerStopped = st.Source == counterSourceNitradoStopped
		return s
	}
	if p := eng.PresenceSnapshot(); p.Presence.Known {
		s.PlayersKnown, s.PlayersOnline, s.PlayersSource = true, p.OnlineCount, counterSourceADMPlayerList
		if p.Presence.State == killfeed.PresenceBootReset {
			s.PlayersSource = counterSourceADMBootReset
		}
	}
	return s
}

// watchedServerIDs lists the servers that have a registered worker engine.
func (a *App) watchedServerIDs() []int64 {
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	ids := make([]int64, 0, len(a.presenceEngines))
	for id := range a.presenceEngines {
		ids = append(ids, id)
	}
	return ids
}

// feedWatchTick samples every watched server once, and forgets servers whose worker is gone.
func (a *App) feedWatchTick(now time.Time) {
	seen := map[int64]bool{}
	for _, id := range a.watchedServerIDs() {
		seen[id] = true
		a.feedWatch.observe(id, a.sampleServer(id, now), now)
	}
	a.feedWatch.mu.Lock()
	for id := range a.feedWatch.entries {
		if !seen[id] {
			delete(a.feedWatch.entries, id)
		}
	}
	a.feedWatch.mu.Unlock()
}

// runFeedWatch samples until ctx ends. It runs in every process: the watch is this process's
// own memory, and its API answers from it.
func (a *App) runFeedWatch(ctx context.Context) {
	ticker := time.NewTicker(feedWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.feedWatchTick(now)
		}
	}
}

// watchedServerState is the full ownerops view of one installation's server: the database
// facts, the worker's state and what the feed watch has timed.
func (a *App) watchedServerState(f repository.FleetFact, rt serverRuntime, runtimeReady bool, now time.Time) ownerops.ServerState {
	s := serverState(f, rt, runtimeReady)
	v := a.feedWatch.view(f.ServerID, now)
	if !v.Watching {
		return s
	}
	s.PlayersKnown, s.PlayersOnline, s.ServerStopped = v.Sample.PlayersKnown, v.Sample.PlayersOnline, v.Sample.ServerStopped
	s.PlayersOnlineFor, s.LogSilentFor, s.SourceBadFor, s.PlayerListSeen = v.PlayersOnlineFor, v.LogSilentFor, v.SourceBadFor, v.PlayerListSeen
	return s
}
