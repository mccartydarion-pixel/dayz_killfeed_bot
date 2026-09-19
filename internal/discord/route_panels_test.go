package discord

import (
	"context"
	"errors"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/routing"
)

// The four migrated route keys must stay identical to the SaaS vocabulary.
func TestMigratedRouteKeysMatchRoutingPackage(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{routeKeyLinkGamertag, routing.RouteLinkGamertag},
		{routeKeyStatsLeaderboards, routing.RouteStatsLeaderboards},
		{routeKeyAutoLeaderboard, routing.RouteAutoLeaderboard},
		{routeKeyAdminLogs, routing.RouteAdminLogs},
	} {
		if c.got != c.want {
			t.Fatalf("route key drifted: %q != %q", c.got, c.want)
		}
	}
}

func TestRouteBindingSemantics(t *testing.T) {
	res := newKeyedResolver()
	res.set(7, 1, routeKeyAdminLogs, "chan-1")

	if got := NewRouteBinding(res, 7, 1, routeKeyAdminLogs).ChannelID(); got != "chan-1" {
		t.Fatalf("route wins: got %q", got)
	}
	// The same guild, a different server: no route (resolved per server, never per guild).
	if got := NewRouteBinding(res, 7, 2, routeKeyAdminLogs).ChannelID(); got != "" {
		t.Fatalf("other server must not inherit the route, got %q", got)
	}
	// A different guild with the same server id: no route (isolation).
	if got := NewRouteBinding(res, 8, 1, routeKeyAdminLogs).ChannelID(); got != "" {
		t.Fatalf("other guild must not resolve, got %q", got)
	}
	// A different key: no route.
	if got := NewRouteBinding(res, 7, 1, routeKeyLinkGamertag).ChannelID(); got != "" {
		t.Fatalf("other key must not resolve, got %q", got)
	}
	// Lookup failure: fall back (empty), never fatal.
	res.fail(7, 1, routeKeyAdminLogs, errors.New("db down"))
	if got := NewRouteBinding(res, 7, 1, routeKeyAdminLogs).ChannelID(); got != "" {
		t.Fatalf("lookup error must fall back, got %q", got)
	}
	// nil-safe
	var nilBinding *RouteBinding
	if nilBinding.ChannelID() != "" || NewRouteBinding(nil, 7, 1, "X").ChannelID() != "" {
		t.Fatal("nil binding / resolver must resolve nothing")
	}
}

func TestResolveGuildRouteChannelsDedupesAndReportsErrors(t *testing.T) {
	res := newKeyedResolver()
	res.set(7, 1, routeKeyLinkGamertag, "chan-A")
	res.set(7, 2, routeKeyLinkGamertag, "chan-A") // two servers, same channel
	res.set(7, 3, routeKeyLinkGamertag, "chan-B")
	res.fail(7, 4, routeKeyLinkGamertag, errors.New("boom"))
	chs, errs := resolveGuildRouteChannels(context.Background(), res, 7, []int64{1, 2, 3, 4, 5}, routeKeyLinkGamertag)
	if len(chs) != 2 || chs[0] != "chan-A" || chs[1] != "chan-B" || errs != 1 {
		t.Fatalf("expected [chan-A chan-B] with 1 error, got %v errs=%d", chs, errs)
	}
}

func TestRoutePanelsCreatesOncePerChannelAndIsIdempotent(t *testing.T) {
	f := newRoutedFixture(t)
	content := PanelContent{Embed: LinkUsernameInfoEmbed(), Components: LinkUsernamePanelComponents()}
	ctx := context.Background()

	res, err := f.panels.Sync(ctx, f.guild, routeKeyLinkGamertag, []string{"chan-A", "chan-B"}, content, false, true)
	if err != nil || res.Created != 2 || res.Errors != 0 {
		t.Fatalf("first sync: %+v err=%v", res, err)
	}
	// Repeating the sync (restart, timer, trigger) must not post again.
	for i := 0; i < 3; i++ {
		res, err = f.panels.Sync(ctx, f.guild, routeKeyLinkGamertag, []string{"chan-A", "chan-B"}, content, false, true)
		if err != nil || res.Created != 0 {
			t.Fatalf("repeat sync %d created a duplicate: %+v err=%v", i, res, err)
		}
	}
	if f.api.sendCount() != 2 || f.api.liveIn("chan-A") != 1 || f.api.liveIn("chan-B") != 1 {
		t.Fatalf("expected exactly one live panel per channel, live=%v", f.api.live())
	}
}

