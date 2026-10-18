package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// Online-players voice counter: ONE authority and fallback policy
// (docs/ONLINE_COUNTER_AND_LINK_CHECK.md).
//
// The counter shows CURRENTLY CONNECTED players of the public server, which
// is a different fact from player activity history. Sources, in order:
//
//  1. Nitrado's live gameserver query (query.player_current / player_max),
//     only when this evaluation's read succeeded AND the response names the
//     requested service (GameserverLive.BelongsTo). A stopped server is a
//     known 0; a restarting one (no query) is not a count.
//  2. The ADM presence tracker, only with evidence that it is complete: a
//     COMPLETE player list reconciled it within onlineCounterADMTrust, or a
//     verified restart (BOOT_RESET) was read from the new boot's start and the
//     pipeline is still reading (onlineCounterPipelineFresh). Connect and
//     disconnect lines alone are never a live count.
//  3. Otherwise UNKNOWN: the channel keeps its last value for
//     onlineCounterUnknownGrace, then shows "?" - never a manufactured 0.
//
// Disagreement: when both 1 and 2 are available and differ, Nitrado is shown
// (it is what the Nitrado panel and players see) and the disagreement is
// recorded with the time it started; one lasting longer than
// onlineCounterADMTrust degrades health (runtime_health.go).
//
// Only one source is ever used per evaluation, so a player is never counted
// twice; the tracker itself is keyed by DayZ ID.
//
// All Discord renames happen on this loop's goroutine or the counter's own
// timers - never on the ADM pipeline, which only pokes this loop. Renames are
// spaced by discord.DefaultCounterMinRenameInterval (Discord allows 2 channel
// renames per 10 minutes) and a 429 is scheduled, not slept through.

const (
	onlineCounterPollInterval = 60 * time.Second
	// onlineCounterMinEval bounds how often a presence change can trigger an
	// extra Nitrado read.
	onlineCounterMinEval = 15 * time.Second
	// onlineCounterADMTrust is how long a complete ADM player list keeps the
	// tracker trustworthy (DayZ writes one every 5 minutes).
	onlineCounterADMTrust = 11 * time.Minute
	// onlineCounterPipelineFresh is how recently the ADM worker must have
	// checked its source for a BOOT_RESET tracker to still be complete.
	onlineCounterPipelineFresh = 2 * time.Minute
	// onlineCounterUnknownGrace holds the last known value through a brief
	// outage before the channel is switched to the unknown state.
	onlineCounterUnknownGrace = 10 * time.Minute
	onlineCounterLiveTimeout  = 10 * time.Second
)

// liveStatusReader is the Nitrado read the counter needs (*nitrado.Client).
type liveStatusReader interface {
	GameserverLive(ctx context.Context, serviceID string) (nitrado.GameserverLive, error)
}

// counterSource is what a running server worker exposes to the counter.
type counterSource struct {
	serviceID string
	live      liveStatusReader
	engine    *killfeed.Engine
}

// onlineReadingInput is everything resolveOnlineReading decides from.
type onlineReadingInput struct {
	ServiceID  string // the service the Nitrado read was for; "" skips the ownership check
	Live       nitrado.GameserverLive
	LiveErr    error
	LiveRead   bool // a Nitrado read was attempted in this evaluation
	Tracker    int
	ADMProven  time.Time // last complete ADM player list reconciliation
	ADMState   string    // killfeed.PresenceEvidence state
	PipelineAt time.Time // last ADM source check by the worker
	KnownSlots int       // capacity remembered from an earlier read
	Now        time.Time
}

// Counter reading sources, reported in diagnostics and logs.
const (
	counterSourceNitrado        = "NITRADO_QUERY"
	counterSourceNitradoStopped = "NITRADO_SERVER_STOPPED"
	counterSourceADMPlayerList  = "ADM_PLAYER_LIST"
	counterSourceADMBootReset   = "ADM_BOOT_RESET"
	counterSourceUnknown        = "UNKNOWN"
)

// nitradoTrusted reports whether this evaluation's Nitrado read may be used.
func nitradoTrusted(in onlineReadingInput) bool {
	if !in.LiveRead || in.LiveErr != nil {
		return false
	}
	return in.ServiceID == "" || in.Live.BelongsTo(in.ServiceID)
}

