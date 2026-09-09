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

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/linking"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/server"
)

// App owns the main runtime dependencies.
type App struct {
	Config       *config.Config
	Nitrado      *nitrado.Client
	Discord      *discord.Client
	HTTPServer   *server.Server
	State        *server.State
	DB           *database.DB
	Guilds       *repository.GuildRepository
	Players      *repository.PlayerRepository
	Kills        *repository.KillRepository
	Deaths       *repository.DeathRepository
	Stats        *repository.StatsRepository
	Sessions     *repository.SessionRepository
	Checkpoints  *repository.CheckpointRepository
	Streaks      *repository.StreakRepository
	Achievements *repository.AchievementRepository
	Links        *repository.LinkRepository
	LinkService  *linking.LinkVerificationService
	persistQueue *killfeed.PersistenceQueue
	cancel       context.CancelFunc
}

// New creates an application instance with the required dependencies.
func New(ctx context.Context, cfg *config.Config) (*App, error) {
	slog.Info("component=startup", "msg", "starting DayZ killfeed")

	nitradoClient := nitrado.NewClient("https://api.nitrado.net", cfg.NitradoToken, nil)
	slog.Info("component=nitrado", "msg", "client configured", "base_url", nitradoClient.BaseURL())

	discordClient, err := discord.New(cfg.DiscordToken)
	if err != nil {
		return nil, fmt.Errorf("create Discord client: %w", err)
	}

	state := server.NewState()
	httpServer, err := server.New(cfg, state)
	if err != nil {
		return nil, fmt.Errorf("create HTTP server: %w", err)
	}

	app := &App{
		Config:     cfg,
		Nitrado:    nitradoClient,
		Discord:    discordClient,
		HTTPServer: httpServer,
		State:      state,
	}

	// --- PostgreSQL (optional): connect + migrate. Degraded mode if unconfigured. ---
	if cfg.DatabaseURL != "" {
		dbCtx, dbCancel := context.WithTimeout(ctx, 20*time.Second)
		db, err := database.Connect(dbCtx, cfg.DatabaseURL)
		dbCancel()
		if err != nil {
			state.SetDatabase(false, 0, 0)
			slog.Error("component=database", "msg", "database unavailable", "err", err.Error())
		} else {
			migCtx, migCancel := context.WithTimeout(ctx, 60*time.Second)
			if err := db.Migrate(migCtx); err != nil {
				migCancel()
				db.Close()
				state.SetDatabase(false, 0, 0)
				return nil, fmt.Errorf("run database migrations: %w", err)
			}
			migCancel()
			total, idle, _ := db.PoolStats()
			state.SetDatabase(true, total, idle)
			app.DB = db
			app.Guilds = repository.NewGuildRepository(db.Pool)
			app.Players = repository.NewPlayerRepository(db.Pool)
			app.Kills = repository.NewKillRepository(db.Pool)
			app.Deaths = repository.NewDeathRepository(db.Pool)
			app.Stats = repository.NewStatsRepository(db.Pool)
			app.Sessions = repository.NewSessionRepository(db.Pool)
			app.Checkpoints = repository.NewCheckpointRepository(db.Pool)
			app.Streaks = repository.NewStreakRepository(db.Pool)
			app.Achievements = repository.NewAchievementRepository(db.Pool)
			app.Links = repository.NewLinkRepository(db.Pool)
			app.LinkService = linking.NewService(app.Links)
			seedCtx, seedCancel := context.WithTimeout(ctx, 10*time.Second)
			seedErr := app.Achievements.EnsureDefinitions(seedCtx)
			seedCancel()
			if seedErr != nil {
				return nil, fmt.Errorf("seed achievement definitions: %w", seedErr)
			}
		}
	} else {
		slog.Warn("component=database", "msg", "DATABASE_URL not configured; persistence disabled (degraded mode)")
		state.SetDatabase(false, 0, 0)
	}

	_, cancel := context.WithCancel(ctx)
	app.cancel = cancel
	return app, nil
}