func TestRoutePanelsRouteChangeMovesPanelWithoutLeavingACopy(t *testing.T) {
	f := newRoutedFixture(t)
	content := PanelContent{Embed: LinkUsernameInfoEmbed()}
	ctx := context.Background()
	_, _ = f.panels.Sync(ctx, f.guild, routeKeyLinkGamertag, []string{"chan-old"}, content, false, true)
	oldMsg := f.store.messageFor(f.guild, routeKeyLinkGamertag, "chan-old")

	res, err := f.panels.Sync(ctx, f.guild, routeKeyLinkGamertag, []string{"chan-new"}, content, false, true)
	if err != nil || res.Created != 1 || res.Removed != 1 {
		t.Fatalf("route change: %+v err=%v", res, err)
	}
	if f.api.liveIn("chan-old") != 0 || f.api.liveIn("chan-new") != 1 {
		t.Fatalf("expected the panel to move, live=%v", f.api.live())
	}
	if f.store.count(f.guild, routeKeyLinkGamertag) != 1 || f.store.messageFor(f.guild, routeKeyLinkGamertag, "chan-old") != "" {
		t.Fatal("expected only the new channel recorded")
	}
	if f.api.deletes[0] != "chan-old/"+oldMsg {
		t.Fatalf("expected the old message retired, deletes=%v", f.api.deletes)
	}
}

func TestRoutePanelsRecreatesOnlyWhenMessageIsGoneNotOnTransientErrors(t *testing.T) {
	f := newRoutedFixture(t)
	content := PanelContent{Embed: LinkUsernameInfoEmbed()}
	ctx := context.Background()
	_, _ = f.panels.Sync(ctx, f.guild, routeKeyLinkGamertag, []string{"chan-A"}, content, false, true)
	msg := f.store.messageFor(f.guild, routeKeyLinkGamertag, "chan-A")

	// Transient failure while verifying: must NOT post a second panel.
	f.api.getErr["chan-A/"+msg] = errors.New("503 upstream")
	res, _ := f.panels.Sync(ctx, f.guild, routeKeyLinkGamertag, []string{"chan-A"}, content, false, true)
	if res.Created != 0 || res.Errors != 1 || f.api.sendCount() != 1 {
		t.Fatalf("a transient error must not duplicate the panel: %+v sends=%d", res, f.api.sendCount())
	}
	delete(f.api.getErr, "chan-A/"+msg)

	// Message deleted by a moderator: recreated exactly once, and recorded.
	_ = f.api.ChannelMessageDelete("chan-A", msg)
	res, _ = f.panels.Sync(ctx, f.guild, routeKeyLinkGamertag, []string{"chan-A"}, content, false, true)
	if res.Created != 1 || f.api.liveIn("chan-A") != 1 || f.store.messageFor(f.guild, routeKeyLinkGamertag, "chan-A") == msg {
		t.Fatalf("expected a fresh recorded panel: %+v live=%v", res, f.api.live())
	}
}

func TestRoutePanelsUnrecordedSendIsRetiredSoNextSyncCannotDuplicate(t *testing.T) {
	f := newRoutedFixture(t)
	f.store.upErr = errors.New("db write failed")
	res, _ := f.panels.Sync(context.Background(), f.guild, routeKeyLinkGamertag, []string{"chan-A"}, PanelContent{Embed: LinkUsernameInfoEmbed()}, false, true)
	if res.Errors != 1 || f.api.liveIn("chan-A") != 0 {
		t.Fatalf("an unrecorded panel must be retired: %+v live=%v", res, f.api.live())
	}
}

