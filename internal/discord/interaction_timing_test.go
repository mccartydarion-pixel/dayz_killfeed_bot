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

func TestInteractionTimingSummary(t *testing.T) {
	timer, now, lines := testTimer()
	// 100 /stats interactions acknowledged after 1..100 ms, one of them late.
	for n := 1; n <= 100; n++ {
		id := fmt.Sprintf("stats-%d", n)
		i := commandInteraction(id, "tok-"+id, "stats", "")
		timer.track(i)
		ack := time.Duration(n) * time.Millisecond
		if n == 100 {
			ack = 2600 * time.Millisecond
		}
		*now = now.Add(ack)
		timer.observe("POST", "/api/v9/interactions/"+id+"/tok-"+id+"/callback", 204, nil)
		timer.finish(i, ack+50*time.Millisecond, true)
		timer.finish(i, time.Hour, true) // a second finish is ignored
	}
	// One button whose handler ended without any acknowledgement.
	button := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "b1", Token: "tok-b1", Type: discordgo.InteractionMessageComponent,
		Data: discordgo.MessageComponentInteractionData{CustomID: "baserent:pay:4411"}}}
	timer.track(button)
	timer.finish(button, 10*time.Millisecond, false)

	got := timer.summary()
	if len(got) != 2 || got[0].Name != "/stats" || got[1].Name != "button baserent:pay:#" {
		t.Fatalf("summary names: %+v", got)
	}
	stats := got[0]
	if stats.Acknowledged != 100 || stats.Handled != 100 || stats.LateAcks != 1 || stats.Unanswered != 0 {
		t.Fatalf("counts: %+v", stats)
	}
	if stats.AckP50Ms != 50 || stats.AckP90Ms != 90 || stats.AckP99Ms != 99 || stats.AckMaxMs != 2600 {
		t.Fatalf("ack percentiles: %+v", stats)
	}
	if stats.TotalP50Ms != 100 || stats.TotalMaxMs != 2650 {
		t.Fatalf("total percentiles: %+v", stats)
	}
	if got[1].Unanswered != 1 || got[1].Acknowledged != 0 || got[1].Handled != 1 {
		t.Fatalf("unanswered button: %+v", got[1])
	}
	// Exactly one warning, for the one late acknowledgement, and it names the
	// interaction only.
	warnings := 0
	for _, line := range *lines {
		if line.level == slog.LevelWarn {
			warnings++
			if line.attrs["interaction"] != "/stats" || strings.Contains(fmt.Sprint(line.attrs), "tok-") {
				t.Fatalf("warning line: %+v", line)
			}
		}
	}
	if warnings != 1 {
		t.Fatalf("want one slow-acknowledgement warning, got %d", warnings)
	}
}

func TestInteractionTimingKeepsRecentSamplesAndBoundsNames(t *testing.T) {
	var series timingSeries
	for n := 0; n < timingSamples; n++ {
		series.add(time.Second)
	}
	for n := 0; n < timingSamples; n++ {
		series.add(time.Millisecond)
	}
	p50, _, p99 := series.percentiles()
	if p50 != time.Millisecond || p99 != time.Millisecond || series.max != time.Second || series.count != 2*timingSamples {
		t.Fatalf("recent window: p50=%s p99=%s max=%s count=%d", p50, p99, series.max, series.count)
	}
	timer, _, _ := testTimer()
	for n := 0; n < maxTimedNames+50; n++ {
		timer.mu.Lock()
		timer.statLocked(fmt.Sprintf("/cmd%c%c%c", 'a'+n%26, 'a'+(n/26)%26, 'a'+(n/676)%26))
		timer.mu.Unlock()
	}
	if len(timer.stats) > maxTimedNames+1 {
		t.Fatalf("timing table grew to %d names", len(timer.stats))
	}
}

func TestInteractionTimerNeverLogsTheRequestError(t *testing.T) {
	timer, _, lines := testTimer()
	timer.track(commandInteraction("77", "secret-token", "stats", ""))
	timer.observe("POST", "/api/v9/interactions/77/secret-token/callback", 0, fmt.Errorf(`Post "https://discord.com/api/v9/interactions/77/secret-token/callback": dial tcp: timeout`))
	if len(*lines) != 1 || strings.Contains(fmt.Sprint((*lines)[0].attrs), "secret-token") {
		t.Fatalf("interaction token logged: %+v", *lines)
	}
}
