//go:build integration

package app

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// countingStore wraps the real repository so tests can prove cache hits
// versus database queries, and inject a lookup failure.
type countingStore struct {
	inner *repository.ChannelRouteRepository
	calls atomic.Int64
	fail  atomic.Bool
}

func (c *countingStore) ResolveChannel(ctx context.Context, guildRowID, serverID int64, routeKey string) (string, bool, error) {
	c.calls.Add(1)
	if c.fail.Load() {
		return "", false, errors.New("injected lookup failure")
	}
	return c.inner.ResolveChannel(ctx, guildRowID, serverID, routeKey)
}

// A single configured server resolves to its channel; a missing route, an
// unknown server, and the wrong guild all report not-found cleanly (no error).
func TestRuntimeSingleServerAndMissingRoutes(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: "chan-1", Name: "killfeed", Type: discordgo.ChannelTypeGuildText})
	server := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "chan-1"}); rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	guild := mustGuildRowID(t, a, fixture.DiscordGuildID)
	ctx := context.Background()

	if got, found, err := a.SaaSChannelRoutes.ResolveChannel(ctx, guild, server, "KILLFEED"); err != nil || !found || got != "chan-1" {
		t.Fatalf("single server: got %q found=%v err=%v", got, found, err)
	}
	if got, found, err := a.SaaSChannelRoutes.ResolveChannel(ctx, guild, server, "ADMIN_LOGS"); err != nil || found || got != "" {
		t.Fatalf("missing route must be a clean not-found: got %q found=%v err=%v", got, found, err)
	}
	if _, found, err := a.SaaSChannelRoutes.ResolveChannel(ctx, guild, server+9999, "KILLFEED"); err != nil || found {
		t.Fatalf("unknown server must be a clean not-found: found=%v err=%v", found, err)
	}
	// Cross-guild: another guild's row id with this server must not resolve.
	other := buildInstallationFixture(t, a, verifier)
	otherGuild := mustGuildRowID(t, a, other.DiscordGuildID)
	if _, found, err := a.SaaSChannelRoutes.ResolveChannel(ctx, otherGuild, server, "KILLFEED"); err != nil || found {
		t.Fatalf("guild B must not resolve guild A's server route: found=%v err=%v", found, err)
	}
}

// Real-database cache behaviour: first lookup queries, repeats hit the cache,
// the key separates servers in one guild and route keys, and an in-process
// route mutation invalidates so the next lookup re-queries and is fresh.
func TestRuntimeCacheAgainstRealDatabase(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	counter := &countingStore{inner: a.SaaSChannelRoutes}
	a.ChannelRoutes = routing.NewResolver(counter, time.Hour)
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	seedGuildChannels(verifier, fixture.DiscordGuildID,
		fakeGuildChannel{ID: "chan-A", Name: "a", Type: discordgo.ChannelTypeGuildText},
		fakeGuildChannel{ID: "chan-B", Name: "b", Type: discordgo.ChannelTypeGuildText},
		fakeGuildChannel{ID: "chan-A2", Name: "a2", Type: discordgo.ChannelTypeGuildText},
	)
	srvA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	inst2 := createSecondInstallation(t, a, fixture)
	srvB := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, inst2, 222222, fixture.OwnerDiscordID)).Server.ID
	saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "chan-A"})
	saveChannelRoutes(t, a, fixture.OrgID, inst2, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "chan-B"})
	guild := mustGuildRowID(t, a, fixture.DiscordGuildID)
	ctx := context.Background()
	a.ChannelRoutes.InvalidateAll()
	counter.calls.Store(0)

	if got, _, _ := a.ChannelRoutes.Resolve(ctx, guild, srvA, "KILLFEED"); got != "chan-A" {
		t.Fatalf("first lookup: %q", got)
	}
	if counter.calls.Load() != 1 {
		t.Fatalf("first lookup must query the database once, got %d", counter.calls.Load())
	}
	for i := 0; i < 5; i++ {
		a.ChannelRoutes.Resolve(ctx, guild, srvA, "KILLFEED")
	}
	if counter.calls.Load() != 1 {
		t.Fatalf("repeat lookups inside the TTL must be cache hits, got %d queries", counter.calls.Load())
	}
	if got, _, _ := a.ChannelRoutes.Resolve(ctx, guild, srvB, "KILLFEED"); got != "chan-B" {
		t.Fatalf("server B must not collide with server A's cache entry, got %q", got)
	}
	if counter.calls.Load() != 2 {
		t.Fatalf("server B needs its own query, got %d", counter.calls.Load())
	}
	if _, found, _ := a.ChannelRoutes.Resolve(ctx, guild, srvA, "ADMIN_LOGS"); found {
		t.Fatal("unconfigured route key must not reuse KILLFEED's entry")
	}
	if counter.calls.Load() != 3 {
		t.Fatalf("route key is part of the cache key, got %d queries", counter.calls.Load())
	}
	saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "chan-A2"})
	if got, _, _ := a.ChannelRoutes.Resolve(ctx, guild, srvA, "KILLFEED"); got != "chan-A2" {
		t.Fatalf("after invalidation expected chan-A2, got %q", got)
	}
	if counter.calls.Load() != 4 {
		t.Fatalf("invalidation must force exactly one re-query, got %d", counter.calls.Load())
	}
}