// LINK_GAMERTAG / STATS_LEADERBOARDS: the new route wins and is the ONLY panel.
func TestSyncerRoutedPanelsWinAndRetireLegacyPanel(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		legacyCh  func(*GuildSetup, string)
		legacyMsg func(*GuildSetup, string)
		read      func(*GuildSetup) string
	}{
		{"LINK_GAMERTAG", routeKeyLinkGamertag,
			func(g *GuildSetup, v string) { g.LinkPanelChannelID = v }, func(g *GuildSetup, v string) { g.LinkPanelMessageID = v },
			func(g *GuildSetup) string { return g.LinkPanelMessageID }},
		{"STATS_LEADERBOARDS", routeKeyStatsLeaderboards,
			func(g *GuildSetup, v string) { g.PlayerStatsChannelID = v }, func(g *GuildSetup, v string) { g.PlayerStatsInfoMessageID = v },
			func(g *GuildSetup) string { return g.PlayerStatsInfoMessageID }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRoutedFixture(t)
			gs := GuildSetup{GuildID: "g1"}
			tc.legacyCh(&gs, "legacy-chan")
			tc.legacyMsg(&gs, "legacy-msg")
			_ = f.setup.Save(gs)
			f.api.seed("legacy-chan", "legacy-msg")
			f.resolver.set(f.guild, 1, tc.key, "route-chan")

			f.syncer.SyncOnce(context.Background())

			if f.api.liveIn("route-chan") != 1 {
				t.Fatalf("expected the routed panel, live=%v", f.api.live())
			}
			if f.api.liveIn("legacy-chan") != 0 {
				t.Fatalf("expected the legacy panel retired (no duplicate), live=%v", f.api.live())
			}
			if got, _ := f.setup.Get("g1"); tc.read(got) != "" {
				t.Fatalf("expected the legacy message id cleared, got %q", tc.read(got))
			}
			// Repeated syncs never add a second copy.
			f.syncer.SyncOnce(context.Background())
			f.syncer.SyncOnce(context.Background())
			if len(f.api.live()) != 1 {
				t.Fatalf("expected exactly one live panel overall, live=%v", f.api.live())
			}
		})
	}
}

// No route configured: the syncer does nothing for that key, so the legacy
// panel (owned by SetupManager) is untouched.
func TestSyncerLegacyFallbackLeavesLegacyPanelAlone(t *testing.T) {
	f := newRoutedFixture(t)
	_ = f.setup.Save(GuildSetup{GuildID: "g1", LinkPanelChannelID: "legacy-chan", LinkPanelMessageID: "legacy-msg", PlayerStatsChannelID: "legacy-stats", PlayerStatsInfoMessageID: "legacy-stats-msg"})
	f.api.seed("legacy-chan", "legacy-msg")
	f.api.seed("legacy-stats", "legacy-stats-msg")

	f.syncer.SyncOnce(context.Background())

	if f.api.sendCount() != 0 || len(f.api.deletes) != 0 {
		t.Fatalf("no route: expected no posts/deletes, sends=%d deletes=%v", f.api.sendCount(), f.api.deletes)
	}
	if got, _ := f.setup.Get("g1"); got.LinkPanelMessageID != "legacy-msg" || got.PlayerStatsInfoMessageID != "legacy-stats-msg" {
		t.Fatalf("legacy ids must be untouched: %+v", got)
	}
	if f.syncer.HasRoute(routeKeyLinkGamertag) || f.syncer.HasRoute(routeKeyStatsLeaderboards) {
		t.Fatal("HasRoute must be false with no routes")
	}
}

