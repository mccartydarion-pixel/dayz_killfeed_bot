//go:build integration

package app

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

func suicideNotice(name string) killfeed.PveDeathNotice {
	return killfeed.PveDeathNotice{Cause: killfeed.DeathCauseSuicide, Name: name}
}

// PVE_FEED against real PostgreSQL and the real resolver: two servers of one
// guild resolve their own installation's route; a server without a route claims
// nothing (and never falls back to KILLFEED); an in-process route change is used
// immediately despite a one-hour cache TTL.
func TestRuntimePveFeedRoutesPerServerAndFollowsRouteChanges(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	a.ChannelRoutes = routing.NewResolver(a.SaaSChannelRoutes, time.Hour)
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	for _, id := range []string{"kf-A", "kf-B", "pve-A", "pve-A2", "pve-B"} {
		seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	serverA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	installB := createSecondInstallation(t, a, fixture)
	serverB := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, installB, 222222, fixture.OwnerDiscordID)).Server.ID
	guildRowID := mustGuildRowID(t, a, fixture.DiscordGuildID)

	// Server A: PVE_FEED -> pve-A. Server B: KILLFEED only.
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-A", "PVE_FEED": "pve-A"}); rr.Code != http.StatusOK {
		t.Fatalf("save A: %d %s", rr.Code, rr.Body.String())
	}
	if rr := saveChannelRoutes(t, a, fixture.OrgID, installB, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-B"}); rr.Code != http.StatusOK {
		t.Fatalf("save B: %d %s", rr.Code, rr.Body.String())
	}

	sink := &hitSink{}
	feedA := discord.NewPveFeedPublisher(sink, a.ChannelRoutes, guildRowID, serverA)
	feedB := discord.NewPveFeedPublisher(sink, a.ChannelRoutes, guildRowID, serverB)
	feedA.Flush() // activates from the real route lookup, as Run does at startup
	feedB.Flush()

	if !feedA.PublishPveDeath(suicideNotice("Alice")) {
		t.Fatal("server A has a PVE_FEED route: it must claim")
	}
	if feedB.PublishPveDeath(suicideNotice("Bob")) {
		t.Fatal("server B has no PVE_FEED route: it must not claim (and must not fall back to KILLFEED)")
	}
	feedA.Flush()
	feedB.Flush()
	if sink.count("pve-A") != 1 || len(sink.sent) != 1 || sink.count("kf-A") != 0 || sink.count("kf-B") != 0 {
		t.Fatalf("expected only A's death in pve-A, got %v", sink.sent)
	}

	// Server B gets a route; A moves theirs. Both apply on the next lookup even
	// though the cache TTL is an hour (in-process writes invalidate the cache).
	if rr := saveChannelRoutes(t, a, fixture.OrgID, installB, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-B", "PVE_FEED": "pve-B"}); rr.Code != http.StatusOK {
		t.Fatalf("save B2: %d %s", rr.Code, rr.Body.String())
	}
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-A", "PVE_FEED": "pve-A2"}); rr.Code != http.StatusOK {
		t.Fatalf("save A2: %d %s", rr.Code, rr.Body.String())
	}
	feedA.Flush()
	feedB.Flush()
	if !feedA.PublishPveDeath(suicideNotice("Alice")) || !feedB.PublishPveDeath(suicideNotice("Bob")) {
		t.Fatal("both servers must claim once routed")
	}
	feedA.Flush()
	feedB.Flush()
	if sink.count("pve-A2") != 1 || sink.count("pve-B") != 1 || sink.count("pve-A") != 1 {
		t.Fatalf("expected each server on its own current route (A moved to pve-A2, B on pve-B), got %v", sink.sent)
	}
}

