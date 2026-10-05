package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// bindLegacyOnlineCounter binds the legacy GuildSetup channel. Only
// bindCounterChannel calls it, and only when the ONLINE_COUNTER route is
// confirmed absent.
func bindLegacyOnlineCounter(store discord.SetupStore, guildID string, counter *discord.VoiceChannelCounter) {
	if store == nil || counter == nil || guildID == "" {
		return
	}
	setup, err := store.Get(guildID)
	if err == nil && setup != nil && setup.OnlinePlayersChannelID != "" {
		counter.SetChannelID(setup.OnlinePlayersChannelID)
	}
}

// runServerWorker builds and runs one fully isolated ADM pipeline for a single
// game_servers row: its own Engine (own PlayerTracker and checkpoint tracker),
// its own PersistenceQueue, and its own killfeed publisher binding. It blocks
// until workerCtx is cancelled (by WorkerManager.Stop/StopAll or shutdown).
// A panic here is caught by WorkerManager's recover() boundary, not here, so
// that the failure is always logged with server_id context in one place.
// rotatingFeedInterval/rotatingFeedBatchSize control the killfeed and
// death-feed channels' rolling display: up to rotatingFeedBatchSize embeds
// visible at once, the whole batch replaced every rotatingFeedInterval.
const (
	rotatingFeedInterval  = 10 * time.Minute
	rotatingFeedBatchSize = 50
)

// feedDeliveryMode reads KILLFEED_DELIVERY_MODE. Only the exact value
// "immediate" enables immediate delivery (each kill/death card posted as soon
// as it is persisted, with a separate rolling 50-card window per feed); anything else, including
// unset, keeps the production rotating cycle unchanged.
func feedDeliveryMode() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("KILLFEED_DELIVERY_MODE")), discord.FeedModeImmediate) {
		return discord.FeedModeImmediate
	}
	return discord.FeedModeRotating
}

