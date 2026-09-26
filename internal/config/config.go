package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Presence rotation bounds (see internal/discord/presence.go). Exported so
// callers/tests can reference the same authoritative values instead of
// duplicating the numbers.
const (
	DefaultPresenceRotationSeconds = 45
	MinPresenceRotationSeconds     = 30
	MaxPresenceRotationSeconds     = 300
)

// Discord heatmap summary refresh bounds (internal/discord/heatmap_board.go).
const (
	DefaultHeatmapDiscordIntervalMinutes = 30
	MinHeatmapDiscordIntervalMinutes     = 5
	MaxHeatmapDiscordIntervalMinutes     = 1440
)

// Config contains the runtime configuration for the application.
type Config struct {
	AppEnv   string
	HTTPPort string
	Port     string

	DiscordToken         string
	DiscordApplicationID string
	DiscordGuildID       string

	NitradoToken     string
	NitradoServiceID string
	// NitradoAPIBaseURL is NITRADO_API_BASE_URL: an isolated-staging-only
	// replacement for the Nitrado API (internal/nitrado/nitradofixture).
	// Load accepts it only with APP_ENV=staging.
	NitradoAPIBaseURL string

	KillfeedChannelID         string
	DatabaseURL               string
	CredentialEncryptionKey   string
	DiscordGuildMembersIntent bool

	// WebsiteAPISecret authenticates the website's read-only runtime status
	// API (see internal/app/runtime_status.go). Empty disables the endpoint.
	WebsiteAPISecret string

	// AdminDiscordIDs is the platform-admin (founder) allowlist from
	// CHAMPION_ADMIN_DISCORD_IDS: Discord user ids that may use the read-only
	// /api/admin surface. It is deliberately independent of organization roles
	// and Discord guild permissions. Empty = nobody is a platform admin (the
	// admin API fails closed).
	AdminDiscordIDs []string

	// CustomEmbedsEnabled (CHAMPION_CUSTOM_EMBEDS_ENABLED, default false) is the rollout
	// switch for Embed Designer runtime rendering. Off: every publisher uses the
	// Champion default cards exactly as before, whatever templates are saved. On:
	// eligible routes render an installation's saved template (falling back to the
	// default on any problem).
	CustomEmbedsEnabled bool

	// ShopCanaryExecution is the Shop Phase 2C.4 canary execution lock (docs/SHOP_DELIVERY_PHASE2C4.md).
	// It is its own switch: no other Shop, economy or delivery setting enables it. Mutating canary
	// operations are allowed only when CHAMPION_SHOP_CANARY_EXECUTION is exactly "enabled" AND the
	// installation is listed in CHAMPION_SHOP_CANARY_INSTALLATION_IDS. Default: locked.
	ShopCanaryExecution ShopCanaryExecution

	// PublicBaseURL is the public origin of this service (no trailing slash), used to build
	// absolute URLs for publicly served assets such as faction logos
	// (/assets/faction-logos/...). CHAMPION_PUBLIC_BASE_URL wins; otherwise it is derived from
	// Railway's RAILWAY_PUBLIC_DOMAIN; otherwise empty and the API returns root-relative URLs.
	PublicBaseURL string

	// Discord bot presence/activity settings (see internal/discord/presence.go).
	DiscordPresenceEnabled bool
	// DiscordPresenceRotationSeconds is already clamped to
	// [MinPresenceRotationSeconds, MaxPresenceRotationSeconds].
	DiscordPresenceRotationSeconds int
	// DiscordPresenceMode is always "static" or "dynamic".
	DiscordPresenceMode string

	// HeatmapDiscordIntervalMinutes is how often the Discord heatmap summary
	// is refreshed (HEATMAP_DISCORD_INTERVAL_MINUTES), already clamped to
	// [MinHeatmapDiscordIntervalMinutes, MaxHeatmapDiscordIntervalMinutes].
	HeatmapDiscordIntervalMinutes int

	// Champion Billing (docs/BILLING.md). StripeSecretKey/StripeWebhookSecret are server-only -
	// never logged, never returned by any API. An empty StripeSecretKey means billing runs
	// unconfigured (every billing action fails closed with BILLING_UNAVAILABLE) rather than the
	// service failing to start; a local/dev environment or a CI run needs no Stripe credentials.
	StripeSecretKey     string
	StripeWebhookSecret string
	// BillingPlansJSON is CHAMPION_BILLING_PLANS_JSON: the authoritative plan catalog (see
	// internal/billing.LoadCatalog). Empty = no plan approved yet ("PRICING DECISION REQUIRED").
	BillingPlansJSON string
	// BillingAllowedOrigins is CHAMPION_BILLING_ALLOWED_ORIGINS: extra site origins (beyond
	// billing.DefaultOrigin) a Checkout/Portal return URL may target, e.g. a local website dev
	// server. Never includes anything the client asserts about itself.
	BillingAllowedOrigins string
}

