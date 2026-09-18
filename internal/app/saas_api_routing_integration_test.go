//go:build integration

package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// Runtime channel routing (KILLFEED) - SQL-level coverage of the resolver's
// installation lookup and its invalidation from the SaaS route writes.

func mustGuildRowID(t *testing.T, a *App, discordGuildID string) int64 {
	t.Helper()
	_, id, err := a.Guilds.GetGuild(context.Background(), discordGuildID)
	if err != nil || id == 0 {
		t.Fatalf("expected a guilds row for %s: id=%d err=%v", discordGuildID, id, err)
	}
	return id
}

// Two DayZ servers in the SAME Discord guild: each resolves to its own
// installation's KILLFEED channel, never the other server's.
func TestRuntimeResolvesKillfeedPerServerInOneGuild(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	seedGuildChannels(verifier, fixture.DiscordGuildID,
		fakeGuildChannel{ID: "chan-A", Name: "killfeed-a", Type: discordgo.ChannelTypeGuildText},
		fakeGuildChannel{ID: "chan-B", Name: "killfeed-b", Type: discordgo.ChannelTypeGuildText},
	)

	first := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID))
	secondInstallationID := createSecondInstallation(t, a, fixture)
	second := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, secondInstallationID, 222222, fixture.OwnerDiscordID))
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "chan-A"}); rr.Code != http.StatusOK {
		t.Fatalf("save A: %d %s", rr.Code, rr.Body.String())
	}
	if rr := saveChannelRoutes(t, a, fixture.OrgID, secondInstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "chan-B"}); rr.Code != http.StatusOK {
		t.Fatalf("save B: %d %s", rr.Code, rr.Body.String())
	}

	guildRowID := mustGuildRowID(t, a, fixture.DiscordGuildID)
	gotA, foundA, err := a.SaaSChannelRoutes.ResolveChannel(context.Background(), guildRowID, first.Server.ID, "KILLFEED")
	if err != nil || !foundA || gotA != "chan-A" {
		t.Fatalf("server A: expected chan-A, got %q found=%v err=%v", gotA, foundA, err)
	}
	gotB, foundB, err := a.SaaSChannelRoutes.ResolveChannel(context.Background(), guildRowID, second.Server.ID, "KILLFEED")
	if err != nil || !foundB || gotB != "chan-B" {
		t.Fatalf("server B: expected chan-B, got %q found=%v err=%v", gotB, foundB, err)
	}
	if _, found, _ := a.SaaSChannelRoutes.ResolveChannel(context.Background(), guildRowID, first.Server.ID, "PVE_FEED"); found {
		t.Fatal("expected an unconfigured route key to resolve as not found")
	}
}

// A route must never resolve through an installation/server pairing that
// crosses a guild or organization boundary, even if the rows were forced
// into that shape directly in the database.
func TestRuntimeResolverRejectsCrossOrganizationRoutes(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	fixtureA := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixtureA.OrgID, fixtureA.OwnerDiscordID)
	seedGuildChannels(verifier, fixtureA.DiscordGuildID, fakeGuildChannel{ID: "chan-A", Name: "killfeed", Type: discordgo.ChannelTypeGuildText})
	serverA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixtureA.OrgID, fixtureA.InstallationID, 111111, fixtureA.OwnerDiscordID)).Server.ID
	if rr := saveChannelRoutes(t, a, fixtureA.OrgID, fixtureA.InstallationID, fixtureA.OwnerDiscordID, map[string]string{"KILLFEED": "chan-A"}); rr.Code != http.StatusOK {
		t.Fatalf("save A: %d %s", rr.Code, rr.Body.String())
	}
	guildA := mustGuildRowID(t, a, fixtureA.DiscordGuildID)
	if got, found, err := a.SaaSChannelRoutes.ResolveChannel(context.Background(), guildA, serverA, "KILLFEED"); err != nil || !found || got != "chan-A" {
		t.Fatalf("baseline: expected chan-A, got %q found=%v err=%v", got, found, err)
	}

	// Organization B forces its installation to point at A's server (only
	// possible via direct SQL) and configures its own KILLFEED route.
	fixtureB := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixtureB.DiscordGuildID, fakeGuildChannel{ID: "chan-B", Name: "killfeed", Type: discordgo.ChannelTypeGuildText})
	if rr := saveChannelRoutes(t, a, fixtureB.OrgID, fixtureB.InstallationID, fixtureB.OwnerDiscordID, map[string]string{"KILLFEED": "chan-B"}); rr.Code != http.StatusOK {
		t.Fatalf("save B: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := a.DB.Pool.Exec(context.Background(), `UPDATE installations SET game_server_id=$2 WHERE id=$1`, fixtureB.InstallationID, serverA); err != nil {
		t.Fatal(err)
	}
	guildB := mustGuildRowID(t, a, fixtureB.DiscordGuildID)
	if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(context.Background(), guildB, serverA, "KILLFEED"); found {
		t.Fatalf("expected B's forced pairing with A's server (a different guild) NOT to resolve, got %q", got)
	}
	if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(context.Background(), guildA, serverA, "KILLFEED"); !found || got != "chan-A" {
		t.Fatalf("expected A's route untouched, got %q found=%v", got, found)
	}

	// Same guild, but the server is claimed by a DIFFERENT organization.
	if _, err := a.DB.Pool.Exec(context.Background(), `UPDATE game_servers SET organization_id=$2 WHERE id=$1`, serverA, fixtureB.OrgID); err != nil {
		t.Fatal(err)
	}
	if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(context.Background(), guildA, serverA, "KILLFEED"); found {
		t.Fatalf("expected a server claimed by another organization NOT to resolve through A's installation, got %q", got)
	}
}

// With a long TTL the cache alone would serve the old channel, but a SaaS
// route write invalidates it, so the very next runtime lookup sees the change.
func TestRuntimeRouteChangePropagatesThroughResolver(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	a.ChannelRoutes = routing.NewResolver(a.SaaSChannelRoutes, time.Hour)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	seedGuildChannels(verifier, fixture.DiscordGuildID,
		fakeGuildChannel{ID: "chan-old", Name: "old", Type: discordgo.ChannelTypeGuildText},
		fakeGuildChannel{ID: "chan-new", Name: "new", Type: discordgo.ChannelTypeGuildText},
	)
	server := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "chan-old"}); rr.Code != http.StatusOK {
		t.Fatalf("save old: %d %s", rr.Code, rr.Body.String())
	}
	guildRowID := mustGuildRowID(t, a, fixture.DiscordGuildID)
	if got, _, _ := a.ChannelRoutes.Resolve(context.Background(), guildRowID, server, "KILLFEED"); got != "chan-old" {
		t.Fatalf("expected chan-old, got %q", got)
	}
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "chan-new"}); rr.Code != http.StatusOK {
		t.Fatalf("save new: %d %s", rr.Code, rr.Body.String())
	}
	if got, _, _ := a.ChannelRoutes.Resolve(context.Background(), guildRowID, server, "KILLFEED"); got != "chan-new" {
		t.Fatalf("expected the runtime resolver to see chan-new immediately, got %q", got)
	}
}
