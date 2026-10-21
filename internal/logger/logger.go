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
	"context"
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
		return componentHandler{slog.NewTextHandler(w, opts)}
	}
	return componentHandler{slog.NewJSONHandler(w, opts)}
}

// componentHandler rewrites the codebase's logging convention,
//
//	slog.Info("component=discord", "msg", "slash commands registered", ...)
//
// into a record whose message is the text and whose `component` key is the
// component: {"msg":"slash commands registered","component":"discord",...}.
// Left alone, JSON output carries two `msg` keys, and Railway shows the first
// one ("component=discord"), hiding the text. Without a "msg" attribute, an
// "event" attribute's value becomes the message (and stays as `event`).
type componentHandler struct{ next slog.Handler }

func (h componentHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h componentHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return componentHandler{h.next.WithAttrs(attrs)}
}

func (h componentHandler) WithGroup(name string) slog.Handler {
	return componentHandler{h.next.WithGroup(name)}
}

func (h componentHandler) Handle(ctx context.Context, r slog.Record) error {
	component, ok := strings.CutPrefix(r.Message, "component=")
	if !ok {
		return h.next.Handle(ctx, r)
	}
	message, event := "", ""
	attrs := make([]slog.Attr, 0, r.NumAttrs()+1)
	attrs = append(attrs, slog.String("component", component))
	r.Attrs(func(a slog.Attr) bool {
		switch {
		case a.Key == slog.MessageKey && message == "":
			message = a.Value.String()
			return true
		case a.Key == "event" && event == "":
			event = a.Value.String()
		}
		attrs = append(attrs, a)
		return true
	})
	switch {
	case message != "":
	case event != "":
		message = event
	default:
		message = r.Message
	}
	out := slog.NewRecord(r.Time, r.Level, message, r.PC)
	out.AddAttrs(attrs...)
	return h.next.Handle(ctx, out)
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
