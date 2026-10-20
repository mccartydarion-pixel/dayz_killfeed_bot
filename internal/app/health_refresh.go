package app

import (
	"context"
	"fmt"
	"time"

	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/operations"
)

// OverallHealth implements discord.PresenceHealthProvider by reusing the
// application's existing health registry (populated by refreshHealth) -
// presence never runs its own duplicate health evaluation.
func (a *App) OverallHealth() health.State {
	if a.HealthRegistry == nil {
		return health.Healthy
	}
	return a.HealthRegistry.Snapshot().Overall
}

// stateHealthComponents maps the runtime state snapshot (server.State.Snapshot) to its health
// components. Keys must match the snapshot contract: Nitrado authentication is "nitrado_connected"
// - the snapshot never had a "nitrado_authenticated" key, which left the Nitrado component
// permanently DEGRADED (and the bot's Discord status idle) until Live Sync phase 2.1.
func stateHealthComponents(snap map[string]any) []health.Component {
	flag := func(key string) bool { v, _ := snap[key].(bool); return v }
	pick := func(ok bool, bad health.State) health.State {
		if ok {
			return health.Healthy
		}
		return bad
	}
	return []health.Component{
		{Name: "database", State: pick(flag("database_connected"), health.Unhealthy), Critical: true},
		{Name: "discord", State: pick(flag("discord_connected"), health.Degraded)},
		{Name: "nitrado", State: pick(flag("nitrado_connected"), health.Degraded)},
		{Name: "adm_pipeline", State: pick(flag("log_source_found"), health.Unhealthy), Critical: true},
	}
}

func (a *App) refreshHealth(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	reportedQueues := make(map[string]bool)
	update := func() {
		if a.HealthRegistry == nil || a.State == nil {
			return
		}
		snap := a.State.Snapshot()
		for _, c := range stateHealthComponents(snap) {
			a.HealthRegistry.Set(c)
		}
		for _, c := range a.admSourceComponents(time.Now()) {
			a.HealthRegistry.Set(c)
		}
		a.HealthRegistry.Set(leaderHealthComponent(a.Leader.Status()))
		if a.ADMHealth != nil {
			var poll, change time.Time
			if v, ok := snap["last_poll"].(string); ok {
				poll, _ = time.Parse(time.RFC3339, v)
			}
			if v, ok := snap["last_log_change"].(string); ok {
				change, _ = time.Parse(time.RFC3339, v)
			}
			online, _ := snap["online_players"].(int)
			file, _ := snap["log_filename"].(string)
			st, reason := a.ADMHealth.Evaluate(operations.ADMHealthSnapshot{LastPollSuccessAt: poll, LastChangeAt: change, OnlinePlayers: online, CurrentFile: file}, time.Now())
			a.HealthRegistry.Set(health.Component{Name: "adm_stall", State: st, Message: reason, Critical: st == health.Unhealthy})
		}
		// Queues belong to running workers only (runServerWorker's exit path
		// removes them); a component reported last tick for a queue that has
		// since gone is removed, so a stopped server never reports a dead queue.
		seen := make(map[string]bool)
		for _, pq := range a.allPersistQueues() {
			depth, capacity, highWater, dropped, oldest := pq.QueueHealth()
			name := fmt.Sprintf("persistence_queue_%d", pq.ServerID())
			seen[name] = true
			q := health.EvaluateQueue(health.QueueHealth{Name: name, Depth: depth, Capacity: capacity, HighWaterMark: highWater, Dropped: uint64(dropped), OldestAge: oldest})
			a.HealthRegistry.Set(health.Component{Name: name, State: q.State, Message: fmt.Sprintf("queue %d/%d high-water %d oldest %s", depth, capacity, highWater, oldest.Round(time.Second)), Critical: true})
		}
		for name := range reportedQueues {
			if !seen[name] {
				a.HealthRegistry.Remove(name)
			}
		}
		reportedQueues = seen
	}
	update()
	for {
		select {
		case <-ticker.C:
			update()
		case <-ctx.Done():
			return
		}
	}
}
