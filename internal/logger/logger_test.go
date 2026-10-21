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
	// under a "msg" attribute; the handler makes the text the one msg key.
	if rec["msg"] != "something happened" || rec["component"] != "test" {
		t.Fatalf("msg = %v, component = %v", rec["msg"], rec["component"])
	}
	if n := strings.Count(buf.String(), `"msg":`); n != 1 {
		t.Fatalf("want exactly one msg key (Railway shows the first), got %d: %s", n, buf.String())
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

func TestComponentConventionBecomesOneMessage(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler("", "", &buf)).With("server_id", 3)
	log.Info("component=discord", "event", "route_fallback", "route_key", "KILLFEED")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "route_fallback" || rec["event"] != "route_fallback" || rec["component"] != "discord" || rec["route_key"] != "KILLFEED" || rec["server_id"] != float64(3) {
		t.Fatalf("event record: %v", rec)
	}
	buf.Reset()
	log.Info("component=discord")
	rec = nil
	_ = json.Unmarshal(buf.Bytes(), &rec)
	if rec["msg"] != "component=discord" || rec["component"] != "discord" {
		t.Fatalf("bare component record: %v", rec)
	}
	buf.Reset()
	log.Info("plain message", "k", "v")
	rec = nil
	_ = json.Unmarshal(buf.Bytes(), &rec)
	if rec["msg"] != "plain message" || rec["component"] != nil {
		t.Fatalf("plain record changed: %v", rec)
	}
}
