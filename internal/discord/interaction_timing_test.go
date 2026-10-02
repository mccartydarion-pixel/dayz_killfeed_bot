package discord

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type loggedLine struct {
	level slog.Level
	attrs map[string]any
}

func testTimer() (*interactionTimer, *time.Time, *[]loggedLine) {
	timer := newInteractionTimer()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	timer.now = func() time.Time { return now }
	var lines []loggedLine
	timer.log = func(level slog.Level, args ...any) {
		attrs := map[string]any{}
		for idx := 0; idx+1 < len(args); idx += 2 {
			attrs[args[idx].(string)] = args[idx+1]
		}
		lines = append(lines, loggedLine{level, attrs})
	}
	return timer, &now, &lines
}

func commandInteraction(id, token, name, sub string) *discordgo.InteractionCreate {
	data := discordgo.ApplicationCommandInteractionData{Name: name}
	if sub != "" {
		data.Options = []*discordgo.ApplicationCommandInteractionDataOption{{Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand}}
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: id, Token: token, Type: discordgo.InteractionApplicationCommand, Data: data}}
}

func TestInteractionTimerLogsAnswerAndCompletion(t *testing.T) {
	timer, now, lines := testTimer()
	timer.track(commandInteraction("1234", "secret-token", "server", "select"))
	*now = now.Add(400 * time.Millisecond)
	timer.observe("POST", "/api/v9/interactions/1234/secret-token/callback", 204, nil)
	*now = now.Add(3 * time.Second)
	timer.observe("PATCH", "/api/v9/webhooks/app/secret-token/messages/@original", 200, nil)
	timer.observe("PATCH", "/api/v9/webhooks/app/secret-token/messages/@original", 200, nil)

	if len(*lines) != 2 {
		t.Fatalf("want an answer and a completion line, got %+v", *lines)
	}
	answer, done := (*lines)[0], (*lines)[1]
	if answer.attrs["msg"] != "interaction answered" || answer.attrs["interaction"] != "/server select" || answer.attrs["answer_ms"] != int64(400) || answer.level != slog.LevelInfo {
		t.Fatalf("answer line: %+v", answer)
	}
	if done.attrs["msg"] != "interaction completed" || done.attrs["total_ms"] != int64(3400) {
		t.Fatalf("completion line: %+v", done)
	}
	for _, line := range *lines {
		if strings.Contains(fmt.Sprint(line.attrs), "secret-token") {
			t.Fatalf("interaction token logged: %+v", line)
		}
	}
}

func TestInteractionTimerWarnsOnSlowAnswer(t *testing.T) {
	timer, now, lines := testTimer()
	timer.track(commandInteraction("99", "tok", "admin", "status"))
	*now = now.Add(2500 * time.Millisecond)
	timer.observe("POST", "/api/v9/interactions/99/tok/callback", 204, nil)
	if len(*lines) != 1 || (*lines)[0].level != slog.LevelWarn {
		t.Fatalf("slow answer not a warning: %+v", *lines)
	}
}

func TestInteractionTimerIgnoresOtherRequests(t *testing.T) {
	timer, _, lines := testTimer()
	timer.track(commandInteraction("5", "tok", "stats", ""))
	timer.observe("PATCH", "/api/v9/webhooks/app/tok/messages/@original", 200, nil) // before any answer
	timer.observe("POST", "/api/v9/channels/1/messages", 200, nil)
	timer.observe("POST", "/api/v9/interactions/6/other/callback", 204, nil) // untracked
	if len(*lines) != 0 {
		t.Fatalf("unexpected lines: %+v", *lines)
	}
}

func TestInteractionLabelMasksNumbers(t *testing.T) {
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{Type: discordgo.InteractionMessageComponent, Data: discordgo.MessageComponentInteractionData{CustomID: "faction_recruit:join:123456"}}}
	if got := interactionLabel(i); got != "button faction_recruit:join:#" {
		t.Fatalf("label = %q", got)
	}
}
