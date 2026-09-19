//go:build integration

package app

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// hitSink records which channel each hit message was sent to.
type hitSink struct {
	mu   sync.Mutex
	sent map[string]int
}

func (h *hitSink) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sent == nil {
		h.sent = map[string]int{}
	}
	h.sent[channelID] += len(data.Embeds)
	return &discordgo.Message{ID: "m", ChannelID: channelID}, nil
}

func (h *hitSink) count(channel string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sent[channel]
}

func integrationHit(attacker, victim string) *killfeed.Event {
	return &killfeed.Event{
		Type:     killfeed.EventPlayerHit,
		Attacker: &killfeed.PlayerRef{Name: attacker, ID: "id-" + attacker},
		Victim:   &killfeed.PlayerRef{Name: victim, ID: "id-" + victim},
		Weapon:   "M4-A1",
	}
}

// HITFEED against real PostgreSQL and the real resolver: two servers of one
// guild each resolve their own installation's route, a server without a route
// publishes nothing (no KILLFEED fallback), and an in-process route change is
// used immediately despite a one-hour cache TTL.
func TestRuntimeHitfeedRoutesPerServerAndFollowsRouteChanges(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	a.ChannelRoutes = routing.NewResolver(a.SaaSChannelRoutes, time.Hour)
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	for _, id := range []string{"kf-A", "kf-B", "hit-A", "hit-A2", "hit-B"} {
		seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	serverA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	installB := createSecondInstallation(t, a, fixture)
	serverB := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, installB, 222222, fixture.OwnerDiscordID)).Server.ID
	guildRowID := mustGuildRowID(t, a, fixture.DiscordGuildID)

	// Server A: HITFEED -> hit-A. Server B: only KILLFEED configured.
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-A", "HITFEED": "hit-A"}); rr.Code != http.StatusOK {
		t.Fatalf("save A: %d %s", rr.Code, rr.Body.String())
	}
	if rr := saveChannelRoutes(t, a, fixture.OrgID, installB, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-B"}); rr.Code != http.StatusOK {
		t.Fatalf("save B: %d %s", rr.Code, rr.Body.String())
	}

	sink := &hitSink{}
	feedA := discord.NewHitfeedPublisher(sink, a.ChannelRoutes, guildRowID, serverA)
	feedB := discord.NewHitfeedPublisher(sink, a.ChannelRoutes, guildRowID, serverB)
	feedA.Flush() // activates from the real route lookup, as Run does at startup
	feedB.Flush()

	feedA.PublishHit(integrationHit("Alice", "Bob"))
	feedB.PublishHit(integrationHit("Carol", "Dave"))
	feedA.Flush()
	feedB.Flush()
	if sink.count("hit-A") != 1 {
		t.Fatalf("expected server A's hit in hit-A, got %v", sink.sent)
	}
	if sink.count("kf-A") != 0 || sink.count("kf-B") != 0 || len(sink.sent) != 1 {
		t.Fatalf("server B has no HITFEED route: nothing may be published (and no KILLFEED fallback), got %v", sink.sent)
	}

	// Server B gets its own route; A changes theirs. Both take effect on the
	// next lookup even though the cache TTL is an hour.
	if rr := saveChannelRoutes(t, a, fixture.OrgID, installB, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-B", "HITFEED": "hit-B"}); rr.Code != http.StatusOK {
		t.Fatalf("save B2: %d %s", rr.Code, rr.Body.String())
	}
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "kf-A", "HITFEED": "hit-A2"}); rr.Code != http.StatusOK {
		t.Fatalf("save A2: %d %s", rr.Code, rr.Body.String())
	}
	feedA.Flush()
	feedB.Flush() // pick up the changed routes
	feedA.PublishHit(integrationHit("Alice", "Bob"))
	feedB.PublishHit(integrationHit("Carol", "Dave"))
	feedA.Flush()
	feedB.Flush()

	if sink.count("hit-A2") != 1 || sink.count("hit-B") != 1 || sink.count("hit-A") != 1 {
		t.Fatalf("expected each server on its own current route (A moved to hit-A2, B on hit-B), got %v", sink.sent)
	}
}