// admEvidence reports whether the ADM tracker is currently complete, and why.
func admEvidence(in onlineReadingInput) (bool, string) {
	if !in.ADMProven.IsZero() && in.Now.Sub(in.ADMProven) <= onlineCounterADMTrust {
		return true, counterSourceADMPlayerList
	}
	if in.ADMState == killfeed.PresenceBootReset && !in.PipelineAt.IsZero() && in.Now.Sub(in.PipelineAt) <= onlineCounterPipelineFresh {
		return true, counterSourceADMBootReset
	}
	return false, ""
}

// resolveOnlineReading picks the authoritative current player count, or
// reports it unknown. It never derives a count from activity history.
func resolveOnlineReading(in onlineReadingInput) (discord.CounterReading, string) {
	slots := in.KnownSlots
	if nitradoTrusted(in) {
		if c := in.Live.Capacity(); c > 0 {
			slots = c
		}
		switch {
		case in.Live.Running() && in.Live.PlayerCurrent != nil:
			return discord.CounterReading{Count: *in.Live.PlayerCurrent, Slots: slots, Known: true}, counterSourceNitrado
		case in.Live.Stopped():
			return discord.CounterReading{Count: 0, Slots: slots, Known: true}, counterSourceNitradoStopped
		}
	}
	if ok, source := admEvidence(in); ok {
		return discord.CounterReading{Count: in.Tracker, Slots: slots, Known: true}, source
	}
	return discord.CounterReading{Slots: slots}, counterSourceUnknown
}

// presenceDisagreement records Nitrado and the proven ADM tracker disagreeing.
type presenceDisagreement struct {
	NitradoCount int       `json:"nitrado_count"`
	ADMCount     int       `json:"adm_count"`
	Since        time.Time `json:"since"`
}

// onlineCounterStatus is the latest evaluation, for diagnostics.
type onlineCounterStatus struct {
	ServerID            int64
	Reading             discord.CounterReading
	Source              string
	NitradoStatus       string
	NitradoCount        *int
	NitradoError        string
	NitradoWrongService bool
	TrackerCount        int
	ADMState            string
	Disagreement        *presenceDisagreement
	EvaluatedAt         time.Time
	LastKnownAt         time.Time
	Held                bool // unknown, but the last known value is still displayed
}

type onlineCounterLoop struct {
	mu           sync.Mutex
	sources      map[int64]counterSource
	status       onlineCounterStatus
	serverStatus map[int64]onlineCounterStatus // latest evaluation per server, public or guild counter
	slots        map[int64]int
	lastKnown    map[int64]time.Time
	disagreement map[int64]*presenceDisagreement
	lastEval     time.Time
	// guilds are the counters of every guild other than the primary one that
	// has an active server and an ONLINE_COUNTER route (guildCounter).
	guilds map[int64]*guildCounter
	// startedAt starts the unknown grace at process start too, so a restart
	// of the bot (workers not registered yet, first Nitrado read failing)
	// never flashes the channel to "?".
	startedAt time.Time
	poke      chan struct{}
}

// guildCounter is one non-primary guild's online-players voice counter: its
// own channel (that guild's ONLINE_COUNTER route, no legacy fallback) renamed
// from the count of its own public server. The primary guild keeps
// App.onlineCounter, which also carries the legacy GuildSetup fallback.
type guildCounter struct {
	guildRowID     int64
	discordGuildID string
	serverID       int64
	counter        *discord.VoiceChannelCounter
}

func (a *App) counterLoop() *onlineCounterLoop {
	a.onlineLoopOnce.Do(func() {
		a.onlineLoop = &onlineCounterLoop{
			sources:      map[int64]counterSource{},
			serverStatus: map[int64]onlineCounterStatus{},
			slots:        map[int64]int{},
			lastKnown:    map[int64]time.Time{},
			disagreement: map[int64]*presenceDisagreement{},
			guilds:       map[int64]*guildCounter{},
			startedAt:    time.Now(),
			poke:         make(chan struct{}, 1),
		}
	})
	return a.onlineLoop
}

func (a *App) registerCounterSource(serverID int64, src counterSource) {
	l := a.counterLoop()
	l.mu.Lock()
	l.sources[serverID] = src
	l.mu.Unlock()
	a.pokeOnlineCounter()
}

