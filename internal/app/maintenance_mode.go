package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// Maintenance mode (server_configs.maintenance_mode, switched from the website's Server Hub or
// the Discord /server panel): while it is on, Champion posts none of the game server's public
// feeds - killfeed, death and PvE feed, hit feed, connections, build feed, economy and bounty
// feeds, hot zone, RP boost and perk store posts - and the server status board says the server
// is under maintenance. Kills, deaths and stats are still recorded, and staff alerts, DMs and the
// status board keep going. Feeds are stopped where they take events in, not where they send:
// a feed with no channel falls back to a legacy channel or queues its cards, and either would
// post once maintenance ends.

// maintenanceTTL is how long a maintenance read is reused; switching it clears the server's entry.
const maintenanceTTL = 30 * time.Second

type maintenanceEntry struct {
	on bool
	at time.Time
}

type maintenanceCache struct {
	mu      sync.Mutex
	entries map[int64]maintenanceEntry
}

// serverInMaintenance reports whether game server serverID is in maintenance. A failed read is
// "not in maintenance": feeds are never lost to a database hiccup.
func (a *App) serverInMaintenance(serverID int64) bool {
	if a == nil || serverID <= 0 {
		return false
	}
	now := time.Now()
	a.maintenance.mu.Lock()
	if e, ok := a.maintenance.entries[serverID]; ok && now.Sub(e.at) < maintenanceTTL {
		a.maintenance.mu.Unlock()
		return e.on
	}
	a.maintenance.mu.Unlock()
	if a.ServerPanels == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	on, err := a.ServerPanels.Maintenance(ctx, serverID)
	if err != nil {
		slog.Warn("component=maintenance", "msg", "maintenance read failed", "server_id", serverID, "err", err.Error())
		return false
	}
	a.maintenance.mu.Lock()
	if a.maintenance.entries == nil {
		a.maintenance.entries = map[int64]maintenanceEntry{}
	}
	a.maintenance.entries[serverID] = maintenanceEntry{on: on, at: now}
	a.maintenance.mu.Unlock()
	return on
}

// maintenanceChanged takes a switched maintenance into effect at once: the cached read is dropped
// and the status board is redrawn.
func (a *App) maintenanceChanged(serverID int64) {
	if a == nil {
		return
	}
	a.maintenance.mu.Lock()
	delete(a.maintenance.entries, serverID)
	a.maintenance.mu.Unlock()
	if a.ServerStatusBoard != nil {
		a.ServerStatusBoard.Trigger()
	}
}

// maintenanceGate is one game server's maintenance check for the feeds of its worker.
type maintenanceGate struct {
	a        *App
	serverID int64
}

func (g maintenanceGate) paused() bool { return g.a.serverInMaintenance(g.serverID) }

// The engine's publishers, gated: while the server is in maintenance an event is dropped where
// the feed takes it in (after the kill or death is recorded).

type gatedKillPublisher struct {
	maintenanceGate
	next killfeed.KillPublisher
}

func (g gatedKillPublisher) PublishKill(ev *killfeed.Event) error {
	if g.paused() {
		return nil
	}
	return g.next.PublishKill(ev)
}

type gatedDeathPublisher struct {
	maintenanceGate
	next killfeed.DeathPublisher
}

func (g gatedDeathPublisher) PublishDeath(ev *killfeed.Event) error {
	if g.paused() {
		return nil
	}
	return g.next.PublishDeath(ev)
}

// gatedPveDeathPublisher claims a death it drops, so it doesn't go to the legacy death feed.
type gatedPveDeathPublisher struct {
	maintenanceGate
	next killfeed.PveDeathPublisher
}

func (g gatedPveDeathPublisher) PublishPveDeath(n killfeed.PveDeathNotice) bool {
	if g.paused() {
		return true
	}
	return g.next.PublishPveDeath(n)
}

type gatedHitPublisher struct {
	maintenanceGate
	next killfeed.HitPublisher
}

func (g gatedHitPublisher) PublishHit(ev *killfeed.Event) {
	if !g.paused() {
		g.next.PublishHit(ev)
	}
}

type gatedConnectionPublisher struct {
	maintenanceGate
	next killfeed.ConnectionPublisher
}

func (g gatedConnectionPublisher) PublishConnection(n killfeed.ConnectionNotice) {
	if !g.paused() {
		g.next.PublishConnection(n)
	}
}

// gatedBuildPublisher gates only the public BUILD_FEED; the raid alarm DMs and the Base Black Box
// beside it in the fan-out keep running.
type gatedBuildPublisher struct {
	maintenanceGate
	next killfeed.BuildPublisher
}

func (g gatedBuildPublisher) PublishBuild(ev *killfeed.Event) {
	if !g.paused() {
		g.next.PublishBuild(ev)
	}
}