// Load reads configuration from environment variables and validates required fields.
func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		AppEnv:                    getEnv("APP_ENV", "development"),
		HTTPPort:                  getEnv("HTTP_PORT", "8080"),
		Port:                      getEnv("PORT", "8080"),
		DiscordToken:              strings.TrimSpace(os.Getenv("DISCORD_TOKEN")),
		DiscordApplicationID:      strings.TrimSpace(os.Getenv("DISCORD_APPLICATION_ID")),
		DiscordGuildID:            strings.TrimSpace(os.Getenv("DISCORD_GUILD_ID")),
		NitradoToken:              strings.TrimSpace(os.Getenv("NITRADO_TOKEN")),
		NitradoServiceID:          strings.TrimSpace(os.Getenv("NITRADO_SERVICE_ID")),
		KillfeedChannelID:         strings.TrimSpace(os.Getenv("KILLFEED_CHANNEL_ID")),
		DatabaseURL:               strings.TrimSpace(os.Getenv("DATABASE_URL")),
		CredentialEncryptionKey:   strings.TrimSpace(os.Getenv("CREDENTIAL_ENCRYPTION_KEY")),
		DiscordGuildMembersIntent: strings.EqualFold(strings.TrimSpace(os.Getenv("DISCORD_GUILD_MEMBERS_INTENT_ENABLED")), "true"),
		WebsiteAPISecret:          strings.TrimSpace(os.Getenv("WEBSITE_API_SECRET")),
		AdminDiscordIDs:           ParseAdminDiscordIDs(os.Getenv("CHAMPION_ADMIN_DISCORD_IDS")),
		CustomEmbedsEnabled:       parseBoolWithDefault(os.Getenv("CHAMPION_CUSTOM_EMBEDS_ENABLED"), false),
		ShopCanaryExecution:       ParseShopCanaryExecution(os.Getenv("CHAMPION_SHOP_CANARY_EXECUTION"), os.Getenv("CHAMPION_SHOP_CANARY_INSTALLATION_IDS")),
		PublicBaseURL:             ParsePublicBaseURL(os.Getenv("CHAMPION_PUBLIC_BASE_URL"), os.Getenv("RAILWAY_PUBLIC_DOMAIN")),

		DiscordPresenceEnabled:         parseBoolWithDefault(os.Getenv("DISCORD_PRESENCE_ENABLED"), true),
		DiscordPresenceRotationSeconds: parsePresenceRotationSeconds(os.Getenv("DISCORD_PRESENCE_ROTATION_SECONDS")),
		DiscordPresenceMode:            parsePresenceMode(os.Getenv("DISCORD_PRESENCE_MODE")),

		HeatmapDiscordIntervalMinutes: parseHeatmapDiscordIntervalMinutes(os.Getenv("HEATMAP_DISCORD_INTERVAL_MINUTES")),

		StripeSecretKey:       strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")),
		StripeWebhookSecret:   strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")),
		BillingPlansJSON:      os.Getenv("CHAMPION_BILLING_PLANS_JSON"),
		BillingAllowedOrigins: os.Getenv("CHAMPION_BILLING_ALLOWED_ORIGINS"),

		NitradoAPIBaseURL: strings.TrimSpace(os.Getenv("NITRADO_API_BASE_URL")),
	}
	if err := ValidateNitradoAPIBaseURL(cfg.NitradoAPIBaseURL, cfg.AppEnv); err != nil {
		return nil, err
	}
	if err := ValidateStagingIsolation(cfg.AppEnv, cfg.StripeSecretKey); err != nil {
		return nil, err
	}

	if cfg.HTTPPort == "" {
		cfg.HTTPPort = "8080"
	}
	if cfg.Port == "" {
		cfg.Port = cfg.HTTPPort
	}

	if cfg.DiscordToken == "" {
		return nil, fmt.Errorf("DISCORD_TOKEN is required")
	}

	return cfg, nil
}

// ValidateNitradoAPIBaseURL allows a Nitrado API replacement only in an
// environment that declares itself staging (an allowlist: a production
// service with APP_ENV unset or anything else refuses to start), and only as
// an absolute http(s) URL.
func ValidateNitradoAPIBaseURL(raw, appEnv string) error {
	if raw == "" {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(appEnv), "staging") {
		return fmt.Errorf("NITRADO_API_BASE_URL is only allowed with APP_ENV=staging")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("NITRADO_API_BASE_URL must be an absolute http(s) URL")
	}
	return nil
}

