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

func TestPresenceDefaultsWhenUnset(t *testing.T) {
	os.Clearenv()
	t.Setenv("DISCORD_TOKEN", "token")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if !cfg.DiscordPresenceEnabled {
		t.Fatal("expected presence enabled by default")
	}
	if cfg.DiscordPresenceRotationSeconds != DefaultPresenceRotationSeconds {
		t.Fatalf("expected default rotation of %d, got %d", DefaultPresenceRotationSeconds, cfg.DiscordPresenceRotationSeconds)
	}
	if cfg.DiscordPresenceMode != "dynamic" {
		t.Fatalf("expected default mode dynamic, got %q", cfg.DiscordPresenceMode)
	}
}

func TestParseBoolWithDefault(t *testing.T) {
	cases := []struct {
		raw  string
		def  bool
		want bool
	}{
		{"", true, true},
		{"", false, false},
		{"true", false, true},
		{"false", true, false},
		{"not-a-bool", true, true},
		{"not-a-bool", false, false},
	}
	for _, tc := range cases {
		if got := parseBoolWithDefault(tc.raw, tc.def); got != tc.want {
			t.Fatalf("parseBoolWithDefault(%q, %v) = %v, want %v", tc.raw, tc.def, got, tc.want)
		}
	}
}

func TestParsePresenceRotationSeconds(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"empty defaults", "", DefaultPresenceRotationSeconds},
		{"invalid defaults", "not-a-number", DefaultPresenceRotationSeconds},
		{"within range kept as-is", "60", 60},
		{"below minimum clamps up", "5", MinPresenceRotationSeconds},
		{"exactly minimum kept", "30", MinPresenceRotationSeconds},
		{"above maximum clamps down", "1000", MaxPresenceRotationSeconds},
		{"exactly maximum kept", "300", MaxPresenceRotationSeconds},
		{"negative clamps to minimum", "-5", MinPresenceRotationSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePresenceRotationSeconds(tc.raw); got != tc.want {
				t.Fatalf("parsePresenceRotationSeconds(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParsePresenceMode(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", "dynamic"},
		{"dynamic", "dynamic"},
		{"static", "static"},
		{"STATIC", "static"},
		{"garbage", "dynamic"},
	}
	for _, tc := range cases {
		if got := parsePresenceMode(tc.raw); got != tc.want {
			t.Fatalf("parsePresenceMode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestLoadParsesPresenceEnvironment(t *testing.T) {
	os.Clearenv()
	t.Setenv("DISCORD_TOKEN", "token")
	t.Setenv("DISCORD_PRESENCE_ENABLED", "false")
	t.Setenv("DISCORD_PRESENCE_ROTATION_SECONDS", "90")
	t.Setenv("DISCORD_PRESENCE_MODE", "static")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if cfg.DiscordPresenceEnabled {
		t.Fatal("expected presence disabled")
	}
	if cfg.DiscordPresenceRotationSeconds != 90 {
		t.Fatalf("expected rotation of 90, got %d", cfg.DiscordPresenceRotationSeconds)
	}
	if cfg.DiscordPresenceMode != "static" {
		t.Fatalf("expected mode static, got %q", cfg.DiscordPresenceMode)
	}
}

func TestParseHeatmapDiscordIntervalMinutes(t *testing.T) {
	cases := map[string]int{"": DefaultHeatmapDiscordIntervalMinutes, "abc": DefaultHeatmapDiscordIntervalMinutes, "1": MinHeatmapDiscordIntervalMinutes, "45": 45, "99999": MaxHeatmapDiscordIntervalMinutes}
	for in, want := range cases {
		if got := parseHeatmapDiscordIntervalMinutes(in); got != want {
			t.Errorf("parseHeatmapDiscordIntervalMinutes(%q) = %d, want %d", in, got, want)
		}
	}
}
