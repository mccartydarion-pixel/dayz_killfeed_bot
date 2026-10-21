package database

// migrations0065to0125 holds migrations 0065 through 0125, in execution order. It is one fragment of the
// registry assembled in migrations.go; never edit an applied migration.
var migrations0065to0125 = []Migration{
	{
		// P0 2026-09-26 Verified-role reconciliation (docs/ONLINE_COUNTER_AND_LINK_CHECK.md). Additive,
		// nullable columns only; no row is rewritten. Existing VERIFIED links keep role_sync_status NULL
		// (never reconciled automatically - their role state predates tracking); links verified from
		// now on are PENDING until Discord confirms the role, so a failed assignment survives restarts.
		Name: "0065_player_link_role_sync",
		SQL:  PlayerLinkRoleSyncSQL,
	},
	{
		// P0 2026-09-26 immediate killfeed journal (docs/incidents/2026-09-26-staging-infrastructure.md).
		// New table only; no existing row is read or rewritten. Written only by feeds running
		// KILLFEED_DELIVERY_MODE=immediate, so it stays empty under the production default.
		Name: "0066_discord_feed_cards",
		SQL:  DiscordFeedCardsSQL,
	},
	{
		// Ranked RP storage only. No runtime awards or public rank source are wired.
		Name: "0067_ranked_ledger_foundation",
		SQL:  RankedLedgerFoundationSQL,
	},
	{
		// Champion Shop: the attempt ledger's artifact_path may also be the custom/ location
		// (docs/SHOP_CUSTOM_RELOCATION.md). Forward-only CHECK widening; no row is read or rewritten.
		Name: "0068_shop_delivery_attempt_artifact_path",
		SQL:  ShopAttemptArtifactPathSQL,
	},
	{
		// Inert Core Eight Base Boost registration; no detector reader or notifier.
		Name: "0070_case_base_registration",
		SQL:  CASEBaseRegistrationSQL,
	},
	{
		// Inert owner preferences; no detector reads or release flags.
		Name: "0071_case_detector_settings",
		SQL:  CASEDetectorSettingsSQL,
	},
	{
		// Base Raid Alarm: owner switch (off by default) and alarm log. Additive only.
		Name: "0072_base_raid_alarm",
		SQL:  BaseRaidAlarmSQL,
	},
	{
		// Faction Hub: real DayZ flag catalog; flag and armband exclusive per installation.
		Name: "0073_faction_branding_exclusive",
		SQL:  FactionBrandingExclusiveSQL,
	},
	{
		// Faction Hub: one recruitment card per faction in the FACTION_RECRUITMENT channel.
		Name: "0074_faction_recruitment",
		SQL:  FactionRecruitmentSQL,
	},
	{
		// Owner Hub controls: platform audit log, user bans, installation suspension,
		// owner-granted plans. Additive only.
		Name: "0075_owner_controls",
		SQL:  OwnerControlsSQL,
	},
	{
		// Owner Hub feature flags: per-installation overrides of the env rollout switches.
		Name: "0076_feature_flags",
		SQL:  FeatureFlagsSQL,
	},
	{
		Name: "0077_player_lives_and_daily_activity",
		SQL: `
-- Lives and retention collection (docs/LIVES.md, docs/RETENTION.md). Additive.
-- Neither can be backfilled: location events are deleted after the retention window and
-- player_server_activity keeps only running totals, so both are collected from here on.
--
-- player_lives: one row per ended life, written when a "died" ADM line is durably persisted.
-- playtime_seconds is observed playtime inside the life (the difference between two
-- player_server_activity.total_observed_seconds marks); NULL means the life began before this table
-- existed, so its playtime was never marked and is not guessed. tracked_distance_m sums the straight
-- lines between the life's persisted location samples - a lower bound, never a path.
CREATE TABLE IF NOT EXISTS player_lives (
    id BIGSERIAL PRIMARY KEY,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    season_id BIGINT REFERENCES seasons(id) ON DELETE SET NULL,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ NOT NULL,
    playtime_seconds BIGINT CHECK (playtime_seconds IS NULL OR playtime_seconds >= 0),
    playtime_mark BIGINT NOT NULL DEFAULT 0,
    kills INTEGER NOT NULL DEFAULT 0,
    headshots INTEGER NOT NULL DEFAULT 0,
    longest_kill_m DOUBLE PRECISION,
    tracked_distance_m DOUBLE PRECISION,
    location_samples INTEGER NOT NULL DEFAULT 0,
    cause TEXT NOT NULL CHECK (cause IN ('PVP','SUICIDE','OTHER')),
    killer_player_id BIGINT REFERENCES players(id) ON DELETE SET NULL,
    kill_id BIGINT REFERENCES kills(id) ON DELETE SET NULL,
    weapon TEXT,
    distance_m DOUBLE PRECISION,
    death_fingerprint TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_player_lives_death UNIQUE (guild_id, death_fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_player_lives_player ON player_lives(server_id, player_id, ended_at DESC);
CREATE INDEX IF NOT EXISTS idx_player_lives_longest ON player_lives(guild_id, server_id, playtime_seconds DESC) WHERE playtime_seconds IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_player_lives_deadliest ON player_lives(guild_id, server_id, kills DESC);

-- player_daily_activity: one row per player, server and UTC day the player was observed on.
-- observed_seconds mirrors what player_server_activity adds to its running total; a row with zero
-- seconds still proves presence that day. source BACKFILL rows are presence-only days recovered once
-- from kills/deaths/location events and never carry seconds.
CREATE TABLE IF NOT EXISTS player_daily_activity (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    day DATE NOT NULL,
    observed_seconds BIGINT NOT NULL DEFAULT 0,
    sessions INTEGER NOT NULL DEFAULT 0,
    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    source TEXT NOT NULL DEFAULT 'OBSERVED' CHECK (source IN ('OBSERVED','BACKFILL')),
    PRIMARY KEY (server_id, player_id, day)
);
CREATE INDEX IF NOT EXISTS idx_player_daily_activity_day ON player_daily_activity(server_id, day);

-- server_hourly_activity: concurrency per server and hour, fed by the 30-second presence checkpoint.
CREATE TABLE IF NOT EXISTS server_hourly_activity (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    hour TIMESTAMPTZ NOT NULL,
    peak_players INTEGER NOT NULL DEFAULT 0,
    player_seconds BIGINT NOT NULL DEFAULT 0,
    samples INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (server_id, hour)
);
`,
	},
	{
		Name: "0078_player_recap_prefs",
		SQL: `
-- Death recap DMs (docs/LIVES.md). Opt-in per Discord user and guild: no row, or death_recap FALSE,
-- means the bot never DMs that player. Only a VERIFIED link is ever resolved to a recipient.
CREATE TABLE IF NOT EXISTS player_recap_prefs (
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    discord_user_id TEXT NOT NULL,
    death_recap BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (guild_id, discord_user_id)
);
`,
	},
	{
		Name: "0079_player_card_shares",
		SQL: `
-- Champion Card share links (docs/CHAMPION_CARD.md). A player creates a link for their own card on
-- one installation and can revoke it; the token is the only address of the public image. At most
-- one active link per player and installation.
CREATE TABLE IF NOT EXISTS player_card_shares (
    token TEXT PRIMARY KEY,
    installation_id BIGINT NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    guild_id BIGINT NOT NULL REFERENCES guilds(id) ON DELETE CASCADE,
    server_id BIGINT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    player_id BIGINT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_player_card_shares_active ON player_card_shares(installation_id, player_id) WHERE revoked_at IS NULL;
`,
	},
	{
		Name: "0080_installation_feature_settings",
		SQL: `
-- Opt-in feature settings, one row per installation (no row = every feature off, default tuning).
-- Hot zones (docs/HOT_ZONES.md), public fight replay (docs/FIGHT_REPLAY.md), the cross-server
-- network listing (docs/NETWORK.md) and the feed identity (docs/FEED_IDENTITY.md).
CREATE TABLE IF NOT EXISTS installation_feature_settings (
    installation_id BIGINT PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
    hot_zones_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    hot_zone_window_minutes INTEGER NOT NULL DEFAULT 60 CHECK (hot_zone_window_minutes BETWEEN 15 AND 360),
    hot_zone_min_kills INTEGER NOT NULL DEFAULT 6 CHECK (hot_zone_min_kills BETWEEN 2 AND 500),
    hot_zone_radius_m INTEGER NOT NULL DEFAULT 500 CHECK (hot_zone_radius_m BETWEEN 100 AND 2000),
    hot_zone_duration_minutes INTEGER NOT NULL DEFAULT 30 CHECK (hot_zone_duration_minutes BETWEEN 5 AND 240),
    hot_zone_cooldown_minutes INTEGER NOT NULL DEFAULT 120 CHECK (hot_zone_cooldown_minutes BETWEEN 0 AND 1440),
    hot_zone_first_points INTEGER NOT NULL DEFAULT 500 CHECK (hot_zone_first_points BETWEEN 0 AND 1000000),
    hot_zone_second_points INTEGER NOT NULL DEFAULT 250 CHECK (hot_zone_second_points BETWEEN 0 AND 1000000),
    hot_zone_third_points INTEGER NOT NULL DEFAULT 100 CHECK (hot_zone_third_points BETWEEN 0 AND 1000000),
    fight_replay_public BOOLEAN NOT NULL DEFAULT FALSE,
    fight_replay_delay_minutes INTEGER NOT NULL DEFAULT 60 CHECK (fight_replay_delay_minutes BETWEEN 0 AND 10080),
    network_listed BOOLEAN NOT NULL DEFAULT FALSE,
    network_description TEXT NOT NULL DEFAULT '',
    feed_identity_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    feed_identity_name TEXT NOT NULL DEFAULT '',
    feed_identity_avatar_url TEXT NOT NULL DEFAULT '',
    updated_by_user_id BIGINT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_installation_feature_settings_network ON installation_feature_settings(installation_id) WHERE network_listed;
-- Hot-zone events are found by the server their config names.
CREATE INDEX IF NOT EXISTS idx_competitive_events_hot_zone ON competitive_events(guild_id, ((config->>'server_id')), ends_at DESC) WHERE event_type = 'HOT_ZONE';
`,
	},
	{
		// A PvP kill is the victim's death, but only "died" lines ever wrote a deaths row, so every
		// deaths figure left PvP deaths out (docs/DEATH_COUNTS.md). From here on the kill insert
		// writes the victim's PVP death row; this gives every existing kill one.
		Name: "0081_pvp_death_rows",
		SQL:  PvPDeathBackfillSQL,
	},
	{
		// C.A.S.E. staff alerts: owner switch (off by default) and delivery queue. Additive only.
		Name: "0082_case_staff_alerts",
		SQL:  CASEStaffAlertSQL,
	},
	{
		// Security Marketplace: owner offer (off by default) and player purchases paid
		// from the existing Champion Points ledger. Additive only.
		Name: "0083_security_service_sales",
		SQL:  SecurityServiceSQL,
	},
	{
		// The owner's own Discord invite, shown on the public network listing (docs/NETWORK.md).
		Name: "0084_network_discord_invite",
		SQL:  `ALTER TABLE installation_feature_settings ADD COLUMN IF NOT EXISTS network_discord_invite_url TEXT NOT NULL DEFAULT '';`,
	},
	{
		// C.A.S.E. staff verdicts on test-run findings. Additive only.
		Name: "0085_case_shadow_verdicts",
		SQL:  CASEShadowVerdictSQL,
	},
	{
		// Perimeter Watch: owner switch (off by default), alert log, and letting the
		// Security Marketplace sell it. Additive; widens two service_id checks.
		Name: "0086_perimeter_watch",
		SQL:  PerimeterWatchSQL,
	},
	{
		Name: "0087_shop_order_confirmations",
		SQL:  ShopOrderConfirmationsSQL,
	},
	{
		Name: "0088_shop_order_confirmation_discord",
		SQL:  ShopOrderConfirmationDiscordSQL,
	},
	{
		// Base Black Box: per-base history of nearby players and dismantles, off by
		// default and sellable. Additive; widens two service_id checks.
		Name: "0089_base_black_box",
		SQL:  BaseBlackBoxSQL,
	},
	{
		// Faction Security: raid and perimeter messages also go to the base owner's
		// faction. Off by default, sellable. Additive; widens two service_id checks.
		Name: "0090_faction_security",
		SQL:  FactionSecuritySQL,
	},
	{
		// Base registration requests from players, approved or declined by the
		// server owner. Additive (one table).
		Name: "0091_case_base_requests",
		SQL:  CaseBaseRequestSQL,
	},
	{
		// Sentinel Pro bundle: widens the Security Marketplace service checks.
		Name: "0092_sentinel_pro",
		SQL:  SentinelProSQL,
	},
	{
		// Security Store panel: the Discord channel and message the bot keeps
		// up to date with what's on sale. Additive (one table).
		Name: "0093_security_store_panel",
		SQL:  SecurityStorePanelSQL,
	},
	{
		// Gifted paid time: price 0, no ledger entry, the giving owner recorded.
		Name: "0094_security_gifts",
		SQL:  SecurityGiftSQL,
	},
	{
		// Base rent for player-requested bases: settings, payments, reminders and
		// the paused check. Additive.
		Name: "0095_base_rent",
		SQL:  BaseRentSQL,
	},
	{
		// Gifted rent days: price 0, no ledger entry, the giving owner recorded.
		Name: "0096_base_rent_gifts",
		SQL:  BaseRentGiftSQL,
	},
	{
		// When each server last got the daily paused-bases staff notice. Additive.
		Name: "0097_base_rent_digests",
		SQL:  BaseRentDigestSQL,
	},
	{
		// Rent-free bases, and a due date that restarts when rent is switched
		// back on or resumed for a base. Additive table plus a replaced function.
		Name: "0098_base_rent_exemptions",
		SQL:  BaseRentExemptionSQL,
	},
	{
		// Base transfers to a faction mate, approved by the server owner. Additive.
		Name: "0099_base_transfers",
		SQL:  BaseTransferSQL,
	},
	{
		Name: "0100_shop_delivery_attempt_buyer_fulfilment",
		SQL:  ShopAttemptBuyerFulfilmentSQL,
	},
	{
		Name: "0101_shop_auto_delivery",
		SQL:  ShopAutoDeliverySQL,
	},
	{
		// A server owner's own C.A.S.E. evidence switch, counted only when the
		// platform allows self-serve. Additive.
		Name: "0102_case_evidence_optins",
		SQL:  CaseEvidenceOptinSQL,
	},
	{
		// Owner Hub operations: automation switches, fleet incidents, broadcasts and the
		// daily briefing guard (docs/OWNER_OPS.md). Additive only.
		Name: "0103_owner_ops",
		SQL:  OwnerOpsSQL,
	},
	{
		// Owner-built events: template, announce flag and once-only announcement stamps. Additive.
		Name: "0104_event_builder",
		SQL:  EventBuilderSQL,
	},
	{
		// Scheduled stats season rollovers and per-server Ranked resets. Additive.
		Name: "0105_season_planner",
		SQL:  SeasonPlannerSQL,
	},
	{
		// Which Discord invite each member joined through, and when they left. Additive.
		Name: "0106_invite_tracking",
		SQL:  InviteTrackingSQL,
	},
	{
		// Automatic Champion Point reward rules. Additive.
		Name: "0107_reward_rules",
		SQL:  RewardRulesSQL,
	},
	{
		// In-game name changes, recorded by a trigger from now on. Additive.
		Name: "0108_player_name_history",
		SQL:  PlayerNameHistorySQL,
	},
	{
		// Supporter / VIP tiers and memberships. Additive.
		Name: "0109_vip_tiers",
		SQL:  VIPTiersSQL,
	},
	{
		// Verified Nitrado tail-read trust, kept across restarts. Additive.
		Name: "0110_nitrado_tail_trust",
		SQL:  NitradoTailTrustSQL,
	},
	{
		// The perk store: offers, purchases and charges. Additive.
		Name: "0111_perk_store",
		SQL:  PerkStoreSQL,
	},
	{
		// Indexes for case-insensitive name lookups, the per-server kill time window and the
		// retention sweeps (docs/PERFORMANCE.md "Query hygiene"). Additive.
		Name: "0112_query_hygiene_indexes",
		SQL:  QueryHygieneIndexesSQL,
	},
	{
		Name: "0113_shop_delivery_attempt_marker",
		SQL:  ShopAttemptMarkerSQL,
	},
	{
		Name: "0114_shop_auto_delivery_marker",
		SQL:  ShopAutoDeliveryMarkerSQL,
	},
	{
		// Double RP windows and the multiplier each ranked award was earned under. Additive.
		Name: "0115_ranked_rp_boosts",
		SQL:  RankedRPBoostsSQL,
	},
	{
		// Optional ranked bonuses (bounty, underdog, revenge, daily first kill), rank-up and weekly
		// recap announcements. Additive.
		Name: "0116_ranked_bonuses",
		SQL:  RankedBonusesSQL,
	},
	{
		// The channel /features created, so the next /features removes only that one. Additive.
		Name: "0117_guild_features_channel",
		SQL:  GuildFeaturesChannelSQL,
	},
	{
		// Automation switches and the state of each automation (feature upgrades). Additive.
		Name: "0118_feature_upgrades",
		SQL:  FeatureUpgradesSQL,
	},
	{
		// Live map (docs/LIVE_MAP.md): who can see the public kill/pressure map (LISTED: only while
		// the server is listed on the network - the default; PUBLIC; OFF), how far behind real time
		// it runs, and whether verified players get the faction layer. Additive.
		Name: "0119_live_map_settings",
		SQL: `
ALTER TABLE installation_feature_settings ADD COLUMN IF NOT EXISTS live_map_visibility TEXT NOT NULL DEFAULT 'LISTED' CHECK (live_map_visibility IN ('LISTED','PUBLIC','OFF'));
ALTER TABLE installation_feature_settings ADD COLUMN IF NOT EXISTS live_map_delay_seconds INTEGER NOT NULL DEFAULT 120 CHECK (live_map_delay_seconds BETWEEN 0 AND 3600);
ALTER TABLE installation_feature_settings ADD COLUMN IF NOT EXISTS live_map_faction_layer BOOLEAN NOT NULL DEFAULT TRUE;
`,
	},
	{
		// Daily and weekly challenges, the battle pass and territory control
		// (docs/PROGRESSION.md). Everything is off until switched on. Additive.
		Name: "0120_progression",
		SQL:  ProgressionSQL,
	},
	{
		// A UAV players buy with Champion Points: every player's position on their live map for the
		// time bought (docs/LIVE_MAP.md "UAV"). Off until the owner turns it on. Additive.
		Name: "0121_live_map_uav",
		SQL:  LiveMapUAVSQL,
	},
	{
		// Map rotation with a player vote (docs/MAP_ROTATION.md): settings, maps, votes, ballots and
		// the switch log. Off until an owner turns it on, behind the map_rotation feature flag.
		// Additive.
		Name: "0122_map_rotation",
		SQL:  MapRotationSQL,
	},
	{
		// Map rotation: a map's spawn points are uploaded on the website and stored here, instead
		// of being read from the server's custom folder at a switch (docs/MAP_ROTATION.md).
		// Additive: two nullable columns.
		Name: "0123_map_rotation_spawn_upload",
		SQL:  MapRotationSpawnUploadSQL,
	},
	{
		// A game server's name follows its Nitrado name unless the owner typed one in Champion
		// (docs/SERVER_NAME_SYNC.md). Two columns, and existing servers whose current name came
		// from an audited rename are marked custom. Additive.
		Name: "0124_server_name_sync",
		SQL:  ServerNameSyncSQL,
	},
	{
		// Platform staff: Discord accounts that may read the Owner Hub and change nothing
		// (docs/ADMIN_API.md "Roles"). One new table. Additive.
		Name: "0125_platform_staff",
		SQL:  PlatformStaffSQL,
	},
}
