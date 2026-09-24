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
	// C.A.S.E. is a distinct, default-disabled server-scoped add-on.
	CaseBillingEnabled bool
	CaseVerifiedThrough string
	CaseWatchPriceID string
	CaseProPriceID string
	CaseCommandPriceID string
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
		PublicBaseURL:             ParsePublicBaseURL(os.Getenv("CHAMPION_PUBLIC_BASE_URL"), os.Getenv("RAILWAY_PUBLIC_DOMAIN")),

		DiscordPresenceEnabled:         parseBoolWithDefault(os.Getenv("DISCORD_PRESENCE_ENABLED"), true),
		DiscordPresenceRotationSeconds: parsePresenceRotationSeconds(os.Getenv("DISCORD_PRESENCE_ROTATION_SECONDS")),
		DiscordPresenceMode:            parsePresenceMode(os.Getenv("DISCORD_PRESENCE_MODE")),

		HeatmapDiscordIntervalMinutes: parseHeatmapDiscordIntervalMinutes(os.Getenv("HEATMAP_DISCORD_INTERVAL_MINUTES")),

		StripeSecretKey:       strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")),
		StripeWebhookSecret:   strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")),
		BillingPlansJSON:      os.Getenv("CHAMPION_BILLING_PLANS_JSON"),
		BillingAllowedOrigins: os.Getenv("CHAMPION_BILLING_ALLOWED_ORIGINS"),
		CaseBillingEnabled: parseBoolWithDefault(os.Getenv("CHAMPION_CASE_BILLING_ENABLED"), false),
		CaseVerifiedThrough: strings.TrimSpace(os.Getenv("CHAMPION_CASE_VERIFIED_THROUGH")),
		CaseWatchPriceID: strings.TrimSpace(os.Getenv("CHAMPION_CASE_WATCH_PRICE_ID")),
		CaseProPriceID: strings.TrimSpace(os.Getenv("CHAMPION_CASE_PRO_PRICE_ID")),
		CaseCommandPriceID: strings.TrimSpace(os.Getenv("CHAMPION_CASE_COMMAND_PRICE_ID")),
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
