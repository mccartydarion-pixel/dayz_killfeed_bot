package config

import (
	"os"
	"testing"
)

func TestLoadValidatesRequiredSettings(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "token")
	t.Setenv("NITRADO_TOKEN", "nitrado-token")
	t.Setenv("APP_ENV", "development")
	t.Setenv("HTTP_PORT", "8080")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if cfg.DiscordToken != "token" {
		t.Fatalf("expected Discord token to be loaded")
	}
	if cfg.NitradoToken != "nitrado-token" {
		t.Fatalf("expected Nitrado token to be loaded")
	}
}

func TestLoadRejectsMissingRequiredSettings(t *testing.T) {
	os.Clearenv()
	t.Setenv("DISCORD_TOKEN", "")
	t.Setenv("NITRADO_TOKEN", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected missing configuration to fail")
	}
}