func (a *App) unregisterCounterSource(serverID int64) {
	l := a.counterLoop()
	l.mu.Lock()
	delete(l.sources, serverID)
	l.mu.Unlock()
}

// pokeOnlineCounter asks the loop to re-evaluate soon. Never blocks, so it is
// safe to call from the ADM pipeline.
func (a *App) pokeOnlineCounter() {
	select {
	case a.counterLoop().poke <- struct{}{}:
	default:
	}
}

// counterServes reports whether a counter (the public one or a guild's)
// currently shows serverID, so its presence changes are worth a poke.
func (a *App) counterServes(serverID int64) bool {
	if a.ownsPublicCounter(serverID) {
		return true
	}
	l := a.counterLoop()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, g := range l.guilds {
		if g.serverID == serverID {
			return true
		}
	}
	return false
}

// OnlineCounterStatus returns the latest counter evaluation of the public
// (primary guild) server.
func (a *App) OnlineCounterStatus() onlineCounterStatus {
	l := a.counterLoop()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

// onlineCounterStatusFor returns the latest evaluation made for serverID by
// whichever counter shows it (the public one or its guild's). ok is false when
// no counter has evaluated that server.
func (a *App) onlineCounterStatusFor(serverID int64) (onlineCounterStatus, bool) {
	l := a.counterLoop()
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.serverStatus[serverID]
	return st, ok && !st.EvaluatedAt.IsZero()
}

// runOnlineCounter drives the voice counter until ctx ends. It evaluates once
// at start (startup/restart recovery: the channel's actual name is read and
// reconciled before any rename).
func (a *App) runOnlineCounter(ctx context.Context, counter *discord.VoiceChannelCounter) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=voice_counter", "event", "panic_recovered", "err", fmt.Sprint(r))
		}
	}()
	l := a.counterLoop()
	ticker := time.NewTicker(onlineCounterPollInterval)
	defer ticker.Stop()
	evaluate := func() {
		a.evaluateOnlineCounter(ctx, counter)
		a.evaluateGuildCounters(ctx)
	}
	evaluate()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			evaluate()
		case <-l.poke:
			l.mu.Lock()
			wait := onlineCounterMinEval - time.Since(l.lastEval)
			l.mu.Unlock()
			if wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
			evaluate()
		}
	}
}

