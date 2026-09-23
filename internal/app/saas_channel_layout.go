package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Channel System V2 (docs/SAAS_API.md "Channel layout").
//
// A route key names a feature (KILLFEED); a destination is the Discord
// channel one or more routes share (combat-feed carries KILLFEED and
// PVE_FEED). FEATURE ROUTE != DISCORD CHANNEL.
//
// NO BLANK CHANNEL policy: auto-setup creates a destination only when a
// route anchoring it has a working producer, and after setup every created
// channel must show Champion content - its persistent panel, or one starter
// card for a live feed that has not produced an event yet.

// ChannelHealth is one destination's or route's state after setup.
type ChannelHealth string

const (
	// HealthActive: a real publisher or panel is connected.
	HealthActive ChannelHealth = "ACTIVE"
	// HealthDisabled: the customer turned the feature off.
	HealthDisabled ChannelHealth = "DISABLED"
	// HealthNotRequired: the feature needs no Discord destination.
	HealthNotRequired ChannelHealth = "NOT_REQUIRED"
	// HealthBlocked: the backend or event source is not implemented yet.
	HealthBlocked ChannelHealth = "BLOCKED"
	// HealthBroken: it should work, but runtime verification failed.
	HealthBroken ChannelHealth = "BROKEN"
)

// Route-level details for mapped routes that do not produce yet.
const (
	detailNotYetProducing = "NOT_YET_PRODUCING_EVENTS"
	detailSourceBlocked   = "SOURCE_BLOCKED"
)

const (
	categoryLive  = "LIVE"
	categoryHub   = "HUB"
	categoryStaff = "STAFF"
)

// championCategory is one Champion-managed Discord category.
type championCategory struct {
	Key     string
	Name    string
	Private bool // hidden from @everyone; channels under it inherit that
}

var championCategories = []championCategory{
	{categoryLive, "🏆 CHAMPION • LIVE", false},
	{categoryHub, "🏆 CHAMPION • HUB", false},
	{categoryStaff, "🔒 CHAMPION • STAFF", true},
}

// starterCard is the single restrained message a live-feed channel gets
// until its first real event.
type starterCard struct {
	Title string
	Body  string
}

// championDestination is one logical Discord destination.
type championDestination struct {
	Key         string
	Label       string
	Category    string
	ChannelName string
	// Routes are every route key mapped to this channel. A route whose
	// producer does not exist yet (ADMIN_ALERTS, BUILD_FEED) is still mapped
	// so it is ready, but never justifies the channel on its own.
	Routes []string
	// Anchors are the routes whose working producer justifies creating the
	// channel. For a panel destination that is the panel itself.
	Anchors []string
	// Starter is posted once when a feed channel has no Champion content.
	// Panel destinations have none: their panel is their content.
	Starter *starterCard
}

