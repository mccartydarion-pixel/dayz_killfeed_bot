//go:build integration

package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

func connectedNotice(name string) killfeed.ConnectionNotice {
	return killfeed.ConnectionNotice{Kind: killfeed.ConnectionConnected, Name: name}
}

// CONNECTIONS against real PostgreSQL and the real resolver: two servers of one
// guild resolve their own installation's route, a server without a route
// publishes nothing (no KILLFEED/HITFEED fallback), and an in-process route
// change is used immediately despite a one-hour cache TTL. hitSink (the
// channel-recording Discord fake) is shared with the HITFEED integration test.
func TestRuntimeConnectionsRoutesPerServerAndFollowsRouteChanges(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	a.ChannelRoutes = routing.NewResolver(a.SaaSChannelRoutes, time.Hour)
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	for _, id := range []string{"kf-A", "kf-B", "hit-B", "conn-A", "conn-A2", "conn-B"} {
		seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	serverA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	installB := createSecondInstallation(t, a, fixture)
	serverB := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, installB, 222222, fixture.OwnerDiscordID)).Server.ID
	guildRowID := mustGuildRowID(t, a, fixture.DiscordGuildID)

	// Server A: CONNECTIONS -> conn-A. Server B: KILLFEED and HITFEED only.
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-A", "CONNECTIONS": "conn-A"}); rr.Code != http.StatusOK {
		t.Fatalf("save A: %d %s", rr.Code, rr.Body.String())
	}
	if rr := saveChannelRoutes(t, a, fixture.OrgID, installB, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-B", "HITFEED": "hit-B"}); rr.Code != http.StatusOK {
		t.Fatalf("save B: %d %s", rr.Code, rr.Body.String())
	}

	sink := &hitSink{}
	feedA := discord.NewConnectionsPublisher(sink, a.ChannelRoutes, guildRowID, serverA)
	feedB := discord.NewConnectionsPublisher(sink, a.ChannelRoutes, guildRowID, serverB)
	feedA.Flush() // activates from the real route lookup, as Run does at startup
	feedB.Flush()

	feedA.PublishConnection(connectedNotice("Alice"))
	feedB.PublishConnection(connectedNotice("Bob"))
	feedA.Flush()
	feedB.Flush()
	if sink.count("conn-A") != 1 {
		t.Fatalf("expected server A's notice in conn-A, got %v", sink.sent)
	}
	if len(sink.sent) != 1 || sink.count("kf-B") != 0 || sink.count("hit-B") != 0 || sink.count("kf-A") != 0 {
		t.Fatalf("server B has no CONNECTIONS route: nothing may be published (no KILLFEED/HITFEED fallback), got %v", sink.sent)
	}

	// Server B gets a route; A moves theirs. Both apply on the next lookup even
	// though the cache TTL is an hour (in-process writes invalidate the cache).
	if rr := saveChannelRoutes(t, a, fixture.OrgID, installB, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-B", "CONNECTIONS": "conn-B"}); rr.Code != http.StatusOK {
		t.Fatalf("save B2: %d %s", rr.Code, rr.Body.String())
	}
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-A", "CONNECTIONS": "conn-A2"}); rr.Code != http.StatusOK {
		t.Fatalf("save A2: %d %s", rr.Code, rr.Body.String())
	}
	feedA.Flush()
	feedB.Flush() // pick up the changed routes
	feedA.PublishConnection(connectedNotice("Alice"))
	feedB.PublishConnection(connectedNotice("Bob"))
	feedA.Flush()
	feedB.Flush()

	if sink.count("conn-A2") != 1 || sink.count("conn-B") != 1 || sink.count("conn-A") != 1 {
		t.Fatalf("expected each server on its own current route (A moved to conn-A2, B on conn-B), got %v", sink.sent)
	}
}

// A CONNECTIONS route must never resolve through an installation/server pairing
// that crosses a guild or organization boundary.
func TestRuntimeConnectionsRejectCrossOrganization(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)

	fixtureA := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixtureA.OrgID, fixtureA.OwnerDiscordID)
	for _, id := range []string{"a-kf", "a-conn"} {
		seedGuildChannels(verifier, fixtureA.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	serverA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixtureA.OrgID, fixtureA.InstallationID, 111111, fixtureA.OwnerDiscordID)).Server.ID
	if rr := saveChannelRoutes(t, a, fixtureA.OrgID, fixtureA.InstallationID, fixtureA.OwnerDiscordID, map[string]string{"KILLFEED": "a-kf", "CONNECTIONS": "a-conn"}); rr.Code != http.StatusOK {
		t.Fatalf("save A: %d %s", rr.Code, rr.Body.String())
	}
	guildA := mustGuildRowID(t, a, fixtureA.DiscordGuildID)
	ctx := context.Background()
	if got, found, err := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, "CONNECTIONS"); err != nil || !found || got != "a-conn" {
		t.Fatalf("baseline: expected a-conn, got %q found=%v err=%v", got, found, err)
	}

	// Organization B forces its installation onto A's server (only possible via
	// direct SQL) and configures its own CONNECTIONS route.
	fixtureB := buildInstallationFixture(t, a, verifier)
	for _, id := range []string{"b-kf", "b-conn"} {
		seedGuildChannels(verifier, fixtureB.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	if rr := saveChannelRoutes(t, a, fixtureB.OrgID, fixtureB.InstallationID, fixtureB.OwnerDiscordID, map[string]string{"KILLFEED": "b-kf", "CONNECTIONS": "b-conn"}); rr.Code != http.StatusOK {
		t.Fatalf("save B: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE installations SET game_server_id=$2 WHERE id=$1`, fixtureB.InstallationID, serverA); err != nil {
		t.Fatal(err)
	}
	guildB := mustGuildRowID(t, a, fixtureB.DiscordGuildID)
	if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildB, serverA, "CONNECTIONS"); found {
		t.Fatalf("B's forced pairing with A's server must not resolve, got %q", got)
	}
	if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, "CONNECTIONS"); !found || got != "a-conn" {
		t.Fatalf("A's route must be untouched, got %q found=%v", got, found)
	}

	// Same guild, but the server is claimed by a DIFFERENT organization.
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE game_servers SET organization_id=$2 WHERE id=$1`, serverA, fixtureB.OrgID); err != nil {
		t.Fatal(err)
	}
	if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, "CONNECTIONS"); found {
		t.Fatalf("a server claimed by another organization must not resolve through A's installation, got %q", got)
	}

	// And the publisher wired to that isolated lookup publishes nothing.
	sink := &hitSink{}
	feed := discord.NewConnectionsPublisher(sink, routing.NewResolver(a.SaaSChannelRoutes, time.Hour), guildA, serverA)
	feed.Flush()
	feed.PublishConnection(connectedNotice("Alice"))
	feed.Flush()
	if len(sink.sent) != 0 {
		t.Fatalf("an isolated (cross-organization) server must publish nothing, got %v", sink.sent)
	}
}
