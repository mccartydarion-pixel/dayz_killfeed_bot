package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestJSONHandlerEmitsRailwayKeys(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler("", "", &buf))
	log.Warn("component=test", "msg", "something happened", "server_id", 7)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("default output is not JSON: %v: %q", err, buf.String())
	}
	if rec["level"] != "warn" {
		t.Fatalf("level = %v, want lowercase warn", rec["level"])
	}
	if _, ok := rec["time"]; !ok {
		t.Fatal("time key missing")
	}
	// The codebase logs the component as the slog message and the human text
	// under a "msg" attribute; slog keeps the last duplicate, so msg is the text.
	if rec["msg"] != "something happened" {
		t.Fatalf("msg = %v", rec["msg"])
	}
	if rec["server_id"] != float64(7) {
		t.Fatalf("server_id = %v", rec["server_id"])
	}
}

func TestLogLevelAndTextEscapeHatch(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler("debug", "text", &buf))
	log.Debug("component=test", "k", "v")
	out := buf.String()
	if !strings.Contains(out, "level=debug") || !strings.Contains(out, "k=v") || strings.HasPrefix(out, "{") {
		t.Fatalf("expected text output with debug enabled, got %q", out)
	}
	buf.Reset()
	log = slog.New(NewHandler("", "", &buf))
	log.Debug("component=test")
	if buf.Len() != 0 {
		t.Fatalf("debug must be filtered at the default level, got %q", buf.String())
	}
	buf.Reset()
	log = slog.New(NewHandler("error", "json", &buf))
	log.Warn("component=test")
	if buf.Len() != 0 {
		t.Fatalf("warn must be filtered at LOG_LEVEL=error, got %q", buf.String())
	}
	if got := ParseLevel(" WARNING "); got != slog.LevelWarn {
		t.Fatalf("ParseLevel = %v", got)
	}
}