// championDestinations is Champion's default Discord layout.
var championDestinations = []championDestination{
	{
		Key: "COMBAT_FEED", Label: "Combat Feed", Category: categoryLive, ChannelName: "🔫・combat-feed",
		Routes: []string{"KILLFEED", "PVE_FEED"}, Anchors: []string{"KILLFEED", "PVE_FEED"},
		Starter: &starterCard{"🔫 COMBAT FEED", "Champion combat events will appear here.\n\n**Includes**\n• Player eliminations\n• Deaths\n• PvE events"},
	},
	{
		Key: "HITFEED", Label: "Hitfeed", Category: categoryLive, ChannelName: "🎯・hitfeed",
		Routes: []string{"HITFEED"}, Anchors: []string{"HITFEED"},
		Starter: &starterCard{"🎯 HITFEED", "Champion hit reports will appear here."},
	},
	{
		Key: "BOUNTIES", Label: "Bounties", Category: categoryLive, ChannelName: "💀・bounties",
		Routes: []string{"BOUNTY", "BOUNTY_TRACKING"}, Anchors: []string{"BOUNTY"},
	},
	{
		Key: "CONNECTIONS", Label: "Connections", Category: categoryLive, ChannelName: "🟢・connections",
		Routes: []string{"CONNECTIONS"}, Anchors: []string{"CONNECTIONS"},
		Starter: &starterCard{"🟢 CONNECTIONS", "Player connection and disconnection activity will appear here."},
	},
	{
		Key: "HEATMAPS", Label: "Heatmaps", Category: categoryLive, ChannelName: "🗺️・heatmaps",
		Routes: []string{"HEATMAPS"}, Anchors: []string{"HEATMAPS"},
		Starter: &starterCard{"🗺️ HEATMAPS", "Champion heatmap summaries will appear here."},
	},
	{
		Key: "LEADERBOARDS", Label: "Leaderboards", Category: categoryHub, ChannelName: "📊・leaderboards",
		Routes: []string{"AUTO_LEADERBOARD", "STATS_LEADERBOARDS"}, Anchors: []string{"AUTO_LEADERBOARD", "STATS_LEADERBOARDS"},
	},
	{
		Key: "PLAYER_LINK", Label: "Player Link", Category: categoryHub, ChannelName: "🔗・player-link",
		Routes: []string{"LINK_GAMERTAG"}, Anchors: []string{"LINK_GAMERTAG"},
	},
	{
		Key: "ECONOMY", Label: "Economy", Category: categoryHub, ChannelName: "💰・economy",
		Routes: []string{"ECONOMY", "SHOP"}, Anchors: []string{"ECONOMY", "SHOP"},
		Starter: &starterCard{"💰 CHAMPION ECONOMY", "Champion Points and shop activity will appear here."},
	},
	{
		Key: "ADMIN_LOGS", Label: "Admin Logs", Category: categoryStaff, ChannelName: "🛡️・admin-logs",
		Routes: []string{"ADMIN_LOGS", "ADMIN_ALERTS", "BUILD_FEED"}, Anchors: []string{"ADMIN_LOGS", "ADMIN_ALERTS", "BUILD_FEED"},
		Starter: &starterCard{"🛡️ ADMIN LOGS", "Champion staff diagnostics will appear here.\n\n**Includes**\n• ADM log health"},
	},
}

// routeProducer is what actually publishes a route today.
type routeProducer struct {
	Health ChannelHealth
	Detail string
}

// routeProducerAudit is the audited producer behind every route key -
// verified against the runtime code, never assumed from the layout. App.
// channelRouteProducers downgrades these to BROKEN when the producer is not
// instantiated in this process.
var routeProducerAudit = map[string]routeProducer{
	"KILLFEED":           {HealthActive, "KillfeedPublisher (per server worker)"},
	"PVE_FEED":           {HealthActive, "PveFeedPublisher (per server worker)"},
	"HITFEED":            {HealthActive, "HitfeedPublisher (per server worker)"},
	"BOUNTY":             {HealthActive, "BountyBoard persistent board"},
	"BOUNTY_TRACKING":    {HealthActive, "BountyTracker lifecycle feed"},
	"CONNECTIONS":        {HealthActive, "ConnectionsPublisher (per server worker)"},
	"HEATMAPS":           {HealthBlocked, "no Discord heatmap publisher yet; website heatmaps are unaffected"},
	"AUTO_LEADERBOARD":   {HealthActive, "LeaderboardScheduler persistent leaderboard"},
	"STATS_LEADERBOARDS": {HealthActive, "RouteSyncer player stats panel"},
	"LINK_GAMERTAG":      {HealthActive, "RouteSyncer link panel"},
	"ECONOMY":            {HealthActive, "EconomyFeed"},
	"SHOP":               {HealthActive, "shop purchases/refunds, published by EconomyFeed on the ECONOMY route"},
	"ADMIN_LOGS":         {HealthActive, "ADMMonitorPublisher ADM health (per server worker)"},
	"ADMIN_ALERTS":       {HealthBlocked, detailNotYetProducing},
	"BUILD_FEED":         {HealthBlocked, detailSourceBlocked},
}

// championRouteKeys is the fixed set of valid route_key values - never an
// arbitrary client-supplied string.
var championRouteKeys = func() map[string]bool {
	out := map[string]bool{}
	for _, d := range championDestinations {
		for _, r := range d.Routes {
			out[r] = true
		}
	}
	return out
}()