// readServerCount resolves one server's current player count from its
// registered sources (Nitrado live query, then the proven ADM tracker) and
// records the evaluation under that server. It makes no Discord call.
func (a *App) readServerCount(ctx context.Context, serverID int64, now time.Time) (discord.CounterReading, onlineCounterStatus) {
	l := a.counterLoop()
	l.mu.Lock()
	src, haveSource := l.sources[serverID]
	in := onlineReadingInput{KnownSlots: l.slots[serverID], Now: now}
	l.mu.Unlock()

	status := onlineCounterStatus{ServerID: serverID, EvaluatedAt: now, ADMState: killfeed.PresenceUnknown}
	if haveSource {
		if src.live != nil && src.serviceID != "" {
			liveCtx, cancel := context.WithTimeout(ctx, onlineCounterLiveTimeout)
			in.Live, in.LiveErr = src.live.GameserverLive(liveCtx, src.serviceID)
			cancel()
			in.LiveRead, in.ServiceID = true, src.serviceID
			if in.LiveErr != nil {
				status.NitradoError = in.LiveErr.Error()
			} else if !in.Live.BelongsTo(src.serviceID) {
				status.NitradoWrongService = true
				status.NitradoError = "response does not name the requested service"
			} else {
				status.NitradoStatus = in.Live.Status
				status.NitradoCount = in.Live.PlayerCurrent
			}
		}
		if src.engine != nil {
			in.Tracker = src.engine.PlayerTracker().OnlineCount()
			in.ADMProven = src.engine.PlayerListStats().LastCompleteSnapshotAt
			in.ADMState = src.engine.PresenceEvidence().State
			if d := src.engine.Diagnostics(); d != nil {
				in.PipelineAt = d.Snapshot().LastMetadataCheck
			}
			status.ADMState = in.ADMState
		}
	}
	reading, source := resolveOnlineReading(in)
	status.Reading, status.Source, status.TrackerCount = reading, source, in.Tracker

	// Explicit disagreement between two trusted sources.
	var disagreement *presenceDisagreement
	if admOK, _ := admEvidence(in); admOK && source == counterSourceNitrado && reading.Count != in.Tracker {
		disagreement = &presenceDisagreement{NitradoCount: reading.Count, ADMCount: in.Tracker, Since: now}
	}

	l.mu.Lock()
	if reading.Slots > 0 {
		l.slots[serverID] = reading.Slots
	}
	if reading.Known {
		l.lastKnown[serverID] = now
	}
	if disagreement != nil {
		if prev := l.disagreement[serverID]; prev != nil {
			disagreement.Since = prev.Since
		}
		l.disagreement[serverID] = disagreement
	} else {
		delete(l.disagreement, serverID)
	}
	status.Disagreement = disagreement
	status.LastKnownAt = l.lastKnown[serverID]
	graceFrom := status.LastKnownAt
	if l.startedAt.After(graceFrom) {
		graceFrom = l.startedAt
	}
	status.Held = !reading.Known && now.Sub(graceFrom) < onlineCounterUnknownGrace
	previous := l.serverStatus[serverID]
	l.serverStatus[serverID] = status
	l.mu.Unlock()

	if previous.Source != source || previous.Reading != reading {
		slog.Info("component=voice_counter", "event", "reading", "server_id", serverID, "source", source,
			"known", reading.Known, "count", reading.Count, "slots", reading.Slots, "tracker_count", in.Tracker,
			"adm_state", in.ADMState, "nitrado_status", status.NitradoStatus, "nitrado_error", status.NitradoError != "")
	}
	if disagreement != nil && (previous.Disagreement == nil || *previous.Disagreement != *disagreement) {
		slog.Warn("component=presence", "event", "presence_disagreement", "server_id", serverID,
			"nitrado_count", disagreement.NitradoCount, "adm_count", disagreement.ADMCount, "since", disagreement.Since.UTC().Format(time.RFC3339), "shown", "nitrado")
	}
	return reading, status
}

// publishCounter renames counter to reading when this process is the leader.
// The first publish to a channel reads its current name first (Reconcile), so
// an identical name is never renamed, a stale one is corrected, and ownership
// is validated.
func (a *App) publishCounter(counter *discord.VoiceChannelCounter, reading discord.CounterReading) {
	if !a.isSingletonLeader() {
		// Another process renames the channel. This one keeps its reading current for its own
		// API and health output, and takes over the renaming if it becomes the leader.
		return
	}
	if counter.LastName() == "" {
		if err := counter.Reconcile(reading); err != nil {
			slog.Warn("component=voice_counter", "event", "reconcile_failed", "channel_id", counter.ChannelID(), "err", err.Error())
		}
		return
	}
	counter.Publish(reading)
}

// evaluateOnlineCounter resolves the public server's current count and
// publishes it to the primary guild's voice counter.
func (a *App) evaluateOnlineCounter(ctx context.Context, counter *discord.VoiceChannelCounter) {
	l := a.counterLoop()
	now := time.Now()
	a.counterOwnerMu.RLock()
	serverID := a.publicCounterServerID
	a.counterOwnerMu.RUnlock()

	l.mu.Lock()
	l.lastEval = now
	l.mu.Unlock()
	if serverID == 0 {
		return // no installation selected: leave the channel alone
	}
	reading, status := a.readServerCount(ctx, serverID, now)
	l.mu.Lock()
	l.status = status
	l.mu.Unlock()

	if reading.Known && a.State != nil {
		a.State.SetOnlinePlayers(reading.Count)
	}
	if counter == nil || status.Held {
		return
	}
	a.bindCounterChannel(ctx, counter)
	a.publishCounter(counter, reading)
	if a.State != nil {
		a.State.SetOnlineCounter(counter.LastPublished(), counter.UpdateErrors(), counter.PermissionBlocked())
	}
}

