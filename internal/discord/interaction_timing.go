package discord

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Interaction timing: every command, button and form logs how long the bot
// took to answer Discord (which must happen within 3 s) and, for deferred
// replies, how long until the real reply. Timing is taken where the HTTP
// requests leave the bot, so no handler has to opt in. Interaction tokens are
// only used as map keys and are never logged.

// slowAnswer is when an answer is logged as a warning: past it, the user is
// close to Discord's 3 s "The application did not respond".
const slowAnswer = 2 * time.Second

// lateAnswer is past which an acknowledgement is counted as late: Discord has
// very likely already shown "The application did not respond".
const lateAnswer = 2500 * time.Millisecond

// interactionTTL is how long Discord accepts edits to an interaction reply.
const interactionTTL = 15 * time.Minute

type timedInteraction struct {
	label     string
	finished  bool
	received  time.Time
	created   time.Time
	answered  bool
	completed bool
}

type interactionTimer struct {
	mu      sync.Mutex
	byID    map[string]*timedInteraction
	byToken map[string]*timedInteraction
	stats   map[string]*interactionStat
	now     func() time.Time
	log     func(level slog.Level, args ...any)
}

func newInteractionTimer() *interactionTimer {
	return &interactionTimer{
		byID:    map[string]*timedInteraction{},
		byToken: map[string]*timedInteraction{},
		stats:   map[string]*interactionStat{},
		now:     time.Now,
		log: func(level slog.Level, args ...any) {
			slog.Log(context.Background(), level, "component=discord", args...)
		},
	}
}

// track records an interaction the first time a handler sees it.
func (t *interactionTimer) track(i *discordgo.InteractionCreate) {
	if t == nil || i == nil || i.Interaction == nil || i.ID == "" {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.byID[i.ID]; ok {
		return
	}
	for id, entry := range t.byID {
		if now.Sub(entry.received) > interactionTTL {
			delete(t.byID, id)
		}
	}
	for token, entry := range t.byToken {
		if now.Sub(entry.received) > interactionTTL {
			delete(t.byToken, token)
		}
	}
	entry := &timedInteraction{label: interactionLabel(i), received: now}
	if created, err := discordgo.SnowflakeTimestamp(i.ID); err == nil {
		entry.created = created
	}
	t.byID[i.ID] = entry
	if i.Token != "" {
		t.byToken[i.Token] = entry
	}
}

// observe is called after each Discord REST request with its method and path.
func (t *interactionTimer) observe(method, path string, status int, err error) {
	if t == nil {
		return
	}
	kind, key := classifyInteractionRequest(method, path)
	if kind == "" {
		return
	}
	now := t.now()
	t.mu.Lock()
	var entry *timedInteraction
	if kind == "answer" {
		entry = t.byID[key]
	} else {
		entry = t.byToken[key]
	}
	if entry == nil || (kind == "answer" && entry.answered) || (kind == "complete" && (entry.completed || !entry.answered)) {
		t.mu.Unlock()
		return
	}
	if kind == "answer" {
		entry.answered = true
	} else {
		entry.completed = true
	}
	label, received, created := entry.label, entry.received, entry.created
	elapsed := now.Sub(received)
	if kind == "answer" {
		stat := t.statLocked(label)
		stat.ack.add(elapsed)
		if elapsed > lateAnswer {
			stat.lateAcks++
		}
	}
	t.mu.Unlock()

	msg := "interaction completed"
	if kind == "answer" {
		msg = "interaction answered"
	}
	args := []any{"msg", msg, "interaction", label, "status", status}
	if err != nil {
		// Not err.Error(): a transport error quotes the request URL, which
		// contains the interaction token.
		args = append(args, "err", "request_failed")
	}
	if kind == "answer" {
		args = append(args, "answer_ms", elapsed.Milliseconds())
		if !created.IsZero() {
			// Gateway delivery: Discord creating the interaction -> the bot receiving it.
			args = append(args, "gateway_ms", received.Sub(created).Milliseconds())
		}
		level := slog.LevelInfo
		if elapsed >= slowAnswer || err != nil || status >= 400 {
			level = slog.LevelWarn
		}
		t.log(level, args...)
		return
	}
	args = append(args, "total_ms", elapsed.Milliseconds())
	t.log(slog.LevelInfo, args...)
}

// classifyInteractionRequest recognises the first answer to an interaction
// (POST /interactions/{id}/{token}/callback, keyed by ID) and its later reply
// (PATCH /webhooks/{app}/{token}/messages/@original, or a followup POST to
// /webhooks/{app}/{token}, keyed by token).
func classifyInteractionRequest(method, path string) (kind, key string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for idx, part := range parts {
		switch part {
		case "interactions":
			if method == http.MethodPost && len(parts) == idx+4 && parts[idx+3] == "callback" {
				return "answer", parts[idx+1]
			}
			return "", ""
		case "webhooks":
			rest := parts[idx+1:]
			if method == http.MethodPost && len(rest) == 2 {
				return "complete", rest[1]
			}
			if method == http.MethodPatch && len(rest) == 4 && rest[2] == "messages" && rest[3] == "@original" {
				return "complete", rest[1]
			}
			return "", ""
		}
	}
	return "", ""
}

var digitRun = regexp.MustCompile(`[0-9]+`)

// interactionLabel names an interaction for logs: the command and subcommand,
// or a button/form custom ID with its numbers masked.
func interactionLabel(i *discordgo.InteractionCreate) string {
	switch i.Type {
	case discordgo.InteractionApplicationCommand, discordgo.InteractionApplicationCommandAutocomplete:
		data := i.ApplicationCommandData()
		label := "/" + data.Name
		options := data.Options
		for len(options) > 0 {
			opt := options[0]
			if opt.Type != discordgo.ApplicationCommandOptionSubCommand && opt.Type != discordgo.ApplicationCommandOptionSubCommandGroup {
				break
			}
			label += " " + opt.Name
			options = opt.Options
		}
		if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
			label = "autocomplete " + label
		}
		return label
	case discordgo.InteractionMessageComponent:
		return "button " + maskCustomID(i.MessageComponentData().CustomID)
	case discordgo.InteractionModalSubmit:
		return "form " + maskCustomID(i.ModalSubmitData().CustomID)
	}
	return "interaction"
}

func maskCustomID(customID string) string {
	masked := digitRun.ReplaceAllString(customID, "#")
	if len(masked) > 60 {
		masked = masked[:60]
	}
	return masked
}

// timingTransport reports every Discord REST request to the timer.
type timingTransport struct {
	base  http.RoundTripper
	timer *interactionTimer
}

func (t *timingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	t.timer.observe(req.Method, req.URL.Path, status, err)
	return resp, err
}
