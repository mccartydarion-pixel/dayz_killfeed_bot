package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestWarnWeakWebsiteSecretLogsLengthOnly(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	if WarnWeakWebsiteSecret("") {
		t.Fatal("an unset secret must not warn")
	}
	if WarnWeakWebsiteSecret(strings.Repeat("x", 32)) {
		t.Fatal("a 32-byte secret must not warn")
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected log output: %s", buf.String())
	}
	const weak = "short-secret-value"
	if !WarnWeakWebsiteSecret(weak) {
		t.Fatal("a short secret must warn")
	}
	out := buf.String()
	if !strings.Contains(out, "event=weak_website_secret") || !strings.Contains(out, "length=18") {
		t.Fatalf("warning missing event/length: %s", out)
	}
	if strings.Contains(out, weak) {
		t.Fatalf("warning leaked the secret: %s", out)
	}
}

func TestLoadParsesRequireSignedIdentity(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "token")
	t.Setenv("CHAMPION_REQUIRE_SIGNED_IDENTITY", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequireSignedIdentity {
		t.Fatal("RequireSignedIdentity must default to false")
	}
	t.Setenv("CHAMPION_REQUIRE_SIGNED_IDENTITY", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RequireSignedIdentity {
		t.Fatal("CHAMPION_REQUIRE_SIGNED_IDENTITY=true must enable it")
	}
}