// evaluateGuildCounters runs the online counter of every guild other than
// the primary one: each guild with an active server and an ONLINE_COUNTER
// route gets its own counter on that route's channel, renamed from the count
// of its own public server (its selected public server, else its first
// active server). A guild without the route has no counter and no fallback;
// a route lookup error keeps the current binding (not evidence the route is
// gone). The primary guild is evaluateOnlineCounter's.
func (a *App) evaluateGuildCounters(ctx context.Context) {
	primaryGuild, allGuilds := a.counterProviders()
	if primaryGuild == nil || allGuilds == nil || a.ChannelRoutes == nil || a.voiceNamer == nil {
		return
	}
	guilds, err := allGuilds(ctx)
	if err != nil {
		slog.Warn("component=voice_counter", "event", "guild_list_failed", "err", err.Error())
		return
	}
	// The primary guild is the public counter's; without its row id this pass cannot tell it
	// apart from the others, so it keeps the current counters rather than building a second
	// counter on the primary guild's channel (the same lookup-error rule as the routes).
	primaryRowID, _, listErr := primaryGuild(ctx)
	if listErr != nil || primaryRowID == 0 {
		slog.Warn("component=voice_counter", "event", "primary_guild_lookup_failed", "err", fmt.Sprint(listErr))
		return
	}
	l := a.counterLoop()
	now := time.Now()
	seen := map[int64]bool{}
	for _, g := range guilds {
		if g.GuildRowID == primaryRowID || len(g.ServerIDs) == 0 {
			continue
		}
		serverID, discordGuildID := a.guildCounterServer(ctx, g)
		channelID, outcome := a.guildCounterRouteChannel(ctx, g.GuildRowID, serverID, g.ServerIDs)
		l.mu.Lock()
		gc := l.guilds[g.GuildRowID]
		switch outcome {
		case counterRouteFound:
			if gc == nil {
				gc = &guildCounter{guildRowID: g.GuildRowID, counter: discord.NewVoiceChannelCounter(a.voiceNamer, channelID)}
				counter := gc
				// The guild's public server can change later; report each publish on the
				// server the counter shows at that moment.
				gc.counter.OnPublish(func(count int, result string) {
					l.mu.Lock()
					sid := counter.serverID
					l.mu.Unlock()
					a.recordVoicePublishFor(sid, count, result)
				})
				gc.counter.OnFault(func(class, faultChannelID string) {
					l.mu.Lock()
					sid := counter.serverID
					l.mu.Unlock()
					a.alertChannelAccess(counter.guildRowID, sid, routing.RouteOnlineCounter, faultChannelID, class)
				})
				l.guilds[g.GuildRowID] = gc
				slog.Info("component=voice_counter", "event", "guild_counter_started", "guild_row_id", g.GuildRowID, "server_id", serverID, "channel_id", channelID)
			} else if gc.counter.ChannelID() != channelID {
				slog.Info("component=voice_counter", "event", "bound_to_route", "guild_row_id", g.GuildRowID, "channel_id", channelID, "previous_channel_id", gc.counter.ChannelID())
				gc.counter.SetChannelID(channelID)
			}
		case counterRouteAbsent:
			if gc != nil {
				slog.Info("component=voice_counter", "event", "guild_counter_stopped", "guild_row_id", g.GuildRowID, "reason", "route removed")
				gc.counter.Stop() // never rename a channel from a timer armed before the route went
				delete(l.guilds, g.GuildRowID)
				gc = nil
			}
		}
		if gc != nil {
			seen[g.GuildRowID] = true
			gc.serverID = serverID
			if discordGuildID != "" && gc.discordGuildID != discordGuildID {
				gc.discordGuildID = discordGuildID
				gc.counter.SetGuildID(discordGuildID)
			}
		}
		l.mu.Unlock()
		if gc == nil {
			continue
		}
		reading, status := a.readServerCount(ctx, serverID, now)
		if status.Held {
			continue
		}
		a.publishCounter(gc.counter, reading)
	}
	// A guild whose last active server was removed loses its counter.
	l.mu.Lock()
	for id, gc := range l.guilds {
		if !seen[id] {
			slog.Info("component=voice_counter", "event", "guild_counter_stopped", "guild_row_id", id, "reason", "no active server")
			gc.counter.Stop()
			delete(l.guilds, id)
		}
	}
	l.mu.Unlock()
}

