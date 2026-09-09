package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/server"
)

// App owns the main runtime dependencies.
type App struct {
	Config     *config.Config
	Nitrado    *nitrado.Client
	Discord    *discord.Client
	HTTPServer *server.Server
	State      *server.State
	cancel     context.CancelFunc
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

	_, cancel := context.WithCancel(ctx)

	return &App{
		Config:     cfg,
		Nitrado:    nitradoClient,
		Discord:    discordClient,
		HTTPServer: httpServer,
		State:      state,
		cancel:     cancel,
	}, nil
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
	setupStore := discord.NewInMemorySetupStore() // in-memory for Phase 3.1; PostgreSQL in Phase 4
	session := a.Discord.Session()
	api := discord.NewSessionAPI(session)
	setupManager := discord.NewSetupManager(api, setupStore, a.Discord.BotID())
	setupHandler := discord.NewSetupHandler(setupManager)
	if a.Config.DiscordGuildID != "" {
		if err := discord.RegisterSetupCommand(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register /setup command", "err", err.Error())
		}
	}
	a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			if i.ApplicationCommandData().Name == "setup" {
				setupHandler.Handle(s, i)
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
	slog.Info("component=shutdown", "msg", "shutdown complete")
	time.Sleep(50 * time.Millisecond)
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
