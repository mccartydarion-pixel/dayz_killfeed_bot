package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

const routeKeyServerStatus = "SERVER_STATUS"

const (
	serverStatusInterval = RouteSyncInterval
	// serverStatusLinkStale: no successful ADM poll for this long means
	// Champion's link to the server's logs is degraded.
	serverStatusLinkStale = 3 * time.Minute
)

// ServerStatusBoard keeps one persistent server-status message per
// SERVER_STATUS-routed channel, edited in place through RoutePanels (so
// restarts and route changes never duplicate it). It shows only what the
// server workers actually observe - Champion's ADM link, log freshness and
// the tracked online count. There is no uptime, latency or game-server
// power state: Champion does not measure them.
type ServerStatusBoard struct {
	resolver    RouteResolver
	servers     GuildServersFunc
	panels      *RoutePanels
	serverNames ServerNameFunc
	now         func() time.Time
	trigger     chan struct{}

	mu        sync.Mutex
	snapshots map[int64]killfeed.AdmSnapshot
	syncMu    sync.Mutex
}

func NewServerStatusBoard(resolver RouteResolver, servers GuildServersFunc, panels *RoutePanels) *ServerStatusBoard {
	return &ServerStatusBoard{resolver: resolver, servers: servers, panels: panels, now: time.Now, trigger: make(chan struct{}, 1), snapshots: map[int64]killfeed.AdmSnapshot{}}
}

// SetServerNames labels each server's section.
func (b *ServerStatusBoard) SetServerNames(f ServerNameFunc) {
	if b != nil {
		b.serverNames = f
	}
}

// Observe records one server's latest ADM snapshot (fed by its worker).
func (b *ServerStatusBoard) Observe(serverID int64, snap killfeed.AdmSnapshot) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.snapshots[serverID] = snap
	b.mu.Unlock()
}

// Trigger asks for an immediate reconcile (non-blocking, coalesced); nil-safe.
func (b *ServerStatusBoard) Trigger() {
	if b == nil {
		return
	}
	select {
	case b.trigger <- struct{}{}:
	default:
	}
}

// Run reconciles at startup, on Trigger and every interval. Unchanged content
// is never re-edited (RoutePanels.SyncEach skips it).
func (b *ServerStatusBoard) Run(ctx context.Context) {
	if b == nil {
		return
	}
	ticker := time.NewTicker(serverStatusInterval)
	defer ticker.Stop()
	b.SyncOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.trigger:
			b.SyncOnce(ctx)
		case <-ticker.C:
			b.SyncOnce(ctx)
		}
	}
}

// SyncOnce places or refreshes the status message in every routed channel.
func (b *ServerStatusBoard) SyncOnce(ctx context.Context) {
	if b == nil || b.resolver == nil || b.servers == nil || b.panels == nil {
		return
	}
	b.syncMu.Lock()
	defer b.syncMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=server_status", "msg", "sync panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	guildRowID, serverIDs, err := b.servers(ctx)
	if err != nil {
		slog.Warn("component=server_status", "event", "server_status_servers_failed", "err", err.Error())
		return
	}
	byChannel := map[string][]int64{}
	var order []string
	lookupErrs := 0
	for _, serverID := range serverIDs {
		ch, found, rerr := b.resolver.Resolve(ctx, guildRowID, serverID, routeKeyServerStatus)
		if rerr != nil {
			lookupErrs++
			slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", routeKeyServerStatus, "guild_id", guildRowID, "server_id", serverID, "reason", "lookup_error", "err", rerr.Error())
			continue
		}
		if !found || ch == "" {
			continue
		}
		if _, seen := byChannel[ch]; !seen {
			order = append(order, ch)
		}
		byChannel[ch] = append(byChannel[ch], serverID)
	}
	if len(order) == 0 && lookupErrs > 0 {
		return // unknown state: leave whatever is live
	}
	now := b.now()
	contentFor := func(channelID string) (PanelContent, error) {
		sections := make([]ServerStatusSection, 0, len(byChannel[channelID]))
		b.mu.Lock()
		for _, serverID := range byChannel[channelID] {
			snap, seen := b.snapshots[serverID]
			name := ""
			if b.serverNames != nil {
				name = b.serverNames(serverID)
			}
			sections = append(sections, ServerStatusSection{ServerName: name, Seen: seen, Snapshot: snap})
		}
		b.mu.Unlock()
		return PanelContent{Embed: BuildServerStatusEmbed(sections, now)}, nil
	}
	if _, err := b.panels.SyncEach(ctx, guildRowID, routeKeyServerStatus, order, contentFor, lookupErrs == 0); err != nil {
		slog.Warn("component=server_status", "event", "server_status_sync_failed", "err", err.Error())
	}
}

// ServerStatusSection is one server inside a status message.
type ServerStatusSection struct {
	ServerName string
	Seen       bool // a worker has reported at least one snapshot
	Snapshot   killfeed.AdmSnapshot
}

// ServerStatusLink classifies Champion's link to a server's ADM logs.
func ServerStatusLink(s ServerStatusSection, now time.Time) string {
	switch {
	case !s.Seen || s.Snapshot.LastPoll.IsZero():
		return "WAITING FOR FIRST POLL"
	case now.Sub(s.Snapshot.LastPoll) > serverStatusLinkStale:
		return "DEGRADED"
	case s.Snapshot.State != killfeed.StatePolling:
		return "LOCATING LOG"
	default:
		return "CONNECTED"
	}
}

// BuildServerStatusEmbed renders the status message from observed values
// only. Relative Discord timestamps keep the text stable between refreshes,
// so an unchanged server is never re-edited.
func BuildServerStatusEmbed(sections []ServerStatusSection, now time.Time) *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed("📡 SERVER STATUS", presentation.Steel)
	if len(sections) == 0 {
		embed.Description = "No server is connected to this channel yet."
	}
	for _, s := range sections {
		title := "Server"
		if strings.TrimSpace(s.ServerName) != "" {
			title = presentation.SafeName(s.ServerName, 60)
		}
		lines := []string{"**Champion Link:** " + ServerStatusLink(s, now)}
		if s.Seen {
			if !s.Snapshot.LastLogChange.IsZero() {
				lines = append(lines, fmt.Sprintf("**ADM Log:** updated <t:%d:R>", s.Snapshot.LastLogChange.Unix()))
			}
			lines = append(lines, "**Players Online:** "+presentation.FormatThousands(int64(s.Snapshot.OnlineCount)))
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: title, Value: strings.Join(lines, "\n")})
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: presentation.FooterLiveIntel}
	return embed
}