// channelRouteProducers returns each route's producer state in this
// process: the audit, downgraded to BROKEN where the producer it names was
// never instantiated (routing disabled, or no service behind it).
func (a *App) channelRouteProducers() map[string]routeProducer {
	if a.channelProducersOverride != nil {
		return a.channelProducersOverride()
	}
	out := make(map[string]routeProducer, len(routeProducerAudit))
	for k, v := range routeProducerAudit {
		out[k] = v
	}
	broken := func(detail string, keys ...string) {
		for _, k := range keys {
			if out[k].Health == HealthActive {
				out[k] = routeProducer{HealthBroken, detail}
			}
		}
	}
	if a.ChannelRoutes == nil {
		broken("runtime routing is not enabled", "KILLFEED", "PVE_FEED", "HITFEED", "BOUNTY", "BOUNTY_TRACKING", "CONNECTIONS",
			"AUTO_LEADERBOARD", "STATS_LEADERBOARDS", "LINK_GAMERTAG", "ECONOMY", "SHOP", "ADMIN_LOGS")
	}
	if a.BountyBoard == nil {
		broken("bounty board is not running", "BOUNTY", "BOUNTY_TRACKING")
	}
	if a.LeaderboardScheduler == nil {
		broken("leaderboard scheduler is not running", "AUTO_LEADERBOARD")
	}
	if a.RouteSyncer == nil {
		broken("route panel syncer is not running", "STATS_LEADERBOARDS", "LINK_GAMERTAG")
	}
	if a.EconomyService == nil {
		broken("economy service is not running", "ECONOMY", "SHOP")
	}
	return out
}

// destinationPlan is one destination's setup decision.
type destinationPlan struct {
	Destination championDestination
	Health      ChannelHealth // ACTIVE: create/reuse the channel; anything else: skip
	Detail      string
	Routes      []RouteHealthReport
}

// planChannelLayout decides which destinations get a channel. A destination
// is ACTIVE when any anchor route's producer is ACTIVE; otherwise it takes
// the most actionable anchor state (BROKEN over DISABLED over BLOCKED).
func planChannelLayout(producers map[string]routeProducer) []destinationPlan {
	rank := map[ChannelHealth]int{HealthBroken: 3, HealthDisabled: 2, HealthBlocked: 1, HealthNotRequired: 0}
	plans := make([]destinationPlan, 0, len(championDestinations))
	for _, d := range championDestinations {
		p := destinationPlan{Destination: d, Health: HealthNotRequired}
		for _, key := range d.Routes {
			prod, ok := producers[key]
			if !ok {
				prod = routeProducer{HealthBlocked, "no producer"}
			}
			p.Routes = append(p.Routes, RouteHealthReport{RouteKey: key, Health: prod.Health, Detail: prod.Detail})
		}
		for _, key := range d.Anchors {
			prod, ok := producers[key]
			if !ok {
				prod = routeProducer{HealthBlocked, "no producer"}
			}
			if prod.Health == HealthActive {
				p.Health, p.Detail = HealthActive, ""
				break
			}
			if rank[prod.Health] > rank[p.Health] {
				p.Health, p.Detail = prod.Health, prod.Detail
			}
		}
		plans = append(plans, p)
	}
	return plans
}

// --- report DTOs --------------------------------------------------------------

// RouteHealthReport is one route's state inside a destination.
type RouteHealthReport struct {
	RouteKey string        `json:"routeKey"`
	Health   ChannelHealth `json:"health"`
	Detail   string        `json:"detail,omitempty"`
}

// DestinationChecks is the post-setup verification of one created channel.
// A channel fails setup when it exists but its producer or its visible
// Champion content is missing.
type DestinationChecks struct {
	ChannelExists     bool `json:"channelExists"`
	RouteMapped       bool `json:"routeMapped"`
	ProducerConnected bool `json:"producerConnected"`
	BotCanSend        bool `json:"botCanSend"`
	VisibleContent    bool `json:"visibleContent"`
}

func (c DestinationChecks) passed() bool {
	return c.ChannelExists && c.RouteMapped && c.ProducerConnected && c.BotCanSend && c.VisibleContent
}