// A PVE_FEED route must never resolve through an installation/server pairing that
// crosses a guild or organization boundary.
func TestRuntimePveFeedRejectsCrossOrganization(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)

	fixtureA := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixtureA.OrgID, fixtureA.OwnerDiscordID)
	for _, id := range []string{"a-kf", "a-pve"} {
		seedGuildChannels(verifier, fixtureA.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	serverA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixtureA.OrgID, fixtureA.InstallationID, 111111, fixtureA.OwnerDiscordID)).Server.ID
	if rr := saveChannelRoutes(t, a, fixtureA.OrgID, fixtureA.InstallationID, fixtureA.OwnerDiscordID, map[string]string{"KILLFEED": "a-kf", "PVE_FEED": "a-pve"}); rr.Code != http.StatusOK {
		t.Fatalf("save A: %d %s", rr.Code, rr.Body.String())
	}
	guildA := mustGuildRowID(t, a, fixtureA.DiscordGuildID)
	ctx := context.Background()
	if got, found, err := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, "PVE_FEED"); err != nil || !found || got != "a-pve" {
		t.Fatalf("baseline: expected a-pve, got %q found=%v err=%v", got, found, err)
	}

	fixtureB := buildInstallationFixture(t, a, verifier)
	for _, id := range []string{"b-kf", "b-pve"} {
		seedGuildChannels(verifier, fixtureB.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	if rr := saveChannelRoutes(t, a, fixtureB.OrgID, fixtureB.InstallationID, fixtureB.OwnerDiscordID, map[string]string{"KILLFEED": "b-kf", "PVE_FEED": "b-pve"}); rr.Code != http.StatusOK {
		t.Fatalf("save B: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE installations SET game_server_id=$2 WHERE id=$1`, fixtureB.InstallationID, serverA); err != nil {
		t.Fatal(err)
	}
	guildB := mustGuildRowID(t, a, fixtureB.DiscordGuildID)
	if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildB, serverA, "PVE_FEED"); found {
		t.Fatalf("B's forced pairing with A's server must not resolve, got %q", got)
	}
	if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, "PVE_FEED"); !found || got != "a-pve" {
		t.Fatalf("A's route must be untouched, got %q found=%v", got, found)
	}

	// Same guild, but the server is claimed by a DIFFERENT organization.
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE game_servers SET organization_id=$2 WHERE id=$1`, serverA, fixtureB.OrgID); err != nil {
		t.Fatal(err)
	}
	if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, "PVE_FEED"); found {
		t.Fatalf("a server claimed by another organization must not resolve through A's installation, got %q", got)
	}

	sink := &hitSink{}
	feed := discord.NewPveFeedPublisher(sink, routing.NewResolver(a.SaaSChannelRoutes, time.Hour), guildA, serverA)
	feed.Flush()
	if feed.PublishPveDeath(suicideNotice("Alice")) {
		t.Fatal("an isolated (cross-organization) server must not claim")
	}
	feed.Flush()
	if len(sink.sent) != 0 {
		t.Fatalf("an isolated server must publish nothing, got %v", sink.sent)
	}
}

// claimProbe wraps a PveDeathPublisher and, at the moment the feed is offered a
// death, checks the database: the death row must already exist.
type claimProbe struct {
	inner     killfeed.PveDeathPublisher
	deaths    *repository.DeathRepository
	countRows func() int
	mu        sync.Mutex
	rowsSeen  []int // deaths rows visible to the DB when each notice arrived
}

func (p *claimProbe) PublishPveDeath(n killfeed.PveDeathNotice) bool {
	p.mu.Lock()
	p.rowsSeen = append(p.rowsSeen, p.countRows())
	p.mu.Unlock()
	return p.inner.PublishPveDeath(n)
}

type legacyDeaths struct {
	mu    sync.Mutex
	types []killfeed.EventType
}

func (l *legacyDeaths) PublishDeath(ev *killfeed.Event) error {
	l.mu.Lock()
	l.types = append(l.types, ev.Type)
	l.mu.Unlock()
	return nil
}

// Persist-before-publish and durable replay dedupe against the REAL deaths table:
// the PVE notice is offered only after the death row is committed, a replay (a
// fresh engine over the same database - i.e. after a restart) publishes nothing
// again, a claimed suicide never also reaches the legacy death feed, and an
// unclaimed one (no route) still does.
func TestRuntimePveFeedPersistsBeforePublishAndDedupesReplays(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	a.ChannelRoutes = routing.NewResolver(a.SaaSChannelRoutes, time.Hour)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	for _, id := range []string{"kf", "pve"} {
		seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	server := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	guildRowID := mustGuildRowID(t, a, fixture.DiscordGuildID)
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf", "PVE_FEED": "pve"}); rr.Code != http.StatusOK {
		t.Fatalf("save routes: %d %s", rr.Code, rr.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deaths := repository.NewDeathRepository(a.DB.Pool)
	adapter := &persistenceStoreAdapter{players: repository.NewPlayerRepository(a.DB.Pool), deaths: deaths}
	countRows := func() int {
		var n int
		_ = a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM deaths WHERE guild_id=$1 AND server_id=$2`, guildRowID, server).Scan(&n)
		return n
	}

	sink := &hitSink{}
	feed := discord.NewPveFeedPublisher(sink, a.ChannelRoutes, guildRowID, server)
	feed.Flush() // route resolved from the real database
	probe := &claimProbe{inner: feed, deaths: deaths, countRows: countRows}

	build := func() (*killfeed.PersistenceQueue, *legacyDeaths) {
		queue := killfeed.NewPersistenceQueueWithServerID(adapter, guildRowID, server, "session")
		go queue.Run(ctx)
		legacy := &legacyDeaths{}
		engine := killfeed.NewEngine(nil, "svc", killfeed.NewADMParser())
		engine.SetPersistence(queue)
		engine.SetPveDeathPublisher(probe)
		engine.SetDeathPublisher(legacy)
		return queue, legacy
	}
	suicideEvent := func() *killfeed.Event {
		return &killfeed.Event{
			Type:      killfeed.EventSuicideAction,
			TimeOfDay: "16:20:00",
			Player:    &killfeed.PlayerRef{Name: "Alice", ID: "pve-integration-alice"},
			Cause:     killfeed.DeathCauseSuicide,
		}
	}

	queue1, legacy1 := build()
	if err := queue1.EnqueueAndWait(ctx, suicideEvent()); err != nil {
		t.Fatalf("persist suicide: %v", err)
	}
	if len(probe.rowsSeen) != 1 || probe.rowsSeen[0] != 1 {
		t.Fatalf("the PVE_FEED must be offered the death only AFTER its row is committed (rows visible at publish: %v)", probe.rowsSeen)
	}
	if len(legacy1.types) != 0 {
		t.Fatalf("a claimed suicide must not also reach the legacy death feed, got %v", legacy1.types)
	}
	feed.Flush()
	if sink.count("pve") != 1 {
		t.Fatalf("expected the suicide published to pve, got %v", sink.sent)
	}

	// A restart: a brand-new engine and queue over the SAME database replay the
	// same line. The durable fingerprint rejects it, so nothing is published again.
	queue2, legacy2 := build()
	if err := queue2.EnqueueAndWait(ctx, suicideEvent()); err != nil {
		t.Fatalf("replay must be accepted as an already-persisted duplicate, got %v", err)
	}
	feed.Flush()
	if len(probe.rowsSeen) != 1 || sink.count("pve") != 1 || countRows() != 1 || len(legacy2.types) != 0 {
		t.Fatalf("a replayed death must not be published or stored again: offers=%d published=%d rows=%d legacy=%v", len(probe.rowsSeen), sink.count("pve"), countRows(), legacy2.types)
	}

	// With the route removed nothing is claimed, so the legacy death feed keeps
	// serving the death exactly as before PVE_FEED existed.
	// (Route saves merge; an empty value deletes a route.)
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf", "PVE_FEED": ""}); rr.Code != http.StatusOK {
		t.Fatalf("remove route: %d %s", rr.Code, rr.Body.String())
	}
	if _, found, _ := a.ChannelRoutes.Resolve(ctx, guildRowID, server, "PVE_FEED"); found {
		t.Fatal("setup: the PVE_FEED route should be gone")
	}
	feed.Flush() // sees the removal
	queue3, legacy3 := build()
	other := suicideEvent()
	other.TimeOfDay = "17:45:00"
	if err := queue3.EnqueueAndWait(ctx, other); err != nil {
		t.Fatalf("persist unclaimed suicide: %v", err)
	}
	if len(legacy3.types) != 1 || legacy3.types[0] != killfeed.EventSuicideAction {
		t.Fatalf("an unclaimed suicide must reach the legacy death feed, got %v", legacy3.types)
	}
	feed.Flush()
	if sink.count("pve") != 1 {
		t.Fatalf("no route: nothing more may be published to the PVE channel, got %v", sink.sent)
	}
}
