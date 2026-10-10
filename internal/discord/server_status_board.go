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
	games     map[int64]GameServerStatus
	syncMu    sync.Mutex
}

// GameServerStatus is what Champion can tell about the game process itself from its log files
// (docs/SERVER_DOWN_ALERT.md). The zero value means "nothing known" and adds nothing to the board.
type GameServerStatus struct {
	// State is "ONLINE", "RESTARTING" or "DOWN"; anything else is shown as unknown.
	State string
	// StartedAt is when the current run started; NextRestart is about when the next start is
	// expected. Both are zero when unknown.
	StartedAt   time.Time
	NextRestart time.Time
	// DownSince is when the logs stopped, while State is "DOWN".
	DownSince time.Time
}

// ObserveGame records one server's game status and refreshes the board when it changed. The
// board is one message edited in place, so a restart never adds a message.
func (b *ServerStatusBoard) ObserveGame(serverID int64, g GameServerStatus) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.games == nil {
		b.games = map[int64]GameServerStatus{}
	}
	changed := b.games[serverID] != g
	b.games[serverID] = g
	b.mu.Unlock()
	if changed {
		b.Trigger()
	}
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
			sections = append(sections, ServerStatusSection{ServerName: name, Seen: seen, Snapshot: snap, Game: b.games[serverID]})
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
	Game       GameServerStatus
}

// gameServerLines are the board's lines about the game process: its state, when it last
// restarted and about when the next restart is due. Discord timestamps keep the text the same
// between refreshes, so the message is edited only when something changed.
func gameServerLines(g GameServerStatus) []string {
	var lines []string
	switch g.State {
	case "ONLINE":
		lines = append(lines, "**Game server:** 🟢 Online")
	case "RESTARTING":
		lines = append(lines, "**Game server:** 🔄 Restarting")
	case "DOWN":
		line := "**Game server:** 🔴 Not running"
		if !g.DownSince.IsZero() {
			line += " since " + presentation.Timestamp(g.DownSince, 't') + " (" + presentation.Timestamp(g.DownSince, 'R') + ")"
		}
		lines = append(lines, line)
	default:
		return nil
	}
	if !g.StartedAt.IsZero() {
		lines = append(lines, "**Last restart:** "+presentation.Timestamp(g.StartedAt, 't')+" ("+presentation.Timestamp(g.StartedAt, 'R')+")")
	}
	if g.State == "ONLINE" && !g.NextRestart.IsZero() {
		lines = append(lines, "**Next restart:** about "+presentation.Timestamp(g.NextRestart, 't')+" ("+presentation.Timestamp(g.NextRestart, 'R')+")")
	}
	return lines
}

// The four states of Champion's link to a server's ADM logs, as the board words them.
const (
	ServerLinkWaiting   = "Waiting for first poll"
	ServerLinkDegraded  = "Degraded"
	ServerLinkLocating  = "Locating log"
	ServerLinkConnected = "Connected"
)

// ServerStatusLink classifies Champion's link to a server's ADM logs.
func ServerStatusLink(s ServerStatusSection, now time.Time) string {
	switch {
	case !s.Seen || s.Snapshot.LastPoll.IsZero():
		return ServerLinkWaiting
	case now.Sub(s.Snapshot.LastPoll) > serverStatusLinkStale:
		return ServerLinkDegraded
	case s.Snapshot.State != killfeed.StatePolling:
		return ServerLinkLocating
	default:
		return ServerLinkConnected
	}
}

// serverStatusColor is the board's colour, by meaning: red when a game server is not running,
// amber while one restarts or as soon as one server's link is degraded, green when every server is connected, neutral while there is nothing to judge yet
// (no server, or still waiting for a first poll or for the log).
func serverStatusColor(sections []ServerStatusSection, now time.Time) int {
	if len(sections) == 0 {
		return presentation.Neutral
	}
	color := presentation.Green
	for _, s := range sections {
		if s.Game.State == "DOWN" {
			return presentation.ErrorRed
		}
	}
	for _, s := range sections {
		if s.Game.State == "RESTARTING" {
			return presentation.Amber
		}
	}
	for _, s := range sections {
		switch ServerStatusLink(s, now) {
		case ServerLinkDegraded:
			return presentation.Amber
		case ServerLinkWaiting, ServerLinkLocating:
			color = presentation.Neutral
		}
	}
	return color
}

// BuildServerStatusEmbed renders the status message from observed values
// only. Relative Discord timestamps keep the text stable between refreshes,
// so an unchanged server is never re-edited.
func BuildServerStatusEmbed(sections []ServerStatusSection, now time.Time) *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed("📡 Server status", serverStatusColor(sections, now))
	if len(sections) == 0 {
		embed.Description = "No server is connected to this channel yet."
	}
	for _, s := range sections {
		title := "Server"
		if strings.TrimSpace(s.ServerName) != "" {
			title = presentation.SafeName(s.ServerName, 60)
		}
		lines := append(gameServerLines(s.Game), "**Champion link:** "+ServerStatusLink(s, now))
		if s.Seen {
			if !s.Snapshot.LastLogChange.IsZero() {
				lines = append(lines, "**ADM log:** updated "+presentation.Timestamp(s.Snapshot.LastLogChange, 'R'))
			}
			lines = append(lines, "**Players online:** "+presentation.FormatThousands(int64(s.Snapshot.OnlineCount)))
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: title, Value: strings.Join(lines, "\n")})
	}
	embed.Footer = presentation.Footer("", presentation.FooterAutoRefresh)
	return embed
}