// ValidateStagingIsolation refuses production-only credentials in a service
// that declares APP_ENV=staging: a live Stripe key there could charge real
// customers. (Discord and Nitrado isolation cannot be proven from a value;
// see docs/incidents/2026-09-26-staging-infrastructure.md.)
func ValidateStagingIsolation(appEnv, stripeSecretKey string) error {
	if !strings.EqualFold(strings.TrimSpace(appEnv), "staging") {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(stripeSecretKey), "sk_live_") || strings.HasPrefix(strings.TrimSpace(stripeSecretKey), "rk_live_") {
		return fmt.Errorf("APP_ENV=staging refuses a live Stripe key (STRIPE_SECRET_KEY); leave it unset or use a test key")
	}
	return nil
}

func getEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

// parseBoolWithDefault parses a boolean env var, falling back to def when the
// value is empty or not a recognized boolean (matches strconv.ParseBool's
// accepted forms: 1/t/T/TRUE/true/True/0/f/F/FALSE/false/False).
// ParseAdminDiscordIDs parses a comma-separated allowlist of Discord user ids.
// Entries are trimmed; blanks, duplicates and anything that is not a plain
// numeric Discord snowflake are dropped (a typo therefore fails closed - it can
// never accidentally match a real user).
func ParseAdminDiscordIDs(raw string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" || seen[id] || len(id) < 5 || len(id) > 25 {
			continue
		}
		numeric := true
		for _, r := range id {
			if r < '0' || r > '9' {
				numeric = false
				break
			}
		}
		if !numeric {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// IsPlatformAdmin reports whether discordUserID is on the platform-admin
// allowlist. Nil-safe; an empty id or empty allowlist is never an admin.
func (c *Config) IsPlatformAdmin(discordUserID string) bool {
	if c == nil {
		return false
	}
	id := strings.TrimSpace(discordUserID)
	if id == "" {
		return false
	}
	for _, allowed := range c.AdminDiscordIDs {
		if allowed == id {
			return true
		}
	}
	return false
}

func parseBoolWithDefault(raw string, def bool) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return v
}

// parsePresenceRotationSeconds parses DISCORD_PRESENCE_ROTATION_SECONDS,
// falling back to DefaultPresenceRotationSeconds when missing or not a valid
// integer, and clamping any valid value to
// [MinPresenceRotationSeconds, MaxPresenceRotationSeconds] so a
// misconfigured value can never rotate faster than the floor.
func parsePresenceRotationSeconds(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultPresenceRotationSeconds
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultPresenceRotationSeconds
	}
	if v < MinPresenceRotationSeconds {
		return MinPresenceRotationSeconds
	}
	if v > MaxPresenceRotationSeconds {
		return MaxPresenceRotationSeconds
	}
	return v
}

// parseHeatmapDiscordIntervalMinutes parses HEATMAP_DISCORD_INTERVAL_MINUTES,
// falling back to the default when missing or invalid and clamping to the
// bounds so a misconfiguration can never refresh faster than the floor.
func parseHeatmapDiscordIntervalMinutes(raw string) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return DefaultHeatmapDiscordIntervalMinutes
	}
	if v < MinHeatmapDiscordIntervalMinutes {
		return MinHeatmapDiscordIntervalMinutes
	}
	if v > MaxHeatmapDiscordIntervalMinutes {
		return MaxHeatmapDiscordIntervalMinutes
	}
	return v
}

// parsePresenceMode normalizes DISCORD_PRESENCE_MODE to exactly "static" or
// "dynamic", defaulting to "dynamic" for anything empty or unrecognized.
func parsePresenceMode(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "static") {
		return "static"
	}
	return "dynamic"
}

// ParsePublicBaseURL resolves the public origin used for absolute asset URLs. An explicit
// value must be an http(s) URL with a host and nothing else (no path, query or credentials) or
// it is ignored; otherwise a Railway public domain becomes https://<domain>; otherwise "".
func ParsePublicBaseURL(explicit, railwayDomain string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		u, err := url.Parse(v)
		if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil &&
			(u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == "" {
			return u.Scheme + "://" + u.Host
		}
		return ""
	}
	if d := strings.TrimSpace(railwayDomain); d != "" && !strings.ContainsAny(d, "/:@ ?#") {
		return "https://" + d
	}
	return ""
}

// ShopCanaryExecution is the parsed canary execution lock.
type ShopCanaryExecution struct {
	Enabled         bool
	InstallationIDs []int64
}

// ParseShopCanaryExecution enables the lock only for the exact word "enabled" (not "true", "1" or
// "yes": an accidental generic boolean never opens it) and only with at least one valid installation
// id. Anything else is locked.
func ParseShopCanaryExecution(mode, ids string) ShopCanaryExecution {
	if strings.TrimSpace(mode) != "enabled" {
		return ShopCanaryExecution{}
	}
	var out []int64
	seen := map[int64]bool{}
	for _, part := range strings.Split(ids, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return ShopCanaryExecution{}
	}
	return ShopCanaryExecution{Enabled: true, InstallationIDs: out}
}
