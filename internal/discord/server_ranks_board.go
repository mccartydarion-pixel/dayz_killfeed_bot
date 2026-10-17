package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

const ServerRanksRefreshInterval = 3 * time.Hour
const routeKeyServerRanks = "SERVER_RANKS"

type ServerRanksReader interface {
	ServerStandings(ctx context.Context, serverID int64, limit int) ([]repository.ServerStanding, error)
}

// ServerRanksBoards maintains a distinct persistent panel per game server, for
// every active server of every guild the provider lists on each sync - so a
// server connected from the website after startup gets its panel without a
// restart. A missing season gets an honest placeholder, never fixture ranks.
type ServerRanksBoards struct {
	resolver RouteResolver
	servers  AllGuildServersFunc
	panels   *RoutePanels
	reader   ServerRanksReader
	nameFn   ServerNameFunc
	syncMu   sync.Mutex // serializes the Run loop with setup-time SyncOnce
}

// SetServerNameFunc makes each panel show its server's current name on each refresh (an owner
// rename or a Nitrado name change). A nil function, or one that returns "", leaves the name off.
// Call it before Run.
func (b *ServerRanksBoards) SetServerNameFunc(f ServerNameFunc) {
	if b != nil {
		b.nameFn = f
	}
}

// ServerRanksInactiveEmbed is the honest notice shown while a server has no
// active Ranked (RP) season. It names the server and says plainly that the
// guild's stats season (/season) is a different thing, so "Season 1 — ACTIVE"
// in /season status and this notice never read as a contradiction.
func ServerRanksInactiveEmbed(serverName string) *discordgo.MessageEmbed {
	desc := "This server's **Ranked (RP) season has not started yet.** Every player is Unranked and no RP is awarded until the server owner starts it in Champion Server Admin → Server Controls → Server Ranked.\n\n" +
		"_The stats season shown by `/season status` is separate: it labels kill/death history and does not award RP._"
	if name := strings.TrimSpace(serverName); name != "" {
		desc = "**" + presentation.SafeName(name, 100) + "**\n\n" + desc
	}
	return &discordgo.MessageEmbed{Author: presentation.ChampionAuthor(), Title: serverRanksTitle, Description: desc, Color: presentation.Neutral, Footer: presentation.AutoRefreshFooter()}
}

func NewServerRanksBoards(resolver RouteResolver, servers AllGuildServersFunc, panels *RoutePanels, reader ServerRanksReader) *ServerRanksBoards {
	return &ServerRanksBoards{resolver: resolver, servers: servers, panels: panels, reader: reader}
}

// Run syncs at startup and every ServerRanksRefreshInterval until ctx ends.
func (b *ServerRanksBoards) Run(ctx context.Context) {
	if b == nil {
		return
	}
	ticker := time.NewTicker(ServerRanksRefreshInterval)
	defer ticker.Stop()
	b.SyncOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.SyncOnce(ctx)
		}
	}
}

// SyncOnce refreshes the panel of every active server of every guild. Never
// fatal; panic-safe.
func (b *ServerRanksBoards) SyncOnce(ctx context.Context) {
	if b == nil || b.resolver == nil || b.servers == nil || b.panels == nil || b.reader == nil {
		return
	}
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=ranked", "msg", "server ranks sync panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	guilds, err := b.servers(ctx)
	if err != nil {
		slog.Warn("component=ranked", "event", "server_ranks_servers_failed", "err", err.Error())
		return
	}
	for _, g := range guilds {
		for _, serverID := range g.ServerIDs {
			b.syncServer(ctx, g.GuildRowID, serverID)
		}
	}
}

// syncServer places or refreshes one server's panel on its SERVER_RANKS route.
func (b *ServerRanksBoards) syncServer(ctx context.Context, guildRowID, serverID int64) {
	if guildRowID <= 0 || serverID <= 0 {
		return
	}
	channel, found, err := b.resolver.Resolve(ctx, guildRowID, serverID, routeKeyServerRanks)
	if err != nil {
		slog.Warn("component=ranked", "event", "route_lookup_failed", "guild_id", guildRowID, "server_id", serverID, "err", err.Error())
		return // unknown route state: keep the last good panel
	}
	key := fmt.Sprintf("%s:%d", routeKeyServerRanks, serverID)
	if !found || channel == "" {
		_, _ = b.panels.Sync(ctx, guildRowID, key, nil, PanelContent{}, false, true)
		return
	}
	rows, err := b.reader.ServerStandings(ctx, serverID, presentation.MaxBoardEntries)
	name := serverNameOf(b.nameFn, serverID)
	var embed *discordgo.MessageEmbed
	switch {
	case err == nil:
		embed = BuildServerRanksEmbed(ServerRanksSnapshot{ServerName: name, UpdatedAt: time.Now().UTC(), Standings: rows})
	case errors.Is(err, repository.ErrRankedIneligible):
		embed = ServerRanksInactiveEmbed(name)
	default:
		slog.Warn("component=ranked", "event", "standings_failed", "server_id", serverID, "err", err.Error())
		return // query failure: preserve last good panel
	}
	if _, err := b.panels.Sync(ctx, guildRowID, key, []string{channel}, PanelContent{Embed: embed}, true, true); err != nil {
		slog.Warn("component=ranked", "event", "panel_sync_failed", "server_id", serverID, "err", err.Error())
	}
}