// Run boots the application and runs until shutdown.
func (a *App) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	state := a.State
	if state == nil {
		state = server.NewState()
		a.State = state
	}

	// Sanitized configuration presence. Values are never logged.
	slog.Info("component=startup", "msg", "configuration loaded",
		"NITRADO_TOKEN_configured", a.Config.NitradoToken != "",
		"NITRADO_SERVICE_ID_configured", a.Config.NitradoServiceID != "",
		"DISCORD_TOKEN_configured", a.Config.DiscordToken != "",
		"KILLFEED_CHANNEL_ID_configured", a.Config.KillfeedChannelID != "",
	)

	// --- Nitrado authentication and service verification ---
	if err := a.Nitrado.AuthenticationCheck(ctx); err != nil {
		state.SetNitrado(false, false, "", "", "")
		logNitradoFailure("authentication", err)
		return fmt.Errorf("nitrado startup failed during authentication: %w", err)
	}

	services, err := a.Nitrado.GetServices(ctx)
	if err != nil {
		state.SetNitrado(false, false, "", "", "")
		logNitradoFailure("service discovery", err)
		return fmt.Errorf("nitrado startup failed during service discovery: %w", err)
	}

	dayZServices := nitrado.FindDayZServices(services)
	if len(dayZServices) == 0 {
		slog.Warn("component=nitrado", "msg", "no DayZ services discovered")
	} else {
		slog.Info("component=nitrado", "msg", "DayZ services discovered", "count", len(dayZServices))
	}

	serviceVerified := false
	logSourceVerified := false
	var serviceGame, serviceType, serviceStatus string

	if a.Config.NitradoServiceID == "" {
		slog.Warn("component=nitrado", "msg", "NITRADO_SERVICE_ID not configured; skipping service verification and log discovery")
	} else {
		service, err := a.Nitrado.ValidateServiceID(ctx, a.Config.NitradoServiceID, services)
		if err != nil {
			state.SetNitrado(true, false, "", "", "")
			slog.Error("component=nitrado", "operation", "service verification", "msg", "service ID not found", "service_id", a.Config.NitradoServiceID)
			return fmt.Errorf("nitrado service verification failed: %w", err)
		}
		serviceVerified = true
		serviceGame, serviceType, serviceStatus = service.Game, service.Type, service.Status
		slog.Info("component=nitrado", "msg", "configured service verified",
			"service_id", a.Config.NitradoServiceID,
			"game", service.Game,
			"service_type", service.Type,
			"status", service.Status,
		)

		// Sanitized inspection of the real service payload for file/log capability fields.
		if err := a.Nitrado.InspectService(ctx, a.Config.NitradoServiceID); err != nil {
			slog.Warn("component=nitrado", "msg", "service payload inspection failed", "err", err.Error())
		}

		// NOTE: discovery runs exclusively inside the killfeed engine worker.
		// A second inline ListLogs here would create a duplicate discovery worker
		// and a log storm, so it has been removed.
	}
	state.SetNitrado(true, serviceVerified, serviceGame, serviceType, serviceStatus)

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
	api := discord.NewSessionAPI(session)
	setupManager := discord.NewSetupManager(api, setupStore, a.Discord.BotID())
	setupHandler := discord.NewSetupHandler(setupManager)
	welcomeHandler := discord.NewWelcomeHandler(setupStore)
	if a.Config.DiscordGuildID != "" {
		if err := discord.RegisterSetupCommand(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register /setup command", "err", err.Error())
		}
	}

	// Stats and leaderboard commands require the database + a guild record.
	if a.Stats != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		statsHandler := discord.NewStatsCommandHandler(a.Stats, a.Guilds, a.Config.DiscordGuildID)
		if err := discord.RegisterStatsCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register stats commands", "err", err.Error())
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			if i.Type != discordgo.InteractionApplicationCommand {
				return
			}
			switch i.ApplicationCommandData().Name {
			case "stats":
				statsHandler.HandleStats(s, i)
			case "leaderboard":
				statsHandler.HandleLeaderboard(s, i)
			}
		})
	}
	if a.LinkService != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		linkHandler := discord.NewLinkCommandHandler(a.LinkService, a.Guilds)
		if err := discord.RegisterLinkCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register link commands", "err", err.Error())
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			if i.Type == discordgo.InteractionMessageComponent {
				if strings.HasPrefix(i.MessageComponentData().CustomID, "champion_unlink_") {
					linkHandler.HandleComponent(s, i)
				}
				return
			}
			if i.Type == discordgo.InteractionApplicationCommand {
				name := i.ApplicationCommandData().Name
				if name == "link" || name == "link-status" || name == "unlink" {
					linkHandler.Handle(s, i)
				}
			}
		})
	}
	a.Discord.AddMemberJoinHandler(welcomeHandler.HandleMemberJoin)

	// Stats/leaderboard commands only work with a database.
	var statsHandler *discord.StatsCommandHandler
	if a.Stats != nil {
		statsHandler = discord.NewStatsCommandHandler(a.Stats, a.Guilds, a.Config.DiscordGuildID)
		if a.Config.DiscordGuildID != "" {
			if err := discord.RegisterStatsCommands(session, a.Config.DiscordGuildID); err != nil {
				slog.Warn("component=discord", "msg", "failed to register stats commands", "err", err.Error())
			}
		}
	}
	a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			name := i.ApplicationCommandData().Name
			switch name {
			case "setup":
				setupHandler.Handle(s, i)
			case "stats":
				if statsHandler != nil {
					statsHandler.HandleStats(s, i)
				} else {
					discord.RespondEphemeral(s, i, "Stats require the database. Set DATABASE_URL.")
				}
			case "leaderboard":
				if statsHandler != nil {
					statsHandler.HandleLeaderboard(s, i)
				} else {
					discord.RespondEphemeral(s, i, "Leaderboard requires the database. Set DATABASE_URL.")
				}
			}
		case discordgo.InteractionMessageComponent:
			setupHandler.HandleResetConfirm(s, i)
		}
	})

	// --- Nitrado log polling engine (real ADM parser + killfeed publisher) ---
	engine := killfeed.NewEngine(a.Nitrado, a.Config.NitradoServiceID, killfeed.NewADMParser())
	engine.SetStateSink(state)

	// Killfeed publisher: stored guild setup wins, env var is the fallback.
	publisher := discord.NewKillfeedPublisher(a.Discord, a.Config.KillfeedChannelID)
	publisher.BindStore(setupStore, a.Config.DiscordGuildID)
	engine.SetKillPublisher(publisher)

	// --- Persistence queue (persist-before-publish, durable dedupe) ---
	if a.DB != nil && a.Players != nil && a.Kills != nil && a.Deaths != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		_, guildRowID, err := a.Guilds.GetGuild(ctx, a.Config.DiscordGuildID)
		if err != nil {
			slog.Warn("component=database", "msg", "could not resolve guild row; persistence disabled", "err", err.Error())
		}
		if guildRowID > 0 {
			store := &persistenceStoreAdapter{players: a.Players, kills: a.Kills, deaths: a.Deaths}
			pq := killfeed.NewPersistenceQueue(store, guildRowID, a.Config.NitradoServiceID)
			engine.SetPersistence(pq)
			a.persistQueue = pq
			go pq.Run(ctx)
			slog.Info("component=database", "msg", "persistence queue started")
		} else {
			slog.Warn("component=database", "msg", "no guild record yet; run /setup to enable persistence")
		}
	}

	// Online players voice counter: renames the configured voice channel on
	// debounced count changes. Uses the single PlayerTracker as the source of truth.
	onlineCounter := discord.NewVoiceChannelCounter(api, "")
	if cfg := setupStore; cfg != nil {
		if gs, err := cfg.Get(a.Config.DiscordGuildID); err == nil && gs != nil && gs.OnlinePlayersChannelID != "" {
			onlineCounter.SetChannelID(gs.OnlinePlayersChannelID)
		}
	}
	engine.OnPlayersChanged(func(count int) {
		state.SetOnlinePlayers(count)
		onlineCounter.Publish(count)
		state.SetOnlineCounter(onlineCounter.LastPublished(), onlineCounter.UpdateErrors(), onlineCounter.PermissionBlocked())
	})

	// Reflect initial setup readiness into the status endpoint.
	if gs, err := setupStore.Get(a.Config.DiscordGuildID); err == nil && gs != nil {
		state.SetSetupReadiness(
			gs.CategoryID != "" && gs.KillfeedChannelID != "" && gs.OnlinePlayersChannelID != "",
			gs.KillfeedChannelID != "",
			gs.OnlinePlayersChannelID != "",
			gs.ServerStatusChannelID != "",
		)
	}

	go func() {
		if err := engine.Start(ctx); err != nil {
			slog.Warn("component=killfeed", "msg", "engine stopped", "err", err.Error())
		}
	}()

	slog.Info("component=startup", "msg", "DayZ killfeed live foundation ready")
	if logSourceVerified {
		slog.Info("component=killfeed", "msg", "live gameplay log source verified")
	}

	if err := a.HTTPServer.ListenAndServe(ctx); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("start HTTP server: %w", err)
	}

	<-ctx.Done()
	slog.Info("component=shutdown", "msg", "shutdown requested")
	a.shutdown()
	return nil
}