// A failed lookup means "unknown", not "no route": nothing is retired, and
// HasRoute (the SetupManager gate) reports legacy.
func TestSyncerLookupFailureFallsBackSafely(t *testing.T) {
	f := newRoutedFixture(t)
	f.resolver.set(f.guild, 1, routeKeyLinkGamertag, "route-chan")
	f.syncer.SyncOnce(context.Background())
	if f.api.liveIn("route-chan") != 1 {
		t.Fatal("setup: expected the routed panel")
	}

	f.resolver.clear(f.guild, 1, routeKeyLinkGamertag)
	f.resolver.fail(f.guild, 1, routeKeyLinkGamertag, errors.New("db down"))
	// Force a re-evaluation past the verify window.
	f.syncer.state[routeKeyLinkGamertag].verified = f.syncer.state[routeKeyLinkGamertag].verified.Add(-2 * routePanelVerifyEvery)
	f.syncer.SyncOnce(context.Background())

	if f.api.liveIn("route-chan") != 1 || f.store.count(f.guild, routeKeyLinkGamertag) != 1 {
		t.Fatalf("a lookup failure must not tear down the routed panel, live=%v", f.api.live())
	}
	if f.syncer.HasRoute(routeKeyLinkGamertag) {
		t.Fatal("HasRoute must report legacy on a failed lookup")
	}
}

// Two servers of one guild with different routes: the guild-level panel is
// published to each server's own channel - and the same channel only once.
func TestSyncerTwoServersSameGuildResolveDifferentChannels(t *testing.T) {
	f := newRoutedFixture(t)
	f.resolver.set(f.guild, 1, routeKeyLinkGamertag, "chan-A")
	f.resolver.set(f.guild, 2, routeKeyLinkGamertag, "chan-B")
	f.resolver.set(f.guild, 1, routeKeyStatsLeaderboards, "chan-S")
	f.resolver.set(f.guild, 2, routeKeyStatsLeaderboards, "chan-S") // shared channel

	f.syncer.SyncOnce(context.Background())

	if f.api.liveIn("chan-A") != 1 || f.api.liveIn("chan-B") != 1 {
		t.Fatalf("expected one link panel in each server's channel, live=%v", f.api.live())
	}
	if f.api.liveIn("chan-S") != 1 {
		t.Fatalf("two servers sharing a channel must share one stats panel, live=%v", f.api.live())
	}
}

// A route of another guild/organization (different guild row) never leaks in.
func TestSyncerCrossGuildIsolation(t *testing.T) {
	f := newRoutedFixture(t)
	f.resolver.set(99, 1, routeKeyLinkGamertag, "other-orgs-channel") // same server id, other guild
	f.syncer.SyncOnce(context.Background())
	if f.api.sendCount() != 0 {
		t.Fatalf("a foreign guild's route must never publish here, live=%v", f.api.live())
	}
}

// Route change propagates on the next sync; the old copy is retired and the
// route removal falls back to legacy via the restore hook.
func TestSyncerRouteChangeAndRemoval(t *testing.T) {
	f := newRoutedFixture(t)
	restored := 0
	f.syncer.SetLegacyRestore(func() { restored++ })
	f.resolver.set(f.guild, 1, routeKeyStatsLeaderboards, "chan-old")
	f.syncer.SyncOnce(context.Background())

	f.resolver.set(f.guild, 1, routeKeyStatsLeaderboards, "chan-new")
	f.syncer.SyncOnce(context.Background())
	if f.api.liveIn("chan-old") != 0 || f.api.liveIn("chan-new") != 1 {
		t.Fatalf("route change must move the panel, live=%v", f.api.live())
	}

	f.resolver.clear(f.guild, 1, routeKeyStatsLeaderboards)
	f.syncer.SyncOnce(context.Background())
	if len(f.api.live()) != 0 || restored != 1 {
		t.Fatalf("route removal must retire the routed panel and restore legacy once: live=%v restored=%d", f.api.live(), restored)
	}
}

// Trigger coalesces and never blocks; nil-safe.
func TestSyncerTriggerIsNonBlockingAndNilSafe(t *testing.T) {
	f := newRoutedFixture(t)
	for i := 0; i < 10; i++ {
		f.syncer.Trigger()
	}
	var nilSyncer *RouteSyncer
	nilSyncer.Trigger()
	nilSyncer.SyncOnce(context.Background())
	if nilSyncer.HasRoute("X") {
		t.Fatal("nil syncer has no routes")
	}
}