func (a *App) runServerWorker(workerCtx context.Context, row repository.GameServer, store *persistenceStoreAdapter, setupStore discord.SetupStore, onlineCounter *discord.VoiceChannelCounter) error {
	workerName := fmt.Sprintf("adm_worker_%d", row.ID)
	// Every goroutine this worker spawns runs under its own cancel so that an
	// early error return (or a panic unwinding through here) tears them down
	// too, not only a cancellation from WorkerManager.
	workerCtx, cancelWorker := context.WithCancel(workerCtx)
	// Per-worker registrations, undone below so a restarted server never leaves
	// a dead queue or feed in health reports or the shutdown drain.
	var ownedFeeds []*discord.RotatingFeed
	var ownedPQ *killfeed.PersistenceQueue
	var ownedLQ *killfeed.LocationQueue
	defer func() {
		cancelWorker()
		for _, f := range ownedFeeds {
			f.WaitDone() // registered only after Run started, so this returns
		}
		a.removeRotatingFeeds(ownedFeeds...)
		a.removePersistQueue(ownedPQ)
		a.removeLocationQueue(ownedLQ)
		caseCollectorRunning.Delete(row.ID)
		if a.Workers != nil {
			a.Workers.Stop(workerName)
		}
		a.unregisterPresenceTracker(row.ID)
		a.unregisterCounterSource(row.ID)
	}()

	client, credentialErr := a.nitradoClientForServer(workerCtx, row)
	if credentialErr != nil {
		return credentialErr
	}
	if a.ActivityRepository != nil {
		if err := a.ActivityRepository.ResetConnectedForRestart(workerCtx, row.GuildID, row.ID); err != nil {
			return fmt.Errorf("reset stale activity session for server %d: %w", row.ID, err)
		}
	}
	a.backfillDailyPresence(workerCtx, row)
	engine := killfeed.NewEngine(client, row.ProviderServiceID, killfeed.NewADMParser())
	engine.SetStateSink(a.State)
	engine.SetDiagnostics(killfeed.NewRuntimeDiagnostics(row.ID))
	// Latency measurement only: lets the engine state a log line's own time in UTC.
	engine.SetServerUTCOffset(a.serverUTCOffsetSource(workerCtx, row))
	a.registerPresenceEngine(row.ID, engine)
	if a.Checkpoints != nil {
		engine.SetDurableCheckpoint(&admCheckpointStoreAdapter{repo: a.Checkpoints}, row.GuildID, row.ID)
	}
	a.registerPresenceTracker(row.ID, engine.PlayerTracker())
	if a.consumeFirstConnect(row.ID) {
		engine.StartAtLogTail()
	}
	// ADM snapshot/download callbacks are single setters on the engine, so
	// every consumer is collected here and fanned out once below.
	var onSnapshot []func(killfeed.AdmSnapshot)
	var onDownload []func(killfeed.DownloadReport)
	if a.Discord != nil && a.Servers != nil {
		if config, configErr := a.Servers.EnsureConfig(workerCtx, row.ID); configErr == nil {
			monitor := discord.NewADMMonitorPublisher(discord.NewSessionAPI(a.Discord.Session()), setupStore, a.Config.DiscordGuildID, row.ID, config.ADMMonitorMessageID, func(messageID string) {
				if err := a.Servers.SetADMMonitorMessage(context.Background(), row.ID, messageID); err != nil {
					slog.Warn("component=adm", "event", "monitor_message_save_failed", "server_id", row.ID, "err", err.Error())
				}
			})
			if a.ChannelRoutes != nil {
				// ADMIN_LOGS resolves per (guild, server) first; the legacy
				// GuildSetup.ADMMonitorChannelID is only the fallback.
				monitor.SetRouting(a.ChannelRoutes, row.GuildID)
			}
			onSnapshot = append(onSnapshot, monitor.Update)
			onDownload = append(onDownload, monitor.HandleDownload)
		}
	}
	if a.ServerStatusBoard != nil {
		board, serverID := a.ServerStatusBoard, row.ID
		onSnapshot = append(onSnapshot, func(s killfeed.AdmSnapshot) { board.Observe(serverID, s) })
	}
	if a.AdminAlerts != nil {
		alerts, guildRowID, serverID := a.AdminAlerts, row.GuildID, row.ID
		onSnapshot = append(onSnapshot, func(s killfeed.AdmSnapshot) { alerts.ObserveSnapshot(guildRowID, serverID, s) })
		onDownload = append(onDownload, func(r killfeed.DownloadReport) { alerts.ObserveDownload(guildRowID, r) })
	}
	if len(onSnapshot) > 0 {
		engine.OnAdmSnapshot(func(s killfeed.AdmSnapshot) {
			for _, f := range onSnapshot {
				f(s)
			}
		})
	}
	if len(onDownload) > 0 {
		engine.OnDownload(func(r killfeed.DownloadReport) {
			for _, f := range onDownload {
				f(r)
			}
		})
	}

	// Base Black Box: per-base history of nearby players and dismantles. Records
	// nothing until the server owner turns it on; never messages or acts.
	var blackBox *baseBlackBoxRecorder
	if a.DB != nil && a.DB.Pool != nil {
		blackBox = newBaseBlackBoxRecorder(repository.NewBaseBlackBoxRepository(a.DB.Pool), row.GuildID, row.ID)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("component=servers", "msg", "base black box panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
				}
			}()
			blackBox.Run(workerCtx)
		}()
	}
	if a.ChannelRoutes != nil && a.Discord != nil && a.Discord.Session() != nil {
		// BUILD_FEED: ADM build/placement actions, present only when the server
		// enables adminLogPlacement / adminLogBuildActions. Bounded queue + one
		// goroutine; no route means nothing is sent.
		buildFeed := discord.NewBuildFeedPublisher(a.feedSender(row.ID), a.ChannelRoutes, row.GuildID, row.ID)
		buildFeed.SetServerName(a.serverNameFunc())
		buildFeed.OnSeen(func() { a.buildActionsSeen.Add(1) })
		var buildPublisher killfeed.BuildPublisher = buildFeed
		if a.DB != nil && a.DB.Pool != nil {
			// Base Raid Alarm: DMs a base owner when someone else dismantles part of
			// their registered base. Off until the server owner turns it on.
			raidAlarm := discord.NewBaseRaidAlarmPublisher(repository.NewBaseRaidAlarmRepository(a.DB.Pool), a.Discord.Session(), row.GuildID, row.ID)
			raidAlarm.SetServerName(a.serverNameFunc())
			raidAlarm.SetFactionSecurity(repository.NewFactionSecurityRepository(a.DB.Pool))
			raidAlarm.SetBlackBoxURL(a.siteURL() + "/dashboard/player/security-store#black-box")
			buildPublisher = buildPublisherFanout{buildFeed, raidAlarm, blackBox}
			go func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("component=servers", "msg", "base raid alarm panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
					}
				}()
				raidAlarm.Run(workerCtx)
			}()
		}
		engine.SetBuildPublisher(buildPublisher)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("component=servers", "msg", "build feed panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
				}
			}()
			buildFeed.Run(workerCtx)
		}()
	}

	publisher := discord.NewKillfeedPublisher(a.Discord, a.Config.KillfeedChannelID)
	publisher.BindStore(setupStore, a.Config.DiscordGuildID)
	if a.ChannelRoutes != nil {
		// KILLFEED resolves per (guild, server) through the installation
		// route model first; the legacy GuildSetup/env channel is only the
		// fallback when no route is configured.
		publisher.SetRouting(a.ChannelRoutes, row.GuildID, row.ID)
	}
	publisher.SetCustomizer(a.embedCustomizer(), row.DisplayName)
	publisher.SetServerNameFunc(a.serverNameFunc())
	engine.SetKillPublisher(publisher)

	if a.ChannelRoutes != nil && a.Discord != nil && a.Discord.Session() != nil {
		// HITFEED: only published when this server's installation has a HITFEED
		// route (no legacy channel, no KILLFEED fallback). Aggregated and rate
		// capped; all route lookups and Discord I/O happen on its own goroutine,
		// so a Discord/DB failure can never stall ADM parsing or kill processing.
		hitFeed := discord.NewHitfeedPublisher(a.feedSender(row.ID), a.ChannelRoutes, row.GuildID, row.ID)
		hitFeed.SetCustomizer(a.embedCustomizer(), a.serverNameFunc())
		engine.SetHitPublisher(hitFeed)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("component=servers", "msg", "hitfeed panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
				}
			}()
			hitFeed.Run(workerCtx)
		}()
	}

	if a.ChannelRoutes != nil && a.Discord != nil && a.Discord.Session() != nil {
		// CONNECTIONS: only published when this server's installation has a
		// CONNECTIONS route (no legacy channel, no KILLFEED or voice-counter
		// fallback). Bounded queue + a single goroutine; route lookups and Discord
		// I/O happen there, so a Discord/DB failure can never stall ADM parsing,
		// presence tracking or persistence.
		connectionsFeed := discord.NewConnectionsPublisher(a.feedSender(row.ID), a.ChannelRoutes, row.GuildID, row.ID)
		connectionsFeed.SetCustomizer(a.embedCustomizer(), a.serverNameFunc())
		engine.SetConnectionPublisher(connectionsFeed)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("component=servers", "msg", "connections feed panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
				}
			}()
			connectionsFeed.Run(workerCtx)
		}()
	}

	var pveFeed *discord.PveFeedPublisher
	if a.ChannelRoutes != nil && a.Discord != nil && a.Discord.Session() != nil {
		// PVE_FEED: provably non-PvP deaths (today: explicit suicides). Only a
		// death the feed CLAIMS (a PVE_FEED route exists for this server) is kept
		// off the legacy death feed; with no route nothing is claimed and the
		// legacy death feed behaves exactly as before. No KILLFEED fallback.
		// Bounded queue + a single goroutine; Discord/DB failures cannot reach
		// persistence, ADM parsing or the other feeds.
		pveFeed = discord.NewPveFeedPublisher(a.feedSender(row.ID), a.ChannelRoutes, row.GuildID, row.ID)
		pveFeed.SetCustomizer(a.embedCustomizer(), a.serverNameFunc())
		engine.SetPveDeathPublisher(pveFeed)
	}

	deathPublisher := discord.NewDeathfeedPublisher(a.Discord, setupStore, a.Config.DiscordGuildID)
	engine.SetDeathPublisher(deathPublisher)

	killFeed := discord.NewRotatingFeed(a.feedSender(row.ID), setupStore, a.Config.DiscordGuildID, func(s *discord.GuildSetup) string { return s.KillfeedChannelID }, rotatingFeedInterval, rotatingFeedBatchSize)
	killFeed.SetRouteChannelResolver(publisher.RouteChannelID)
	killFeed.SetMode(feedDeliveryMode())
	publisher.SetFeed(killFeed)
	deathFeed := discord.NewRotatingFeed(a.feedSender(row.ID), setupStore, a.Config.DiscordGuildID, func(s *discord.GuildSetup) string { return s.DeathChannelID }, rotatingFeedInterval, rotatingFeedBatchSize)
	// Death and suicide cards resolve the installation's PVE_FEED route,
	// separate from KILLFEED. The legacy death channel is the fallback only
	// when that route does not exist or cannot be resolved.
	if a.ChannelRoutes != nil {
		deathFeed.SetRouteChannelResolver(func() string {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			channelID, found, err := a.ChannelRoutes.Resolve(ctx, row.GuildID, row.ID, "PVE_FEED")
			if err != nil {
				slog.Warn("component=discord", "event", "channel_route_fallback", "route_key", "PVE_FEED", "server_id", row.ID, "reason", "lookup_error", "err", err.Error())
				return ""
			}
			if !found {
				return ""
			}
			return channelID
		})
	}
	deathFeed.SetRoute("DEATH_FEED")
	killFeed.SetLatencyServerID(row.ID)
	deathFeed.SetLatencyServerID(row.ID)
	deathFeed.SetMode(feedDeliveryMode())
	deathPublisher.SetFeed(deathFeed)
	if pveFeed != nil {
		pveFeed.SetFeed(deathFeed)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("component=servers", "msg", "pve feed panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
				}
			}()
			pveFeed.Run(workerCtx)
		}()
	}
	if a.DB != nil && a.DB.Pool != nil {
		// Feed journal (migration 0066): immediate mode records every card so
		// a restart replays undelivered cards and takes back the previous
		// process's shown cards. Rotating mode only drains what an earlier
		// immediate process left (rollback), so under the production default
		// the table stays empty.
		journal := repository.NewFeedCardRepository(a.DB.Pool)
		killFeed.SetJournal(journal, fmt.Sprintf("KILLFEED:%d", row.ID))
		deathFeed.SetJournal(journal, fmt.Sprintf("DEATH_FEED:%d", row.ID))
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("component=servers", "msg", "killfeed rotating feed panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
			}
		}()
		killFeed.Run(workerCtx)
	}()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("component=servers", "msg", "death feed rotating feed panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
			}
		}()
		deathFeed.Run(workerCtx)
	}()
	// Registered only now that both Run loops are started: the worker's exit
	// path waits on them (WaitDone) before dropping them from the registry.
	ownedFeeds = append(ownedFeeds, killFeed, deathFeed)
	a.addRotatingFeed(killFeed)
	a.addRotatingFeed(deathFeed)

	if store.ranked != nil {
		go func() {
			reconcile := func() {
				if count, err := store.ranked.ReconcileServerAwards(workerCtx, row.ID); err != nil && workerCtx.Err() == nil {
					slog.Warn("component=ranked", "event", "reconciliation_failed", "server_id", row.ID, "err", err.Error())
				} else if count > 0 {
					slog.Info("component=ranked", "event", "awards_reconciled", "server_id", row.ID, "count", count)
				}
			}
			reconcile()
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-workerCtx.Done():
					return
				case <-ticker.C:
					reconcile()
				}
			}
		}()
	}
	pq := killfeed.NewPersistenceQueueWithServerID(store, row.GuildID, row.ID, row.ProviderServiceID)
	pq.SetKillPostProcessor(store)
	pq.SetDeathPostProcessor(store)
	if a.LinkService != nil {
		pq.SetLinkChallengeObserver(a.LinkService)
	}
	engine.SetPersistence(pq)
	ownedPQ = pq
	a.addPersistQueue(pq)
	// C.A.S.E. Phase 2B is opt-in until source-addressed evidence and replay
	// verification are proven in production. It writes no detector verdicts.
	// Attach before engine.Start; all hit/lifecycle events use the SAME ADM
	// poller and the SAME durable checkpoint as the existing killfeed.
	if caseEvidenceEnabledForServer(row.ID) && a.DB != nil && a.DB.Pool != nil && a.Checkpoints != nil {
		engine.SetEvidenceStore(repository.NewCaseEvidenceRepository(a.DB.Pool))
		engine.SetBuildEvidenceEnabled(caseBuildEvidenceEnabledForServer(row.ID))
		caseCollectorRunning.Store(row.ID, true)
		slog.Info("component=case", "event", "evidence_collector_enabled", "server_id", row.ID)
	}

	// Phase 3 (docs/PLAYER_INTELLIGENCE.md): the location-history pipeline, fully separate from
	// pq above - see internal/killfeed/location_queue.go's package doc for why it must never
	// share pq's blocking EnqueueAndWait semantics.
	lq := killfeed.NewLocationQueue(store, row.GuildID, row.ID)
	lq.SetIntrusionEngine(a.Intrusion)
	if a.DB != nil && a.DB.Pool != nil && a.Discord != nil && a.Discord.Session() != nil {
		// Perimeter Watch: DMs a base owner when someone else is seen near their
		// registered base. Off until the server owner turns it on.
		perimeter := discord.NewPerimeterWatchPublisher(repository.NewPerimeterWatchRepository(a.DB.Pool), a.Discord.Session(), row.GuildID, row.ID)
		perimeter.SetServerName(a.serverNameFunc())
		perimeter.SetFactionSecurity(repository.NewFactionSecurityRepository(a.DB.Pool))
		lq.SetLocationObserver(locationObserverFanout{perimeter, blackBox})
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("component=servers", "msg", "perimeter watch panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
				}
			}()
			perimeter.Run(workerCtx)
		}()
	}
	engine.SetLocationQueue(lq)
	// Live Sync phase 1: the selected ADM file is recorded as the server's current boot session,
	// which current-session location queries trust (docs/CHAMPION_LIVE_SYNC.md).
	if a.Locations != nil {
		engine.SetADMSessionStore(a.Locations)
	}
	ownedLQ = lq
	a.addLocationQueue(lq)

	// Presence changes only nudge the online counter loop (online_counter.go),
	// which resolves the authoritative count and renames the channel on its
	// own goroutine. This callback runs on the ADM pipeline and must never
	// make a Discord or Nitrado call itself: a rename rate limit here would
	// stall kill processing.
	engine.OnPlayersChanged(func(int) {
		if a.ownsPublicCounter(row.ID) {
			a.pokeOnlineCounter()
		}
	})
	engine.OnNewBoot(func(cleared int) {
		// A server restart ended every open session of the previous boot:
		// close them in the activity store too, so phantom "connected" rows
		// stop accruing observed playtime for /link. Accrued time is kept.
		if a.ActivityRepository != nil {
			resetCtx, cancel := context.WithTimeout(workerCtx, 5*time.Second)
			if err := a.ActivityRepository.ResetConnectedForRestart(resetCtx, row.GuildID, row.ID); err != nil {
				slog.Warn("component=link_activity", "event", "server_restart_reset_failed", "server_id", row.ID, "err", err.Error())
			} else {
				slog.Info("component=link_activity", "event", "server_restart_reset", "server_id", row.ID, "cleared_presence", cleared)
			}
			cancel()
		}
	})
	a.registerCounterSource(row.ID, counterSource{serviceID: row.ProviderServiceID, live: client, engine: engine})

	if a.Workers != nil {
		a.Workers.Register(workerName)
		a.Workers.Heartbeat(workerName)
		// Live Sync phase 2.1: the heartbeat advances on every completed poll cycle - a quiet ADM is a
		// working worker. Nitrado failures are reported by the adm_source component, not by faking a
		// dead worker; a worker whose cycles stop still goes stale (WORKER_STALLED).
		engine.OnPollCycle(func(killfeed.PollOutcome) { a.Workers.Heartbeat(workerName) })
	}

	queueDone := make(chan struct{})
	go func() {
		defer close(queueDone)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("component=servers", "msg", "persistence queue panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
				if a.Workers != nil {
					a.Workers.Error(workerName, fmt.Errorf("persistence queue panic: %v", r))
				}
			}
		}()
		pq.Run(workerCtx)
	}()

	locationQueueDone := make(chan struct{})
	go func() {
		defer close(locationQueueDone)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("component=servers", "msg", "location queue panic recovered", "server_id", row.ID, "panic", fmt.Sprint(r))
			}
		}()
		lq.Run(workerCtx)
	}()

	// Live Sync phase 2: the non-ADM log watchers run beside the engine, never inside it.
	liveSyncCtx, stopLiveSync := context.WithCancel(workerCtx)
	liveSyncDone := a.startLiveSync(liveSyncCtx, row, client)

	slog.Info("component=servers", "msg", "server worker running", "server_id", row.ID, "display_name", row.DisplayName)
	err := engine.Start(workerCtx)
	stopLiveSync()
	<-liveSyncDone
	pq.Close()
	<-queueDone
	lq.Close()
	<-locationQueueDone
	if err != nil && a.Workers != nil {
		a.Workers.Error(workerName, err)
	}
	slog.Info("component=servers", "msg", "server worker stopped", "server_id", row.ID)
	return err
}

// feedSender is the Discord sender a server's feeds post through: the bot's feed session, wrapped
// so a server with a feed identity enabled posts under it (docs/FEED_IDENTITY.md).
func (a *App) feedSender(serverID int64) discord.FeedIdentityAPI {
	return a.FeedIdentity.Sender(discord.NewFeedSession(a.Discord.Session()), serverID)
}
