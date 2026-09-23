package discord

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func newAlertFixture() (*AdminAlertPublisher, *fakeHitSender, *keyedResolver, *time.Time) {
	sender := &fakeHitSender{}
	resolver := newKeyedResolver()
	clock := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	p := NewAdminAlertPublisher(sender, resolver)
	p.now = func() time.Time { return clock }
	return p, sender, resolver, &clock
}

// drain sends everything queued, synchronously.
func drain(p *AdminAlertPublisher) {
	for {
		select {
		case a := <-p.queue:
			p.send(context.Background(), a)
		default:
			return
		}
	}
}

func TestAdminAlertADMStaleAlertsOnceAndResolves(t *testing.T) {
	p, sender, resolver, clock := newAlertFixture()
	resolver.set(7, 1, routeKeyAdminAlerts, "chan-admin")
	snap := killfeed.AdmSnapshot{OnlineCount: 4, LastLogChange: clock.Add(-10 * time.Minute)}

	for i := 0; i < 3; i++ { // every poll reports the same stale state
		p.ObserveSnapshot(7, 1, snap)
	}
	drain(p)
	msgs := sender.messages("chan-admin")
	if len(msgs) != 1 {
		t.Fatalf("a stale log alerts once, not once per poll: got %d", len(msgs))
	}
	e := msgs[0].embeds[0]
	if e.Title != "🚨 ADMIN ALERT" || !strings.Contains(e.Description, "ADM STALE") || e.Color != presentation.WarningAmber {
		t.Fatalf("want an amber ADM STALE alert, got %+v", e)
	}

	snap.LastLogChange = *clock
	p.ObserveSnapshot(7, 1, snap)
	drain(p)
	msgs = sender.messages("chan-admin")
	if len(msgs) != 2 || msgs[1].embeds[0].Title != "✅ ALERT RESOLVED" || msgs[1].embeds[0].Color != presentation.SuccessGreen {
		t.Fatalf("want one resolution, got %d messages", len(msgs))
	}
}

func TestAdminAlertNoPlayersOnlineIsNotStale(t *testing.T) {
	p, sender, resolver, clock := newAlertFixture()
	resolver.set(7, 1, routeKeyAdminAlerts, "chan-admin")
	p.ObserveSnapshot(7, 1, killfeed.AdmSnapshot{OnlineCount: 0, LastLogChange: clock.Add(-time.Hour)})
	drain(p)
	if sender.total() != 0 {
		t.Fatal("an empty server's quiet log is not an alert")
	}
}

func TestAdminAlertNitradoFailureAfterConsecutiveFailures(t *testing.T) {
	p, sender, resolver, _ := newAlertFixture()
	resolver.set(7, 1, routeKeyAdminAlerts, "chan-admin")
	fail := killfeed.DownloadReport{ServerID: 1, Result: "failure", ErrorClass: "TIMEOUT"}

	p.ObserveDownload(7, fail)
	p.ObserveDownload(7, fail)
	p.ObserveDownload(7, killfeed.DownloadReport{ServerID: 1, Result: "success"}) // resets the streak
	p.ObserveDownload(7, fail)
	p.ObserveDownload(7, fail)
	drain(p)
	if sender.total() != 0 {
		t.Fatal("non-consecutive failures must not alert")
	}
	p.ObserveDownload(7, fail)
	p.ObserveDownload(7, fail)
	drain(p)
	msgs := sender.messages("chan-admin")
	if len(msgs) != 1 || !strings.Contains(msgs[0].embeds[0].Description, "NITRADO API FAILURE") || msgs[0].embeds[0].Color != presentation.ErrorRed {
		t.Fatalf("want one red NITRADO API FAILURE, got %d", len(msgs))
	}
	p.ObserveDownload(7, killfeed.DownloadReport{ServerID: 1, Result: "success_no_new_events"})
	drain(p)
	if msgs = sender.messages("chan-admin"); len(msgs) != 2 || msgs[1].embeds[0].Title != "✅ ALERT RESOLVED" {
		t.Fatalf("want a resolution after recovery, got %d", len(msgs))
	}
}

func TestAdminAlertNoRouteSendsNothing(t *testing.T) {
	p, sender, _, clock := newAlertFixture()
	p.ObserveSnapshot(7, 1, killfeed.AdmSnapshot{OnlineCount: 2, LastLogChange: clock.Add(-time.Hour)})
	drain(p)
	if sender.total() != 0 {
		t.Fatal("no ADMIN_ALERTS route: nothing is sent, no fallback")
	}
}

func TestIntrusionAdminAlert(t *testing.T) {
	zoneChannel := "chan-zone"
	zone := repository.Zone{GuildID: 7, ServerID: 1, Name: "North Base", ZoneType: "BASE", AlertChannelID: &zoneChannel}
	a, ok := IntrusionAdminAlert(killfeed.IntrusionEvent{Kind: killfeed.AlertUAVIntrusion, Zone: zone, Gamertag: "Raider_1", At: time.Now()})
	if !ok || a.Kind != AlertKindUAVIntrusion || a.GuildRowID != 7 || a.ServerID != 1 || a.SkipChannel != zoneChannel || !strings.Contains(a.Detail, "Raider\\_1") {
		t.Fatalf("unexpected UAV alert: %+v", a)
	}
	if b, _ := IntrusionAdminAlert(killfeed.IntrusionEvent{Kind: killfeed.AlertZoneBanViolation, Zone: zone}); b.Severity != AlertCritical {
		t.Fatal("a ban violation is critical")
	}
	for _, ev := range []killfeed.IntrusionEvent{{Kind: killfeed.AlertZoneExit, Zone: zone}, {Kind: killfeed.AlertZoneIntrusion, Zone: zone, Suppressed: true}} {
		if _, ok := IntrusionAdminAlert(ev); ok {
			t.Fatalf("%s (suppressed=%v) is not an alert", ev.Kind, ev.Suppressed)
		}
	}

	// Never posted twice into the zone's own channel.
	p, sender, resolver, _ := newAlertFixture()
	resolver.set(7, 1, routeKeyAdminAlerts, zoneChannel)
	p.Publish(a)
	drain(p)
	if sender.total() != 0 {
		t.Fatal("ADMIN_ALERTS resolving to the zone's own alert channel must not duplicate the alert")
	}
}

func TestAdminAlertPublishNeverBlocks(t *testing.T) {
	p, _, _, _ := newAlertFixture()
	done := make(chan struct{})
	go func() {
		for i := 0; i < adminAlertQueueSize*3; i++ {
			p.Publish(AdminAlert{Kind: AlertKindADMStale})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full queue")
	}
	var nilP *AdminAlertPublisher
	nilP.Publish(AdminAlert{})
	nilP.ObserveSnapshot(1, 1, killfeed.AdmSnapshot{})
	nilP.ObserveDownload(1, killfeed.DownloadReport{})
}
