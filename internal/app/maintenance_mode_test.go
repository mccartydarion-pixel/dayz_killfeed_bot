package app

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

type countingFeeds struct{ kills, deaths, hits, conns, builds, pve int }

func (c *countingFeeds) PublishKill(*killfeed.Event) error           { c.kills++; return nil }
func (c *countingFeeds) PublishDeath(*killfeed.Event) error          { c.deaths++; return nil }
func (c *countingFeeds) PublishHit(*killfeed.Event)                  { c.hits++ }
func (c *countingFeeds) PublishConnection(killfeed.ConnectionNotice) { c.conns++ }
func (c *countingFeeds) PublishBuild(*killfeed.Event)                { c.builds++ }
func (c *countingFeeds) PublishPveDeath(killfeed.PveDeathNotice) bool {
	c.pve++
	return true
}

// While a server is in maintenance its public feeds take no events; the build fan-out's other
// consumers (raid alarm, Black Box) keep getting them.
func TestMaintenanceGatesPublicFeeds(t *testing.T) {
	a := &App{}
	a.maintenance.entries = map[int64]maintenanceEntry{7: {on: true, at: time.Now()}, 8: {on: false, at: time.Now()}}
	send := func(serverID int64) (*countingFeeds, *countingFeeds) {
		feeds, other := &countingFeeds{}, &countingFeeds{}
		g := maintenanceGate{a: a, serverID: serverID}
		_ = gatedKillPublisher{g, feeds}.PublishKill(&killfeed.Event{})
		_ = gatedDeathPublisher{g, feeds}.PublishDeath(&killfeed.Event{})
		gatedHitPublisher{g, feeds}.PublishHit(&killfeed.Event{})
		gatedConnectionPublisher{g, feeds}.PublishConnection(killfeed.ConnectionNotice{})
		buildPublisherFanout{gatedBuildPublisher{g, feeds}, other}.PublishBuild(&killfeed.Event{})
		if claimed := (gatedPveDeathPublisher{g, feeds}).PublishPveDeath(killfeed.PveDeathNotice{}); !claimed {
			t.Errorf("server %d: a PvE death must be claimed so it never reaches the legacy death feed", serverID)
		}
		return feeds, other
	}
	paused, other := send(7)
	if *paused != (countingFeeds{}) || other.builds != 1 {
		t.Fatalf("in maintenance: feeds %+v, raid alarm %+v", *paused, *other)
	}
	live, _ := send(8)
	if *live != (countingFeeds{1, 1, 1, 1, 1, 1}) {
		t.Fatalf("not in maintenance: %+v", *live)
	}
	a.maintenanceChanged(7)
	if a.serverInMaintenance(7) {
		t.Fatal("a switched server is read again (no repository here: not in maintenance)")
	}
}
