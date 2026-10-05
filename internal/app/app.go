package app

import (
	"context"
	"fmt"
	"golang.org/x/sync/singleflight"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/admin"
	"github.com/yourname/dayz-killfeed/internal/adminrepo"
	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/casebilling"
	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/discord/panels"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	competitiveevents "github.com/yourname/dayz-killfeed/internal/events"
	"github.com/yourname/dayz-killfeed/internal/factionassets"
	"github.com/yourname/dayz-killfeed/internal/factionstats"
	"github.com/yourname/dayz-killfeed/internal/featureflags"
	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/heatmap"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/leader"
	"github.com/yourname/dayz-killfeed/internal/linking"
	"github.com/yourname/dayz-killfeed/internal/livesync"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/operations"
	"github.com/yourname/dayz-killfeed/internal/owneraccess"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
	"github.com/yourname/dayz-killfeed/internal/seasons"
	"github.com/yourname/dayz-killfeed/internal/security"
	"github.com/yourname/dayz-killfeed/internal/server"
	"github.com/yourname/dayz-killfeed/internal/servers"
	"github.com/yourname/dayz-killfeed/internal/shop"
	"github.com/yourname/dayz-killfeed/internal/shop/canaryops"
)

// App owns the main runtime dependencies.
type App struct {
	Config     *config.Config
	Nitrado    *nitrado.Client
	Discord    *discord.Client
	HTTPServer *server.Server
	State      *server.State
	DB         *database.DB
	Guilds     *repository.GuildRepository
	Players    *repository.PlayerRepository
	Kills      *repository.KillRepository
	Deaths     *repository.DeathRepository
	Stats      *repository.StatsRepository
	Ranked     *repository.RankedRepository
	// Upgrades holds the automation switches and state (docs/FEATURE_UPGRADES.md).
	Upgrades *repository.UpgradeRepository
	// Challenges, BattlePass and Territory are progression (docs/PROGRESSION.md).
	Challenges *repository.ChallengeRepository
	BattlePass *repository.BattlePassRepository
	Territory  *repository.TerritoryRepository
	// UAV is the live map UAV players buy (docs/LIVE_MAP.md "UAV").
	UAV *repository.UAVRepository
	// MapRotation is map rotation with a player vote (docs/MAP_ROTATION.md). The three function
	// fields are the worker's doors to the outside (Nitrado, Discord, staff alerts); they are nil in
	// production, where the defaults apply, and set by tests.
	MapRotation          *repository.MapRotationRepository
	mapRotationRemoteFor func(ctx context.Context, t repository.MapRotationTarget) (mapRotationRemote, error)
	mapRotationPost      func(t repository.MapRotationTarget, channelID string, msg *discordgo.MessageSend) error
	mapRotationAlert     func(alert discord.AdminAlert)
	mapRotationRestarts  mapRotationRestartCache
	mapRotationWiping    sync.Map // installations whose saved characters are being cleared right now
	mapRotationWipeCfg   mapRotationWipeConfig
	upgradeRuns          upgradeThrottle
	rankedTagsCache      rankedTagCache
	forecasts            forecastCache
	// routePanels keeps one edited message per routed panel; nil when channel routing is off.
	routePanels  *discord.RoutePanels
	Sessions     *repository.SessionRepository
	Checkpoints  *repository.CheckpointRepository
	Streaks      *repository.StreakRepository
	Achievements *repository.AchievementRepository
	Events       *repository.EventRepository
	EventService *competitiveevents.Service
	// SeasonPlanner holds owner-scheduled stats season rollovers and Ranked resets.
	SeasonPlanner *repository.SeasonPlannerRepository
	// Invites stores Discord invite joins; InviteTracker attributes them (needs Manage Server).
	Invites       *repository.InviteRepository
	InviteTracker *discord.InviteTracker
	// Rewards holds automatic Champion Point reward rules.
	Rewards *repository.RewardRepository
	// PlayerTimeline merges one player's history for staff.
	PlayerTimeline *repository.PlayerTimelineRepository
	// StaffActivity reads the admin audit log as a who-did-what view.
	StaffActivity *repository.StaffActivityRepository
	// VIP holds supporter tiers; VIPRoles adds/removes their Discord roles.
	VIP      *repository.VIPRepository
	VIPRoles vipRoleAPI
	// VIPNotices sends a player the direct message that says they received a tier.
	VIPNotices vipDMAPI
	// Perks is the perk store (docs/PERK_STORE.md); perkAnnouncer replaces the Discord shout-out in tests.
	Perks         *repository.PerkStoreRepository
	perkAnnouncer func(repository.PerkPurchase, *discordgo.MessageEmbed)
	perkInline    bool // tests: make the donations channel before answering, not in the background
	// rpBoostAnnouncer replaces the double RP and other ranked Discord cards in tests.
	rpBoostAnnouncer func(serverID int64, embed *discordgo.MessageEmbed)
	Bounties         *repository.BountyRepository
	// BountyService is the bounty application service (placement, the atomic claim
	// for persisted kills, streak bounties, expiry). Its Discord notifier is
	// optional: bounties are correct without any route or Discord connection.
	BountyService *bounties.Service
	// EconomyService is the Champion Points economy (balances, ledger, credit/debit,
	// history, admin adjustments). Its Discord notifier is optional: the economy is
	// correct without any route or Discord connection.
	EconomyService *economy.Service
	// EconomyAccounts is the installation-scoped web API over the economy (player balance and
	// history, admin lookup and adjustments). It adds no storage; see docs/ECONOMY.md.
	EconomyAccounts *economy.Accounts
	// Shop is the Champion Shop (catalog, purchases with Champion Points); see docs/SHOP.md.
	Shop *shop.Service
	// ShopConfirmations is the buyer confirmation of delivered orders and its support tickets; see
	// docs/SHOP_ORDER_CONFIRMATION.md.
	ShopConfirmations *shop.Confirmations
	// shopConfirmationRepo and shopOrderDesk are the Discord side of that confirmation (the
	// delivered-order DM and the ticket channels); the desk exists once Discord is connected.
	shopConfirmationRepo *repository.ShopConfirmationRepository
	// ShopAuto and shopAttempts back the automatic delivery routes (the owner's switch, the buyer's
	// drop position, the staff review); the worker itself starts only behind config.ShopAutoDelivery.
	ShopAuto      *repository.ShopAutoDeliveryRepository
	shopAttempts  *repository.ShopAttemptRepository
	shopOrderDesk atomic.Pointer[shopOrderDesk]
	// ShopCanary is the Phase 2C.4 canary operator service (docs/SHOP_DELIVERY_PHASE2C4.md); its
	// mutations are locked unless ShopCanaryGate is opened by CHAMPION_SHOP_CANARY_EXECUTION.
	ShopCanary     *canaryops.Service
	ShopCanaryGate canaryops.Gate
	// Billing is the Champion Billing service (Stripe checkout, portal, webhooks); see
	// docs/BILLING.md. Nil-safe: registerBillingRoutes always assigns it, even with no
	// STRIPE_SECRET_KEY configured (Billing.Configured() is then false and every action fails
	// closed with BILLING_UNAVAILABLE rather than panicking).
	Billing *billing.Service
	// CaseDigestOutbox owns paid Watch staff messages across app replicas.
	CaseDigestOutbox *repository.CaseDigestOutbox
	// BountyBoard keeps the persistent public board (BOUNTY route). Nil-safe.
	BountyBoard *discord.BountyBoard
	// HeatmapBoard keeps the persistent PvP heatmap summary (HEATMAPS route),
	// read from the Phase 5 Heatmap service. Nil-safe.
	HeatmapBoard *discord.HeatmapBoard
	// AdminAlerts is the shared operational alert publisher (ADMIN_ALERTS
	// route): ADM stale, repeated Nitrado download failures, zone/UAV/base
	// radar intrusions. Nil-safe.
	AdminAlerts *discord.AdminAlertPublisher
	// ServerStatusBoard keeps the persistent server-status message
	// (SERVER_STATUS route). Nil-safe.
	ServerStatusBoard *discord.ServerStatusBoard
	// onlineCounter is the online-players voice counter; its channel comes
	// from the ONLINE_COUNTER route when one exists (legacy GuildSetup
	// otherwise).
	onlineCounter *discord.VoiceChannelCounter
	// onlineCounterRouted is set while the counter is bound to an
	// ONLINE_COUNTER route. The route is authoritative: the legacy
	// GuildSetup channel is only a fallback and must never override it
	// (a retired legacy channel that was deleted is exactly how the counter
	// ended up renaming an Unknown Channel on every presence change).
	onlineCounterRouted atomic.Bool
	// onlineLoop drives onlineCounter from the authoritative current player
	// count (online_counter.go); setupStore is the legacy channel fallback.
	onlineLoopOnce sync.Once
	onlineLoop     *onlineCounterLoop
	setupStore     discord.SetupStore
	// guildServers lists the configured guild's row id and active servers
	// (set once routing starts).
	guildServers func(ctx context.Context) (int64, []int64, error)
	// buildActionsSeen counts ADM build/placement actions parsed by any server
	// worker - the proof that some server's ADM carries BUILD_FEED lines.
	buildActionsSeen     atomic.Int64
	Points               *repository.PointsRepository
	Seasons              *repository.SeasonRepository
	SeasonService        *seasons.Service
	Factions             *repository.FactionRepository
	Wars                 *repository.PostgresWarRepository
	FactionStats         *repository.FactionStatsRepository
	FactionPresentation  *repository.FactionPresentationRepository
	Anomalies            *repository.AnomalyRepository
	Announcements        *repository.AnnouncementRepository
	AnnouncementService  *discord.CompletionAnnouncementService
	HealthRegistry       *health.Registry
	Workers              *health.WorkerRegistry
	WorkerManager        *servers.WorkerManager
	ADMHealth            *operations.ADMMonitor
	AdminService         *admin.Service
	AnalyticsRepository  *repository.AnalyticsRepository
	WelcomeRepository    *repository.WelcomeRepository
	ActivityRepository   *repository.ActivityRepository
	Servers              *repository.ServerRepository
	CredentialCipher     *security.AESGCM
	CompletionPublisher  *discord.LiveCompletionPublisher
	PanelService         *panels.RefreshService
	LeaderboardScheduler *discord.LeaderboardScheduler
	Links                *repository.LinkRepository
	LinkService          *linking.LinkVerificationService
	PresenceManager      *discord.PresenceManager

	// SaaS customer API dependencies (internal/app/saas_api*.go) - see
	// docs/SAAS_HTTP_API.md. SaaSServers/SaaSGuildConnections operate on the
	// same game_servers/guilds tables as Servers/Guilds above, just through
	// the organization-scoped lookups those don't provide.
	SaaSUsers            *repository.UserRepository
	SaaSOrganizations    *repository.OrganizationRepository
	SaaSGuildConnections *repository.GuildConnectionRepository
	SaaSServers          *repository.SaaSServerRepository
	SaaSInstallations    *repository.InstallationRepository
	SaaSSubscriptions    *repository.SubscriptionRepository
	SaaSCredentials      *repository.CredentialRepository
	SaaSChannelRoutes    *repository.ChannelRouteRepository
	// SaaSRetiredChannels records Champion-owned channels no route uses any
	// more (Channel System V2 cleanup). Nil-safe at every call site.
	SaaSRetiredChannels *repository.RetiredChannelRepository
	// SaaSPlayer backs the player-facing API (Champion Access Model Phase 2 Part A/B,
	// docs/PLAYER_API.md) - which installations a verified DayZ player is legitimately
	// associated with, and their per-installation stats. Read-only; never touched by ChannelRoutes.
	SaaSPlayer *repository.PlayerServerRepository
	// Client Admin Control Plane (Phase 1, docs/CLIENT_ADMIN.md): Discord-role -> Champion
	// permission-level mapping, the tenant admin audit log, and the shared scope/warnings/
	// server-name/access-list repository the new admin capability routes use.
	Permissions *repository.PermissionsRepository
	AdminAudit  *repository.AuditRepository
	ClientAdmin *repository.ClientAdminRepository
	// PlatformOwner is the Owner Hub write model (docs/ADMIN_API.md "Owner controls").
	PlatformOwner *repository.PlatformOwnerRepository
	// PlatformOps stores the Owner Hub operations state (docs/OWNER_OPS.md): automation
	// switches, fleet incidents, broadcasts and the daily briefing guard. The ownerOps* fields
	// are the monitor's in-memory grace tracking and its test seams.
	PlatformOps         *repository.PlatformOpsRepository
	ownerOpsMu          sync.Mutex
	ownerOpsSeen        map[string]time.Time
	ownerOpsReady       func() bool
	ownerOpsDM          func(discordUserID, text string) error
	ownerOpsChannelPost func(channelID, text string) error
	// feedWatch times, per server and in memory, how long players have been online and the log
	// silent (feed_watch.go). serverStatusFacts is the short cache behind GET .../admin/server-status.
	feedWatch           feedWatch
	serverStatusMu      sync.Mutex
	serverStatusFacts   map[int64]repository.FleetFact
	serverStatusFactsAt time.Time
	// discordReady replaces the gateway check in tests.
	discordReady func() bool
	// deploy is the start-up self-check (deploy_selfcheck.go).
	deploy deploySelfCheck
	// FeatureFlags resolves the owner's per-installation overrides of the env rollout switches.
	FeatureFlags *featureflags.Resolver
	// Locations backs Champion Phase 3 (docs/PLAYER_INTELLIGENCE.md): the authoritative player
	// directory and persisted ADM location-event history.
	Locations *repository.LocationRepository
	// LiveSync backs Champion Live Sync phase 2 (docs/CHAMPION_LIVE_SYNC.md): per-source
	// checkpoints and records of the RPT/script/crash/restart logs. liveSyncSupervisors holds each
	// running server's watcher supervisor for diagnostics.
	LiveSync            *repository.LiveSyncRepository
	liveSyncMu          sync.Mutex
	liveSyncSupervisors map[int64]*livesync.Supervisor
	// Zones/ZoneCache/Intrusion back Champion Phase 4 (docs/ZONES_UAV_RADAR.md): installation-scoped
	// geographic zones and the stateful UAV/Base Radar intrusion engine consuming Phase 3's location
	// events. ZoneCache is a single, process-wide, per-server cache (Invalidate is called by every
	// zone CRUD mutation in saas_api_zones.go); Intrusion is a single, process-wide engine instance -
	// every server's LocationQueue shares both, since neither carries per-server state of its own
	// (the cache keys internally by server id; the engine is stateless besides its metrics counters).
	Zones     *repository.ZoneRepository
	ZoneCache *killfeed.ZoneCache
	Intrusion *killfeed.IntrusionEngine
	// Lives records one row per ended life and derives lives in progress (docs/LIVES.md); LifeRecap
	// DMs the opt-in death recap. Both nil-safe.
	Lives     *repository.LifeRepository
	LifeRecap *discord.LifeRecapNotifier
	// Cards reads Champion Card figures and stores share links (docs/CHAMPION_CARD.md);
	// renderedCards caches rendered public cards.
	Cards         *repository.CardRepository
	renderedCards *renderedCardCache
	cardCacheOnce sync.Once
	// FeatureSettings stores each installation's opt-in feature settings (hot zones, public fight
	// replay, network listing, feed identity). hotZoneKills is the heatmap aggregation hot zones
	// are detected from; hotZoneChecked throttles that check per guild (docs/HOT_ZONES.md).
	FeatureSettings *repository.FeatureSettingsRepository
	hotZoneKills    hotZoneKillSource
	hotZoneMu       sync.Mutex
	hotZoneChecked  map[int64]time.Time
	// Network reads the opt-in cross-server directory and leaderboards (docs/NETWORK.md);
	// networkResponses caches its public responses.
	Network          *repository.NetworkRepository
	networkResponses *networkCache
	networkCacheOnce sync.Once
	// Fights reads kills and position samples for fight replays (docs/FIGHT_REPLAY.md).
	Fights *repository.FightRepository
	// LiveMap reads the live map (docs/LIVE_MAP.md). liveMapPublic caches the public responses,
	// liveMapNitradoCache the per-installation Nitrado clock facts and scheduled tasks, and
	// liveMapAudited throttles the staff view's audit rows; saasLiveMapLimiter throttles the
	// player faction layer per acting user.
	LiveMap             *repository.LiveMapRepository
	liveMapMu           sync.Mutex
	liveMapPublic       map[string]liveMapCacheEntry
	liveMapBuilds       singleflight.Group
	liveMapNitradoCache map[int64]liveMapNitradoFacts
	liveMapAudited      map[string]time.Time
	saasLiveMapLimiter  *saasRateLimiter
	// Retention reads the retention dashboard from the daily/hourly activity rollups (docs/RETENTION.md).
	Retention *repository.RetentionRepository
	// FeedIdentity posts feed messages under an installation's own name and avatar
	// (docs/FEED_IDENTITY.md). Nil-safe: without it every feed posts as the bot.
	FeedIdentity *discord.FeedIdentity
	// Heatmap backs Champion Phase 5 (docs/HEATMAPS.md): PvP kill/death, player-activity, and
	// zone-intrusion heatmap queries aggregated from Phase 3/4's persisted data. Independent of
	// the killfeed pipeline - a pure, cacheable read path, never wired into any worker goroutine.
	Heatmap *heatmap.Service
	// ChannelRoutes is the runtime feature -> Discord channel resolver
	// (internal/routing), a short-TTL cache over SaaSChannelRoutes. Nil-safe:
	// with no database, publishers simply use their legacy channel.
	ChannelRoutes *routing.Resolver
	// GuildRoutePanels durably records which message holds each routed panel
	// (LINK_GAMERTAG, STATS_LEADERBOARDS, AUTO_LEADERBOARD) in which channel.
	GuildRoutePanels *repository.GuildRoutePanelRepository
	// RouteSyncer keeps those guild-level routed artifacts in step with the
	// installation routes. Nil-safe: without it routes are simply not synced.
	RouteSyncer       *discord.RouteSyncer
	ServerRanksBoards []*discord.ServerRanksBoard
	// adminSaaS is the cross-tenant, read-only platform-admin read model behind
	// /api/admin (internal/adminrepo); adminChannelNames optionally overrides the
	// Discord-cache channel name lookup (tests).
	adminSaaS adminReader
	// adminRoutes is every pattern registered through adminHandle (the only way onto
	// /api/admin), in registration order. platformStaff optionally overrides the staff list
	// (tests); otherwise PlatformOwner is the staff list.
	adminRoutes   []string
	platformStaff platformStaffStore
	// OwnerAccess knows which organizations and installations belong to a platform owner
	// (docs/ADMIN_API.md "Platform owner access"). Nil when there is no database.
	OwnerAccess *owneraccess.Resolver
	// EmbedTemplates persists custom embed templates (storage + API only; no
	// publisher reads them - runtime rendering is not enabled).
	EmbedTemplates *embedtemplates.Service
	// EmbedActivations stores each installation's per-route Default/Custom selection.
	EmbedActivations embedActivationStore
	// EmbedRenderer renders saved custom templates at publish time (Embed Designer
	// Phase 4). It exists whenever the database does, but publishers are only wired to
	// it when CHAMPION_CUSTOM_EMBEDS_ENABLED is true; it is also the cache the template
	// save/reset handlers invalidate.
	EmbedRenderer       *embedrender.Renderer
	serverNames         *serverNameCache
	serverNamesOnce     sync.Once
	adminChannelNames   func(channelID string) string
	saasDiscordVerifier discordGuildVerifier
	// embedTestLimiter bounds Embed Designer test sends (per actor and per
	// installation). Nil allows everything (tests that do not exercise it).
	embedTestLimiter *embedTestLimiter
	// channelProducersOverride replaces channelRouteProducers' runtime audit
	// in tests. Nil in production.
	channelProducersOverride  func() map[string]routeProducer
	saasNitradoClientFactory  func(token string) *nitrado.Client
	saasSyncLimiter           *saasRateLimiter
	saasOrgCreateLimiter      *saasRateLimiter
	saasDiscordVerifyLimiter  *saasRateLimiter
	saasNitradoConnectLimiter *saasRateLimiter
	// FactionHub is the web-first Faction Hub store (docs/FACTIONS.md); the four
	// limiters throttle faction creation and join applications per acting user.
	FactionHub *repository.FactionHubRepository
	// factionRecruitAPI posts, edits and deletes faction recruitment cards (nil without Discord).
	factionRecruitAPI           recruitMessageAPI
	saasFactionCreateLimiter    *saasRateLimiter
	saasFactionCreateDayLimiter *saasRateLimiter
	saasFactionApplyLimiter     *saasRateLimiter
	saasFactionApplyDayLimiter  *saasRateLimiter
	// FactionAssets stores faction logos (Phase 4); the two limiters throttle uploads.
	FactionAssets *factionassets.Service
	// FactionHubStats derives faction competitive stats, achievements and activity (Phase 5).
	FactionHubStats           *factionstats.Service
	saasFactionLogoLimiter    *saasRateLimiter
	saasFactionLogoDayLimiter *saasRateLimiter
	saasEconomyAdjustLimiter  *saasRateLimiter
	saasEconomyHistoryLimiter *saasRateLimiter
	saasShopPurchaseLimiter   *saasRateLimiter
	saasShopAdminLimiter      *saasRateLimiter
	saasBillingActionLimiter  *saasRateLimiter
	saasPublicCatalogLimiter  *saasRateLimiter
	caseWatchDigestLimiter    *saasRateLimiter
	// Test seams; nil in production. Both callbacks fail closed by default.
	caseWatchPrivacyCheck   func(context.Context, string, string) error
	caseWatchRequesterCheck func(context.Context, repository.AdminScope, int64) (bool, error)
	caseWatchSender         func(context.Context, string, *discordgo.MessageEmbed) (string, error)
	caseWatchMessageLookup  func(context.Context, string, string) (*discordgo.Message, string, error)
	// saasAdminActionLimiter throttles the Client Admin Control Plane's higher-risk mutation
	// routes (restart/stop/whitelist/banlist/permission changes/etc); saasAdminReadLimiter
	// throttles its read routes (audit log, warnings list, permissions list).
	saasAdminActionLimiter *saasRateLimiter
	saasAdminReadLimiter   *saasRateLimiter
	// saasServerRestartLimiter/saasServerStopLimiter apply the task's own per-capability rate
	// limits ("restart: 1 per 5 minutes", "stop/start: 1 per minute") - tighter than the general
	// admin-action limiter above, since these are the highest-blast-radius live actions.
	saasServerRestartLimiter *saasRateLimiter
	saasServerStopLimiter    *saasRateLimiter
	// discordRoleCacheMu/discordRoleCache cache one actor's live Discord guild roles briefly
	// (discordRoleCacheTTL) so a burst of admin actions from the same person doesn't each cost a
	// separate Discord REST round trip - mirrors discord.Client.botGuildRoles' own
	// state-cache-first pattern, but for an arbitrary member rather than the bot itself.
	discordRoleCacheMu sync.Mutex
	discordRoleCache   map[string]discordRoleCacheEntry
	persistQueuesMu    sync.Mutex
	persistQueues      []*killfeed.PersistenceQueue
	// locationQueuesMu/locationQueues mirror persistQueues exactly, for the Phase 3 location-
	// history pipeline (docs/PLAYER_INTELLIGENCE.md) - one LocationQueue per running server worker.
	locationQueuesMu      sync.Mutex
	locationQueues        []*killfeed.LocationQueue
	rotatingFeedsMu       sync.Mutex
	rotatingFeeds         []*discord.RotatingFeed
	firstConnectMu        sync.Mutex
	firstConnectServers   map[int64]bool
	counterOwnerMu        sync.RWMutex
	publicCounterServerID int64
	presenceMu            sync.Mutex
	presenceTrackers      map[int64]*killfeed.PlayerTracker
	presenceEngines       map[int64]*killfeed.Engine
	cancel                context.CancelFunc

	// Leader elects the one process that runs the singleton background workers when several
	// processes share the database (singleton_leader.go). Nil always leads.
	Leader     *leader.Elector
	stopLeader func()
}

