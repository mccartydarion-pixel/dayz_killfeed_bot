package app

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// registerPresenceTracker exposes a running ServerWorker's live PlayerTracker
// for diagnostics, keyed by the exact game_servers row ID it owns.
func (a *App) registerPresenceTracker(serverID int64, tracker *killfeed.PlayerTracker) {
	a.presenceMu.Lock()
	if a.presenceTrackers == nil {
		a.presenceTrackers = make(map[int64]*killfeed.PlayerTracker)
	}
	a.presenceTrackers[serverID] = tracker
	a.presenceMu.Unlock()
}

func (a *App) registerPresenceEngine(serverID int64, engine *killfeed.Engine) {
	a.presenceMu.Lock()
	if a.presenceEngines == nil {
		a.presenceEngines = make(map[int64]*killfeed.Engine)
	}
	a.presenceEngines[serverID] = engine
	a.presenceMu.Unlock()
}

// unregisterPresenceTracker removes a worker's tracker once it stops, so
// diagnostics never read a stale reference for a server that is no longer live.
func (a *App) unregisterPresenceTracker(serverID int64) {
	a.presenceMu.Lock()
	delete(a.presenceTrackers, serverID)
	delete(a.presenceEngines, serverID)
	a.presenceMu.Unlock()
}

func (a *App) livePresenceSnapshot(serverID int64) (killfeed.PresenceSnapshot, bool) {
	a.presenceMu.Lock()
	engine, ok := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if !ok || engine == nil {
		return killfeed.PresenceSnapshot{}, false
	}
	return engine.PresenceSnapshot(), true
}

func (a *App) livePipelineSnapshot(serverID int64) (killfeed.RuntimeDiagnosticSnapshot, bool) {
	a.presenceMu.Lock()
	engine, ok := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if !ok || engine == nil || engine.Diagnostics() == nil {
		return killfeed.RuntimeDiagnosticSnapshot{}, false
	}
	return engine.Diagnostics().Snapshot(), true
}

// selectedServerEngine returns the live Engine for the guild's currently
// selected public server, so live tooling (e.g. the ADM source scan) reuses
// the exact same running Nitrado client instead of constructing a new one.
func (a *App) selectedServerEngine(ctx context.Context) (*killfeed.Engine, int64, bool) {
	if a.Guilds == nil {
		return nil, 0, false
	}
	guild, _, err := a.Guilds.GetGuild(ctx, a.Config.DiscordGuildID)
	if err != nil || guild == nil || guild.SelectedPublicServerID == 0 {
		return nil, 0, false
	}
	a.presenceMu.Lock()
	engine, ok := a.presenceEngines[guild.SelectedPublicServerID]
	a.presenceMu.Unlock()
	if !ok || engine == nil {
		return nil, guild.SelectedPublicServerID, false
	}
	return engine, guild.SelectedPublicServerID, true
}

func (a *App) recordPublicVoicePublish(count int, result string) {
	a.counterOwnerMu.RLock()
	serverID := a.publicCounterServerID
	a.counterOwnerMu.RUnlock()
	a.presenceMu.Lock()
	engine := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if engine != nil {
		engine.RecordVoicePublish(count, result)
	}
}

// livePresenceCount returns the exact running worker's online count for a
// server, or (0, false) if no worker is currently registered for it.
func (a *App) livePresenceCount(serverID int64) (int, bool) {
	a.presenceMu.Lock()
	tracker, ok := a.presenceTrackers[serverID]
	a.presenceMu.Unlock()
	if !ok || tracker == nil {
		return 0, false
	}
	return tracker.OnlineCount(), true
}

// PresenceCounts implements discord.PresenceStatsProvider by summing the
// already-running presence trackers/engines - the same in-memory state
// livePresenceCount/livePipelineSnapshot already read - so the Discord bot
// presence never triggers a second Nitrado poll of its own. ok is false when
// no server worker is registered yet (nothing reliable to show).
func (a *App) PresenceCounts() (totalPlayers, onlineServers, configuredServers int, ok bool) {
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	if len(a.presenceTrackers) == 0 {
		return 0, 0, 0, false
	}
	configuredServers = len(a.presenceTrackers)
	for id, tracker := range a.presenceTrackers {
		if tracker == nil {
			continue
		}
		totalPlayers += tracker.OnlineCount()
		if engine, found := a.presenceEngines[id]; found && engine != nil && engine.Diagnostics() != nil {
			if engine.Diagnostics().Snapshot().WorkerRunning {
				onlineServers++
			}
		}
	}
	return totalPlayers, onlineServers, configuredServers, true
}

func (a *App) ownsPublicCounter(serverID int64) bool {
	a.counterOwnerMu.RLock()
	defer a.counterOwnerMu.RUnlock()
	return a.publicCounterServerID == serverID
}

