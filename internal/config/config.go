package config

import (
	"fmt"
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

	// Discord bot presence/activity settings (see internal/discord/presence.go).
	DiscordPresenceEnabled bool
	// DiscordPresenceRotationSeconds is already clamped to
	// [MinPresenceRotationSeconds, MaxPresenceRotationSeconds].
	DiscordPresenceRotationSeconds int
	// DiscordPresenceMode is always "static" or "dynamic".
	DiscordPresenceMode string
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

		DiscordPresenceEnabled:         parseBoolWithDefault(os.Getenv("DISCORD_PRESENCE_ENABLED"), true),
		DiscordPresenceRotationSeconds: parsePresenceRotationSeconds(os.Getenv("DISCORD_PRESENCE_ROTATION_SECONDS")),
		DiscordPresenceMode:            parsePresenceMode(os.Getenv("DISCORD_PRESENCE_MODE")),
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

// parsePresenceMode normalizes DISCORD_PRESENCE_MODE to exactly "static" or
// "dynamic", defaulting to "dynamic" for anything empty or unrecognized.
func parsePresenceMode(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "static") {
		return "static"
	}
	return "dynamic"
}
