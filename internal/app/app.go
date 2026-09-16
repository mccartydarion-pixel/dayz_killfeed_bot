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
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/admin"
	"github.com/yourname/dayz-killfeed/internal/analytics"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/discord/panels"
	competitiveevents "github.com/yourname/dayz-killfeed/internal/events"
	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/linking"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/operations"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/seasons"
	"github.com/yourname/dayz-killfeed/internal/security"
	"github.com/yourname/dayz-killfeed/internal/server"
	"github.com/yourname/dayz-killfeed/internal/servers"
)

// App owns the main runtime dependencies.
type App struct {
	Config                *config.Config
	Nitrado               *nitrado.Client
	Discord               *discord.Client
	HTTPServer            *server.Server
	State                 *server.State
	DB                    *database.DB
	Guilds                *repository.GuildRepository
	Players               *repository.PlayerRepository
	Kills                 *repository.KillRepository
	Deaths                *repository.DeathRepository
	Stats                 *repository.StatsRepository
	Sessions              *repository.SessionRepository
	Checkpoints           *repository.CheckpointRepository
	Streaks               *repository.StreakRepository
	Achievements          *repository.AchievementRepository
	Events                *repository.EventRepository
	EventService          *competitiveevents.Service
	Bounties              *repository.BountyRepository
	Points                *repository.PointsRepository
	Seasons               *repository.SeasonRepository
	SeasonService         *seasons.Service
	Factions              *repository.FactionRepository
	Wars                  *repository.PostgresWarRepository
	FactionStats          *repository.FactionStatsRepository
	FactionPresentation   *repository.FactionPresentationRepository
	Anomalies             *repository.AnomalyRepository
	Announcements         *repository.AnnouncementRepository
	AnnouncementService   *discord.CompletionAnnouncementService
	HealthRegistry        *health.Registry
	Workers               *health.WorkerRegistry
	WorkerManager         *servers.WorkerManager
	ADMHealth             *operations.ADMMonitor
	AdminService          *admin.Service
	AnalyticsRepository   *repository.AnalyticsRepository
	WelcomeRepository     *repository.WelcomeRepository
	ActivityRepository    *repository.ActivityRepository
	Servers               *repository.ServerRepository
	CredentialCipher      *security.AESGCM
	CompletionPublisher   *discord.LiveCompletionPublisher
	PanelService          *panels.RefreshService
	LeaderboardScheduler  *discord.LeaderboardScheduler
	Links                 *repository.LinkRepository
	LinkService           *linking.LinkVerificationService
	persistQueuesMu       sync.Mutex
	persistQueues         []*killfeed.PersistenceQueue
	firstConnectMu        sync.Mutex
	firstConnectServers   map[int64]bool
	counterOwnerMu        sync.RWMutex
	publicCounterServerID int64
	presenceMu            sync.Mutex
	presenceTrackers      map[int64]*killfeed.PlayerTracker
	presenceEngines       map[int64]*killfeed.Engine
	cancel                context.CancelFunc
}

// registerPresenceTracker exposes a running ServerWorker's live PlayerTracker
// for diagnostics, keyed by the exact game_servers row ID it owns.
func (a *App) registerPresenceTracker(serverID int64, tracker *killfeed.PlayerTracker) {
	a.presenceMu.Lock()
	if a.presenceTrackers == nil {
		a.presenceTrackers = make(map[int64]*killfeed.PlayerTracker)
	}
	a.presenceTrackers[serverID] = tracker
	a.presenceMu.Unlock()
}

func (a *App) registerPresenceEngine(serverID int64, engine *killfeed.Engine) {
	a.presenceMu.Lock()
	if a.presenceEngines == nil {
		a.presenceEngines = make(map[int64]*killfeed.Engine)
	}
	a.presenceEngines[serverID] = engine
	a.presenceMu.Unlock()
}

// unregisterPresenceTracker removes a worker's tracker once it stops, so
// diagnostics never read a stale reference for a server that is no longer live.
func (a *App) unregisterPresenceTracker(serverID int64) {
	a.presenceMu.Lock()
	delete(a.presenceTrackers, serverID)
	delete(a.presenceEngines, serverID)
	a.presenceMu.Unlock()
}

func (a *App) livePresenceSnapshot(serverID int64) (killfeed.PresenceSnapshot, bool) {
	a.presenceMu.Lock()
	engine, ok := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if !ok || engine == nil {
		return killfeed.PresenceSnapshot{}, false
	}
	return engine.PresenceSnapshot(), true
}

func (a *App) livePipelineSnapshot(serverID int64) (killfeed.RuntimeDiagnosticSnapshot, bool) {
	a.presenceMu.Lock()
	engine, ok := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if !ok || engine == nil || engine.Diagnostics() == nil {
		return killfeed.RuntimeDiagnosticSnapshot{}, false
	}
	return engine.Diagnostics().Snapshot(), true
}

// selectedServerEngine returns the live Engine for the guild's currently
// selected public server, so live tooling (e.g. the ADM source scan) reuses
// the exact same running Nitrado client instead of constructing a new one.
func (a *App) selectedServerEngine(ctx context.Context) (*killfeed.Engine, int64, bool) {
	if a.Guilds == nil {
		return nil, 0, false
	}
	guild, _, err := a.Guilds.GetGuild(ctx, a.Config.DiscordGuildID)
	if err != nil || guild == nil || guild.SelectedPublicServerID == 0 {
		return nil, 0, false
	}
	a.presenceMu.Lock()
	engine, ok := a.presenceEngines[guild.SelectedPublicServerID]
	a.presenceMu.Unlock()
	if !ok || engine == nil {
		return nil, guild.SelectedPublicServerID, false
	}
	return engine, guild.SelectedPublicServerID, true
}

func (a *App) recordPublicVoicePublish(count int, result string) {
	a.counterOwnerMu.RLock()
	serverID := a.publicCounterServerID
	a.counterOwnerMu.RUnlock()
	a.presenceMu.Lock()
	engine := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if engine != nil {
		engine.RecordVoicePublish(count, result)
	}
}

// livePresenceCount returns the exact running worker's online count for a
// server, or (0, false) if no worker is currently registered for it.
func (a *App) livePresenceCount(serverID int64) (int, bool) {
	a.presenceMu.Lock()
	tracker, ok := a.presenceTrackers[serverID]
	a.presenceMu.Unlock()
	if !ok || tracker == nil {
		return 0, false
	}
	return tracker.OnlineCount(), true
}

// markFirstConnect flags a server so its next worker start seeds the ADM
// checkpoint at the log tail instead of byte 0 (see ConnectServer).
func (a *App) markFirstConnect(serverID int64) {
	a.firstConnectMu.Lock()
	if a.firstConnectServers == nil {
		a.firstConnectServers = make(map[int64]bool)
	}
	a.firstConnectServers[serverID] = true
	a.firstConnectMu.Unlock()
}