// ChannelDestinationReport is one destination's outcome.
type ChannelDestinationReport struct {
	Key         string              `json:"key"`
	Label       string              `json:"label"`
	Category    string              `json:"category"`
	ChannelID   string              `json:"channelId,omitempty"`
	ChannelName string              `json:"channelName"`
	Health      ChannelHealth       `json:"health"`
	Detail      string              `json:"detail,omitempty"`
	Created     bool                `json:"created"`
	StarterSent bool                `json:"starterSent"`
	Routes      []RouteHealthReport `json:"routes"`
	Checks      *DestinationChecks  `json:"checks,omitempty"`
}

// RetirableChannel is a Champion-managed channel or category no route
// references any more. Champion never deletes it; the customer may.
type RetirableChannel struct {
	ChannelID    string   `json:"channelId"`
	ChannelName  string   `json:"channelName,omitempty"`
	Kind         string   `json:"kind"` // CHANNEL | CATEGORY
	FormerRoutes []string `json:"formerRoutes,omitempty"`
}

// --- applying the layout ---------------------------------------------------------

// channelLayoutDiscord is the Discord surface applying the layout needs;
// discordGuildVerifier satisfies it.
type channelLayoutDiscord interface {
	ListAllGuildChannels(guildID string) ([]discord.RawGuildChannel, error)
	CreateGuildCategory(guildID, name string) (*discord.RawGuildChannel, error)
	CreatePrivateGuildCategory(guildID, name string) (*discord.RawGuildChannel, error)
	CreateGuildTextChannel(guildID, name, parentCategoryID string) (*discord.RawGuildChannel, error)
	Verify(guildID, channelID string) discord.Verification
	SendChannelEmbed(channelID string, embed *discordgo.MessageEmbed) error
	ChannelHasBotMessage(channelID string) (bool, error)
}

// channelRouteWriter persists route rows; *repository.ChannelRouteRepository
// satisfies it.
type channelRouteWriter interface {
	UpsertRoute(ctx context.Context, organizationID, installationID int64, routeKey, channelID string, managedByChampion bool) error
	DeleteRoute(ctx context.Context, organizationID, installationID int64, routeKey string) error
}

type channelLayoutInput struct {
	OrganizationID, InstallationID int64
	GuildID                        string
	Existing                       []repository.ChannelRoute
	Producers                      map[string]routeProducer
	// SyncPanels posts/restores persistent panels for the just-written
	// routes before verification. Optional.
	SyncPanels func(ctx context.Context)
}

type channelLayoutResult struct {
	Categories   map[string]discord.RawGuildChannel // by category key
	Routes       map[string]ChannelRouteInfo
	Destinations []ChannelDestinationReport
	Retirable    []RetirableChannel
}

// errKillfeedUnavailable: the combat feed cannot be created, so there is
// nothing Champion could configure.
var errKillfeedUnavailable = fmt.Errorf("killfeed producer unavailable")

