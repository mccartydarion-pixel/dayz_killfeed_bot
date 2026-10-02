// Package logger builds the process-wide slog logger.
//
// Output is JSON by default so the hosting platform (Railway) can parse the
// record's severity from the `level` key instead of guessing it from text:
// every record carries `time`, `level` (lowercase: debug/info/warn/error) and
// `msg`, followed by the record's attributes. LOG_LEVEL selects the minimum
// level (debug, info, warn, error; default info). LOG_FORMAT=text restores
// the human-readable key=value format for local runs.
package logger

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New builds the logger from LOG_LEVEL and LOG_FORMAT, writing to stdout.
func New() *slog.Logger {
	return slog.New(NewHandler(os.Getenv("LOG_LEVEL"), os.Getenv("LOG_FORMAT"), os.Stdout))
}

// NewHandler builds the handler New uses, for callers that supply the
// settings and destination themselves (tests, tooling).
func NewHandler(levelName, format string, w io.Writer) slog.Handler {
	opts := &slog.HandlerOptions{Level: ParseLevel(levelName), ReplaceAttr: replaceAttr}
	if strings.EqualFold(strings.TrimSpace(format), "text") {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// ParseLevel maps a LOG_LEVEL value to a slog level; anything unrecognised
// (including empty) is info.
func ParseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// replaceAttr lowercases the built-in level value ("INFO" -> "info") so log
// platforms that match severity by value recognise it; key names are slog's
// defaults (time, level, msg).
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.LevelKey {
		if level, ok := a.Value.Any().(slog.Level); ok {
			return slog.String(slog.LevelKey, strings.ToLower(level.String()))
		}
	}
	return a
}
