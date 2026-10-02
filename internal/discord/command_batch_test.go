package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

type overwriteRecorder struct {
	calls   int
	appID   string
	guildID string
	names   []string
}

func (o *overwriteRecorder) ApplicationCommandBulkOverwrite(appID, guildID string, cmds []*discordgo.ApplicationCommand, _ ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	o.calls++
	o.appID, o.guildID = appID, guildID
	for _, c := range cmds {
		o.names = append(o.names, c.Name)
	}
	return cmds, nil
}

func TestCommandBatchSendsEveryCommandInOneRequest(t *testing.T) {
	batch := NewCommandBatch("app")
	for _, register := range []func(CommandRegistrar, string) error{RegisterSetupCommand, RegisterServerCommands, RegisterLinkCommands, RegisterStatsCommands} {
		if err := register(batch, "guild"); err != nil {
			t.Fatal(err)
		}
	}
	// A repeat registration replaces the queued command rather than adding a duplicate.
	if err := RegisterSetupCommand(batch, "guild"); err != nil {
		t.Fatal(err)
	}
	rec := &overwriteRecorder{}
	n, err := batch.Flush(rec, "guild")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"setup", "server", "link", "link-status", "unlink", "stats", "leaderboard"}
	if rec.calls != 1 || n != len(want) || rec.appID != "app" || rec.guildID != "guild" {
		t.Fatalf("flush: calls=%d n=%d app=%q guild=%q", rec.calls, n, rec.appID, rec.guildID)
	}
	for idx, name := range want {
		if rec.names[idx] != name {
			t.Fatalf("command %d = %q, want %q (all: %v)", idx, rec.names[idx], name, rec.names)
		}
	}
}

func TestEmptyCommandBatchNeverWipesTheGuild(t *testing.T) {
	rec := &overwriteRecorder{}
	if n, err := NewCommandBatch("app").Flush(rec, "guild"); err != nil || n != 0 || rec.calls != 0 {
		t.Fatalf("empty flush sent a request: n=%d err=%v calls=%d", n, err, rec.calls)
	}
	if err := RegisterSetupCommand(NewCommandBatch(""), "guild"); err == nil {
		t.Fatal("a batch without an application ID must not queue commands")
	}
}
