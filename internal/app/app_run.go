package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/heatmapimage"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/server"
	"github.com/yourname/dayz-killfeed/internal/servers"
)

// Run boots the application and runs until shutdown.
func (a *App) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	state := a.State
	if state == nil {
		state = server.NewState()
		a.State = state
	}

	// Faction logo bytes orphaned by a failed delete or rollback are swept hourly (only objects
	// older than two hours and referenced by no asset row).
	if a.FactionAssets != nil {
		go a.FactionAssets.RunSweeper(ctx, time.Hour, 2*time.Hour)
	}
	// Delivered Shop orders the buyer never answered are completed once their deadline passes.
	if a.ShopConfirmations != nil {
		go a.runShopConfirmationSweeper(ctx)
	}
	// The Shop automatic delivery worker: off unless its own lock is opened (report or enabled).
	a.startShopDeliveryWorker(ctx)
	// Map rotation with a player vote: it does nothing for an installation unless the map_rotation
	// feature flag, the plan and the owner's own switch are all on (docs/MAP_ROTATION.md).
	a.startMapRotationWorker(ctx)
	// A server's name follows its Nitrado name unless the owner typed one in Champion: one read
	// per server about every twenty minutes (docs/SERVER_NAME_SYNC.md).
	a.startServerNameSync(ctx)
	// Tournament mode: the clock that opens sign-up and check-in, starts the draw and enforces
	// the match timers runs on the leader (docs/TOURNAMENTS.md).
	a.startTournamentScheduler(ctx)
	// Faction Hub achievements: kills queue an evaluation (drained every 5 seconds, one evaluation
	// per affected faction), and a reconcile - one minute after start, then daily - backfills
	// factions that already qualify and unlocks the time-based ones. Unlocking is silent.
	if a.FactionHubStats != nil {
		go a.FactionHubStats.Run(ctx, 5*time.Second)
		go a.FactionHubStats.RunReconciler(ctx, time.Minute, 24*time.Hour)
	}

	// Sanitized configuration presence. Values are never logged.
	slog.Info("component=startup", "msg", "configuration loaded",
		"NITRADO_TOKEN_configured", a.Config.NitradoToken != "",
		"NITRADO_SERVICE_ID_configured", a.Config.NitradoServiceID != "",
		"DISCORD_TOKEN_configured", a.Config.DiscordToken != "",
		"KILLFEED_CHANNEL_ID_configured", a.Config.KillfeedChannelID != "",
	)

	// expectedWorkers is how many server workers this start-up set out to run (the deploy self-check).
	expectedWorkers := 0

	// --- Nitrado authentication and service verification ---
	logSourceVerified := false
	authenticated, serviceVerified, serviceGame, serviceType, serviceStatus := a.verifyNitrado(ctx)
	state.SetNitrado(authenticated, serviceVerified, serviceGame, serviceType, serviceStatus)

	// --- Discord connection and access validation ---
	if err := a.Discord.Start(ctx); err != nil {
		return fmt.Errorf("start Discord session: %w", err)
	}
	defer func() {
		if err := a.Discord.Close(); err != nil {
			slog.Error("component=discord", "msg", "error closing Discord connection", "err", err.Error())
		}
	}()

	verification := a.Discord.Verify(a.Config.DiscordGuildID, a.Config.KillfeedChannelID)
	state.SetDiscord(true, a.Discord.BotUsername(), verification.GuildFound, verification.ChannelFound, verification.Missing)

	// --- Discord bot presence: a professional activity, set immediately now
	// that Discord is connected, then an optional rotation worker. Reuses
	// already-known application state (PresenceCounts/OverallHealth) -
	// never a second Nitrado poll or health check. ---
	if a.Config.DiscordPresenceEnabled {
		a.PresenceManager = discord.NewPresenceManager(
			a.Discord, a, a,
			discord.PresenceMode(a.Config.DiscordPresenceMode),
			time.Duration(a.Config.DiscordPresenceRotationSeconds)*time.Second,
		)
		a.PresenceManager.Start(ctx)
	}

	// requiredCommandsOK gates readiness: /setup, /server, and /link are the
	// commands a fresh guild depends on, so a registration failure among them
	// must not be reported as ready (see section 9 of the startup repair pass).
	requiredCommandsOK := true

	// --- Champion setup: store, manager, slash command ---
	// Use durable PostgreSQL-backed storage when connected; in-memory otherwise.
	var setupStore discord.SetupStore
	if a.Guilds != nil {
		setupStore = discord.NewPostgresSetupStore(a.Guilds)
		slog.Info("component=startup", "msg", "guild setup store: postgresql")
	} else {
		setupStore = discord.NewInMemorySetupStore()
		slog.Warn("component=startup", "msg", "guild setup store: in-memory (not durable across restarts)")
	}
	session := a.Discord.Session()
	// Slash commands are queued here and sent in one request once every
	// handler below is installed: creating them one by one hit Discord's
	// rate limit and left commands unanswered for ~80 s after each deploy.
	appID, _ := discord.ApplicationID(session)
	if appID == "" {
		appID = a.Config.DiscordApplicationID
	}
	commands := discord.NewCommandBatch(appID)
	api := discord.NewSessionAPI(session)
	a.factionRecruitAPI = api
	setupManager := discord.NewSetupManager(api, setupStore, a.Discord.BotID())
	if a.LinkService != nil && a.Config.DiscordGuildID != "" {
		verifiedRole := discord.NewVerifiedRoleAssigner(a.Discord, setupStore, a.Config.DiscordGuildID)
		a.LinkService.SetRoleAssigner(verifiedRole)
		a.LinkService.SetNotifier(verifiedRole)
		go a.singleton(ctx, "verified_role_reconciler", a.runRoleReconciler)
	}
	// Every command, button and form is a route on this router; each route
	// names how it is acknowledged when its handler is slow.
	routes := a.Discord.Interactions()
	setupHandler := discord.NewSetupHandler(setupManager, a.Guilds, a.WelcomeRepository)
	// /setup runs the same Channel System V2 layout engine as the website's
	// one-click setup and repair - there is one channel blueprint.
	setupHandler.SetLayout(a.DiscordSetupLayout)
	welcomeHandler := discord.NewPersistentWelcomeHandler(setupStore, a.WelcomeRepository, a.Guilds)
	if a.WelcomeRepository != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		welcomeCommands := discord.NewWelcomeCommandHandler(a.WelcomeRepository, a.Guilds, setupStore)
		if err := discord.RegisterWelcomeCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register welcome commands", "err", err.Error())
		} else {
			slog.Info("component=discord", "msg", "welcome commands queued")
		}
		routes.Command("welcome", discord.AckPrivate, welcomeCommands.Handle)
	}
	if a.AnalyticsRepository != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		analyticsHandler := discord.NewAnalyticsCommandHandler(a.AnalyticsRepository, a.Guilds)
		if err := discord.RegisterAnalyticsCommands(commands, a.Config.DiscordGuildID, a.Config.DiscordApplicationID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register analytics commands", "err", err.Error())
		}
		routes.Command("matchup", discord.AckPrivate, analyticsHandler.Handle)
		routes.Command("weapon", discord.AckPrivate, analyticsHandler.Handle)
	}
	if a.AdminService != nil && a.Config.DiscordGuildID != "" {
		adminHandler := discord.NewAdminCommandHandler(a.AdminService, a.LinkService, a.Guilds)
		if err := discord.RegisterAdminCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register admin commands", "err", err.Error())
		} else {
			slog.Info("component=discord", "msg", "admin commands queued")
		}
		routes.Command("admin", discord.AckPrivate, adminHandler.Handle)
	}
	if a.Servers != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		serverHandler := discord.NewServerCommandHandler(a.Servers, a.Guilds, a.CredentialCipher, a)
		if err := discord.RegisterServerCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Error("component=discord", "msg", "failed to register server commands", "err", err.Error())
			requiredCommandsOK = false
		} else {
			slog.Info("component=discord", "msg", "server commands queued")
		}
		// /server connect opens the token form, which Discord cannot defer.
		routes.Command("server", discord.AckPrivate, serverHandler.Handle, discord.SubAck{Path: "connect", Ack: discord.AckSelf})
		routes.Autocomplete("server", serverHandler.Handle)
		routes.ModalPrefix("champion_server_", discord.AckPrivate, serverHandler.Handle)
	}
	if a.AnnouncementService != nil && a.Config.DiscordGuildID != "" {
		a.CompletionPublisher = discord.NewLiveCompletionPublisher(a.AnnouncementService, api, setupStore, a.Seasons, a.Wars, a.Events, a.Players, a.Factions, a.Guilds, a.Config.DiscordGuildID)
	}
	if a.SeasonService != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		seasonHandler := discord.NewSeasonCommandHandler(a.SeasonService, a.Guilds)
		if a.Ranked != nil && a.Servers != nil {
			seasonHandler.SetRankedStatus(rankedSeasonStatus{servers: a.Servers, ranked: a.Ranked})
		}
		if err := discord.RegisterSeasonCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register season commands", "err", err.Error())
		}
		routes.Command("season", discord.AckPrivate, seasonHandler.Handle)
	}
	if a.EventService != nil && a.Events != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		eventHandler := discord.NewEventCommandHandler(a.EventService, a.Events, a.Guilds)
		if err := discord.RegisterEventCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register event commands", "err", err.Error())
		}
		routes.Command("event", discord.AckPrivate, eventHandler.Handle)
	}
	if a.Bounties != nil && a.Players != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		bountyHandler := discord.NewBountyCommandHandler(a.Bounties, a.BountyService, a.Players, a.Guilds)
		if err := discord.RegisterBountyCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register bounty commands", "err", err.Error())
		}
		routes.Command("bounty", discord.AckPrivate, bountyHandler.Handle)
	}
	if a.EconomyService != nil && a.Players != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		economyHandler := discord.NewEconomyCommandHandler(a.EconomyService, a.Players, a.Guilds, linkedPlayerLookup{a.LinkService})
		if err := discord.RegisterEconomyCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register economy commands", "err", err.Error())
		} else {
			slog.Info("component=discord", "msg", "economy commands queued")
		}
		routes.Command("economy", discord.AckPrivate, economyHandler.Handle)
	}
	if a.Points != nil && a.Players != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		pointsHandler := discord.NewPointsCommandHandler(a.Points, a.Players, a.Guilds)
		if err := discord.RegisterPointsCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register points command", "err", err.Error())
		}
		routes.Command("points", discord.AckPrivate, pointsHandler.Handle)
	}
	if a.Wars != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		warHandler := discord.NewWarCommandHandler(a.Wars, a.Guilds, a.Seasons, a.Factions, a.Links, a.FactionStats, a.FactionPresentation, a.Players)
		if err := discord.RegisterWarCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register faction war commands", "err", err.Error())
		}
		routes.Command("faction", discord.AckPrivate, warHandler.Handle)
	}
	if a.Config.DiscordGuildID != "" {
		if err := discord.RegisterSetupCommand(commands, a.Config.DiscordGuildID); err != nil {
			slog.Error("component=discord", "msg", "failed to queue /setup command", "err", err.Error())
			requiredCommandsOK = false
		} else {
			slog.Info("component=discord", "msg", "setup commands queued")
		}
	}

	// Stats and leaderboard commands require the database + a guild record.
	if a.Stats != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		statsHandler := discord.NewStatsCommandHandler(a.Stats, a.Guilds, a.Config.DiscordGuildID)
		if err := discord.RegisterStatsCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register stats commands", "err", err.Error())
		} else {
			slog.Info("component=discord", "msg", "stats commands queued")
		}
		routes.Command("stats", discord.AckPrivate, statsHandler.HandleStats)
		routes.Command("leaderboard", discord.AckPrivate, statsHandler.HandleLeaderboard)
	}
	a.registerLifeCommands(ctx, session, commands)
	a.registerCardCommand(session, commands)
	a.registerTournamentCommand(session, commands)
	a.registerFeaturesCommand(commands)
	a.registerBaseCommands(session, commands)
	if a.LinkService != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		linkHandler := discord.NewLinkCommandHandler(a.LinkService, a.Guilds)
		if err := discord.RegisterLinkCommands(commands, a.Config.DiscordGuildID); err != nil {
			slog.Error("component=discord", "msg", "failed to register link commands", "err", err.Error())
			requiredCommandsOK = false
		} else {
			slog.Info("component=discord", "msg", "link commands queued")
		}
		routes.Command("link", discord.AckPrivate, linkHandler.Handle)
		routes.Command("link-status", discord.AckPrivate, linkHandler.Handle)
		routes.Command("unlink", discord.AckPrivate, linkHandler.Handle)
		routes.ComponentPrefix("champion_unlink_", discord.AckPrivate, linkHandler.HandleComponent)
	}
	if a.Guilds != nil && (a.LinkService != nil || a.Stats != nil) && a.Config.DiscordGuildID != "" {
		publicPanels := discord.NewPublicPanelHandler(a.LinkService, a.Stats, a.Guilds)
		publicPanels.SetEconomy(a.EconomyService)
		publicPanels.Register(routes)
	}
	a.Discord.AddMemberJoinHandler(welcomeHandler.HandleMemberJoin)
	if a.VIP != nil && session != nil {
		a.VIPRoles = session
		a.VIPNotices = session
	}
	if a.Invites != nil {
		a.InviteTracker = discord.NewInviteTracker(session, a.Invites)
		a.Discord.AddInviteTracking(ctx, a.InviteTracker)
		if a.Config.DiscordGuildID != "" {
			go a.InviteTracker.Snapshot(a.Config.DiscordGuildID)
		}
	}

	routes.Command("setup", discord.AckPrivate, setupHandler.Handle)
	// Only the /setup reset buttons: every other button has its own route.
	routes.ComponentPrefix("champion_reset_", discord.AckPrivate, setupHandler.HandleResetConfirm)
	// Faction recruitment card: Join answers privately; Apply opens a form,
	// which Discord cannot defer; the submitted form answers privately.
	routes.ComponentPrefix(factionRecruitPrefix, discord.AckPrivate, a.HandleFactionRecruitInteraction)
	routes.ComponentPrefix(factionRecruitApply, discord.AckSelf, a.HandleFactionRecruitInteraction)
	routes.ModalPrefix(factionRecruitPrefix, discord.AckPrivate, a.HandleFactionRecruitInteraction)
	// Shop order buttons: "Received" and the submitted issue form replace the
	// order message; "Report an issue" opens a form.
	routes.ComponentPrefix(discord.ShopOrderPrefix, discord.AckUpdate, a.HandleShopOrderInteraction)
	routes.ComponentPrefix(discord.ShopOrderIssuePrefix, discord.AckSelf, a.HandleShopOrderInteraction)
	routes.ModalPrefix(discord.ShopOrderPrefix, discord.AckUpdate, a.HandleShopOrderInteraction)

	// Every handler is installed, so the commands can go live in one request.
	if a.Config.DiscordGuildID != "" && session != nil {
		started := time.Now()
		if n, err := commands.Flush(session, a.Config.DiscordGuildID); err != nil {
			slog.Error("component=discord", "msg", "failed to register slash commands", "err", err.Error())
			requiredCommandsOK = false
		} else {
			slog.Info("component=discord", "msg", "slash commands registered", "count", n, "duration_ms", time.Since(started).Milliseconds())
		}
	}

	// All interaction handlers are now installed; readiness must not be
	// reported before this point (see section 7/9 of the startup repair pass).
	slog.Info("component=discord", "msg", "interaction handlers ready", "required_commands_ok", requiredCommandsOK)
	state.SetHandlersReady(requiredCommandsOK)

	// Delivered Shop orders: the buyer's DM with its two buttons, and the ticket channels.
	a.startShopOrderDesk(ctx, session)

	// --- Online players voice counter: renames the configured voice channel on
	// debounced count changes. Shared across servers (one voice channel per guild
	// today; per-server counters are a known gap, see Section 1 report). ---
	onlineCounter := discord.NewVoiceChannelCounter(api, "")
	onlineCounter.SetGuildID(a.Config.DiscordGuildID)
	a.onlineCounter = onlineCounter
	a.setupStore = setupStore
	onlineCounter.OnPublish(func(count int, result string) { a.recordPublicVoicePublish(count, result) })
	if cfg := setupStore; cfg != nil {
		if gs, err := cfg.Get(a.Config.DiscordGuildID); err == nil && gs != nil && gs.OnlinePlayersChannelID != "" {
			onlineCounter.SetChannelID(gs.OnlinePlayersChannelID)
		}
	}
	go a.runOnlineCounter(ctx, onlineCounter)
	// Every process evaluates the count (its API answers from it); only the leader renames the
	// channel. A new leader publishes at once instead of waiting for the next poll.
	go a.singleton(ctx, "online_counter_publish", func(leaderCtx context.Context) {
		a.pokeOnlineCounter()
		<-leaderCtx.Done()
	})
	if a.AdminService != nil {
		a.AdminService.SetPipelineDiagnostics(func(diagCtx context.Context) map[string]any {
			out := map[string]any{"worker": "NOT FOUND", "classification": "UNKNOWN"}
			guild, _, err := a.Guilds.GetGuild(diagCtx, a.Config.DiscordGuildID)
			if err != nil || guild == nil || guild.SelectedPublicServerID == 0 {
				return out
			}
			snapshot, found := a.livePipelineSnapshot(guild.SelectedPublicServerID)
			if !found {
				return out
			}
			out["worker"] = snapshot.WorkerRunning
			out["server_id"] = snapshot.ServerID
			out["selected_adm"] = snapshot.SelectedADM
			out["newest_adm"] = snapshot.NewestADM
			out["selection_match"] = snapshot.SelectionMatch
			out["selection_reason"] = snapshot.SelectionReason
			out["metadata"] = fmt.Sprintf("last=%s changed=%t size=%d modified=%s", diagnosticTime(snapshot.LastMetadataCheck), snapshot.LastMetadataChanged, snapshot.RemoteSize, diagnosticTime(snapshot.RemoteModified))
			out["download"] = fmt.Sprintf("attempt=%s success=%s bytes=%d new_bytes=%d", diagnosticTime(snapshot.LastDownloadAttempt), diagnosticTime(snapshot.LastDownloadSuccess), snapshot.DownloadedBytes, snapshot.NewBytes)
			out["reader"] = fmt.Sprintf("complete_lines=%d partial=%t", snapshot.CompleteLines, snapshot.PartialLineBuffered)
			out["parser"] = fmt.Sprintf("event=%s at=%s", snapshot.LastParsedEventType, diagnosticTime(snapshot.LastParsedEventAt))
			out["persistence"] = fmt.Sprintf("event=%s result=%s at=%s", snapshot.LastPersistenceEvent, snapshot.LastPersistenceResult, diagnosticTime(snapshot.LastPersistenceAt))
			out["checkpoint"] = fmt.Sprintf("offset=%d remote_size=%d saved=%s", snapshot.CheckpointOffset, snapshot.CheckpointRemoteSize, diagnosticTime(snapshot.CheckpointLastSaved))
			out["presence"] = fmt.Sprintf("tracker=%d connect=%s disconnect=%s", snapshot.TrackerCount, diagnosticTime(snapshot.LastConnectAt), diagnosticTime(snapshot.LastDisconnectAt))
			out["voice"] = fmt.Sprintf("desired=%d published=%d actual=%d result=%s", snapshot.DesiredVoiceCount, snapshot.LastVoicePublishedCount, snapshot.ActualDiscordVoiceCount, snapshot.LastVoicePublishResult)
			out["kill"] = fmt.Sprintf("parsed=%s persisted=%s published=%s", diagnosticTime(snapshot.LastKillParsedAt), diagnosticTime(snapshot.LastKillPersistedAt), diagnosticTime(snapshot.LastKillPublishedAt))
			out["last_failure"] = fmt.Sprintf("stage=%s class=%s at=%s", snapshot.LastErrorStage, snapshot.LastErrorClass, diagnosticTime(snapshot.LastErrorAt))
			out["source_freshness"] = fmt.Sprintf("selected_age=%s last_remote_write=%s last_new_bytes=%s classification=%s", diagnosticDuration(snapshot.RemoteModified), diagnosticTime(snapshot.RemoteModified), diagnosticDuration(snapshot.LastDownloadSuccess), snapshot.Classification())
			out["cold_start"] = fmt.Sprintf("baseline=%t offset=%d", snapshot.ColdStartBaseline, snapshot.ColdStartBaselineOffset)
			out["stale_source_probe"] = fmt.Sprintf("last_probe=%s result=%s metadata_size=%d direct_size=%d content_changed=%t checkpoint=%d unread=%d classification=%s", diagnosticTime(snapshot.LastProbeAt), snapshot.ProbeResult, snapshot.ProbeMetadataSize, snapshot.ProbeDirectSize, snapshot.ProbeContentChanged, snapshot.CheckpointOffset, snapshot.ProbeUnreadBytes, snapshot.ProbeClassification)
			out["classification"] = snapshot.Classification()
			out["timeline"] = strings.Join(snapshot.RecentEvents, "\n")
			return out
		})
	}
	if a.AdminService != nil {
		a.AdminService.SetADMSourceScan(func(scanCtx context.Context) (map[string]any, error) {
			engine, serverID, found := a.selectedServerEngine(scanCtx)
			if !found {
				return map[string]any{"server": serverID, "candidates": 0, "root_cause": "NITRADO_SOURCE_UNAVAILABLE"}, nil
			}
			result, err := killfeed.ScanADMSources(scanCtx, engine.LogSource(), engine.ServiceID(), engine.SelectedName(), 30*time.Second)
			if err != nil || result == nil {
				return map[string]any{"server": serverID, "candidates": 0, "root_cause": "NITRADO_SOURCE_UNAVAILABLE"}, nil
			}
			activeSource := result.ActiveSource
			if activeSource == "" {
				activeSource = "NONE"
			}
			out := map[string]any{
				"server":             serverID,
				"candidates":         len(result.Candidates),
				"current_selected":   result.CurrentSelected,
				"active_source":      activeSource,
				"active_size_before": result.ActiveSizeBefore,
				"active_size_after":  result.ActiveSizeAfter,
				"content_changed":    result.ContentChanged,
				"new_adm_created":    result.NewADMCreated,
				"recommendation":     result.Recommendation,
				"root_cause":         result.RootCause,
			}
			for _, c := range result.Candidates {
				if c.Name == result.CurrentSelected {
					out["current_classification"] = c.Classification
					break
				}
			}
			return out, nil
		})
	}
	if a.AdminService != nil {
		a.AdminService.SetPresenceDiagnostics(func(diagCtx context.Context) map[string]any {
			out := map[string]any{"selected_server_id_resolved": false, "selected_server_worker_found": false, "classification": "UNKNOWN"}
			guild, _, err := a.Guilds.GetGuild(diagCtx, a.Config.DiscordGuildID)
			if err != nil || guild == nil || guild.SelectedPublicServerID == 0 {
				return out
			}
			selectedID := guild.SelectedPublicServerID
			out["selected_server_id"] = selectedID
			out["selected_server_id_resolved"] = true
			snapshot, found := a.livePresenceSnapshot(selectedID)
			out["selected_server_worker_found"] = found
			if !found {
				out["classification"] = "WRONG_SERVER_WORKER_SELECTED"
				return out
			}
			out["tracker_count"] = snapshot.OnlineCount
			out["tracked_entries"] = snapshot.TrackedEntries
			out["last_presence_event"] = snapshot.LastEventType
			out["last_connect_at"] = diagnosticTime(snapshot.LastConnectAt)
			out["last_disconnect_at"] = diagnosticTime(snapshot.LastDisconnectAt)
			out["last_persistence_result"] = snapshot.LastPersistenceResult
			out["last_voice_publish_count"] = snapshot.LastVoicePublishCount
			out["last_voice_publish_result"] = snapshot.LastVoicePublishResult
			out["discord_voice_counter"] = onlineCounter.LastPublished()
			actualCount, actualKnown, actualErr := onlineCounter.ActualCount()
			if actualKnown {
				out["actual_discord_count"] = actualCount
			}
			if channel, channelErr := api.Channel(onlineCounter.ChannelID()); channelErr == nil && channel != nil {
				out["discord_voice_channel"] = channel.Name
			}
			if actualErr != nil {
				out["actual_discord_count"] = "UNAVAILABLE"
			}
			// The counter publishes the authoritative current count (Nitrado
			// query, or proven ADM evidence), not the raw tracker.
			desired := snapshot
			if st := a.OnlineCounterStatus(); st.ServerID == selectedID && !st.EvaluatedAt.IsZero() {
				out["counter_source"] = st.Source
				out["counter_known"] = st.Reading.Known
				out["counter_desired_name"] = st.Reading.Name()
				out["counter_held_last_known"] = st.Held
				out["counter_evaluated_at"] = diagnosticTime(st.EvaluatedAt)
				out["adm_presence_state"] = st.ADMState
				out["nitrado_status"] = st.NitradoStatus
				if st.NitradoCount != nil {
					out["nitrado_player_current"] = *st.NitradoCount
				} else {
					out["nitrado_player_current"] = "UNKNOWN"
				}
				if st.NitradoError != "" {
					out["nitrado_error"] = "UNAVAILABLE"
				}
				if st.NitradoWrongService {
					out["nitrado_error"] = "WRONG_SERVICE"
				}
				out["tracker_matches_nitrado"] = st.NitradoCount != nil && *st.NitradoCount == st.TrackerCount
				if st.Disagreement != nil {
					out["presence_disagreement_since"] = diagnosticTime(st.Disagreement.Since)
				}
				if st.Reading.Known {
					desired.OnlineCount = st.Reading.Count
				}
			}
			out["counter_health"] = onlineCounter.Health()
			out["classification"] = classifyPresenceActual(desired, actualCount, actualKnown, true, true)
			return out
		})
	}

	// --- Multi-server ADM runtime ---
	// WorkerManager is the sole production owner of ADM engines: it constructs one
	// independent Engine (own PlayerTracker/checkpoint tracker) and one independent
	// PersistenceQueue per active game_servers row, and isolates each with a
	// recover() boundary so a panic or Nitrado outage on one server cannot affect
	// any other server's worker.
	if a.DB != nil && a.Players != nil && a.Kills != nil && a.Deaths != nil && a.Guilds != nil && a.Servers != nil && a.Config.DiscordGuildID != "" {
		_, guildRowID, err := a.Guilds.GetGuild(ctx, a.Config.DiscordGuildID)
		if err != nil {
			slog.Warn("component=database", "msg", "could not resolve guild row; persistence disabled", "err", err.Error())
		}
		if guildRowID > 0 {
			activeServers, listErr := a.Servers.ListActiveByGuild(ctx, guildRowID)
			if listErr != nil {
				return fmt.Errorf("enumerate active game servers: %w", listErr)
			}
			if len(activeServers) == 0 {
				slog.Warn("component=servers", "msg", "no active game servers for this guild yet; run /server connect and /server select to enable the killfeed")
			}
			if len(activeServers) > 0 {
				guildRecord, _, guildRecordErr := a.Guilds.GetGuild(ctx, a.Config.DiscordGuildID)
				persistedSelection := int64(0)
				if guildRecordErr == nil && guildRecord != nil {
					persistedSelection = guildRecord.SelectedPublicServerID
				}
				selectedServerID, selectedOK := selectPublicCounterServer(persistedSelection, activeServers)
				a.counterOwnerMu.Lock()
				if selectedOK {
					a.publicCounterServerID = selectedServerID
					if persistedSelection == 0 {
						if err := a.Guilds.SetSelectedPublicServer(ctx, a.Config.DiscordGuildID, a.publicCounterServerID); err != nil {
							slog.Warn("component=servers", "event", "public_counter_selection_persist_failed", "err", err.Error())
						}
					}
				} else if persistedSelection > 0 {
					slog.Warn("component=servers", "event", "public_counter_selection_unresolved", "selected_server_id", persistedSelection)
				}
				a.counterOwnerMu.Unlock()
			}
			if a.SeasonService != nil {
				seasonCtx, seasonCancel := context.WithTimeout(ctx, 10*time.Second)
				if _, seasonErr := a.SeasonService.EnsureDefaultSeason(seasonCtx, guildRowID, time.Now().UTC()); seasonErr != nil {
					slog.Warn("component=seasons", "msg", "could not ensure default season", "err", seasonErr.Error())
				}
				seasonCancel()
			}
			// Guild-level routed artifacts (link/stats panels, the persistent
			// leaderboard) follow the union of the AUTO_LEADERBOARD /
			// LINK_GAMERTAG / STATS_LEADERBOARDS routes of every active
			// server in the guild, each resolved on its own (guild, server)
			// identity through the shared resolver.
			routingEnabled := a.ChannelRoutes != nil && a.GuildRoutePanels != nil
			guildServers := func(ctx context.Context) (int64, []int64, error) {
				rows, listErr := a.Servers.ListActiveByGuild(ctx, guildRowID)
				if listErr != nil {
					return 0, nil, listErr
				}
				ids := make([]int64, 0, len(rows))
				for _, r := range rows {
					ids = append(ids, r.ID)
				}
				return guildRowID, ids, nil
			}
			var routePanels *discord.RoutePanels
			if routingEnabled {
				routePanels = discord.NewRoutePanels(api, discord.NewRoutePanelStore(a.GuildRoutePanels))
				a.routePanels = routePanels
			}
			if routingEnabled && a.Ranked != nil {
				for _, serverRow := range activeServers {
					board := discord.NewServerRanksBoard(a.ChannelRoutes, routePanels, a.Ranked, guildRowID, serverRow.ID, serverRow.DisplayName)
					board.SetServerNameFunc(a.serverNameFunc())
					a.ServerRanksBoards = append(a.ServerRanksBoards, board)
					go a.singleton(ctx, fmt.Sprintf("server_ranks_board_%d", serverRow.ID), board.Run)
				}
			}
			if routingEnabled && a.EconomyService != nil {
				// ECONOMY: the public transaction feed, per (guild, server) through the
				// shared resolver, no fallback. Fed only after a transaction committed
				// (admin adjustments and bounty payouts).
				economyFeed := discord.NewEconomyFeed(session, a.ChannelRoutes, guildServers)
				economyFeed.SetCustomizer(a.embedCustomizer(), a.serverNameFunc())
				a.EconomyService.SetNotifier(economyFeed)
				if a.BountyService != nil {
					a.BountyService.SetEconomyNotifier(economyFeed)
				}
				go economyFeed.Run(ctx)
			}
			if routingEnabled && a.BountyService != nil {
				// BOUNTY (public board, one persistent message per routed channel) and
				// BOUNTY_TRACKING (lifecycle feed), both resolved per (guild, server)
				// through the shared resolver. They only report state that is already
				// committed; with no routes they are no-ops and the database is unaffected.
				bountyTracker := discord.NewBountyTracker(session, a.ChannelRoutes, guildServers)
				bountyTracker.SetCustomizer(a.embedCustomizer(), a.serverNameFunc())
				a.BountyBoard = discord.NewBountyBoard(a.ChannelRoutes, guildServers, routePanels, a.Bounties)
				a.BountyService.SetNotifier(discord.BountyEvents{Tracker: bountyTracker, Board: a.BountyBoard})
				go bountyTracker.Run(ctx)
				go a.singleton(ctx, "bounty_board", a.BountyBoard.Run)
			}
			if routingEnabled {
				a.guildServers = guildServers
				// Completion announcements follow the SERVER_STATUS route.
				a.CompletionPublisher.SetRouting(a.ChannelRoutes, guildServers)
				// SERVER_STATUS: one persistent status message per routed
				// channel, from what the server workers observe.
				a.ServerStatusBoard = discord.NewServerStatusBoard(a.ChannelRoutes, guildServers, routePanels)
				a.ServerStatusBoard.SetServerNames(a.serverNameFunc())
				go a.singleton(ctx, "server_status_board", a.ServerStatusBoard.Run)
				a.syncOnlineCounterRoute(ctx)
			}
			if routingEnabled {
				// ADMIN_ALERTS: operational conditions reported by the server
				// workers and the zone engine; with no route nothing is sent.
				a.AdminAlerts = discord.NewAdminAlertPublisher(session, a.ChannelRoutes)
				// Paid Watch messages use the durable outbox exclusively. The
				// legacy in-memory queue has NO premium authorizer in production.
				a.AdminAlerts.SetServerNames(a.serverNameFunc())
				go a.AdminAlerts.Run(ctx)
				// C.A.S.E. staff alerts: sends nothing until a detector is
				// released and an owner turns staff alerts on.
				a.startCaseStaffAlerts(ctx)
				// Paid Base Raid Alarm expiry DMs (one per ended purchase).
				a.startSecurityExpiryWorker(ctx)
				// Base Black Box history clean-up (each server's retention).
				a.startBaseBlackBoxPruner(ctx)
				// Security Store panel in Discord (refreshed every 10 minutes).
				a.startSecurityPanelWorker(ctx)
				// Base rent reminders (due soon / paused), one DM each.
				a.startBaseRentReminders(ctx)
				if a.CaseDigestOutbox != nil && a.Config.CaseAccessEnabled {
					go a.runCaseDigestWorker(ctx)
				}
			}
			if routingEnabled && a.Heatmap != nil {
				// HEATMAPS: one persistent PvP summary per routed channel, read
				// from the Phase 5 aggregates (never Nitrado) on a configurable
				// interval; with no route it is a no-op.
				interval := time.Duration(config.DefaultHeatmapDiscordIntervalMinutes) * time.Minute
				if a.Config != nil {
					interval = time.Duration(a.Config.HeatmapDiscordIntervalMinutes) * time.Minute
				}
				a.HeatmapBoard = discord.NewHeatmapBoard(a.ChannelRoutes, guildServers, routePanels, a.Heatmap, interval)
				a.HeatmapBoard.SetServerNames(a.serverNameFunc())
				a.HeatmapBoard.SetPicture(&heatmapimage.Tiles{BaseURL: a.siteURL()}, a.heatmapMapOf)
				go a.singleton(ctx, "heatmap_board", a.HeatmapBoard.Run)
			}
			if a.Stats != nil {
				legacyLeaderboardChannel, legacyLeaderboardMessage := "", ""
				if gs, gsErr := setupStore.Get(a.Config.DiscordGuildID); gsErr == nil && gs != nil {
					legacyLeaderboardChannel, legacyLeaderboardMessage = gs.LeaderboardsChannelID, gs.LeaderboardMessageID
				}
				if legacyLeaderboardChannel != "" || routingEnabled {
					leaderboardPanel := discord.NewLeaderboardPanel(api, legacyLeaderboardChannel, legacyLeaderboardMessage, discord.DefaultLeaderboardConfig())
					a.LeaderboardScheduler = discord.NewLeaderboardScheduler(leaderboardPanel, a.Stats, guildRowID, discord.DefaultLeaderboardConfig(), func(messageID string) {
						if latest, latestErr := setupStore.Get(a.Config.DiscordGuildID); latestErr == nil && latest != nil {
							latest.LeaderboardMessageID = messageID
							_ = setupStore.Save(*latest)
						}
					})
					// The guild V3 ranks embed follows its selected public server's
					// active Ranked season. The dedicated boards remain per-server.
					if a.Ranked != nil && a.Servers != nil {
						a.LeaderboardScheduler.SetRankSource(discord.ServerSeasonRankReader{Servers: a.Servers, Ranked: a.Ranked})
					}
					a.LeaderboardScheduler.SetServerNames(guildServers, a.serverNameFunc())
					if a.Upgrades != nil {
						a.LeaderboardScheduler.SetMovementStore(a.Upgrades)
					}
					if routingEnabled {
						a.LeaderboardScheduler.SetRouting(a.ChannelRoutes, guildServers, routePanels,
							discord.NewLegacyLeaderboardRetirer(api, setupStore, a.Config.DiscordGuildID))
					}
					go a.singleton(ctx, "leaderboard_scheduler", a.LeaderboardScheduler.Run)
					if a.AdminService != nil {
						a.AdminService.SetLeaderboardRefresh(func(refreshCtx context.Context) error {
							return a.LeaderboardScheduler.RefreshOnce(refreshCtx)
						})
					}
				}
			}
			if routingEnabled {
				a.RouteSyncer = discord.NewRouteSyncer(a.ChannelRoutes, guildServers, routePanels, api, setupStore, a.Config.DiscordGuildID)
				if a.LeaderboardScheduler != nil {
					a.RouteSyncer.SetLeaderboard(a.LeaderboardScheduler)
				}
				a.RouteSyncer.SetLegacyRestore(func() {
					if _, _, ensureErr := setupManager.RestoreLegacyPanels(a.Config.DiscordGuildID); ensureErr != nil {
						slog.Warn("component=discord", "event", "legacy_panel_restore_failed", "err", ensureErr.Error())
					}
				})
				setupManager.SetRouteGate(a.RouteSyncer.HasRoute)
				go a.singleton(ctx, "route_syncer", a.RouteSyncer.Run)
			}
			store := &persistenceStoreAdapter{players: a.Players, kills: a.Kills, deaths: a.Deaths, seasons: a.Seasons, ranked: a.Ranked, factions: a.Factions, wars: a.Wars, events: a.Events, vip: a.VIP, bounties: a.Bounties, bountySvc: a.BountyService, streaks: a.Streaks, anomalies: a.Anomalies, activity: a.ActivityRepository, servers: a.Servers, stats: a.Stats, analytics: a.AnalyticsRepository, factionStats: a.FactionHubStats, locations: a.Locations, zones: a.Zones, lives: a.Lives, lifeRecap: a.LifeRecap, tournaments: a.TournamentService, rankedTags: a.killfeedRankedTagsOn, panelDirty: func() {
				if a.LeaderboardScheduler != nil {
					a.LeaderboardScheduler.MarkDirty()
				}
			}}

			serversByID := make(map[int64]repository.GameServer, len(activeServers))
			for _, row := range activeServers {
				serversByID[row.ID] = row
			}

			// WorkerManager is constructed here even with zero active servers so
			// that /server select (a dynamic first connect) always has a runtime
			// to attach a worker to on a fresh guild.
			a.WorkerManager = servers.NewWorkerManager(func(workerCtx context.Context, workerServerID int64) error {
				row, ok := serversByID[workerServerID]
				if !ok {
					if fetched, fetchErr := a.Servers.GetByID(workerCtx, workerServerID); fetchErr == nil && fetched != nil {
						row = *fetched
					} else {
						return fmt.Errorf("server worker %d: unknown game_servers row", workerServerID)
					}
				}
				return a.runServerWorker(workerCtx, row, store, setupStore, onlineCounter)
			})
			// A failed worker is restarted under supervision (jittered backoff)
			// unless its game_servers row was deactivated in the meantime - an
			// intentional disconnect/suspend must not be undone by the supervisor.
			a.WorkerManager.SetRestartGate(func(gateCtx context.Context, workerServerID int64) bool {
				lookupCtx, cancelLookup := context.WithTimeout(gateCtx, 5*time.Second)
				defer cancelLookup()
				row, err := a.Servers.GetByID(lookupCtx, workerServerID)
				if err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						return false
					}
					slog.Warn("component=servers", "msg", "restart gate could not read game_servers; allowing restart", "server_id", workerServerID, "err", err.Error())
					return true
				}
				return row.Active
			})

			expectedWorkers = len(activeServers)
			for _, row := range activeServers {
				if err := a.WorkerManager.Start(ctx, row.ID); err != nil {
					slog.Error("component=servers", "msg", "failed to start server worker", "server_id", row.ID, "err", err.Error())
					continue
				}
				slog.Info("component=servers", "msg", "server worker started", "server_id", row.ID, "display_name", row.DisplayName)
			}

			if a.EventService != nil || a.CompletionPublisher != nil {
				go a.singleton(ctx, "competitive_schedulers", func(leaderCtx context.Context) {
					a.runCompetitiveSchedulers(leaderCtx, guildRowID)
				})
			}
		} else {
			slog.Warn("component=database", "msg", "no guild record yet; run /setup to enable persistence")
		}
	}

	// Reflect initial setup readiness into the status endpoint.
	if gs, err := setupStore.Get(a.Config.DiscordGuildID); err == nil && gs != nil {
		state.SetSetupReadiness(
			gs.CategoryID != "" && gs.KillfeedChannelID != "" && gs.OnlinePlayersChannelID != "",
			gs.KillfeedChannelID != "",
			gs.OnlinePlayersChannelID != "",
			gs.ServerStatusChannelID != "",
		)
	}

	slog.Info("component=startup", "msg", "DayZ killfeed live foundation ready")
	if logSourceVerified {
		slog.Info("component=killfeed", "msg", "live gameplay log source verified")
	}

	// Start-up is done: from here the process serves. The self-check logs one line once the
	// leader lock, the Discord gateway and every server worker are in place (docs/DEPLOY.md).
	a.markReady(ctx, expectedWorkers)

	if err := a.HTTPServer.ListenAndServe(ctx); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("start HTTP server: %w", err)
	}

	<-ctx.Done()
	slog.Info("component=shutdown", "msg", "shutdown requested")
	a.shutdown()
	return nil
}