// applyChannelLayout creates or reuses every ACTIVE destination, maps its
// routes, unmaps routes of skipped destinations, posts starter cards where a
// channel would otherwise be blank, and verifies each channel. Reuse is
// ID-first (a persisted route channel already carrying the V2 name), then
// by name under the category; a channel is created only when neither exists,
// so repeat runs never duplicate. Nothing is ever deleted in Discord.
func applyChannelLayout(ctx context.Context, d channelLayoutDiscord, w channelRouteWriter, in channelLayoutInput) (*channelLayoutResult, error) {
	plans := planChannelLayout(in.Producers)
	for _, p := range plans {
		if p.Destination.Key == "COMBAT_FEED" && p.Health != HealthActive {
			return nil, errKillfeedUnavailable
		}
	}

	channels, err := d.ListAllGuildChannels(in.GuildID)
	if err != nil {
		return nil, fmt.Errorf("list guild channels: %w", err)
	}
	byID := make(map[string]discord.RawGuildChannel, len(channels))
	for _, ch := range channels {
		byID[ch.ID] = ch
	}
	existing := make(map[string]repository.ChannelRoute, len(in.Existing))
	for _, r := range in.Existing {
		existing[r.RouteKey] = r
	}

	// 1. ID-first reuse: a persisted route channel that already is this
	//    destination's V2 channel. Its parent hints the category.
	reused := map[string]discord.RawGuildChannel{}
	categoryHint := map[string]string{}
	for _, p := range plans {
		if p.Health != HealthActive {
			continue
		}
		for _, key := range p.Destination.Routes {
			r, ok := existing[key]
			if !ok {
				continue
			}
			ch, ok := byID[r.ChannelID]
			if ok && ch.Type == discordgo.ChannelTypeGuildText && strings.EqualFold(ch.Name, p.Destination.ChannelName) {
				reused[p.Destination.Key] = ch
				if parent, ok := byID[ch.ParentID]; ok && parent.Type == discordgo.ChannelTypeGuildCategory && categoryHint[p.Destination.Category] == "" {
					categoryHint[p.Destination.Category] = parent.ID
				}
				break
			}
		}
	}

	// 2. Categories, only those an ACTIVE destination needs.
	needed := map[string]bool{}
	for _, p := range plans {
		if p.Health == HealthActive {
			needed[p.Destination.Category] = true
		}
	}
	categories := map[string]discord.RawGuildChannel{}
	for _, cat := range championCategories {
		if !needed[cat.Key] {
			continue
		}
		resolved, err := resolveLayoutCategory(d, in.GuildID, channels, cat, categoryHint[cat.Key])
		if err != nil {
			return nil, fmt.Errorf("category %s: %w", cat.Key, err)
		}
		categories[cat.Key] = resolved
	}

	// 3. Channels, then routes.
	result := &channelLayoutResult{Categories: categories, Routes: map[string]ChannelRouteInfo{}}
	finalChannel := map[string]bool{}
	for _, p := range plans {
		dest := p.Destination
		report := ChannelDestinationReport{Key: dest.Key, Label: dest.Label, Category: dest.Category, ChannelName: dest.ChannelName, Health: p.Health, Detail: p.Detail, Routes: p.Routes}
		if p.Health != HealthActive {
			// No producer: no channel, and no Champion route left pointing
			// at a channel nothing will ever post to. A customer's own route
			// is left alone.
			for _, key := range dest.Routes {
				if r, ok := existing[key]; ok && r.ManagedByChampion {
					if err := w.DeleteRoute(ctx, in.OrganizationID, in.InstallationID, key); err != nil {
						return nil, fmt.Errorf("unmap route %s: %w", key, err)
					}
				}
			}
			result.Destinations = append(result.Destinations, report)
			continue
		}
		ch, created, err := resolveLayoutChannel(d, in.GuildID, channels, categories[dest.Category].ID, dest.ChannelName, reused[dest.Key])
		if err != nil {
			return nil, fmt.Errorf("channel %s: %w", dest.Key, err)
		}
		report.ChannelID, report.ChannelName, report.Created = ch.ID, ch.Name, created
		finalChannel[ch.ID] = true
		for _, key := range dest.Routes {
			if err := w.UpsertRoute(ctx, in.OrganizationID, in.InstallationID, key, ch.ID, true); err != nil {
				return nil, fmt.Errorf("map route %s: %w", key, err)
			}
			result.Routes[key] = ChannelRouteInfo{ChannelID: ch.ID, ChannelName: ch.Name, ManagedByChampion: true}
		}
		report.Checks = &DestinationChecks{ChannelExists: true, RouteMapped: true, ProducerConnected: true}
		result.Destinations = append(result.Destinations, report)
	}
	// Stored routes outside the vocabulary (a removed route such as CASINO)
	// are unmapped too.
	for key := range existing {
		if !championRouteKeys[key] {
			if err := w.DeleteRoute(ctx, in.OrganizationID, in.InstallationID, key); err != nil {
				return nil, fmt.Errorf("unmap route %s: %w", key, err)
			}
		}
	}

	// 4. Persistent panels first, so verification sees them.
	if in.SyncPanels != nil {
		in.SyncPanels(ctx)
	}

	// 5. Verify, posting one starter card where a feed channel is blank.
	for i := range result.Destinations {
		rep := &result.Destinations[i]
		if rep.Checks == nil {
			continue
		}
		dest := destinationByKey(rep.Key)
		v := d.Verify(in.GuildID, rep.ChannelID)
		rep.Checks.BotCanSend = v.ChannelFound && len(v.Missing) == 0
		has, err := d.ChannelHasBotMessage(rep.ChannelID)
		if err != nil {
			slog.Warn("component=saas_api", "msg", "read channel content failed", "destination", rep.Key, "err", err.Error())
		}
		if err == nil && !has && dest.Starter != nil && rep.Checks.BotCanSend {
			if serr := d.SendChannelEmbed(rep.ChannelID, starterEmbed(*dest.Starter)); serr != nil {
				slog.Warn("component=saas_api", "msg", "starter card failed", "destination", rep.Key, "err", serr.Error())
			} else {
				has, rep.StarterSent = true, true
			}
		}
		rep.Checks.VisibleContent = has
		if !rep.Checks.passed() {
			rep.Health = HealthBroken
			rep.Detail = brokenDetail(*rep.Checks, dest.Starter == nil)
		}
	}

	// 6. Champion-managed channels no route references any more. Reported,
	//    never deleted.
	former := map[string][]string{}
	var order []string
	for _, r := range in.Existing {
		if !r.ManagedByChampion || finalChannel[r.ChannelID] {
			continue
		}
		if _, seen := former[r.ChannelID]; !seen {
			order = append(order, r.ChannelID)
		}
		former[r.ChannelID] = append(former[r.ChannelID], r.RouteKey)
	}
	for _, id := range order {
		ch, ok := byID[id]
		if !ok {
			continue // already gone from Discord
		}
		result.Retirable = append(result.Retirable, RetirableChannel{ChannelID: id, ChannelName: ch.Name, Kind: "CHANNEL", FormerRoutes: former[id]})
	}
	return result, nil
}

