package discord

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/heatmap"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

const routeKeyHeatmaps = "HEATMAPS"

// HeatmapQuerier is the Phase 5 heatmap read path (docs/HEATMAPS.md);
// *heatmap.Service satisfies it. The board never touches Nitrado or raw
// events - it only reads Champion's persisted aggregates.
type HeatmapQuerier interface {
	Query(ctx context.Context, req heatmap.Request) (*heatmap.Result, error)
}

const (
	// heatmapRouteCheckInterval notices a route change made outside this
	// process; the aggregate query itself only runs every refresh interval.
	heatmapRouteCheckInterval = RouteSyncInterval
	heatmapWindow             = heatmap.DefaultWindow
	heatmapResolution         = heatmap.DefaultResolution
	heatmapHotZones           = 3
	heatmapTitle              = "🗺️ PVP HEATMAP UPDATE"
)

// HeatmapBoard keeps one persistent PvP heatmap summary per HEATMAPS-routed
// channel, edited in place (never a new message per refresh). The message
// is recorded through RoutePanels, so a restart edits the same message
// instead of posting another.
type HeatmapBoard struct {
	resolver    RouteResolver
	servers     GuildServersFunc
	panels      *RoutePanels
	heatmaps    HeatmapQuerier
	serverNames ServerNameFunc
	interval    time.Duration
	now         func() time.Time
	trigger     chan struct{}

	mu          sync.Mutex // serializes the Run loop with setup-time SyncOnce
	lastSig     string
	lastRefresh time.Time
}

func NewHeatmapBoard(resolver RouteResolver, servers GuildServersFunc, panels *RoutePanels, heatmaps HeatmapQuerier, interval time.Duration) *HeatmapBoard {
	return &HeatmapBoard{resolver: resolver, servers: servers, panels: panels, heatmaps: heatmaps, interval: interval, now: time.Now, trigger: make(chan struct{}, 1)}
}

// SetServerNames labels each server's section when several servers share
// one channel.
func (b *HeatmapBoard) SetServerNames(f ServerNameFunc) {
	if b != nil {
		b.serverNames = f
	}
}

// Trigger asks for an immediate refresh (non-blocking, coalesced); nil-safe.
func (b *HeatmapBoard) Trigger() {
	if b == nil {
		return
	}
	select {
	case b.trigger <- struct{}{}:
	default:
	}
}

// Run refreshes at startup, on Trigger, when the routed channels change, and
// every interval.
func (b *HeatmapBoard) Run(ctx context.Context) {
	if b == nil {
		return
	}
	ticker := time.NewTicker(heatmapRouteCheckInterval)
	defer ticker.Stop()
	b.SyncOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.trigger:
			b.SyncOnce(ctx)
		case <-ticker.C:
			b.sync(ctx, false)
		}
	}
}

// SyncOnce refreshes every routed channel now.
func (b *HeatmapBoard) SyncOnce(ctx context.Context) { b.sync(ctx, true) }

