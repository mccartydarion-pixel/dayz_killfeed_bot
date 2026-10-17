//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/heatmap"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// panelPoster is the Discord surface the persistent panels post through,
// recorded in the fake verifier so the layout verification sees the panels
// exactly as it sees a starter card.
type panelPoster struct {
	*board
	verifier *fakeDiscordVerifier
}

func (p *panelPoster) post(ch string) (*discordgo.Message, error) {
	msg, err := p.board.send(ch)
	if err == nil {
		if p.verifier.botPosts == nil {
			p.verifier.botPosts = map[string]int{}
		}
		p.verifier.botPosts[ch]++
	}
	return msg, err
}

func (p *panelPoster) ChannelMessageSendComplex(ch string, _ *discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	return p.post(ch)
}
func (p *panelPoster) ChannelMessageSendEmbeds(ch string, _ []*discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	return p.post(ch)
}
func (p *panelPoster) ChannelMessageEditEmbeds(ch, id string, _ []*discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	return p.board.edit(ch, id)
}
func (p *panelPoster) ChannelMessageSendEmbed(ch string, _ *discordgo.MessageEmbed) (*discordgo.Message, error) {
	return p.post(ch)
}

// stubHeatmaps answers every aggregate query with no activity.
type stubHeatmaps struct{}

func (stubHeatmaps) Query(_ context.Context, req heatmap.Request) (*heatmap.Result, error) {
	return &heatmap.Result{Type: req.Type, Resolution: req.Resolution}, nil
}

// A customer's installation lives in a guild that is NOT the bot's configured
// guild. Applying the Champion layout must still leave every panel destination
// (the six without a starter card) verified, because the boards sync every
// guild with an active server - the production "Broken 6" was exactly these
// six never receiving their panel in a non-primary guild.
func TestChannelLayoutPanelsServeNonPrimaryGuild(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	ctx := context.Background()
	a.Config.DiscordGuildID = fmt.Sprintf("primary-guild-%d", time.Now().UnixNano())
	a.ChannelRoutes = routing.NewResolver(a.SaaSChannelRoutes, time.Hour)
	a.GuildRoutePanels = repository.NewGuildRoutePanelRepository(a.DB.Pool)
	a.Servers = repository.NewServerRepository(a.DB.Pool)
	a.Bounties = repository.NewBountyRepository(a.DB.Pool)
	fixture := buildInstallationFixture(t, a, verifier)
	advanceFixtureToServerSelected(t, a, fixture)
	if fixture.DiscordGuildID == a.Config.DiscordGuildID {
		t.Fatal("the installation must be in a guild other than the bot's configured guild")
	}
	guildRowID := mustGuildRowID(t, a, fixture.DiscordGuildID)

	// The production wiring: every board follows every guild with an active
	// server, grouped from the server table on each sync.
	poster := &panelPoster{board: newBoard(), verifier: verifier}
	panels := discord.NewRoutePanels(poster, discord.NewRoutePanelStore(a.GuildRoutePanels))
	allGuilds := func(ctx context.Context) ([]discord.GuildServers, error) {
		rows, err := a.Servers.ListActive(ctx)
		if err != nil {
			return nil, err
		}
		return groupServersByGuild(rows), nil
	}
	a.RouteSyncer = discord.NewRouteSyncer(a.ChannelRoutes, allGuilds, panels, poster, discord.NewInMemorySetupStore(), a.Config.DiscordGuildID, 0)
	a.BountyBoard = discord.NewBountyBoard(a.ChannelRoutes, allGuilds, panels, a.Bounties)
	a.HeatmapBoard = discord.NewHeatmapBoard(a.ChannelRoutes, allGuilds, panels, stubHeatmaps{}, time.Minute)
	a.ServerStatusBoard = discord.NewServerStatusBoard(a.ChannelRoutes, allGuilds, panels)
	a.ServerRanksBoards = discord.NewServerRanksBoards(a.ChannelRoutes, allGuilds, panels, repository.NewRankedRepository(a.DB.Pool))
	a.LeaderboardScheduler = discord.NewLeaderboardScheduler(discord.NewLeaderboardPanel(poster, "", "", discord.DefaultLeaderboardConfig()),
		repository.NewStatsRepository(a.DB.Pool), 0, discord.DefaultLeaderboardConfig(), func(string) {})
	a.LeaderboardScheduler.SetRouting(a.ChannelRoutes, allGuilds, panels, nil)
	a.RouteSyncer.SetLeaderboard(a.LeaderboardScheduler)
	// The audited producers block SERVER_RANKS because the fixture normally
	// runs no ranks board; this test does.
	a.channelProducersOverride = func() map[string]routeProducer {
		out := auditProducers()
		out["SERVER_RANKS"] = routeProducerAudit["SERVER_RANKS"]
		return out
	}
	// Boot: the syncer records the routes as they are (none yet), exactly
	// as RouteSyncer.Run does before any layout is applied.
	a.RouteSyncer.SyncOnce(ctx)

	// The first run creates the channels; the fake verifier only reports a
	// channel reachable once told so, as the live bot would once it is in it.
	if rr := autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false); rr.Code != http.StatusOK {
		t.Fatalf("first auto-setup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	for _, ch := range verifier.channels[fixture.DiscordGuildID] {
		verifier.channelFound[fixture.DiscordGuildID+"|"+ch.ID] = true
	}
	rr := autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false)
	if rr.Code != http.StatusOK {
		t.Fatalf("second auto-setup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBody[AutoSetupChannelsResponse](t, rr)
	if resp.Summary == nil || len(resp.Summary.Broken) != 0 {
		t.Fatalf("expected no broken destination in a non-primary guild, got %+v\n%+v", resp.Summary, resp.Destinations)
	}
	panelDestinations := []string{"BOUNTIES", "HEATMAPS", "SERVER_STATUS", "LEADERBOARDS", "SERVER_RANKS", "PLAYER_LINK"}
	for _, key := range panelDestinations {
		var rep *ChannelDestinationReport
		for i := range resp.Destinations {
			if resp.Destinations[i].Key == key {
				rep = &resp.Destinations[i]
			}
		}
		if rep == nil || rep.Health != HealthActive || rep.Checks == nil || !rep.Checks.VisibleContent {
			t.Fatalf("destination %s must verify with its panel visible, got %+v", key, rep)
		}
	}

	// Each panel is recorded under the customer's guild - never the primary one.
	server, err := a.Servers.ListActiveByGuild(ctx, guildRowID)
	if err != nil || len(server) != 1 {
		t.Fatalf("expected the installation's one active server, got %v err=%v", server, err)
	}
	for _, key := range []string{"BOUNTY", "HEATMAPS", "SERVER_STATUS", "AUTO_LEADERBOARD", "STATS_LEADERBOARDS", "LINK_GAMERTAG", "SERVER_RANKS:" + strconv.FormatInt(server[0].ID, 10)} {
		rows, err := a.GuildRoutePanels.List(ctx, guildRowID, key)
		if err != nil || len(rows) != 1 {
			t.Fatalf("expected one recorded %s panel for guild %d, got %v err=%v", key, guildRowID, rows, err)
		}
	}
}