// guildCounterServer picks the server a guild's counter shows: the guild's
// selected public server when it is one of its active servers, else the
// first active server. It also returns the guild's Discord id (for channel
// ownership validation), "" when unknown.
func (a *App) guildCounterServer(ctx context.Context, g discord.GuildServers) (int64, string) {
	serverID := g.ServerIDs[0]
	discordGuildID := ""
	if a.Guilds != nil {
		if rec, err := a.Guilds.GetGuildByID(ctx, g.GuildRowID); err == nil && rec != nil {
			discordGuildID = rec.DiscordGuildID
			for _, id := range g.ServerIDs {
				if id == rec.SelectedPublicServerID {
					serverID = id
					break
				}
			}
		}
	}
	return serverID, discordGuildID
}

// guildCounterRouteChannel resolves a guild's ONLINE_COUNTER route, the
// counter's own server first, with the same outcomes as
// onlineCounterRouteChannel.
func (a *App) guildCounterRouteChannel(ctx context.Context, guildRowID, preferred int64, serverIDs []int64) (string, string) {
	order := append([]int64{preferred}, serverIDs...)
	lookupFailed := false
	for _, serverID := range order {
		channelID, found, err := a.ChannelRoutes.Resolve(ctx, guildRowID, serverID, routing.RouteOnlineCounter)
		if err != nil {
			lookupFailed = true
			continue
		}
		if found && channelID != "" {
			return channelID, counterRouteFound
		}
	}
	if lookupFailed {
		return "", counterRouteError
	}
	return "", counterRouteAbsent
}

// recordVoicePublishFor reports a counter publish on the server's own engine
// diagnostics.
func (a *App) recordVoicePublishFor(serverID int64, count int, result string) {
	a.presenceMu.Lock()
	engine := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if engine != nil {
		engine.RecordVoicePublish(count, result)
	}
}

// knownPresenceCount returns the count the counter would show for serverID
// (its latest known reading), for health output. known is false while the
// count is unknown or no counter shows the server.
func (a *App) knownPresenceCount(serverID int64) (int, bool) {
	st, ok := a.onlineCounterStatusFor(serverID)
	if !ok || !st.Reading.Known {
		return 0, false
	}
	return st.Reading.Count, true
}

// counterProviders returns the guild providers the counter loop reads: the primary guild's
// (row id and servers) and every guild's. They are set once routing starts, after the loop is
// running, so the hand-off is synchronised.
func (a *App) counterProviders() (func(context.Context) (int64, []int64, error), discord.AllGuildServersFunc) {
	a.counterProvidersMu.RLock()
	defer a.counterProvidersMu.RUnlock()
	return a.guildServers, a.allGuildServers
}

// setCounterProviders publishes the guild providers to the counter loop.
func (a *App) setCounterProviders(primary func(context.Context) (int64, []int64, error), all discord.AllGuildServersFunc) {
	a.counterProvidersMu.Lock()
	a.guildServers, a.allGuildServers = primary, all
	a.counterProvidersMu.Unlock()
	a.pokeOnlineCounter()
}

// alertChannelAccess tells a guild's admins that Champion lost the use of one of its routed
// channels: an ADMIN_ALERTS card for the guild's server, once per fault (the publisher dedupes
// nothing here; callers raise it on a fault transition only).
func (a *App) alertChannelAccess(guildRowID, serverID int64, routeKey, channelID, class string) {
	if a.AdminAlerts == nil {
		return
	}
	if serverID == 0 {
		if _, all := a.counterProviders(); all != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if guilds, err := all(ctx); err == nil {
				for _, g := range guilds {
					if g.GuildRowID == guildRowID && len(g.ServerIDs) > 0 {
						serverID = g.ServerIDs[0]
					}
				}
			}
			cancel()
		}
	}
	detail := "Champion can no longer post in <#" + channelID + ">. Give the Champion role access to the channel (or its category), or run Repair Champion Discord Layout in the Server Hub."
	if class == discord.CounterFaultUnknownChannel {
		detail = "The channel for " + routeKey + " was deleted. Run Repair Champion Discord Layout in the Server Hub to recreate it, or re-map the route."
	}
	a.AdminAlerts.Publish(discord.AdminAlert{
		GuildRowID: guildRowID, ServerID: serverID,
		Kind: discord.AlertKindChannelAccess, Severity: discord.AlertCritical,
		Headline: "Champion lost access to a channel",
		Detail:   detail,
		Fields:   [][2]string{{"Route", routeKey}, {"Channel", "<#" + channelID + ">"}, {"Fault", class}},
	})
}