func (b *HeatmapBoard) sync(ctx context.Context, force bool) {
	if b == nil || b.resolver == nil || b.servers == nil || b.panels == nil || b.heatmaps == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=heatmap_board", "msg", "sync panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	guildRowID, serverIDs, err := b.servers(ctx)
	if err != nil {
		slog.Warn("component=heatmap_board", "event", "heatmap_board_servers_failed", "err", err.Error())
		return
	}
	byChannel := map[string][]int64{}
	var order []string
	lookupErrs := 0
	for _, serverID := range serverIDs {
		ch, found, rerr := b.resolver.Resolve(ctx, guildRowID, serverID, routeKeyHeatmaps)
		if rerr != nil {
			lookupErrs++
			slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", routeKeyHeatmaps, "guild_id", guildRowID, "server_id", serverID, "reason", "lookup_error", "err", rerr.Error())
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
	sig := routeSignature(byChannel, order)
	if !force && sig == b.lastSig && now.Sub(b.lastRefresh) < b.interval {
		return
	}

	to := now.UTC()
	from := to.Add(-heatmapWindow)
	contentFor := func(channelID string) (PanelContent, error) {
		sections := make([]heatmapSection, 0, len(byChannel[channelID]))
		for _, serverID := range byChannel[channelID] {
			res, qerr := b.heatmaps.Query(ctx, heatmap.Request{GuildID: guildRowID, ServerID: serverID, Type: heatmap.TypePvPKills, From: from, To: to, Resolution: heatmapResolution})
			if qerr != nil {
				return PanelContent{}, qerr // this channel keeps its last good summary
			}
			name := ""
			if b.serverNames != nil {
				name = b.serverNames(serverID)
			}
			sections = append(sections, heatmapSection{ServerName: name, Result: res})
		}
		return PanelContent{Embed: BuildHeatmapSummaryEmbed(sections, heatmapWindow, heatmapResolution, to)}, nil
	}
	if _, err := b.panels.SyncEach(ctx, guildRowID, routeKeyHeatmaps, order, contentFor, lookupErrs == 0); err != nil {
		slog.Warn("component=heatmap_board", "event", "heatmap_board_sync_failed", "err", err.Error())
		return
	}
	b.lastSig, b.lastRefresh = sig, now
}

func routeSignature(byChannel map[string][]int64, order []string) string {
	keys := append([]string(nil), order...)
	sort.Strings(keys)
	var sb strings.Builder
	for _, ch := range keys {
		fmt.Fprintf(&sb, "%s=%v;", ch, byChannel[ch])
	}
	return sb.String()
}

// heatmapSection is one server's aggregate inside a channel's summary.
type heatmapSection struct {
	ServerName string
	Result     *heatmap.Result
}

// BuildHeatmapSummaryEmbed renders the PvP heatmap summary from real Phase 5
// aggregates only. Zero activity is reported as zero - never padded with
// invented hot zones.
func BuildHeatmapSummaryEmbed(sections []heatmapSection, window time.Duration, resolution int, at time.Time) *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed(heatmapTitle, presentation.CombatRed)
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "Window", Value: heatmapWindowLabel(window), Inline: true},
		{Name: "Type", Value: "PvP Kills", Inline: true},
		{Name: "Resolution", Value: fmt.Sprintf("%dm", resolution), Inline: true},
	}
	multi := len(sections) > 1
	for _, s := range sections {
		var total int64
		var cells []heatmap.Cell
		if s.Result != nil {
			total, cells = s.Result.TotalEvents, s.Result.Cells
		}
		prefix := ""
		if multi && strings.TrimSpace(s.ServerName) != "" {
			prefix = presentation.SafeName(s.ServerName, 60) + " • "
		}
		embed.Fields = append(embed.Fields,
			&discordgo.MessageEmbedField{Name: prefix + "Activity", Value: presentation.Plural(total, "Event", "Events")},
			&discordgo.MessageEmbedField{Name: prefix + "Hot Zones", Value: hotZonesText(cells)},
		)
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: presentation.FooterLiveIntel}
	presentation.StampEmbed(embed, at)
	return embed
}

// hotZonesText lists the busiest cells, most kills first (ties by position
// so the text is stable between refreshes).
func hotZonesText(cells []heatmap.Cell) string {
	if len(cells) == 0 {
		return "No PvP kills recorded in this window."
	}
	sorted := append([]heatmap.Cell(nil), cells...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Count != sorted[j].Count {
			return sorted[i].Count > sorted[j].Count
		}
		if sorted[i].CellX != sorted[j].CellX {
			return sorted[i].CellX < sorted[j].CellX
		}
		return sorted[i].CellZ < sorted[j].CellZ
	})
	if len(sorted) > heatmapHotZones {
		sorted = sorted[:heatmapHotZones]
	}
	lines := make([]string, 0, len(sorted))
	for i, c := range sorted {
		lines = append(lines, fmt.Sprintf("`#%d` X: %s • Z: %s — **%s**", i+1,
			presentation.FormatThousands(int64(math.Round(c.CenterX))),
			presentation.FormatThousands(int64(math.Round(c.CenterZ))),
			presentation.Plural(c.Count, "Kill", "Kills")))
	}
	return strings.Join(lines, "\n")
}

func heatmapWindowLabel(d time.Duration) string {
	if h := int(d.Hours()); h > 0 && d%time.Hour == 0 {
		return fmt.Sprintf("Last %d %s", h, map[bool]string{true: "Hour", false: "Hours"}[h == 1])
	}
	return "Last " + d.String()
}