// Re-associating an installation with a different DayZ server changes which
// server resolves to its routes; the cache must not keep serving the old one.
func TestRuntimeServerChangeInvalidatesCache(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	a.ChannelRoutes = routing.NewResolver(a.SaaSChannelRoutes, time.Hour)
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: "chan-1", Name: "killfeed", Type: discordgo.ChannelTypeGuildText})
	srvA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "chan-1"})
	guild := mustGuildRowID(t, a, fixture.DiscordGuildID)
	ctx := context.Background()

	if got, _, _ := a.ChannelRoutes.Resolve(ctx, guild, srvA, "KILLFEED"); got != "chan-1" {
		t.Fatalf("server A before the change: %q", got)
	}
	srvB := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 222222, fixture.OwnerDiscordID)).Server.ID
	if got, found, _ := a.ChannelRoutes.Resolve(ctx, guild, srvB, "KILLFEED"); !found || got != "chan-1" {
		t.Fatalf("server B must resolve to the installation's route right after re-selection, got %q found=%v", got, found)
	}
	if got, found, _ := a.ChannelRoutes.Resolve(ctx, guild, srvA, "KILLFEED"); found {
		t.Fatalf("server A is no longer associated and must not keep resolving from cache, got %q", got)
	}
}

// fakeFeedAPI records where the rotating feed actually posts.
type fakeFeedAPI struct{ sent []string }

func (f *fakeFeedAPI) ChannelMessagesBulkDelete(channelID string, messages []string, options ...discordgo.RequestOption) error {
	return nil
}

func (f *fakeFeedAPI) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.sent = append(f.sent, channelID)
	return &discordgo.Message{ID: "m"}, nil
}

// End to end against the real database: a real KillfeedPublisher backed by
// the real resolver publishes a kill through the rotating feed. Covers the
// route-wins / route-absent / lookup-error / nothing-configured cases and
// asserts every kill is delivered exactly once, to exactly one channel.
func TestRuntimeKillfeedPublisherAndFeedAgainstRealDatabase(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	counter := &countingStore{inner: a.SaaSChannelRoutes}
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: "route-chan", Name: "killfeed", Type: discordgo.ChannelTypeGuildText})
	server := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	guild := mustGuildRowID(t, a, fixture.DiscordGuildID)

	client, err := discord.New("integration-test-token")
	if err != nil {
		t.Fatal(err)
	}
	ev := &killfeed.Event{
		Type:   killfeed.EventPlayerKill,
		Victim: &killfeed.PlayerRef{Name: "Victim"},
		Killer: &killfeed.PlayerRef{Name: "Killer"},
	}

	// run publishes one kill and returns every channel the feed posted to.
	run := func(legacy string) ([]string, error) {
		legacyStore := discord.NewInMemorySetupStore()
		if legacy != "" {
			_ = legacyStore.Save(discord.GuildSetup{GuildID: "g1", KillfeedChannelID: legacy})
		}
		pub := discord.NewKillfeedPublisher(client, "")
		pub.BindStore(legacyStore, "g1")
		pub.SetRouting(routing.NewResolver(counter, time.Hour), guild, server)
		api := &fakeFeedAPI{}
		feed := discord.NewRotatingFeed(api, legacyStore, "g1", func(s *discord.GuildSetup) string { return s.KillfeedChannelID }, time.Hour, 10)
		feed.SetRouteChannelResolver(pub.RouteChannelID)
		pub.SetFeed(feed)
		ctx, cancel := context.WithCancel(context.Background())
		go feed.Run(ctx)
		pubErr := pub.PublishKill(ev)
		cancel()
		feed.WaitDone() // Run's final on-shutdown flush posts what was queued
		return api.sent, pubErr
	}

	// 1. No route yet: legacy channel, exactly once.
	if sent, err := run("legacy-chan"); err != nil || len(sent) != 1 || sent[0] != "legacy-chan" {
		t.Fatalf("route absent: expected one post to legacy-chan, got %v err=%v", sent, err)
	}
	// 2. Nothing configured anywhere: safely skipped, nothing posted.
	if sent, err := run(""); err == nil || len(sent) != 0 {
		t.Fatalf("nothing configured: expected a skip and no posts, got %v err=%v", sent, err)
	}
	// 3. Route configured AND legacy set: route only, once - never both.
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "route-chan"}); rr.Code != http.StatusOK {
		t.Fatalf("save route: %d %s", rr.Code, rr.Body.String())
	}
	if sent, err := run("legacy-chan"); err != nil || len(sent) != 1 || sent[0] != "route-chan" {
		t.Fatalf("route+legacy: expected exactly one post to route-chan, got %v err=%v", sent, err)
	}
	// 4. Lookup failure: legacy fallback, kill still delivered once.
	counter.fail.Store(true)
	if sent, err := run("legacy-chan"); err != nil || len(sent) != 1 || sent[0] != "legacy-chan" {
		t.Fatalf("lookup error: expected one post to legacy-chan, got %v err=%v", sent, err)
	}
}
