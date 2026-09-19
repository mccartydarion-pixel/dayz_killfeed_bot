//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// The four migrated route keys (LINK_GAMERTAG, STATS_LEADERBOARDS,
// AUTO_LEADERBOARD, ADMIN_LOGS) against real PostgreSQL, the real resolver and
// the real route-panel store: two servers in one guild, cross-organization
// isolation, and in-process propagation with a one-hour cache TTL.

var migratedRouteKeys = []string{"LINK_GAMERTAG", "STATS_LEADERBOARDS", "AUTO_LEADERBOARD", "ADMIN_LOGS"}

// board is an in-memory Discord: RoutePanelAPI + MessageEditor + delete.
type board struct {
	mu   sync.Mutex
	next int
	live map[string]bool // "channel/id"
}

func newBoard() *board { return &board{live: map[string]bool{}} }

func (b *board) send(ch string) (*discordgo.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	id := fmt.Sprintf("m%d", b.next)
	b.live[ch+"/"+id] = true
	return &discordgo.Message{ID: id, ChannelID: ch}, nil
}

func (b *board) edit(ch, id string) (*discordgo.Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.live[ch+"/"+id] {
		return nil, &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusNotFound}, Message: &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage}}
	}
	return &discordgo.Message{ID: id, ChannelID: ch}, nil
}

func (b *board) ChannelMessageSendComplex(ch string, _ *discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	return b.send(ch)
}
func (b *board) ChannelMessageEditComplex(ch, id string, _ *discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	return b.edit(ch, id)
}
func (b *board) ChannelMessageSendEmbed(ch string, _ *discordgo.MessageEmbed) (*discordgo.Message, error) {
	return b.send(ch)
}
func (b *board) ChannelMessageEditEmbed(ch, id string, _ *discordgo.MessageEmbed) (*discordgo.Message, error) {
	return b.edit(ch, id)
}
func (b *board) ChannelMessage(ch, id string) (*discordgo.Message, error) { return b.edit(ch, id) }
func (b *board) ChannelMessageDelete(ch, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.live, ch+"/"+id)
	return nil
}
func (b *board) inChannel(ch string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for k := range b.live {
		if len(k) > len(ch) && k[:len(ch)+1] == ch+"/" {
			n++
		}
	}
	return n
}
func (b *board) all() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for k := range b.live {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestRuntimeMigratedRoutesTwoServersOneGuildEndToEnd(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	// One hour: only cache invalidation on the in-process write can explain an
	// immediate change; the TTL alone never would.
	a.ChannelRoutes = routing.NewResolver(a.SaaSChannelRoutes, time.Hour)
	a.GuildRoutePanels = repository.NewGuildRoutePanelRepository(a.DB.Pool)
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	for _, id := range []string{"kf-A", "kf-B", "link-A", "link-A2", "link-B", "stats-S", "lb-A", "lb-B", "adm-A", "adm-B"} {
		seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}

	serverA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	installB := createSecondInstallation(t, a, fixture)
	serverB := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, installB, 222222, fixture.OwnerDiscordID)).Server.ID
	guildRowID := mustGuildRowID(t, a, fixture.DiscordGuildID)

	save := func(installationID int64, routes map[string]string) {
		t.Helper()
		if rr := saveChannelRoutes(t, a, fixture.OrgID, installationID, fixture.OwnerDiscordID, routes); rr.Code != http.StatusOK {
			t.Fatalf("save routes: %d %s", rr.Code, rr.Body.String())
		}
	}

	board := newBoard()
	servers := func(context.Context) (int64, []int64, error) { return guildRowID, []int64{serverA, serverB}, nil }
	panels := discord.NewRoutePanels(board, discord.NewRoutePanelStore(a.GuildRoutePanels))
	syncer := discord.NewRouteSyncer(a.ChannelRoutes, servers, panels, board, discord.NewInMemorySetupStore(), fixture.DiscordGuildID)
	a.RouteSyncer = syncer // the route-write handlers Trigger it, exactly as in production

	save(fixture.InstallationID, map[string]string{"KILLFEED": "kf-A", "LINK_GAMERTAG": "link-A", "STATS_LEADERBOARDS": "stats-S", "AUTO_LEADERBOARD": "lb-A", "ADMIN_LOGS": "adm-A"})
	save(installB, map[string]string{"KILLFEED": "kf-B", "LINK_GAMERTAG": "link-B", "STATS_LEADERBOARDS": "stats-S", "AUTO_LEADERBOARD": "lb-B", "ADMIN_LOGS": "adm-B"})

	// Per-server resolution: each server sees only its own installation's routes.
	want := map[int64]map[string]string{
		serverA: {"LINK_GAMERTAG": "link-A", "STATS_LEADERBOARDS": "stats-S", "AUTO_LEADERBOARD": "lb-A", "ADMIN_LOGS": "adm-A"},
		serverB: {"LINK_GAMERTAG": "link-B", "STATS_LEADERBOARDS": "stats-S", "AUTO_LEADERBOARD": "lb-B", "ADMIN_LOGS": "adm-B"},
	}
	for server, keys := range want {
		for key, channel := range keys {
			if got, found, err := a.ChannelRoutes.Resolve(context.Background(), guildRowID, server, key); err != nil || !found || got != channel {
				t.Fatalf("server %d %s: expected %s, got %q found=%v err=%v", server, key, channel, got, found, err)
			}
		}
	}

	// Guild-level panels follow the union of the servers' routes; the shared
	// stats channel gets exactly one panel.
	syncer.SyncOnce(context.Background())
	if board.inChannel("link-A") != 1 || board.inChannel("link-B") != 1 || board.inChannel("stats-S") != 1 {
		t.Fatalf("expected one panel per routed channel, live=%v", board.all())
	}
	syncer.SyncOnce(context.Background())
	if len(board.all()) != 3 {
		t.Fatalf("a repeat sync must not add panels, live=%v", board.all())
	}

	// ADM monitors are per server: each publishes to its own ADMIN_LOGS channel.
	for _, server := range []int64{serverA, serverB} {
		m := discord.NewADMMonitorPublisher(board, discord.NewInMemorySetupStore(), fixture.DiscordGuildID, server, "", func(string) {})
		m.SetRouting(a.ChannelRoutes, guildRowID)
		m.Update(killfeed.AdmSnapshot{State: killfeed.StatePolling, CurrentFile: "DayZServer.ADM"})
	}
	if board.inChannel("adm-A") != 1 || board.inChannel("adm-B") != 1 {
		t.Fatalf("expected each server's monitor in its own channel, live=%v", board.all())
	}

	// In-process route change: visible on the very next lookup despite the
	// 1-hour TTL, and the routed panel moves with no leftover copy.
	save(fixture.InstallationID, map[string]string{"KILLFEED": "kf-A", "LINK_GAMERTAG": "link-A2", "STATS_LEADERBOARDS": "stats-S", "AUTO_LEADERBOARD": "lb-A", "ADMIN_LOGS": "adm-A"})
	if got, _, _ := a.ChannelRoutes.Resolve(context.Background(), guildRowID, serverA, "LINK_GAMERTAG"); got != "link-A2" {
		t.Fatalf("expected the resolver to see link-A2 immediately, got %q", got)
	}
	syncer.SyncOnce(context.Background())
	if board.inChannel("link-A") != 0 || board.inChannel("link-A2") != 1 || board.inChannel("link-B") != 1 {
		t.Fatalf("expected the link panel to move to link-A2, live=%v", board.all())
	}
}