func diagnosticTime(value time.Time) string {
	if value.IsZero() {
		return "NEVER OBSERVED"
	}
	return value.UTC().Format(time.RFC3339)
}

func diagnosticDuration(value time.Time) string {
	if value.IsZero() {
		return "NEVER"
	}
	duration := time.Since(value)
	if duration < 0 {
		duration = 0
	}
	return duration.Round(time.Second).String()
}

func selectPublicCounterServer(selectedID int64, active []repository.GameServer) (int64, bool) {
	if selectedID > 0 {
		for _, server := range active {
			if server.ID == selectedID {
				return selectedID, true
			}
		}
		return 0, false
	}
	if len(active) == 0 {
		return 0, false
	}
	return active[0].ID, true
}

// addPersistQueue registers a per-server persistence queue for health reporting
// and shutdown draining. Safe for concurrent use across worker goroutines.
func (a *App) addPersistQueue(pq *killfeed.PersistenceQueue) {
	if a == nil || pq == nil {
		return
	}
	a.persistQueuesMu.Lock()
	a.persistQueues = append(a.persistQueues, pq)
	a.persistQueuesMu.Unlock()
}

// removePersistQueue forgets a worker's queue once the worker has stopped, so a
// restarted server never leaves a dead queue in the health report.
func (a *App) removePersistQueue(pq *killfeed.PersistenceQueue) {
	if a == nil || pq == nil {
		return
	}
	a.persistQueuesMu.Lock()
	a.persistQueues = slices.DeleteFunc(a.persistQueues, func(q *killfeed.PersistenceQueue) bool { return q == pq })
	a.persistQueuesMu.Unlock()
	if a.HealthRegistry != nil {
		a.HealthRegistry.Remove(fmt.Sprintf("persistence_queue_%d", pq.ServerID()))
	}
}

// allPersistQueues returns a snapshot copy of the currently known persistence
// queues (one per running server worker).
func (a *App) allPersistQueues() []*killfeed.PersistenceQueue {
	a.persistQueuesMu.Lock()
	defer a.persistQueuesMu.Unlock()
	out := make([]*killfeed.PersistenceQueue, len(a.persistQueues))
	copy(out, a.persistQueues)
	return out
}

// addLocationQueue/allLocationQueues mirror addPersistQueue/allPersistQueues exactly, for the
// Phase 3 location-history pipeline's per-server queues (used by the admin performance snapshot).
func (a *App) addLocationQueue(lq *killfeed.LocationQueue) {
	if a == nil || lq == nil {
		return
	}
	a.locationQueuesMu.Lock()
	a.locationQueues = append(a.locationQueues, lq)
	a.locationQueuesMu.Unlock()
}

func (a *App) removeLocationQueue(lq *killfeed.LocationQueue) {
	if a == nil || lq == nil {
		return
	}
	a.locationQueuesMu.Lock()
	a.locationQueues = slices.DeleteFunc(a.locationQueues, func(q *killfeed.LocationQueue) bool { return q == lq })
	a.locationQueuesMu.Unlock()
}

func (a *App) allLocationQueues() []*killfeed.LocationQueue {
	a.locationQueuesMu.Lock()
	defer a.locationQueuesMu.Unlock()
	out := make([]*killfeed.LocationQueue, len(a.locationQueues))
	copy(out, a.locationQueues)
	return out
}

func (a *App) addRotatingFeed(f *discord.RotatingFeed) {
	a.rotatingFeedsMu.Lock()
	a.rotatingFeeds = append(a.rotatingFeeds, f)
	a.rotatingFeedsMu.Unlock()
}

// removeRotatingFeeds forgets a stopped worker's feeds. Callers must only do
// this after the feeds' Run has returned (WaitDone), so the shutdown flush in
// App.shutdown never loses a feed that still has cards to post.
func (a *App) removeRotatingFeeds(feeds ...*discord.RotatingFeed) {
	if len(feeds) == 0 {
		return
	}
	a.rotatingFeedsMu.Lock()
	a.rotatingFeeds = slices.DeleteFunc(a.rotatingFeeds, func(f *discord.RotatingFeed) bool { return slices.Contains(feeds, f) })
	a.rotatingFeedsMu.Unlock()
}

// allRotatingFeeds returns a snapshot copy of the currently known rotating
// feeds (killfeed + death-feed, one pair per running server worker).
func (a *App) allRotatingFeeds() []*discord.RotatingFeed {
	a.rotatingFeedsMu.Lock()
	defer a.rotatingFeedsMu.Unlock()
	out := make([]*discord.RotatingFeed, len(a.rotatingFeeds))
	copy(out, a.rotatingFeeds)
	return out
}
