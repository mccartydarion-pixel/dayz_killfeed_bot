package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
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

	KillfeedChannelID string
	DatabaseURL       string
}

// Load reads configuration from environment variables and validates required fields.
func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		AppEnv:               getEnv("APP_ENV", "development"),
		HTTPPort:             getEnv("HTTP_PORT", "8080"),
		Port:                 getEnv("PORT", "8080"),
		DiscordToken:         strings.TrimSpace(os.Getenv("DISCORD_TOKEN")),
		DiscordApplicationID: strings.TrimSpace(os.Getenv("DISCORD_APPLICATION_ID")),
		DiscordGuildID:       strings.TrimSpace(os.Getenv("DISCORD_GUILD_ID")),
		NitradoToken:         strings.TrimSpace(os.Getenv("NITRADO_TOKEN")),
		NitradoServiceID:     strings.TrimSpace(os.Getenv("NITRADO_SERVICE_ID")),
		KillfeedChannelID:    strings.TrimSpace(os.Getenv("KILLFEED_CHANNEL_ID")),
		DatabaseURL:          strings.TrimSpace(os.Getenv("DATABASE_URL")),
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
	if cfg.NitradoToken == "" {
		return nil, fmt.Errorf("NITRADO_TOKEN is required")
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