// A route must never resolve through an installation/server pairing that
// crosses a guild or organization boundary - for every migrated key.
func TestRuntimeMigratedRoutesRejectCrossOrganization(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)

	fixtureA := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixtureA.OrgID, fixtureA.OwnerDiscordID)
	for _, id := range []string{"a-kf", "a-link", "a-stats", "a-lb", "a-adm"} {
		seedGuildChannels(verifier, fixtureA.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	serverA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixtureA.OrgID, fixtureA.InstallationID, 111111, fixtureA.OwnerDiscordID)).Server.ID
	if rr := saveChannelRoutes(t, a, fixtureA.OrgID, fixtureA.InstallationID, fixtureA.OwnerDiscordID,
		map[string]string{"KILLFEED": "a-kf", "LINK_GAMERTAG": "a-link", "STATS_LEADERBOARDS": "a-stats", "AUTO_LEADERBOARD": "a-lb", "ADMIN_LOGS": "a-adm"}); rr.Code != http.StatusOK {
		t.Fatalf("save A: %d %s", rr.Code, rr.Body.String())
	}
	guildA := mustGuildRowID(t, a, fixtureA.DiscordGuildID)
	ctx := context.Background()
	for _, key := range migratedRouteKeys {
		if _, found, err := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, key); err != nil || !found {
			t.Fatalf("baseline %s: found=%v err=%v", key, found, err)
		}
	}

	// Organization B forces its installation onto A's server (only possible
	// via direct SQL) and configures routes of its own.
	fixtureB := buildInstallationFixture(t, a, verifier)
	for _, id := range []string{"b-kf", "b-link", "b-stats", "b-lb", "b-adm"} {
		seedGuildChannels(verifier, fixtureB.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	if rr := saveChannelRoutes(t, a, fixtureB.OrgID, fixtureB.InstallationID, fixtureB.OwnerDiscordID,
		map[string]string{"KILLFEED": "b-kf", "LINK_GAMERTAG": "b-link", "STATS_LEADERBOARDS": "b-stats", "AUTO_LEADERBOARD": "b-lb", "ADMIN_LOGS": "b-adm"}); rr.Code != http.StatusOK {
		t.Fatalf("save B: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE installations SET game_server_id=$2 WHERE id=$1`, fixtureB.InstallationID, serverA); err != nil {
		t.Fatal(err)
	}
	guildB := mustGuildRowID(t, a, fixtureB.DiscordGuildID)
	for _, key := range migratedRouteKeys {
		if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildB, serverA, key); found {
			t.Fatalf("%s: B's forced pairing with A's server must not resolve, got %q", key, got)
		}
		if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, key); !found || got == "" {
			t.Fatalf("%s: A's route must be untouched, got %q found=%v", key, got, found)
		}
	}

	// Same guild, but the server is claimed by a DIFFERENT organization.
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE game_servers SET organization_id=$2 WHERE id=$1`, serverA, fixtureB.OrgID); err != nil {
		t.Fatal(err)
	}
	for _, key := range migratedRouteKeys {
		if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, key); found {
			t.Fatalf("%s: a server claimed by another organization must not resolve through A's installation, got %q", key, got)
		}
	}
}