// consumeFirstConnect reports and clears the first-connect flag for a server,
// so only the triggering start (not later restarts) skips log history.
func (a *App) consumeFirstConnect(serverID int64) bool {
	a.firstConnectMu.Lock()
	defer a.firstConnectMu.Unlock()
	if a.firstConnectServers[serverID] {
		delete(a.firstConnectServers, serverID)
		return true
	}
	return false
}

// ConnectServer implements discord.ServerRuntime: it marks the server as a
// first connect (tail-start) and starts its worker via WorkerManager. Safe to
// call for a server that was never active at startup (e.g. /server select on
// a fresh guild) because WorkerManager's factory resolves unknown IDs from
// the database.
func (a *App) ConnectServer(ctx context.Context, serverID int64) error {
	if a.WorkerManager == nil {
		return fmt.Errorf("killfeed runtime is not initialized (database required)")
	}
	a.markFirstConnect(serverID)
	a.counterOwnerMu.Lock()
	a.publicCounterServerID = serverID
	a.counterOwnerMu.Unlock()
	if a.Guilds != nil && a.Config != nil {
		if _, guildID, err := a.Guilds.GetGuild(ctx, a.Config.DiscordGuildID); err == nil && guildID > 0 {
			if err := a.Guilds.SetSelectedPublicServer(ctx, a.Config.DiscordGuildID, serverID); err != nil {
				slog.Warn("component=servers", "event", "public_counter_selection_persist_failed", "err", err.Error())
			}
		}
	}
	if a.WorkerManager.Running(serverID) {
		return nil
	}
	return a.WorkerManager.Start(ctx, serverID)
}

func (a *App) ownsPublicCounter(serverID int64) bool {
	a.counterOwnerMu.RLock()
	defer a.counterOwnerMu.RUnlock()
	return a.publicCounterServerID == serverID
}

func diagnosticTime(value time.Time) string {
	if value.IsZero() {
		return "NEVER OBSERVED"
	}
	return value.UTC().Format(time.RFC3339)
}

func diagnosticDuration(value time.Time) string {
	if value.IsZero() {
		return "NEVER"
	}
	duration := time.Since(value)
	if duration < 0 {
		duration = 0
	}
	return duration.Round(time.Second).String()
}

func selectPublicCounterServer(selectedID int64, active []repository.GameServer) (int64, bool) {
	if selectedID > 0 {
		for _, server := range active {
			if server.ID == selectedID {
				return selectedID, true
			}
		}
		return 0, false
	}
	if len(active) == 0 {
		return 0, false
	}
	return active[0].ID, true
}

// DisconnectServer implements discord.ServerRuntime: stops the worker, if any.
func (a *App) DisconnectServer(serverID int64) {
	if a.WorkerManager != nil {
		a.WorkerManager.Stop(serverID)
	}
}

// RepairServer implements discord.ServerRuntime: restarts the worker without
// the first-connect tail-start (a repair must not skip missed activity).
func (a *App) RepairServer(ctx context.Context, serverID int64) error {
	if a.WorkerManager == nil {
		return fmt.Errorf("killfeed runtime is not initialized (database required)")
	}
	if a.WorkerManager.Running(serverID) {
		return nil
	}
	return a.WorkerManager.Start(ctx, serverID)
}

// addPersistQueue registers a per-server persistence queue for health reporting
// and shutdown draining. Safe for concurrent use across worker goroutines.
func (a *App) addPersistQueue(pq *killfeed.PersistenceQueue) {
	if a == nil || pq == nil {
		return
	}
	a.persistQueuesMu.Lock()
	a.persistQueues = append(a.persistQueues, pq)
	a.persistQueuesMu.Unlock()
}

// allPersistQueues returns a snapshot copy of the currently known persistence
// queues (one per running server worker).
func (a *App) allPersistQueues() []*killfeed.PersistenceQueue {
	a.persistQueuesMu.Lock()
	defer a.persistQueuesMu.Unlock()
	out := make([]*killfeed.PersistenceQueue, len(a.persistQueues))
	copy(out, a.persistQueues)
	return out
}

