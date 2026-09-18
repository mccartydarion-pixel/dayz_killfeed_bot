package discord

import (
	"context"
	"errors"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// fakeRouteResolver answers Resolve from a (server -> channel) map and
// records how it was called, standing in for routing.Resolver.
type fakeRouteResolver struct {
	byServer map[int64]string
	err      error
	calls    int
	lastKey  string
}

func (f *fakeRouteResolver) Resolve(ctx context.Context, guildRowID, serverID int64, routeKey string) (string, bool, error) {
	f.calls++
	f.lastKey = routeKey
	if f.err != nil {
		return "", false, f.err
	}
	id, ok := f.byServer[serverID]
	return id, ok, nil
}

func routedPublisher(t *testing.T, resolver RouteResolver, serverID int64, legacyChannel, envChannel string) *KillfeedPublisher {
	t.Helper()
	store := NewInMemorySetupStore()
	if legacyChannel != "" {
		_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: legacyChannel})
	}
	p := NewKillfeedPublisher(&Client{session: &discordgo.Session{}}, envChannel)
	p.BindStore(store, "g1")
	p.SetRouting(resolver, 7, serverID)
	return p
}

func TestKillfeedRouteKeyMatchesSaaSVocabulary(t *testing.T) {
	if routeKeyKillfeed != "KILLFEED" {
		t.Fatalf("route key drifted from the SaaS vocabulary: %q", routeKeyKillfeed)
	}
}

// A configured KILLFEED route is the channel used.
func TestKillfeedUsesRouteWhenPresent(t *testing.T) {
	res := &fakeRouteResolver{byServer: map[int64]string{1: "route-chan"}}
	p := routedPublisher(t, res, 1, "", "")
	if got := p.channelID(); got != "route-chan" {
		t.Fatalf("expected the route channel, got %q", got)
	}
	if res.lastKey != "KILLFEED" {
		t.Fatalf("expected the KILLFEED route to be resolved, got %q", res.lastKey)
	}
}

// No route -> legacy setup channel, then env, exactly as before routes existed.
func TestKillfeedFallsBackToLegacyWhenNoRoute(t *testing.T) {
	res := &fakeRouteResolver{byServer: map[int64]string{}}
	if got := routedPublisher(t, res, 1, "legacy-chan", "env-chan").channelID(); got != "legacy-chan" {
		t.Fatalf("expected the legacy setup channel, got %q", got)
	}
	if got := routedPublisher(t, res, 1, "", "env-chan").channelID(); got != "env-chan" {
		t.Fatalf("expected the env fallback, got %q", got)
	}
}

// Both exist -> exactly one channel is chosen (the route), so nothing can double-post.
func TestKillfeedRouteWinsOverLegacyNoDuplicate(t *testing.T) {
	res := &fakeRouteResolver{byServer: map[int64]string{1: "route-chan"}}
	p := routedPublisher(t, res, 1, "legacy-chan", "env-chan")
	if got := p.channelID(); got != "route-chan" {
		t.Fatalf("expected only the route channel, got %q", got)
	}
}

// End to end: the rotating feed must also post ONLY to the route, and only
// once per embed, even though a legacy channel is configured.
func TestKillfeedFeedPostsOnlyToRouteChannel(t *testing.T) {
	res := &fakeRouteResolver{byServer: map[int64]string{1: "route-chan"}}
	p := routedPublisher(t, res, 1, "legacy-chan", "")
	api := &fakeRotatingFeedAPI{}
	feed := newTestRotatingFeed(api, "legacy-chan")
	feed.SetRouteChannelResolver(p.RouteChannelID)
	p.SetFeed(feed)

	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill"})
	feed.flush()

	if len(api.sent) != 1 || api.sent[0] != "route-chan" {
		t.Fatalf("expected exactly one send to the route channel, got %v", api.sent)
	}
}

// With no route the feed still posts to the legacy channel, unchanged.
func TestKillfeedFeedUsesLegacyWhenNoRoute(t *testing.T) {
	res := &fakeRouteResolver{byServer: map[int64]string{}}
	p := routedPublisher(t, res, 1, "legacy-chan", "")
	api := &fakeRotatingFeedAPI{}
	feed := newTestRotatingFeed(api, "legacy-chan")
	feed.SetRouteChannelResolver(p.RouteChannelID)

	feed.Enqueue(&discordgo.MessageEmbed{Title: "kill"})
	feed.flush()

	if len(api.sent) != 1 || api.sent[0] != "legacy-chan" {
		t.Fatalf("expected one send to the legacy channel, got %v", api.sent)
	}
}

// Two servers in one guild each publish to their own channel.
func TestKillfeedTwoServersSameGuildRouteSeparately(t *testing.T) {
	res := &fakeRouteResolver{byServer: map[int64]string{1: "chan-A", 2: "chan-B"}}
	a := routedPublisher(t, res, 1, "shared-legacy", "")
	b := routedPublisher(t, res, 2, "shared-legacy", "")
	if a.channelID() != "chan-A" || b.channelID() != "chan-B" {
		t.Fatalf("expected server A -> chan-A and server B -> chan-B, got %q and %q", a.channelID(), b.channelID())
	}
	// A server with no route of its own still uses the guild's legacy channel.
	c := routedPublisher(t, res, 3, "shared-legacy", "")
	if c.channelID() != "shared-legacy" {
		t.Fatalf("expected the unrouted server to use legacy, got %q", c.channelID())
	}
}

// A route that changes between lookups is picked up on the next lookup.
func TestKillfeedPicksUpRouteChange(t *testing.T) {
	res := &fakeRouteResolver{byServer: map[int64]string{1: "old-chan"}}
	p := routedPublisher(t, res, 1, "", "")
	if p.channelID() != "old-chan" {
		t.Fatal("expected old-chan first")
	}
	res.byServer[1] = "new-chan"
	if got := p.channelID(); got != "new-chan" {
		t.Fatalf("expected new-chan after the route changed, got %q", got)
	}
}

// A lookup error never propagates - the legacy channel is used.
func TestKillfeedRouteLookupErrorFallsBackToLegacy(t *testing.T) {
	res := &fakeRouteResolver{err: errors.New("db down")}
	p := routedPublisher(t, res, 1, "legacy-chan", "")
	if got := p.channelID(); got != "legacy-chan" {
		t.Fatalf("expected the legacy fallback on a lookup error, got %q", got)
	}
}

// No route and no legacy channel -> nothing to publish to, reported without
// panicking and without ever touching Discord.
func TestKillfeedNoRouteNoLegacySkipsPublish(t *testing.T) {
	res := &fakeRouteResolver{byServer: map[int64]string{}}
	p := routedPublisher(t, res, 1, "", "")
	if got := p.channelID(); got != "" {
		t.Fatalf("expected no channel, got %q", got)
	}
	if err := p.PublishKill(nil); err == nil {
		t.Fatal("expected the existing channel-not-configured result, not a silent success")
	}
}

// Without a resolver the publisher behaves exactly as it did pre-routing.
func TestKillfeedWithoutResolverIsUnchanged(t *testing.T) {
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: "legacy-chan"})
	p := NewKillfeedPublisher(&Client{session: &discordgo.Session{}}, "env-chan")
	p.BindStore(store, "g1")
	if got := p.channelID(); got != "legacy-chan" {
		t.Fatalf("expected legacy, got %q", got)
	}
}
