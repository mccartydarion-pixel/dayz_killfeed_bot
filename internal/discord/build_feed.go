package discord

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

const routeKeyBuildFeed = "BUILD_FEED"

const (
	buildFeedFlushInterval = 5 * time.Second
	buildFeedQueueCap      = 200
	// Up to buildFeedMaxCards actions are sent as individual cards (one
	// message); a larger burst becomes one summary card.
	buildFeedMaxCards     = 5
	buildFeedSummaryLines = 10
	buildFeedFooter       = "CHAMPION • STAFF INTELLIGENCE"
)

// buildItem is what the feed keeps of one build action.
type buildItem struct {
	Player     string
	Action     killfeed.BuildAction
	HasPos     bool
	MapX, MapZ float64
}

// BuildFeedPublisher publishes one server's ADM build/placement actions to
// its BUILD_FEED route. Actions only exist when the server writes them to
// the ADM (adminLogPlacement / adminLogBuildActions); nothing is invented.
// A bounded queue flushed on its own goroutine keeps Discord off the ADM
// hot path; with no route nothing is sent (no fallback).
type BuildFeedPublisher struct {
	sender     HitSender
	resolver   RouteResolver
	guildRowID int64
	serverID   int64
	serverName ServerNameFunc
	onSeen     func()

	mu      sync.Mutex
	pending []buildItem
	dropped int
}

func NewBuildFeedPublisher(sender HitSender, resolver RouteResolver, guildRowID, serverID int64) *BuildFeedPublisher {
	return &BuildFeedPublisher{sender: sender, resolver: resolver, guildRowID: guildRowID, serverID: serverID}
}

// SetServerName labels cards with the server's name.
func (p *BuildFeedPublisher) SetServerName(f ServerNameFunc) {
	if p != nil {
		p.serverName = f
	}
}

// OnSeen is called for every parsed build action - the proof that this
// server's ADM actually carries build lines.
func (p *BuildFeedPublisher) OnSeen(f func()) {
	if p != nil {
		p.onSeen = f
	}
}

// PublishBuild implements killfeed.BuildPublisher. Never blocks.
func (p *BuildFeedPublisher) PublishBuild(ev *killfeed.Event) {
	if p == nil || ev == nil || ev.Build == nil || ev.Player == nil {
		return
	}
	if p.onSeen != nil {
		p.onSeen()
	}
	item := buildItem{Player: ev.Player.Name, Action: *ev.Build}
	if pos := ev.Player.Position; pos != nil {
		// ADM pos=<x, y, altitude>: the first two values are the map plane.
		item.HasPos, item.MapX, item.MapZ = true, pos.X, pos.Y
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) >= buildFeedQueueCap {
		p.dropped++
		return
	}
	p.pending = append(p.pending, item)
}

// Run flushes pending actions every few seconds until ctx ends, then once
// more.
func (p *BuildFeedPublisher) Run(ctx context.Context) {
	if p == nil {
		return
	}
	ticker := time.NewTicker(buildFeedFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.Flush(context.Background())
			return
		case <-ticker.C:
			p.Flush(ctx)
		}
	}
}

// Flush sends everything pending now.
func (p *BuildFeedPublisher) Flush(ctx context.Context) {
	if p == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=build_feed", "msg", "flush panic recovered", "server_id", p.serverID, "panic", fmt.Sprint(r))
		}
	}()
	p.mu.Lock()
	items, dropped := p.pending, p.dropped
	p.pending, p.dropped = nil, 0
	p.mu.Unlock()
	if len(items) == 0 || p.sender == nil || p.resolver == nil {
		return
	}
	channel, found, err := p.resolver.Resolve(ctx, p.guildRowID, p.serverID, routeKeyBuildFeed)
	if err != nil {
		slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", routeKeyBuildFeed, "guild_id", p.guildRowID, "server_id", p.serverID, "reason", "lookup_error", "err", err.Error())
		return
	}
	if !found || channel == "" {
		return
	}
	server := ""
	if p.serverName != nil {
		server = p.serverName(p.serverID)
	}
	var embeds []*discordgo.MessageEmbed
	if len(items) <= buildFeedMaxCards && dropped == 0 {
		for _, it := range items {
			embeds = append(embeds, BuildActivityEmbed(it, server))
		}
	} else {
		embeds = []*discordgo.MessageEmbed{BuildActivitySummaryEmbed(items, dropped, server)}
	}
	if _, err := p.sender.ChannelMessageSendComplex(channel, &discordgo.MessageSend{Embeds: embeds}); err != nil {
		slog.Warn("component=build_feed", "event", "build_feed_send_failed", "server_id", p.serverID, "err", err.Error())
	}
}

// BuildActivityEmbed renders one action with only the fields the ADM line
// actually carried.
func BuildActivityEmbed(it buildItem, server string) *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed("🏗️ BUILD ACTIVITY", presentation.FactionGold)
	// add skips a value the line did not carry (SafeName would turn "" into a
	// placeholder).
	add := func(name, value string, max int) {
		if strings.TrimSpace(value) == "" {
			return
		}
		if max > 0 {
			value = presentation.SafeName(value, max)
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: name, Value: value, Inline: true})
	}
	add("Player", it.Player, presentation.MaxRankNameRunes)
	add("Action", it.Action.Action, 0)
	add("Object", it.Action.Object, 64)
	add(targetPreposition(it.Action.Action), it.Action.Target, 64)
	add("Tool", it.Action.Tool, 64)
	if it.HasPos {
		add("Location", formatMapPosition(it.MapX, it.MapZ), 0)
	}
	add("Server", server, 60)
	embed.Footer = &discordgo.MessageEmbedFooter{Text: buildFeedFooter}
	return embed
}

// BuildActivitySummaryEmbed condenses a burst into one card.
func BuildActivitySummaryEmbed(items []buildItem, dropped int, server string) *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed("🏗️ BUILD ACTIVITY", presentation.FactionGold)
	lines := make([]string, 0, buildFeedSummaryLines+1)
	for i, it := range items {
		if i == buildFeedSummaryLines {
			break
		}
		line := fmt.Sprintf("**%s** %s **%s**", presentation.SafeName(it.Player, presentation.MaxRankNameRunes), strings.ToLower(it.Action.Action), presentation.SafeName(it.Action.Object, 64))
		if it.Action.Target != "" {
			line += " " + strings.ToLower(targetPreposition(it.Action.Action)) + " " + presentation.SafeName(it.Action.Target, 64)
		}
		if it.HasPos {
			line += " • " + formatMapPosition(it.MapX, it.MapZ)
		}
		lines = append(lines, line)
	}
	if more := len(items) - len(lines) + dropped; more > 0 {
		lines = append(lines, fmt.Sprintf("_+%s not shown_", presentation.Plural(int64(more), "more action", "more actions")))
	}
	embed.Description = strings.Join(lines, "\n")
	if strings.TrimSpace(server) != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Server", Value: presentation.SafeName(server, 60), Inline: true})
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: buildFeedFooter}
	return embed
}

// targetPreposition: a part is built on a structure, dismantled from it.
func targetPreposition(action string) string {
	if action == "Dismantled" {
		return "From"
	}
	return "On"
}

func formatMapPosition(x, z float64) string {
	return "X: " + presentation.FormatThousands(int64(math.Round(x))) + " • Z: " + presentation.FormatThousands(int64(math.Round(z)))
}
