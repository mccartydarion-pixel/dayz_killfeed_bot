package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// ServerRanksBoard maintains a distinct persistent panel per game server.
// It is not attached to the runtime until the setup route and season gate are
// wired. A missing season gets an honest placeholder, never fixture ranks.
type ServerRanksBoard struct {
	resolver RouteResolver
	panels   *RoutePanels
	reader   ServerRanksReader
	guildID  int64
	serverID int64
	name     string
}

func NewServerRanksBoard(resolver RouteResolver, panels *RoutePanels, reader ServerRanksReader, guildID, serverID int64, name string) *ServerRanksBoard {
	return &ServerRanksBoard{resolver: resolver, panels: panels, reader: reader, guildID: guildID, serverID: serverID, name: name}
}

func (b *ServerRanksBoard) Run(ctx context.Context) {
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

func (b *ServerRanksBoard) SyncOnce(ctx context.Context) {
	if b == nil || b.resolver == nil || b.panels == nil || b.reader == nil || b.guildID <= 0 || b.serverID <= 0 {
		return
	}
	channel, found, err := b.resolver.Resolve(ctx, b.guildID, b.serverID, routeKeyServerRanks)
	if err != nil {
		slog.Warn("component=ranked", "event", "route_lookup_failed", "server_id", b.serverID, "err", err.Error())
		return // unknown route state: keep the last good panel
	}
	key := fmt.Sprintf("%s:%d", routeKeyServerRanks, b.serverID)
	if !found || channel == "" {
		_, _ = b.panels.Sync(ctx, b.guildID, key, nil, PanelContent{}, false, true)
		return
	}
	rows, err := b.reader.ServerStandings(ctx, b.serverID, presentation.MaxBoardEntries)
	var embed *discordgo.MessageEmbed
	switch {
	case err == nil:
		embed = BuildServerRanksEmbed(ServerRanksSnapshot{ServerName: b.name, UpdatedAt: time.Now().UTC(), Standings: rows})
	case errors.Is(err, repository.ErrRankedIneligible):
		embed = &discordgo.MessageEmbed{Title: "🎖️ SERVER RANKS 🎖️", Description: "This server's Ranked season has not started yet.", Color: presentation.ChampionGold}
	default:
		slog.Warn("component=ranked", "event", "standings_failed", "server_id", b.serverID, "err", err.Error())
		return // query failure: preserve last good panel
	}
	if _, err := b.panels.Sync(ctx, b.guildID, key, []string{channel}, PanelContent{Embed: embed}, true, true); err != nil {
		slog.Warn("component=ranked", "event", "panel_sync_failed", "server_id", b.serverID, "err", err.Error())
	}
}
