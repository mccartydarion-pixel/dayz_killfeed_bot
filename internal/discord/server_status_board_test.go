package discord

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func TestServerStatusShowsOnlyObservedValues(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	healthy := ServerStatusSection{ServerName: "Champions", Seen: true, Snapshot: killfeed.AdmSnapshot{State: killfeed.StatePolling, LastPoll: now.Add(-30 * time.Second), LastLogChange: now.Add(-time.Minute), OnlineCount: 17}}
	text := embedText(BuildServerStatusEmbed([]ServerStatusSection{healthy}, now))
	for _, want := range []string{"📡 SERVER STATUS", "Champions", "Champion Link:** CONNECTED", "Players Online:** 17", "ADM Log:** updated <t:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	for _, invented := range []string{"Uptime", "Latency", "Ping"} {
		if strings.Contains(text, invented) {
			t.Fatalf("%q is not measured and must not appear:\n%s", invented, text)
		}
	}
	stale := healthy
	stale.Snapshot.LastPoll = now.Add(-10 * time.Minute)
	if ServerStatusLink(stale, now) != "DEGRADED" {
		t.Fatal("no poll for 10 minutes is DEGRADED")
	}
	waiting := embedText(BuildServerStatusEmbed([]ServerStatusSection{{ServerName: "New"}}, now))
	if !strings.Contains(waiting, "WAITING FOR FIRST POLL") || strings.Contains(waiting, "Players Online") {
		t.Fatalf("before any snapshot nothing is invented:\n%s", waiting)
	}
}

func TestServerStatusBoardOneMessageEditedInPlace(t *testing.T) {
	f := newRoutedFixture(t)
	f.resolver.set(f.guild, 1, routeKeyServerStatus, "chan-status")
	servers := func(context.Context) (int64, []int64, error) { return f.guild, f.servers, nil }
	b := NewServerStatusBoard(f.resolver, servers, f.panels)
	ctx := context.Background()
	b.SyncOnce(ctx)
	b.Observe(1, killfeed.AdmSnapshot{State: killfeed.StatePolling, LastPoll: time.Now(), OnlineCount: 3})
	b.SyncOnce(ctx)
	b.SyncOnce(ctx)
	if f.api.sendCount() != 1 || f.api.liveIn("chan-status") != 1 {
		t.Fatalf("one status message, edited in place, live=%v", f.api.live())
	}
	// A restart (fresh board, same durable store) edits the same message.
	NewServerStatusBoard(f.resolver, servers, NewRoutePanels(f.api, f.store)).SyncOnce(ctx)
	if f.api.sendCount() != 1 {
		t.Fatal("restart must not duplicate the status message")
	}
	// Route change moves it without leaving a copy.
	f.resolver.set(f.guild, 1, routeKeyServerStatus, "chan-new")
	b.SyncOnce(ctx)
	if f.api.liveIn("chan-new") != 1 || f.api.liveIn("chan-status") != 0 {
		t.Fatalf("status must follow its route, live=%v", f.api.live())
	}
	var nilBoard *ServerStatusBoard
	nilBoard.Observe(1, killfeed.AdmSnapshot{})
	nilBoard.SyncOnce(ctx)
	nilBoard.Trigger()
}