// New creates an application instance with the required dependencies.
func New(ctx context.Context, cfg *config.Config) (*App, error) {
	slog.Info("component=startup", "msg", "starting DayZ killfeed")

	if cfg.NitradoAPIBaseURL != "" {
		// Isolated staging only (config.Load refuses it unless APP_ENV=staging):
		// every Nitrado client in this process talks to the read-only fixture.
		nitrado.SetAPIBaseURLOverride(cfg.NitradoAPIBaseURL)
		slog.Warn("component=nitrado", "msg", "NITRADO_API_BASE_URL override active: using the staging Nitrado fixture, not the real Nitrado API")
	}
	nitradoClient := nitrado.NewClient(nitrado.DefaultBaseURL, cfg.NitradoToken, nil)
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
			// The self-check reports how many migrations the database has (deploy_selfcheck.go).
			countCtx, countCancel := context.WithTimeout(ctx, 5*time.Second)
			var applied int
			if err := db.Pool.QueryRow(countCtx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err == nil {
				app.deploy.setMigrations(applied)
			}
			countCancel()
			app.startLeaderElection()
			app.Guilds = repository.NewGuildRepository(db.Pool)
			app.Players = repository.NewPlayerRepository(db.Pool)
			app.Kills = repository.NewKillRepository(db.Pool)
			app.Deaths = repository.NewDeathRepository(db.Pool)
			app.Stats = repository.NewStatsRepository(db.Pool)
			app.Ranked = repository.NewRankedRepository(db.Pool)
			app.Upgrades = repository.NewUpgradeRepository(db.Pool)
			app.Challenges = repository.NewChallengeRepository(db.Pool)
			app.BattlePass = repository.NewBattlePassRepository(db.Pool)
			app.Territory = repository.NewTerritoryRepository(db.Pool)
			app.UAV = repository.NewUAVRepository(db.Pool)
			app.MapRotation = repository.NewMapRotationRepository(db.Pool)
			app.Sessions = repository.NewSessionRepository(db.Pool)
			app.Checkpoints = repository.NewCheckpointRepository(db.Pool)
			app.Streaks = repository.NewStreakRepository(db.Pool)
			app.Achievements = repository.NewAchievementRepository(db.Pool)
			app.Events = repository.NewEventRepository(db.Pool)
			app.EventService = competitiveevents.NewService(app.Events)
			app.Announcements = repository.NewAnnouncementRepository(db.Pool)
			app.AnnouncementService = discord.NewCompletionAnnouncementService(app.Announcements)
			app.Bounties = repository.NewBountyRepository(db.Pool)
			app.BountyService = bounties.NewService(app.Bounties, nil)
			app.BountyService.SetPlacerNotifier(app.notifyBountyPlacer)
			app.EconomyService = economy.NewService(repository.NewEconomyRepository(db.Pool), nil)
			app.EconomyAccounts = economy.NewAccounts(app.EconomyService, repository.NewEconomyRepository(db.Pool))
			app.Shop = shop.NewService(repository.NewShopRepository(db.Pool), app.EconomyAccounts, app.EconomyService)
			app.shopConfirmationRepo = repository.NewShopConfirmationRepository(db.Pool)
			app.ShopAuto = repository.NewShopAutoDeliveryRepository(db.Pool)
			app.shopAttempts = repository.NewShopAttemptRepository(db.Pool)
			app.ShopConfirmations = shop.NewConfirmations(app.shopConfirmationRepo, app.EconomyAccounts)
			app.Points = repository.NewPointsRepository(db.Pool)
			app.Seasons = repository.NewSeasonRepository(db.Pool)
			app.SeasonPlanner = repository.NewSeasonPlannerRepository(db.Pool)
			app.Invites = repository.NewInviteRepository(db.Pool)
			app.Rewards = repository.NewRewardRepository(db.Pool)
			app.PlayerTimeline = repository.NewPlayerTimelineRepository(db.Pool)
			app.StaffActivity = repository.NewStaffActivityRepository(db.Pool)
			app.VIP = repository.NewVIPRepository(db.Pool)
			app.Perks = repository.NewPerkStoreRepository(db.Pool)
			restoreCtx, restoreCancel := context.WithTimeout(ctx, 5*time.Second)
			nitrado.RestoreTailTrust(restoreCtx, repository.NewTailTrustRepository(db.Pool))
			restoreCancel()
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
			app.SaaSUsers = repository.NewUserRepository(db.Pool)
			app.SaaSOrganizations = repository.NewOrganizationRepository(db.Pool)
			app.ShopCanaryGate = canaryops.NewGate(cfg.ShopCanaryExecution.Enabled, cfg.ShopCanaryExecution.InstallationIDs).WithOverride(func(installationID int64) (bool, bool) {
				if app.FeatureFlags == nil {
					return false, false
				}
				ov := app.FeatureFlags.Overrides(installationID)
				v, ok := ov[featureflags.ShopCanary]
				return v, ok
			})
			app.ShopCanary = canaryops.New(repository.NewShopAttemptRepository(db.Pool), repository.NewShopRepository(db.Pool), app.SaaSOrganizations, app.EconomyAccounts, app.ShopCanaryGate)
			slog.Info("component=shop_canary", "execution_enabled", cfg.ShopCanaryExecution.Enabled, "installations", len(cfg.ShopCanaryExecution.InstallationIDs))
			app.SaaSGuildConnections = repository.NewGuildConnectionRepository(db.Pool)
			app.SaaSServers = repository.NewSaaSServerRepository(db.Pool)
			app.SaaSInstallations = repository.NewInstallationRepository(db.Pool)
			app.SaaSSubscriptions = repository.NewSubscriptionRepository(db.Pool)
			app.CaseDigestOutbox = repository.NewCaseDigestOutbox(db.Pool)
			app.SaaSPlayer = repository.NewPlayerServerRepository(db.Pool)
			if billingCatalog, err := billing.LoadCatalog(cfg.BillingPlansJSON); err != nil {
				// A malformed catalog is a startup-time configuration error (see
				// billing.LoadCatalog): refusing to start beats silently selling nothing, or the
				// wrong thing.
				return nil, fmt.Errorf("load billing plan catalog: %w", err)
			} else {
				var provider billing.Provider
				if cfg.StripeSecretKey != "" {
					provider = billing.NewStripeProvider(cfg.StripeSecretKey)
				}
				app.Billing = billing.NewService(app.SaaSSubscriptions, billingCatalog, provider, billing.Options{
					AllowedOrigins: billing.ParseAllowedOrigins(cfg.BillingAllowedOrigins), WebhookSecret: cfg.StripeWebhookSecret,
				})
				// Store and route C.A.S.E. independently of the one-row base
				// subscription. The checkout flag defaults false in every environment.
				if err := app.Billing.ConfigureCaseAddons(repository.NewCaseAddonSubscriptionRepository(db.Pool), billing.CaseOptions{
					Enabled:         cfg.CaseBillingEnabled,
					AccessEnabled:   cfg.CaseAccessEnabled,
					VerifiedThrough: casebilling.Tier(cfg.CaseVerifiedThrough),
					PriceIDs: map[casebilling.Tier]string{
						casebilling.Watch:   cfg.CaseWatchPriceID,
						casebilling.Pro:     cfg.CaseProPriceID,
						casebilling.Command: cfg.CaseCommandPriceID,
					},
					StripeKeyMode: billing.ClassifyStripeKey(cfg.StripeSecretKey),
					// The isolated staging service (APP_ENV=staging) must run on
					// a Stripe test key. Keyed on the explicit staging marker, not
					// "anything but production", so an unset APP_ENV elsewhere
					// can never block an existing deployment from starting.
					RequireTestMode: cfg.AppEnv == "staging",
				}); err != nil {
					return nil, fmt.Errorf("configure case add-on billing: %w", err)
				}
			}
			app.SaaSCredentials = repository.NewCredentialRepository(db.Pool)
			app.SaaSChannelRoutes = repository.NewChannelRouteRepository(db.Pool)
			app.SaaSRetiredChannels = repository.NewRetiredChannelRepository(db.Pool)
			app.Permissions = repository.NewPermissionsRepository(db.Pool)
			app.AdminAudit = repository.NewAuditRepository(db.Pool)
			app.PlatformOwner = repository.NewPlatformOwnerRepository(db.Pool)
			app.PlatformOps = repository.NewPlatformOpsRepository(db.Pool)
			go app.singleton(ctx, "owner_ops", app.runOwnerOps)
			go app.runFeedWatch(ctx)
			// Platform owner access: an organization owned by an account on
			// CHAMPION_ADMIN_DISCORD_IDS has every plan feature and its feature switches
			// default to on. Loaded before anything asks a plan or flag question.
			app.OwnerAccess = owneraccess.New(app.PlatformOwner, cfg.AdminDiscordIDs, owneraccess.DefaultTTL)
			ownerCtx, ownerCancel := context.WithTimeout(ctx, 5*time.Second)
			if err := app.OwnerAccess.Refresh(ownerCtx); err != nil {
				slog.Warn("component=owneraccess", "msg", "initial load failed; no organization has owner access until the next refresh", "err", err.Error())
			}
			ownerCancel()
			entitlements.SetOwnerOrganizations(app.OwnerAccess.Organization)
			app.FeatureFlags = featureflags.New(app.PlatformOwner, featureflags.DefaultTTL)
			app.FeatureFlags.SetOwnerInstallations(app.OwnerAccess.Installation)
			caseFlags = app.FeatureFlags
			flagCtx, flagCancel := context.WithTimeout(ctx, 5*time.Second)
			if err := app.FeatureFlags.Refresh(flagCtx); err != nil {
				slog.Warn("component=featureflags", "msg", "initial load failed; env defaults apply until the next refresh", "err", err.Error())
			}
			flagCancel()
			optinCtx, optinCancel := context.WithTimeout(ctx, 5*time.Second)
			loadCaseEvidenceOptins(optinCtx, repository.NewCaseEvidenceOptinRepository(db.Pool))
			optinCancel()
			app.ClientAdmin = repository.NewClientAdminRepository(db.Pool)
			app.Locations = repository.NewLocationRepository(db.Pool)
			app.Lives = repository.NewLifeRepository(db.Pool)
			app.Cards = repository.NewCardRepository(db.Pool)
			app.FeatureSettings = repository.NewFeatureSettingsRepository(db.Pool)
			app.Retention = repository.NewRetentionRepository(db.Pool)
			app.Fights = repository.NewFightRepository(db.Pool)
			app.Network = repository.NewNetworkRepository(db.Pool)
			app.LiveMap = repository.NewLiveMapRepository(db.Pool)
			app.hotZoneKills = repository.NewHeatmapRepository(db.Pool)
			app.FeedIdentity = discord.NewFeedIdentity(app.FeatureSettings)
			go app.runLocationRetention(ctx)
			app.LiveSync = repository.NewLiveSyncRepository(db.Pool)
			go app.runLiveSyncRetention(ctx)
			go app.runDataRetention(ctx)
			app.Zones = repository.NewZoneRepository(db.Pool)
			app.ZoneCache = killfeed.NewZoneCache(app.Zones)
			app.Intrusion = killfeed.NewIntrusionEngine(app.Zones, app.ZoneCache, intrusionRoleChecker{app: app}, intrusionPublisher{app: app})
			app.Heatmap = heatmap.NewService(repository.NewHeatmapRepository(db.Pool), heatmap.NewCache(heatmapCacheTTL), heatmap.NewMetrics())
			app.adminSaaS = adminrepo.New(db.Pool)
			embedRepo := repository.NewEmbedTemplateRepository(db.Pool)
			app.EmbedTemplates = embedtemplates.NewService(embedRepo)
			app.EmbedActivations = embedRepo
			app.FactionHub = repository.NewFactionHubRepository(db.Pool)
			app.FactionAssets = factionassets.NewService(repository.NewPostgresAssetStore(db.Pool), app.FactionHub)
			app.FactionHubStats = factionstats.NewService(repository.NewHubStatsRepository(db.Pool), factionstats.Options{})
			entitlements.SetEnforced(cfg.PlanGatingEnabled)
			if cfg.PlanGatingEnabled {
				slog.Info("component=entitlements", "event", "plan_gating_enabled")
			}
			// The renderer is always wired; whether an installation's templates render is decided
			// per installation (customEmbedsFor: owner override, else CHAMPION_CUSTOM_EMBEDS_ENABLED).
			app.EmbedRenderer = embedrender.New(embedrender.Options{Source: embedRepo, Enabled: true, Gate: app.customEmbedsFor})
			if cfg.CustomEmbedsEnabled {
				slog.Info("component=embedrender", "event", "custom_embeds_enabled")
			}
			app.ChannelRoutes = routing.NewResolver(app.SaaSChannelRoutes, routing.DefaultTTL)
			app.GuildRoutePanels = repository.NewGuildRoutePanelRepository(db.Pool)
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

	app.registerRuntimeStatusAPI()
	app.registerSaaSAPI()
	app.registerAdminAPI()

	_, cancel := context.WithCancel(ctx)
	app.cancel = cancel
	return app, nil
}

// shutdownBudget bounds the part of a shutdown that can wait on other systems (stopping the
// workers, flushing the feeds to Discord). Railway sends SIGTERM and kills the container
// `drainingSeconds` later (30 in production, docs/DEPLOY.md): the budget leaves room to close
// Discord and the database inside that window instead of being cut off mid-flush.
const (
	shutdownBudget     = 20 * time.Second
	shutdownWorkerStop = 12 * time.Second
)

func (a *App) shutdown() {
	started := time.Now()
	deadline := started.Add(shutdownBudget)
	if a.cancel != nil {
		a.cancel()
	}
	// Give the leader lock up first, so a process standing by takes the singleton work over while
	// this one is still flushing. The singleton workers here already stopped with Run's context.
	if a.stopLeader != nil {
		a.stopLeader()
	}
	if a.WorkerManager != nil {
		// The process is exiting: a worker that does not stop in time is left behind rather than
		// waited on for the manager's usual 30 seconds.
		a.WorkerManager.SetStopTimeout(shutdownWorkerStop)
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
	// Wait for every rotating killfeed/death-feed to finish its on-shutdown
	// flush (see RotatingFeed.Run) before closing Discord, so a redeploy never
	// silently drops whatever was enqueued since the last 10-minute cycle.
	feeds := a.allRotatingFeeds()
	if len(feeds) > 0 {
		if waitFeedsFlushed(feeds, time.Until(deadline)) {
			slog.Info("component=shutdown", "msg", "rotating feeds flushed", "count", len(feeds))
		} else {
			// Immediate-mode cards are journalled and replayed by the next process; a rotating
			// batch that did not go out is the loss (the events themselves are stored).
			slog.Warn("component=shutdown", "msg", "rotating feeds did not finish flushing before the shutdown deadline", "count", len(feeds))
		}
	}
	if a.HTTPServer != nil {
		httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelHTTP()
		if err := a.HTTPServer.Shutdown(httpCtx); err != nil {
			slog.Error("component=shutdown", "msg", "HTTP server shutdown failed", "err", err.Error())
		} else {
			slog.Info("component=shutdown", "msg", "HTTP server stopped")
		}
	}
	if a.PresenceManager != nil {
		// Stop blocks until the rotation/health goroutine has fully exited,
		// so it can never race a presence update against the session closing
		// right below - no goroutine leak, no use-after-close.
		a.PresenceManager.Stop()
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
	slog.Info("component=shutdown", "msg", "shutdown complete", "duration_ms", time.Since(started).Milliseconds())
	time.Sleep(50 * time.Millisecond)
}

// feedFlusher is the part of a rotating feed a shutdown waits on.
type feedFlusher interface{ WaitDone() }

// waitFeedsFlushed waits for every feed's final flush, but no longer than limit (at least two
// seconds, so a slow worker stop does not take the flush's whole share). It reports whether all
// of them finished.
func waitFeedsFlushed[F feedFlusher](feeds []F, limit time.Duration) bool {
	if limit < 2*time.Second {
		limit = 2 * time.Second
	}
	done := make(chan struct{})
	go func() {
		for _, f := range feeds {
			f.WaitDone()
		}
		close(done)
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