// New creates an application instance with the required dependencies.
func New(ctx context.Context, cfg *config.Config) (*App, error) {
	slog.Info("component=startup", "msg", "starting DayZ killfeed")

	nitradoClient := nitrado.NewClient("https://api.nitrado.net", cfg.NitradoToken, nil)
	slog.Info("component=nitrado", "msg", "client configured", "base_url", nitradoClient.BaseURL())

	// Welcomer consumes GuildMemberAdd, so request only the Guild Members
	// privileged intent. Discord Developer Portal approval remains required.
	discordClient, err := discord.New(cfg.DiscordToken, true)
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
	app.HealthRegistry = health.NewRegistry()
	app.Workers = health.NewWorkerRegistry()
	app.ADMHealth = operations.NewADMMonitor()
	app.AdminService = admin.NewService(state, app.HealthRegistry)
	app.AdminService.SetWorkers(app.Workers)
	go app.refreshHealth(ctx)
	if cfg.CredentialEncryptionKey != "" {
		cipher, err := security.NewAESGCM(cfg.CredentialEncryptionKey, 1)
		if err != nil {
			return nil, fmt.Errorf("credential encryption configuration: %w", err)
		}
		app.CredentialCipher = cipher
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
			app.Events = repository.NewEventRepository(db.Pool)
			app.EventService = competitiveevents.NewService(app.Events)
			app.Announcements = repository.NewAnnouncementRepository(db.Pool)
			app.AnnouncementService = discord.NewCompletionAnnouncementService(app.Announcements)
			app.Bounties = repository.NewBountyRepository(db.Pool)
			app.Points = repository.NewPointsRepository(db.Pool)
			app.Seasons = repository.NewSeasonRepository(db.Pool)
			app.SeasonService = seasons.NewService(app.Seasons)
			app.Factions = repository.NewFactionRepository(db.Pool)
			app.Wars = repository.NewPostgresWarRepository(db.Pool)
			app.FactionStats = repository.NewFactionStatsRepository(db.Pool)
			app.FactionPresentation = repository.NewFactionPresentationRepository(db.Pool)
			app.Anomalies = repository.NewAnomalyRepository(db.Pool)
			app.AnalyticsRepository = repository.NewAnalyticsRepository(db.Pool)
			app.Servers = repository.NewServerRepository(db.Pool)
			app.WelcomeRepository = repository.NewWelcomeRepository(db.Pool)
			app.ActivityRepository = repository.NewActivityRepository(db.Pool)
			app.Links = repository.NewLinkRepository(db.Pool)
			app.LinkService = linking.NewService(app.Links, app.ActivityRepository, app.Servers, app.Links)
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
	if app.AdminService != nil {
		app.AdminService.SetLinkDiagnostics(func(diagCtx context.Context) map[string]any {
			result := map[string]any{
				"database":                      "ERROR",
				"player_repository":             "ERROR",
				"activity_repository":           "ERROR",
				"selected_server":               "NOT RESOLVED",
				"observed_players":              "unavailable",
				"last_player_connect_persisted": "unknown",
			}
			if app.DB == nil || app.Guilds == nil || app.Servers == nil || app.ActivityRepository == nil || app.Players == nil {
				return result
			}
			if pingErr := app.DB.Pool.Ping(diagCtx); pingErr != nil {
				return result
			}
			result["database"] = "CONNECTED"
			_, guildID, guildErr := app.Guilds.GetGuild(diagCtx, cfg.DiscordGuildID)
			if guildErr != nil || guildID == 0 {
				return result
			}
			if _, playerErr := app.Players.CountForGuild(diagCtx, guildID); playerErr == nil {
				result["player_repository"] = "HEALTHY"
			}
			serverID, serverErr := app.Servers.ConnectedServerID(diagCtx, guildID)
			if serverErr != nil {
				return result
			}
			result["selected_server"] = "RESOLVED"
			// observed_players must reflect the live running worker's tracker,
			// never a database row count, so it matches Online Players exactly.
			if liveCount, ok := app.livePresenceCount(serverID); ok {
				result["observed_players"] = liveCount
			}
			_, lastObserved, activityErr := app.ActivityRepository.Diagnostic(diagCtx, guildID, serverID)
			if activityErr != nil {
				return result
			}
			result["activity_repository"] = "HEALTHY"
			if lastObserved != nil {
				result["last_player_connect_persisted"] = lastObserved.UTC().Format(time.RFC3339)
			}
			return result
		})
	}

	_, cancel := context.WithCancel(ctx)
	app.cancel = cancel
	return app, nil
}

func (a *App) nitradoEnabled() bool {
	if a == nil || a.Config == nil {
		return false
	}
	return strings.TrimSpace(a.Config.NitradoToken) != ""
}

func nitradoClientFromConnection(cipher security.CredentialCipher, connection repository.NitradoConnection) (*nitrado.Client, error) {
	if cipher == nil {
		return nil, errors.New("credential encryption is not configured")
	}
	token, err := cipher.Decrypt(connection.Ciphertext, connection.Nonce, connection.KeyVersion)
	if err != nil {
		return nil, fmt.Errorf("decrypt Nitrado credential: %w", err)
	}
	if strings.TrimSpace(string(token)) == "" {
		return nil, errors.New("stored Nitrado credential is empty")
	}
	return nitrado.NewClient(nitrado.DefaultBaseURL, string(token), nil), nil
}

func (a *App) nitradoClientForServer(ctx context.Context, row repository.GameServer) (*nitrado.Client, error) {
	if a == nil || a.Servers == nil {
		return nil, errors.New("server repository is not initialized")
	}
	connection, err := a.Servers.GetConnection(ctx, row.GuildID)
	if err != nil {
		return nil, fmt.Errorf("load Nitrado credential for guild %d: %w", row.GuildID, err)
	}
	return nitradoClientFromConnection(a.CredentialCipher, *connection)
}

func (a *App) verifyNitrado(ctx context.Context) (authenticated, verified bool, game, serviceType, status string) {
	if !a.nitradoEnabled() {
		slog.Warn("component=nitrado", "msg", "NITRADO_TOKEN not configured; operating in degraded mode without live service verification")
		return false, false, "", "", ""
	}
	if err := a.Nitrado.AuthenticationCheck(ctx); err != nil {
		logNitradoFailure("authentication", err)
		slog.Warn("component=nitrado", "msg", "Nitrado verification unavailable; continuing in degraded mode so credentials can be repaired with /server connect")
		return false, false, "", "", ""
	}

	services, err := a.Nitrado.GetServices(ctx)
	if err != nil {
		logNitradoFailure("service discovery", err)
		slog.Warn("component=nitrado", "msg", "Nitrado service discovery unavailable; continuing in degraded mode")
		return true, false, "", "", ""
	}
	dayZServices := nitrado.FindDayZServices(services)
	if len(dayZServices) == 0 {
		slog.Warn("component=nitrado", "msg", "no DayZ services discovered")
	} else {
		slog.Info("component=nitrado", "msg", "DayZ services discovered", "count", len(dayZServices))
	}

	if a.Config.NitradoServiceID == "" {
		slog.Warn("component=nitrado", "msg", "NITRADO_SERVICE_ID not configured; skipping service verification and log discovery")
		return true, false, "", "", ""
	}
	service, err := a.Nitrado.ValidateServiceID(ctx, a.Config.NitradoServiceID, services)
	if err != nil {
		slog.Warn("component=nitrado", "operation", "service verification", "msg", "configured service could not be verified; continuing in degraded mode", "service_id", a.Config.NitradoServiceID, "err", err.Error())
		return true, false, "", "", ""
	}
	slog.Info("component=nitrado", "msg", "configured service verified",
		"service_id", a.Config.NitradoServiceID,
		"game", service.Game,
		"service_type", service.Type,
		"status", service.Status,
	)
	if err := a.Nitrado.InspectService(ctx, a.Config.NitradoServiceID); err != nil {
		slog.Warn("component=nitrado", "msg", "service payload inspection failed", "err", err.Error())
	}
	return true, true, service.Game, service.Type, service.Status
}

func bindOnlineCounter(store discord.SetupStore, guildID string, counter *discord.VoiceChannelCounter) {
	if store == nil || counter == nil || guildID == "" {
		return
	}
	setup, err := store.Get(guildID)
	if err == nil && setup != nil && setup.OnlinePlayersChannelID != "" {
		counter.SetChannelID(setup.OnlinePlayersChannelID)
	}
}

func (a *App) refreshHealth(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	update := func() {
		if a.HealthRegistry == nil || a.State == nil {
			return
		}
		snap := a.State.Snapshot()
		critical := func(key string) bool { v, _ := snap[key].(bool); return v }
		a.HealthRegistry.Set(health.Component{Name: "database", State: map[bool]health.State{true: health.Healthy, false: health.Unhealthy}[critical("database_connected")], Critical: true})
		a.HealthRegistry.Set(health.Component{Name: "discord", State: map[bool]health.State{true: health.Healthy, false: health.Degraded}[critical("discord_connected")]})
		a.HealthRegistry.Set(health.Component{Name: "nitrado", State: map[bool]health.State{true: health.Healthy, false: health.Degraded}[critical("nitrado_authenticated")]})
		a.HealthRegistry.Set(health.Component{Name: "adm_pipeline", State: map[bool]health.State{true: health.Healthy, false: health.Unhealthy}[critical("log_source_found")], Critical: true})
		if a.ADMHealth != nil {
			var poll, change time.Time
			if v, ok := snap["last_poll"].(string); ok {
				poll, _ = time.Parse(time.RFC3339, v)
			}
			if v, ok := snap["last_log_change"].(string); ok {
				change, _ = time.Parse(time.RFC3339, v)
			}
			online, _ := snap["online_players"].(int)
			file, _ := snap["log_filename"].(string)
			st, reason := a.ADMHealth.Evaluate(operations.ADMHealthSnapshot{LastPollSuccessAt: poll, LastChangeAt: change, OnlinePlayers: online, CurrentFile: file}, time.Now())
			a.HealthRegistry.Set(health.Component{Name: "adm_stall", State: st, Message: reason, Critical: st == health.Unhealthy})
		}
		for _, pq := range a.allPersistQueues() {
			depth, capacity, highWater, dropped, oldest := pq.QueueHealth()
			name := fmt.Sprintf("persistence_queue_%d", pq.ServerID())
			q := health.EvaluateQueue(health.QueueHealth{Name: name, Depth: depth, Capacity: capacity, HighWaterMark: highWater, Dropped: uint64(dropped), OldestAge: oldest})
			a.HealthRegistry.Set(health.Component{Name: name, State: q.State, Message: fmt.Sprintf("queue %d/%d high-water %d oldest %s", depth, capacity, highWater, oldest.Round(time.Second)), Critical: true})
		}
	}
	update()
	for {
		select {
		case <-ticker.C:
			update()
		case <-ctx.Done():
			return
		}
	}
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
	api := discord.NewSessionAPI(session)
	setupManager := discord.NewSetupManager(api, setupStore, a.Discord.BotID())
	if a.LinkService != nil && a.Config.DiscordGuildID != "" {
		verifiedRole := discord.NewVerifiedRoleAssigner(a.Discord, setupStore, a.Config.DiscordGuildID)
		a.LinkService.SetRoleAssigner(verifiedRole)
		a.LinkService.SetNotifier(verifiedRole)
	}
	setupHandler := discord.NewSetupHandler(setupManager, a.Guilds, a.WelcomeRepository)
	welcomeHandler := discord.NewPersistentWelcomeHandler(setupStore, a.WelcomeRepository, a.Guilds)
	if a.WelcomeRepository != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		welcomeCommands := discord.NewWelcomeCommandHandler(a.WelcomeRepository, a.Guilds, setupStore)
		if err := discord.RegisterWelcomeCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register welcome commands", "err", err.Error())
		} else {
			slog.Info("component=discord", "msg", "welcome commands registered")
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			if i.Type == discordgo.InteractionApplicationCommand && i.ApplicationCommandData().Name == "welcome" {
				welcomeCommands.Handle(s, i)
			}
		})
	}
	if a.AnalyticsRepository != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		analyticsHandler := discord.NewAnalyticsCommandHandler(a.AnalyticsRepository, a.Guilds)
		if err := discord.RegisterAnalyticsCommands(session, a.Config.DiscordGuildID, a.Config.DiscordApplicationID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register analytics commands", "err", err.Error())
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			if i.Type == discordgo.InteractionApplicationCommand && (i.ApplicationCommandData().Name == "matchup" || i.ApplicationCommandData().Name == "weapon") {
				analyticsHandler.Handle(s, i)
			}
		})
	}
	if a.AdminService != nil && a.Config.DiscordGuildID != "" {
		adminHandler := discord.NewAdminCommandHandler(a.AdminService, a.LinkService, a.Guilds)
		if err := discord.RegisterAdminCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register admin commands", "err", err.Error())
		} else {
			slog.Info("component=discord", "msg", "admin commands registered")
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			if i.Type == discordgo.InteractionApplicationCommand && i.ApplicationCommandData().Name == "admin" {
				adminHandler.Handle(s, i)
			}
		})
	}
	if a.Servers != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		serverHandler := discord.NewServerCommandHandler(a.Servers, a.Guilds, a.CredentialCipher, a)
		if err := discord.RegisterServerCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Error("component=discord", "msg", "failed to register server commands", "err", err.Error())
			requiredCommandsOK = false
		} else {
			slog.Info("component=discord", "msg", "server commands registered")
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			switch i.Type {
			case discordgo.InteractionApplicationCommand, discordgo.InteractionApplicationCommandAutocomplete:
				if i.ApplicationCommandData().Name == "server" {
					serverHandler.Handle(s, i)
				}
			case discordgo.InteractionModalSubmit:
				if strings.HasPrefix(i.ModalSubmitData().CustomID, "champion_server_") {
					serverHandler.Handle(s, i)
				}
			}
		})
	}
	if a.AnnouncementService != nil && a.Config.DiscordGuildID != "" {
		a.CompletionPublisher = discord.NewLiveCompletionPublisher(a.AnnouncementService, api, setupStore, a.Seasons, a.Wars, a.Events, a.Guilds, a.Config.DiscordGuildID)
	}
	if a.SeasonService != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		seasonHandler := discord.NewSeasonCommandHandler(a.SeasonService, a.Guilds)
		if err := discord.RegisterSeasonCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register season commands", "err", err.Error())
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			if i.Type == discordgo.InteractionApplicationCommand && i.ApplicationCommandData().Name == "season" {
				seasonHandler.Handle(s, i)
			}
		})
	}
	if a.EventService != nil && a.Events != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		eventHandler := discord.NewEventCommandHandler(a.EventService, a.Events, a.Guilds)
		if err := discord.RegisterEventCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register event commands", "err", err.Error())
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			if i.Type == discordgo.InteractionApplicationCommand && i.ApplicationCommandData().Name == "event" {
				eventHandler.Handle(s, i)
			}
		})
	}
	if a.Bounties != nil && a.Players != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		bountyHandler := discord.NewBountyCommandHandler(a.Bounties, a.Players, a.Guilds)
		if err := discord.RegisterBountyCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register bounty commands", "err", err.Error())
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			if i.Type == discordgo.InteractionApplicationCommand && i.ApplicationCommandData().Name == "bounty" {
				bountyHandler.Handle(s, i)
			}
		})
	}
	if a.Points != nil && a.Players != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		pointsHandler := discord.NewPointsCommandHandler(a.Points, a.Players, a.Guilds)
		if err := discord.RegisterPointsCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register points command", "err", err.Error())
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			if i.Type == discordgo.InteractionApplicationCommand && i.ApplicationCommandData().Name == "points" {
				pointsHandler.Handle(s, i)
			}
		})
	}
	if a.Wars != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		warHandler := discord.NewWarCommandHandler(a.Wars, a.Guilds, a.Seasons, a.Factions, a.Links, a.FactionStats, a.FactionPresentation)
		if err := discord.RegisterWarCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register faction war commands", "err", err.Error())
		}
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			if i.Type == discordgo.InteractionApplicationCommand && i.ApplicationCommandData().Name == "faction" {
				warHandler.Handle(s, i)
			}
		})
	}
	if a.Config.DiscordGuildID != "" {
		if err := discord.RegisterSetupCommand(session, a.Config.DiscordGuildID); err != nil {
			slog.Error("component=discord", "msg", "failed to register /setup command", "err", err.Error())
			requiredCommandsOK = false
		} else {
			slog.Info("component=discord", "msg", "setup commands registered")
		}
	}

	// Stats and leaderboard commands require the database + a guild record.
	if a.Stats != nil && a.Guilds != nil && a.Config.DiscordGuildID != "" {
		statsHandler := discord.NewStatsCommandHandler(a.Stats, a.Guilds, a.Config.DiscordGuildID)
		if err := discord.RegisterStatsCommands(session, a.Config.DiscordGuildID); err != nil {
			slog.Warn("component=discord", "msg", "failed to register stats commands", "err", err.Error())
		} else {
			slog.Info("component=discord", "msg", "stats commands registered")
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
			slog.Error("component=discord", "msg", "failed to register link commands", "err", err.Error())
			requiredCommandsOK = false
		} else {
			slog.Info("component=discord", "msg", "link commands registered")
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
	if a.Guilds != nil && (a.LinkService != nil || a.Stats != nil) && a.Config.DiscordGuildID != "" {
		publicPanels := discord.NewPublicPanelHandler(a.LinkService, a.Stats, a.Guilds)
		a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			switch i.Type {
			case discordgo.InteractionMessageComponent:
				customID := i.MessageComponentData().CustomID
				if strings.HasPrefix(customID, "champion:link:") || strings.HasPrefix(customID, "champion:stats:") {
					publicPanels.HandleComponent(s, i)
				}
			case discordgo.InteractionModalSubmit:
				customID := i.ModalSubmitData().CustomID
				if customID == "champion:link:modal:v1" || customID == "champion:stats:search:modal:v1" {
					publicPanels.HandleModal(s, i)
				}
			}
		})
	}
	a.Discord.AddMemberJoinHandler(welcomeHandler.HandleMemberJoin)

	a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			name := i.ApplicationCommandData().Name
			switch name {
			case "setup":
				setupHandler.Handle(s, i)
			}
		case discordgo.InteractionMessageComponent:
			setupHandler.HandleResetConfirm(s, i)
		}
	})

	// All interaction handlers are now installed; readiness must not be
	// reported before this point (see section 7/9 of the startup repair pass).
	slog.Info("component=discord", "msg", "interaction handlers ready", "required_commands_ok", requiredCommandsOK)
	state.SetHandlersReady(requiredCommandsOK)

	// --- Online players voice counter: renames the configured voice channel on
	// debounced count changes. Shared across servers (one voice channel per guild
	// today; per-server counters are a known gap, see Section 1 report). ---
	onlineCounter := discord.NewVoiceChannelCounter(api, "")
	onlineCounter.OnPublish(func(count int, result string) { a.recordPublicVoicePublish(count, result) })
	if cfg := setupStore; cfg != nil {
		if gs, err := cfg.Get(a.Config.DiscordGuildID); err == nil && gs != nil && gs.OnlinePlayersChannelID != "" {
			onlineCounter.SetChannelID(gs.OnlinePlayersChannelID)
		}
	}
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
			out["classification"] = classifyPresenceActual(snapshot, actualCount, actualKnown, true, true)
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
			if a.Stats != nil {
				if gs, gsErr := setupStore.Get(a.Config.DiscordGuildID); gsErr == nil && gs != nil && gs.LeaderboardsChannelID != "" {
					leaderboardPanel := discord.NewLeaderboardPanel(api, gs.LeaderboardsChannelID, gs.LeaderboardMessageID, discord.DefaultLeaderboardConfig())
					a.LeaderboardScheduler = discord.NewLeaderboardScheduler(leaderboardPanel, a.Stats, guildRowID, discord.DefaultLeaderboardConfig(), func(messageID string) {
						if latest, latestErr := setupStore.Get(a.Config.DiscordGuildID); latestErr == nil && latest != nil {
							latest.LeaderboardMessageID = messageID
							_ = setupStore.Save(*latest)
						}
					})
					go a.LeaderboardScheduler.Run(ctx)
					if a.AdminService != nil {
						a.AdminService.SetLeaderboardRefresh(func(refreshCtx context.Context) error {
							return a.LeaderboardScheduler.RefreshOnce(refreshCtx)
						})
					}
				}
			}
			store := &persistenceStoreAdapter{players: a.Players, kills: a.Kills, deaths: a.Deaths, seasons: a.Seasons, factions: a.Factions, wars: a.Wars, events: a.Events, bounties: a.Bounties, streaks: a.Streaks, anomalies: a.Anomalies, activity: a.ActivityRepository, servers: a.Servers, stats: a.Stats, analytics: a.AnalyticsRepository, panelDirty: func() {
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

			for _, row := range activeServers {
				if err := a.WorkerManager.Start(ctx, row.ID); err != nil {
					slog.Error("component=servers", "msg", "failed to start server worker", "server_id", row.ID, "err", err.Error())
					continue
				}
				slog.Info("component=servers", "msg", "server worker started", "server_id", row.ID, "display_name", row.DisplayName)
			}

			if a.EventService != nil || a.CompletionPublisher != nil {
				go a.runCompetitiveSchedulers(ctx, guildRowID)
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

	if err := a.HTTPServer.ListenAndServe(ctx); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("start HTTP server: %w", err)
	}

	<-ctx.Done()
	slog.Info("component=shutdown", "msg", "shutdown requested")
	a.shutdown()
	return nil
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
	rotatingFeedBatchSize = 10
)

func (a *App) runServerWorker(workerCtx context.Context, row repository.GameServer, store *persistenceStoreAdapter, setupStore discord.SetupStore, onlineCounter *discord.VoiceChannelCounter) error {
	workerName := fmt.Sprintf("adm_worker_%d", row.ID)
	defer func() {
		if a.Workers != nil {
			a.Workers.Stop(workerName)
		}
		a.unregisterPresenceTracker(row.ID)
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
	bindOnlineCounter(setupStore, a.Config.DiscordGuildID, onlineCounter)
	engine := killfeed.NewEngine(client, row.ProviderServiceID, killfeed.NewADMParser())
	engine.SetStateSink(a.State)
	engine.SetDiagnostics(killfeed.NewRuntimeDiagnostics(row.ID))
	a.registerPresenceEngine(row.ID, engine)
	if a.Checkpoints != nil {
		engine.SetDurableCheckpoint(&admCheckpointStoreAdapter{repo: a.Checkpoints}, row.GuildID, row.ID)
	}
	a.registerPresenceTracker(row.ID, engine.PlayerTracker())
	if a.consumeFirstConnect(row.ID) {
		engine.StartAtLogTail()
	}
	if a.Discord != nil && a.Servers != nil {
		if config, configErr := a.Servers.EnsureConfig(workerCtx, row.ID); configErr == nil {
			monitor := discord.NewADMMonitorPublisher(discord.NewSessionAPI(a.Discord.Session()), setupStore, a.Config.DiscordGuildID, row.ID, config.ADMMonitorMessageID, func(messageID string) {
				if err := a.Servers.SetADMMonitorMessage(context.Background(), row.ID, messageID); err != nil {
					slog.Warn("component=adm", "event", "monitor_message_save_failed", "server_id", row.ID, "err", err.Error())
				}
			})
			engine.OnAdmSnapshot(monitor.Update)
			engine.OnDownload(monitor.HandleDownload)
		}
	}

	publisher := discord.NewKillfeedPublisher(a.Discord, a.Config.KillfeedChannelID)
	publisher.BindStore(setupStore, a.Config.DiscordGuildID)
	engine.SetKillPublisher(publisher)

	deathPublisher := discord.NewDeathfeedPublisher(a.Discord, setupStore, a.Config.DiscordGuildID)
	engine.SetDeathPublisher(deathPublisher)

	killFeed := discord.NewRotatingFeed(a.Discord.Session(), setupStore, a.Config.DiscordGuildID, func(s *discord.GuildSetup) string { return s.KillfeedChannelID }, rotatingFeedInterval, rotatingFeedBatchSize)
	publisher.SetFeed(killFeed)
	deathFeed := discord.NewRotatingFeed(a.Discord.Session(), setupStore, a.Config.DiscordGuildID, func(s *discord.GuildSetup) string { return s.DeathChannelID }, rotatingFeedInterval, rotatingFeedBatchSize)
	deathPublisher.SetFeed(deathFeed)
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

	pq := killfeed.NewPersistenceQueueWithServerID(store, row.GuildID, row.ID, row.ProviderServiceID)
	pq.SetKillPostProcessor(store)
	pq.SetDeathPostProcessor(store)
	if a.LinkService != nil {
		pq.SetLinkChallengeObserver(a.LinkService)
	}
	engine.SetPersistence(pq)
	a.addPersistQueue(pq)

	engine.OnPlayersChanged(func(count int) {
		if !a.ownsPublicCounter(row.ID) {
			return
		}
		a.State.SetOnlinePlayers(count)
		if onlineCounter != nil {
			bindOnlineCounter(setupStore, a.Config.DiscordGuildID, onlineCounter)
			onlineCounter.Publish(count)
			_ = onlineCounter.Reconcile(count)
			a.State.SetOnlineCounter(onlineCounter.LastPublished(), onlineCounter.UpdateErrors(), onlineCounter.PermissionBlocked())
		}
	})
	if a.ownsPublicCounter(row.ID) && onlineCounter != nil {
		bindOnlineCounter(setupStore, a.Config.DiscordGuildID, onlineCounter)
		_ = onlineCounter.Reconcile(engine.PlayerTracker().OnlineCount())
	}

	if a.Workers != nil {
		a.Workers.Register(workerName)
		a.Workers.Heartbeat(workerName)
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

	slog.Info("component=servers", "msg", "server worker running", "server_id", row.ID, "display_name", row.DisplayName)
	err := engine.Start(workerCtx)
	pq.Close()
	<-queueDone
	if err != nil && a.Workers != nil {
		a.Workers.Error(workerName, err)
	}
	slog.Info("component=servers", "msg", "server worker stopped", "server_id", row.ID)
	return err
}

func (a *App) runCompetitiveSchedulers(ctx context.Context, guildID int64) {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	tick := func() {
		now := time.Now().UTC()
		if a.CompletionPublisher != nil {
			if err := a.CompletionPublisher.RecoverPending(ctx, guildID); err != nil {
				slog.Warn("component=announcements", "msg", "completion recovery failed", "err", err.Error())
			}
		}
		if a.EventService != nil {
			if err := a.EventService.SchedulerTick(ctx, now); err != nil {
				slog.Warn("component=events", "msg", "event scheduler tick failed", "err", err.Error())
			}
		}
		if ended, err := a.Events.GetEndedUnfinalized(ctx, guildID, 25); err == nil {
			for _, event := range ended {
				if err := a.EventService.FinalizeEvent(ctx, guildID, event.ID, now); err != nil {
					slog.Warn("component=events", "msg", "event finalization failed", "event_id", event.ID, "err", err.Error())
				} else if a.CompletionPublisher != nil {
					if err := a.CompletionPublisher.PublishPendingEventCompletion(ctx, guildID, event.ID); err != nil {
						slog.Warn("component=events", "msg", "event completion announcement failed", "event_id", event.ID, "err", err.Error())
					}
				}
			}
		}
		if a.Bounties != nil {
			if err := a.Bounties.Expire(ctx, now); err != nil {
				slog.Warn("component=bounty", "msg", "bounty expiry failed", "err", err.Error())
			}
		}
	}
	tick()
	for {
		select {
		case <-ticker.C:
			tick()
		case <-ctx.Done():
			return
		}
	}
}

func (a *App) shutdown() {
	if a.cancel != nil {
		a.cancel()
	}
	if a.WorkerManager != nil {
		a.WorkerManager.StopAll()
	}
	// Flush every per-server persistence queue (drain pending events) before
	// closing the DB. Each worker already closes its own queue when its context
	// is cancelled; this is a defensive second pass (Close is idempotent).
	queues := a.allPersistQueues()
	for _, pq := range queues {
		pq.Close()
	}
	if len(queues) > 0 {
		slog.Info("component=shutdown", "msg", "persistence queues drained", "count", len(queues))
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
	players    *repository.PlayerRepository
	kills      *repository.KillRepository
	deaths     *repository.DeathRepository
	seasons    *repository.SeasonRepository
	factions   *repository.FactionRepository
	wars       *repository.PostgresWarRepository
	events     *repository.EventRepository
	bounties   *repository.BountyRepository
	streaks    *repository.StreakRepository
	anomalies  *repository.AnomalyRepository
	activity   *repository.ActivityRepository
	servers    *repository.ServerRepository
	stats      *repository.StatsRepository
	analytics  *repository.AnalyticsRepository
	panelDirty func()
}

type admCheckpointStoreAdapter struct {
	repo *repository.CheckpointRepository
}

func (s *admCheckpointStoreAdapter) LoadADMCheckpoint(ctx context.Context, guildID, serverID int64) (*killfeed.DurableCheckpoint, error) {
	checkpoint, err := s.repo.LoadADMCheckpoint(ctx, guildID, serverID)
	if err != nil || checkpoint == nil {
		return nil, err
	}
	return &killfeed.DurableCheckpoint{Filename: checkpoint.Filename, RemoteModifiedAt: checkpoint.RemoteModifiedAt, RemoteSize: checkpoint.RemoteSize, ProcessedOffset: checkpoint.ProcessedOffset, PendingPartialLine: checkpoint.PendingPartialLine}, nil
}

func (s *admCheckpointStoreAdapter) SaveADMCheckpoint(ctx context.Context, guildID, serverID int64, sessionID string, checkpoint killfeed.DurableCheckpoint) error {
	return s.repo.SaveADMCheckpoint(ctx, repository.ADMCheckpoint{ServerID: serverID, SessionID: sessionID, Filename: checkpoint.Filename, RemoteModifiedAt: checkpoint.RemoteModifiedAt, RemoteSize: checkpoint.RemoteSize, ProcessedOffset: checkpoint.ProcessedOffset, PendingPartialLine: checkpoint.PendingPartialLine}, guildID)
}

func (p *persistenceStoreAdapter) RecordConnect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	if p.activity == nil || serverID == 0 {
		return nil
	}
	return p.activity.Connect(ctx, guildID, serverID, playerID, at)
}
func (p *persistenceStoreAdapter) RecordDisconnect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	if p.activity == nil || serverID == 0 {
		return nil
	}
	return p.activity.Disconnect(ctx, guildID, serverID, playerID, at)
}
func (p *persistenceStoreAdapter) CheckpointConnected(ctx context.Context, guildID, serverID int64, at time.Time) error {
	if p.activity == nil || serverID == 0 {
		return nil
	}
	return p.activity.CheckpointConnected(ctx, guildID, serverID, at)
}

func (p *persistenceStoreAdapter) UpsertPlayer(ctx context.Context, guildID int64, dayzID, displayName string, seenAt time.Time) (int64, error) {
	return p.players.UpsertPlayer(ctx, guildID, dayzID, displayName, seenAt)
}

func (p *persistenceStoreAdapter) InsertKill(ctx context.Context, k repository.KillRecord) error {
	return p.kills.InsertKill(ctx, k)
}

func (p *persistenceStoreAdapter) InsertKillReturning(ctx context.Context, k repository.KillRecord) (int64, error) {
	return p.kills.InsertKillReturning(ctx, k)
}

func (p *persistenceStoreAdapter) InsertDeath(ctx context.Context, d repository.DeathRecord) error {
	return p.deaths.InsertDeath(ctx, d)
}

func (p *persistenceStoreAdapter) ResolveDeathSeason(ctx context.Context, guildID int64, at time.Time) *int64 {
	if p.seasons == nil {
		return nil
	}
	s, err := p.seasons.ResolveAt(ctx, guildID, at)
	if err != nil || s == nil {
		return nil
	}
	return &s.ID
}

func (p *persistenceStoreAdapter) ResolveKillAttribution(ctx context.Context, guildID, killerID, victimID int64, at time.Time) (killerFactionID, victimFactionID, seasonID, warID *int64) {
	if p.seasons != nil {
		if s, err := p.seasons.ResolveAt(ctx, guildID, at); err == nil && s != nil {
			seasonID = &s.ID
		}
	}
	if p.factions == nil {
		return
	}
	k, kErr := p.factions.GetActiveFactionForPlayer(ctx, guildID, killerID)
	v, vErr := p.factions.GetActiveFactionForPlayer(ctx, guildID, victimID)
	if kErr == nil {
		killerFactionID = &k.FactionID
	}
	if vErr == nil {
		victimFactionID = &v.FactionID
	}
	if killerFactionID != nil && victimFactionID != nil && *killerFactionID != *victimFactionID && p.wars != nil {
		if w, err := p.wars.GetActivePair(ctx, guildID, *killerFactionID, *victimFactionID); err == nil && w != nil {
			warID = &w.ID
		}
	}
	return
}

func (p *persistenceStoreAdapter) ProcessPersistedKill(ctx context.Context, killID int64, record repository.KillRecord, ev *killfeed.Event) {
	if p.streaks == nil {
		return
	}
	streak, err := p.streaks.Increment(ctx, record.GuildID, record.KillerPlayerID)
	if err != nil {
		slog.Warn("component=killfeed", "msg", "streak processing failed", "err", err.Error())
		return
	}
	if record.VictimPlayerID > 0 {
		_ = p.streaks.Reset(ctx, record.GuildID, record.VictimPlayerID)
	}
	at := time.Now().UTC()
	if ev != nil && !ev.Timestamp.IsZero() {
		at = ev.Timestamp.UTC()
	}

	// Stat-rich embed fields: best-effort, never block the kill from
	// publishing. A guild without stats/analytics wired just gets an embed
	// without these sections (nil-checked in BuildKillEmbed).
	if ev != nil {
		streakCurrent := streak.Current
		ev.KillerStreak = &streakCurrent
		if p.stats != nil {
			if prof, statErr := p.stats.GetPlayerProfileByPlayerID(ctx, record.GuildID, record.KillerPlayerID); statErr == nil && prof != nil {
				ev.KillerStats = &killfeed.CombatRecord{Kills: prof.Kills, Deaths: prof.Deaths}
			}
			if record.VictimPlayerID > 0 {
				if prof, statErr := p.stats.GetPlayerProfileByPlayerID(ctx, record.GuildID, record.VictimPlayerID); statErr == nil && prof != nil {
					ev.VictimStats = &killfeed.CombatRecord{Kills: prof.Kills, Deaths: prof.Deaths}
				}
			}
		}
		if p.analytics != nil && ev.Killer != nil && ev.Victim != nil && ev.Killer.Name != "" && ev.Victim.Name != "" && ev.Killer.Name != ev.Victim.Name {
			if m, matchupErr := p.analytics.Matchup(ctx, record.GuildID, ev.Killer.Name, ev.Victim.Name, analytics.ScopeLifetime, 0); matchupErr == nil && m != nil {
				ev.Encounters = &killfeed.HeadToHead{KillerWins: m.AKills, VictimWins: m.BKills}
			}
		}
	}
	if p.anomalies != nil && record.KillerPlayerID > 0 && record.VictimPlayerID > 0 && record.KillerPlayerID != record.VictimPlayerID {
		if suspicious, anomalyErr := p.anomalies.ObservePair(ctx, record.GuildID, valueOfID(record.SeasonID), record.KillerPlayerID, record.VictimPlayerID, at); anomalyErr == nil && suspicious {
			slog.Debug("component=anti-farming", "msg", "repeated pair activity observed", "killer_player_id", record.KillerPlayerID, "victim_player_id", record.VictimPlayerID)
		}
	}

	if p.events != nil {
		var eventBadges []string
		if active, listErr := p.events.GetActiveEvents(ctx, record.GuildID); listErr == nil {
			for _, stored := range active {
				competitive := competitiveevents.Event{ID: stored.ID, GuildID: stored.GuildID, SeasonID: stored.SeasonID, Type: stored.Type, Name: stored.Name, Description: stored.Description, Status: stored.Status, StartsAt: stored.StartsAt, EndsAt: stored.EndsAt, Config: stored.Config}
				input := competitiveevents.KillInput{KillID: killID, KillerPlayerID: record.KillerPlayerID, VictimPlayerID: record.VictimPlayerID, KillerFactionID: record.KillerFactionID, VictimFactionID: record.VictimFactionID, WarID: record.WarID, WeaponDisplay: record.WeaponDisplay, Distance: record.Distance, Headshot: record.Headshot, Streak: streak.Current, EventTime: at}
				if score := competitiveevents.Qualify(competitive, input); score.Qualifies {
					if len(eventBadges) < 2 {
						eventBadges = append(eventBadges, "🔥 "+stored.Name)
					}
					if _, scoreErr := p.events.ScoreKill(ctx, stored.ID, killID, score.PlayerID, score.FactionID, score.Points, score.BestDistance, score.BestStreak); scoreErr != nil {
						slog.Warn("component=events", "msg", "event scoring failed", "event_id", stored.ID, "err", scoreErr.Error())
					}
				}
			}
		}
		if ev != nil {
			ev.ActiveEventBadges = eventBadges
		}
	}
	if ev != nil && record.WarID != nil {
		ev.WarBadge = "⚔️ FACTION WAR"
	}

	if p.bounties != nil && record.VictimPlayerID > 0 && record.KillerPlayerID != record.VictimPlayerID && (record.KillerFactionID == nil || record.VictimFactionID == nil || *record.KillerFactionID != *record.VictimFactionID) {
		if bounty, bountyErr := p.bounties.GetActiveAt(ctx, record.GuildID, record.VictimPlayerID, at); bountyErr == nil && bounty != nil {
			if ev != nil {
				ev.BountyTarget = false
			}
			claimed, claimErr := p.bounties.ClaimAndAward(ctx, record.GuildID, bounty.ID, record.KillerPlayerID, killID, valueOfID(record.SeasonID), at)
			if claimErr != nil {
				slog.Debug("component=bounty", "msg", "bounty claim not completed", "err", claimErr.Error())
			} else if ev != nil {
				ev.BountyClaimed = true
				ev.BountyPoints = claimed.RewardPoints
			}
		}
	}
	if p.bounties != nil {
		p.ensureAutomaticBounty(ctx, record, streak.Current, at)
		if ev != nil && record.KillerPlayerID > 0 {
			if bounty, err := p.bounties.GetActiveAt(ctx, record.GuildID, record.KillerPlayerID, at); err == nil && bounty != nil {
				ev.BountyTarget = true
			}
		}
	}
	if p.panelDirty != nil {
		p.panelDirty()
	}
}

// ProcessPersistedDeath fetches the deceased player's stats for the death
// embed. Best-effort: on failure ev.PlayerStats stays nil and the embed
// renders without that section (nil-checked in BuildDeathEmbed).
func (p *persistenceStoreAdapter) ProcessPersistedDeath(ctx context.Context, record repository.DeathRecord, ev *killfeed.Event) {
	if p.stats == nil || ev == nil || record.PlayerID == 0 {
		return
	}
	prof, err := p.stats.GetPlayerProfileByPlayerID(ctx, record.GuildID, record.PlayerID)
	if err != nil || prof == nil {
		return
	}
	ev.PlayerStats = &killfeed.CombatRecord{Kills: prof.Kills, Deaths: prof.Deaths}
}

type contentPanelEditor struct{ api *discord.SessionAPI }

func (e contentPanelEditor) Send(channelID, content string) (string, error) {
	m, err := e.api.ChannelMessageSendContent(channelID, content)
	if m == nil {
		return "", err
	}
	return m.ID, err
}
func (e contentPanelEditor) Edit(channelID, messageID, content string) error {
	_, err := e.api.ChannelMessageEditContent(channelID, messageID, content)
	return err
}

type competitivePanelLoader struct {
	guildID        int64
	events         *repository.EventRepository
	bounties       *repository.BountyRepository
	points         *repository.PointsRepository
	seasons        *repository.SeasonRepository
	guilds         *repository.GuildRepository
	discordGuildID string
}

func (l *competitivePanelLoader) Load(ctx context.Context) (panels.Snapshot, error) {
	if l.guildID == 0 {
		_, id, err := l.guilds.GetGuild(ctx, l.discordGuildID)
		if err != nil {
			return panels.Snapshot{}, err
		}
		l.guildID = id
	}
	out := panels.Snapshot{GeneratedAt: time.Now()}
	active, err := l.events.GetActiveEvents(ctx, l.guildID)
	if err != nil {
		return out, err
	}
	for _, event := range active {
		rows, _ := l.events.Leaderboard(ctx, event.ID, 3)
		line := fmt.Sprintf("%s (%s)", event.Name, event.Type)
		if len(rows) > 0 {
			line += fmt.Sprintf(" — %.0f", rows[0].Score)
		}
		out.EventLines = append(out.EventLines, line)
	}
	bounties, err := l.bounties.ListActive(ctx, l.guildID, 5)
	if err != nil {
		return out, err
	}
	for _, b := range bounties {
		out.BountyLines = append(out.BountyLines, fmt.Sprintf("Player %d — %d pts", b.TargetPlayerID, b.RewardPoints))
	}
	points, err := l.points.Leaderboard(ctx, l.guildID, true, 5)
	if err != nil {
		return out, err
	}
	for _, p := range points {
		out.PointLines = append(out.PointLines, p.DisplayName+" — "+p.Value)
	}
	return out, nil
}

func valueOfID(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func (p *persistenceStoreAdapter) ensureAutomaticBounty(ctx context.Context, record repository.KillRecord, streak int, at time.Time) {
	reward := bounties.RewardForStreak(streak)
	if reward == 0 || record.KillerPlayerID == 0 {
		return
	}
	if current, err := p.bounties.GetActive(ctx, record.GuildID, record.KillerPlayerID); err == nil && current != nil {
		if current.CreatedByType == repository.BountyAutomatic && int64(reward) > current.RewardPoints {
			_ = p.bounties.Upgrade(ctx, record.GuildID, current.ID, reward)
		}
		return
	}
	_, _ = p.bounties.Create(ctx, repository.Bounty{GuildID: record.GuildID, SeasonID: valueOfID(record.SeasonID), TargetPlayerID: record.KillerPlayerID, CreatedByType: repository.BountyAutomatic, RewardPoints: int64(reward), Reason: fmt.Sprintf("%d kill streak", streak), StartsAt: &at}, "")
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