func destinationByKey(key string) championDestination {
	for _, d := range championDestinations {
		if d.Key == key {
			return d
		}
	}
	return championDestination{}
}

func brokenDetail(c DestinationChecks, panel bool) string {
	switch {
	case !c.BotCanSend:
		return "Champion cannot send messages in this channel"
	case !c.VisibleContent && panel:
		return "the persistent panel did not appear in this channel"
	case !c.VisibleContent:
		return "no Champion content could be posted in this channel"
	}
	return "verification failed"
}

// resolveLayoutCategory: hinted ID (parent of a reused channel), then name,
// then create - private for the staff category.
func resolveLayoutCategory(d channelLayoutDiscord, guildID string, channels []discord.RawGuildChannel, cat championCategory, hintID string) (discord.RawGuildChannel, error) {
	if hintID != "" {
		for _, ch := range channels {
			if ch.ID == hintID && ch.Type == discordgo.ChannelTypeGuildCategory {
				return ch, nil
			}
		}
	}
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildCategory && strings.EqualFold(strings.TrimSpace(ch.Name), cat.Name) {
			return ch, nil
		}
	}
	var created *discord.RawGuildChannel
	var err error
	if cat.Private {
		created, err = d.CreatePrivateGuildCategory(guildID, cat.Name)
	} else {
		created, err = d.CreateGuildCategory(guildID, cat.Name)
	}
	if err != nil {
		return discord.RawGuildChannel{}, err
	}
	return *created, nil
}

// resolveLayoutChannel: the ID-reused channel, then a same-named text
// channel under the category, then create. Name recovery never adopts a
// same-named channel elsewhere in the guild.
func resolveLayoutChannel(d channelLayoutDiscord, guildID string, channels []discord.RawGuildChannel, categoryID, name string, reused discord.RawGuildChannel) (discord.RawGuildChannel, bool, error) {
	if reused.ID != "" {
		return reused, false, nil
	}
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildText && ch.ParentID == categoryID && strings.EqualFold(ch.Name, name) {
			return ch, false, nil
		}
	}
	created, err := d.CreateGuildTextChannel(guildID, name, categoryID)
	if err != nil {
		return discord.RawGuildChannel{}, false, err
	}
	return *created, true, nil
}

// starterEmbed renders a starter card: compact, neutral, no giant welcome.
func starterEmbed(s starterCard) *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed(s.Title, presentation.Steel)
	embed.Description = s.Body
	return embed
}