func (a *App) shutdown() {
	if a.cancel != nil {
		a.cancel()
	}
	// Flush the persistence queue (drain pending events) before closing the DB.
	if a.persistQueue != nil {
		a.persistQueue.Close()
		slog.Info("component=shutdown", "msg", "persistence queue drained")
	}
	if a.HTTPServer != nil {
		if err := a.HTTPServer.Shutdown(context.Background()); err != nil {
			slog.Error("component=shutdown", "msg", "HTTP server shutdown failed", "err", err.Error())
		} else {
			slog.Info("component=shutdown", "msg", "HTTP server stopped")
		}
	}
	if a.Discord != nil {
		if err := a.Discord.Close(); err != nil {
			slog.Error("component=shutdown", "msg", "Discord close failed", "err", err.Error())
		} else {
			slog.Info("component=shutdown", "msg", "Discord connection closed")
		}
	}
	if a.DB != nil {
		a.DB.Close()
	}
	slog.Info("component=shutdown", "msg", "shutdown complete")
	time.Sleep(50 * time.Millisecond)
}

// persistenceStoreAdapter adapts the repositories to the killfeed.PersistenceStore
// interface used by the persistence queue worker.
type persistenceStoreAdapter struct {
	players *repository.PlayerRepository
	kills   *repository.KillRepository
	deaths  *repository.DeathRepository
}

