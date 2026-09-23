package discord

import (
	"errors"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

type admFixture struct {
	t        *testing.T
	resolver *keyedResolver
	api      *fakeDiscord
	setup    *InMemorySetupStore
	saved    []string
}

func newADMFixture(t *testing.T, legacyChannel string) *admFixture {
	t.Helper()
	f := &admFixture{t: t, resolver: newKeyedResolver(), api: newFakeDiscord(), setup: NewInMemorySetupStore()}
	_ = f.setup.Save(GuildSetup{GuildID: "g1", ADMMonitorChannelID: legacyChannel})
	return f
}

// publisher builds the monitor of one server (guild row 7); storedMessage is
// the message id persisted from a previous run.
func (f *admFixture) publisher(serverID int64, storedMessage string, routed bool) *ADMMonitorPublisher {
	p := NewADMMonitorPublisher(f.api, f.setup, "g1", serverID, storedMessage, func(id string) { f.saved = append(f.saved, id) })
	if routed {
		p.SetRouting(f.resolver, 7)
	}
	return p
}

var admTick = 0

// admSnap returns a snapshot that always counts as "important" (new file), so
// every Update publishes instead of waiting out the refresh interval.
func admSnap() killfeed.AdmSnapshot {
	admTick++
	return killfeed.AdmSnapshot{State: killfeed.StatePolling, CurrentFile: "DayZServer_" + string(rune('A'+admTick%26)) + ".ADM"}
}

func TestADMRouteKeyIsAdminLogs(t *testing.T) {
	f := newADMFixture(t, "")
	f.resolver.set(7, 1, "ADMIN_LOGS", "route-chan")
	p := f.publisher(1, "", true)
	p.Update(admSnap())
	if f.api.liveIn("route-chan") != 1 {
		t.Fatalf("expected the ADMIN_LOGS route to be resolved, live=%v", f.api.live())
	}
}

// Legacy ADM monitor channel when no route exists.
func TestADMLegacyFallbackWhenNoRoute(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	p := f.publisher(1, "", true)
	p.Update(admSnap())
	if f.api.liveIn("legacy-adm") != 1 || len(f.api.live()) != 1 {
		t.Fatalf("expected the legacy ADM channel, live=%v", f.api.live())
	}
	if len(f.saved) != 1 {
		t.Fatalf("expected the new message id persisted once, saved=%v", f.saved)
	}
}

// Both a route and a legacy channel: exactly one destination (the route).
func TestADMNewRouteWinsWithoutDuplicate(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	f.resolver.set(7, 1, "ADMIN_LOGS", "route-chan")
	p := f.publisher(1, "", true)
	p.Update(admSnap())
	p.Update(admSnap())
	if f.api.liveIn("route-chan") != 1 || f.api.liveIn("legacy-adm") != 0 || len(f.api.live()) != 1 {
		t.Fatalf("expected only the routed monitor message, live=%v", f.api.live())
	}
	if f.api.sendCount() != 1 {
		t.Fatalf("expected one send then in-place edits, sends=%d", f.api.sendCount())
	}
	p.HandleDownload(killfeed.DownloadReport{Result: "success"})
	if f.api.liveIn("legacy-adm") != 0 {
		t.Fatal("download embeds must also go to the route, never the legacy channel")
	}
}

// A lookup failure falls back to legacy (and does not error).
func TestADMLookupFailureFallsBackToLegacy(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	f.resolver.fail(7, 1, "ADMIN_LOGS", errors.New("db down"))
	p := f.publisher(1, "", true)
	p.Update(admSnap())
	if f.api.liveIn("legacy-adm") != 1 {
		t.Fatalf("expected the legacy channel on a failed lookup, live=%v", f.api.live())
	}
}

// Both absent: safe no-op.
func TestADMNoRouteNoLegacyIsNoOp(t *testing.T) {
	f := newADMFixture(t, "")
	p := f.publisher(1, "", true)
	p.Update(admSnap())
	p.HandleDownload(killfeed.DownloadReport{Result: "failure"})
	if f.api.sendCount() != 0 {
		t.Fatalf("expected nothing published, sends=%d", f.api.sendCount())
	}
}

// Route changed while running: the persistent message MOVES (old one retired,
// one new one posted and persisted) instead of editing a stale id forever.
func TestADMRouteChangeMovesMessage(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	f.resolver.set(7, 1, "ADMIN_LOGS", "chan-A")
	p := f.publisher(1, "", true)
	p.Update(admSnap())

	f.resolver.set(7, 1, "ADMIN_LOGS", "chan-B")
	p.Update(admSnap())

	if f.api.liveIn("chan-A") != 0 || f.api.liveIn("chan-B") != 1 || len(f.api.live()) != 1 {
		t.Fatalf("expected the monitor message to move to chan-B, live=%v", f.api.live())
	}
	if len(f.saved) != 2 {
		t.Fatalf("expected the new message id persisted, saved=%v", f.saved)
	}

	// Route removed: back to the legacy channel, again exactly one message.
	f.resolver.clear(7, 1, "ADMIN_LOGS")
	p.Update(admSnap())
	if f.api.liveIn("chan-B") != 0 || f.api.liveIn("legacy-adm") != 1 || len(f.api.live()) != 1 {
		t.Fatalf("expected a single monitor message back in the legacy channel, live=%v", f.api.live())
	}
}

// The route was set while the process was down: the persisted id belongs to the
// legacy channel. The first update must post once in the routed channel and
// retire the stray legacy copy - never keep failing on an unknown message.
func TestADMRouteSetWhileDownRecreatesInRoutedChannel(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	f.api.seed("legacy-adm", "old-msg")
	f.resolver.set(7, 1, "ADMIN_LOGS", "route-chan")
	p := f.publisher(1, "old-msg", true)

	p.Update(admSnap())

	if f.api.liveIn("route-chan") != 1 || f.api.liveIn("legacy-adm") != 0 {
		t.Fatalf("expected one message in the routed channel and the legacy copy retired, live=%v", f.api.live())
	}
	if len(f.saved) != 1 || f.saved[0] == "old-msg" {
		t.Fatalf("expected the new id persisted, saved=%v", f.saved)
	}
	// Subsequent updates edit it in place.
	p.Update(admSnap())
	if f.api.sendCount() != 1 {
		t.Fatalf("expected in-place edits after recreation, sends=%d", f.api.sendCount())
	}
}

// A transient edit failure on a routed monitor must not post a duplicate.
func TestADMTransientEditErrorDoesNotDuplicate(t *testing.T) {
	f := newADMFixture(t, "")
	f.resolver.set(7, 1, "ADMIN_LOGS", "route-chan")
	p := f.publisher(1, "", true)
	p.Update(admSnap())
	msg := f.saved[0]

	f.api.editErr["route-chan/"+msg] = errors.New("503 upstream")
	p.Update(admSnap())
	if f.api.sendCount() != 1 || f.api.liveIn("route-chan") != 1 {
		t.Fatalf("a transient error must not create a second monitor message, live=%v", f.api.live())
	}
}

// Legacy behaviour is unchanged: an unknown stored message on the legacy
// channel is not recreated (no route involved).
func TestADMLegacyUnknownMessageBehaviourUnchanged(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	p := f.publisher(1, "gone", true)
	p.Update(admSnap())
	if f.api.sendCount() != 0 {
		t.Fatalf("legacy path must not change: expected no re-post, sends=%d", f.api.sendCount())
	}
}

// Two servers of one guild resolve their OWN ADMIN_LOGS channel, and a server
// without a route does not inherit its sibling's.
func TestADMTwoServersSameGuildResolveDifferentChannels(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	f.resolver.set(7, 1, "ADMIN_LOGS", "chan-1")
	f.resolver.set(7, 2, "ADMIN_LOGS", "chan-2")
	a := f.publisher(1, "", true)
	b := f.publisher(2, "", true)
	c := f.publisher(3, "", true) // no route: legacy
	a.Update(admSnap())
	b.Update(admSnap())
	c.Update(admSnap())
	if f.api.liveIn("chan-1") != 1 || f.api.liveIn("chan-2") != 1 || f.api.liveIn("legacy-adm") != 1 {
		t.Fatalf("expected each server in its own channel, live=%v", f.api.live())
	}
}

// Another organization's route (a different guild row) never leaks in.
func TestADMCrossGuildRouteIgnored(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	f.resolver.set(99, 1, "ADMIN_LOGS", "foreign-chan")
	p := f.publisher(1, "", true)
	p.Update(admSnap())
	if f.api.liveIn("foreign-chan") != 0 || f.api.liveIn("legacy-adm") != 1 {
		t.Fatalf("foreign route must be ignored, live=%v", f.api.live())
	}
}

// --- ADM download admin-log noise (Champion Performance Phase 1, section 30) ----

// A routine successful download (with or without new events) must never post
// a standalone admin-log message - only a state change (failure, recovery,
// rotation, checkpoint failure) is worth interrupting the channel for. Before
// this fix, HandleDownload posted a brand-new message for every "success"/
// "success_no_new_events" report, which fires roughly once per poll cycle
// during active gameplay.
func TestADMRoutineDownloadDoesNotPostAMessage(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	p := f.publisher(1, "", true)
	p.HandleDownload(killfeed.DownloadReport{Result: "success"})
	p.HandleDownload(killfeed.DownloadReport{Result: "success_no_new_events"})
	p.HandleDownload(killfeed.DownloadReport{Result: "success"})
	if f.api.sendCount() != 0 {
		t.Fatalf("routine downloads must not post admin-log messages, sends=%d", f.api.sendCount())
	}
}

// A failure, then a recovery, must each still post exactly one message - the
// state-change signal this function exists to preserve.
func TestADMFailureAndRecoveryStillPostMessages(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	p := f.publisher(1, "", true)
	p.HandleDownload(killfeed.DownloadReport{Result: "success"}) // routine: no message
	p.HandleDownload(killfeed.DownloadReport{Result: "failure"}) // state change: message
	p.HandleDownload(killfeed.DownloadReport{Result: "success"}) // recovery: message
	if f.api.sendCount() != 2 {
		t.Fatalf("expected exactly one failure message and one recovery message, sends=%d", f.api.sendCount())
	}
}

// A routine download must not force the separate Update() status panel to
// bypass its own refresh-interval throttle - only a notable download should.
// Before this fix, HandleDownload set forceRefresh unconditionally, so the
// persistent panel re-edited on essentially every poll instead of at most
// every killfeed.ADMMonitorRefreshInterval.
func TestADMRoutineDownloadDoesNotForceStatusPanelRefresh(t *testing.T) {
	f := newADMFixture(t, "legacy-adm")
	p := f.publisher(1, "", true)
	snap := admSnap()
	p.Update(snap) // first Update always publishes (no lastRefresh yet)
	firstEdits := len(f.api.edits)

	p.HandleDownload(killfeed.DownloadReport{Result: "success"})
	// The SAME snapshot (same file, no rotation) arriving immediately after
	// the first Update is "not important" and must be skipped by the refresh
	// interval - unless something incorrectly forced a refresh.
	p.Update(snap)
	p.Update(snap)
	if len(f.api.edits) != firstEdits {
		t.Fatalf("a routine download must not force the status panel to re-edit ahead of its throttle, edits=%v", f.api.edits)
	}

	// A notable download (failure) DOES force an immediate refresh, even for
	// an otherwise-unimportant snapshot.
	p.HandleDownload(killfeed.DownloadReport{Result: "failure"})
	p.Update(snap)
	if len(f.api.edits) != firstEdits+1 {
		t.Fatalf("a notable download must force one immediate status panel refresh, edits=%v", f.api.edits)
	}
}

// --- SetupManager gate: never create a legacy panel next to a routed one ----

func TestSetupManagerSkipsLegacyPanelsForRoutedFeatures(t *testing.T) {
	newManager := func() *SetupManager {
		api := newFakeGuildAPI()
		api.channels = []*discordgo.Channel{{ID: "lb"}, {ID: "stats"}, {ID: "link"}}
		store := NewInMemorySetupStore()
		_ = store.Save(GuildSetup{GuildID: "g1", LeaderboardsChannelID: "lb", PlayerStatsChannelID: "stats", LinkPanelChannelID: "link"})
		return NewSetupManager(api, store, "bot")
	}

	// No gate: all three legacy panels are restored.
	setup, _, err := newManager().RestoreLegacyPanels("g1")
	if err != nil {
		t.Fatal(err)
	}
	if setup.LinkPanelMessageID == "" || setup.PlayerStatsInfoMessageID == "" || setup.LeaderboardMessageID == "" {
		t.Fatalf("expected legacy panels without a gate: %+v", setup)
	}

	// Link and stats routed: those legacy panels are not created; leaderboard is.
	m := newManager()
	m.SetRouteGate(func(key string) bool { return key == routeKeyLinkGamertag || key == routeKeyStatsLeaderboards })
	setup, _, err = m.RestoreLegacyPanels("g1")
	if err != nil {
		t.Fatal(err)
	}
	if setup.LinkPanelMessageID != "" || setup.PlayerStatsInfoMessageID != "" {
		t.Fatalf("routed features must not get legacy panels: %+v", setup)
	}
	if setup.LeaderboardMessageID == "" {
		t.Fatal("an unrouted feature keeps its legacy panel")
	}

	// AUTO_LEADERBOARD routed: no legacy leaderboard message.
	m = newManager()
	m.SetRouteGate(func(key string) bool { return key == routeKeyAutoLeaderboard })
	setup, _, _ = m.RestoreLegacyPanels("g1")
	if setup.LeaderboardMessageID != "" || setup.LinkPanelMessageID == "" {
		t.Fatalf("expected only the leaderboard gated: %+v", setup)
	}
}