func (p *persistenceStoreAdapter) UpsertPlayer(ctx context.Context, guildID int64, dayzID, displayName string, seenAt time.Time) (int64, error) {
	return p.players.UpsertPlayer(ctx, guildID, dayzID, displayName, seenAt)
}

func (p *persistenceStoreAdapter) InsertKill(ctx context.Context, k repository.KillRecord) error {
	return p.kills.InsertKill(ctx, k)
}

func (p *persistenceStoreAdapter) InsertDeath(ctx context.Context, d repository.DeathRecord) error {
	return p.deaths.InsertDeath(ctx, d)
}

// playerNames returns the sorted display names of currently online players.
func playerNames(tracker *killfeed.PlayerTracker) []string {
	if tracker == nil {
		return nil
	}
	online := tracker.GetOnlinePlayers()
	names := make([]string, 0, len(online))
	for _, p := range online {
		names = append(names, p.Name)
	}
	return names
}

// logNitradoFailure emits a sanitized, classified failure line. It never
// includes tokens or headers — only HTTP status, operation, and failure kind.
func logNitradoFailure(operation string, err error) {
	var reqErr *nitrado.RequestError
	if errors.As(err, &reqErr) {
		slog.Error("component=nitrado",
			"status", reqErr.StatusCode,
			"operation", operation,
			"kind", string(reqErr.Kind),
			"msg", nitradoFailureMessage(reqErr.Kind),
		)
		return
	}
	slog.Error("component=nitrado", "operation", operation, "kind", string(nitrado.KindTemporary), "msg", err.Error())
}

// nitradoFailureMessage maps a failure kind to a human-readable cause.
func nitradoFailureMessage(kind nitrado.ErrorKind) string {
	switch kind {
	case nitrado.KindAuthentication:
		return "authentication failed"
	case nitrado.KindInvalidEndpoint:
		return "invalid API endpoint"
	case nitrado.KindNotFound:
		return "service ID not found"
	case nitrado.KindPermission:
		return "API permission problem"
	case nitrado.KindTemporary:
		return "temporary Nitrado failure"
	default:
		return "unexpected Nitrado response"
	}
}
