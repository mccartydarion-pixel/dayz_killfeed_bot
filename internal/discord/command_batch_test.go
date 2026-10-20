package discord

import (
	"errors"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type overwriteCall struct {
	appID, guildID string
	names          []string
	guildOnly      []bool // per command: Contexts is exactly [guild]
}

type overwriteRecorder struct {
	calls []overwriteCall
	fail  map[string]error // by guild ID ("" is the global list)
}

func (o *overwriteRecorder) ApplicationCommandBulkOverwrite(appID, guildID string, cmds []*discordgo.ApplicationCommand, _ ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	call := overwriteCall{appID: appID, guildID: guildID, names: []string{}}
	for _, c := range cmds {
		call.names = append(call.names, c.Name)
		call.guildOnly = append(call.guildOnly, c.Contexts != nil && len(*c.Contexts) == 1 && (*c.Contexts)[0] == discordgo.InteractionContextGuild)
	}
	o.calls = append(o.calls, call)
	if err := o.fail[guildID]; err != nil {
		return nil, err
	}
	return cmds, nil
}

func queueTestCommands(t *testing.T) *CommandBatch {
	t.Helper()
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
	return batch
}

func TestCommandBatchGoesGlobalWithPrimaryOnlyCommandsInTheGuild(t *testing.T) {
	rec := &overwriteRecorder{}
	global, guild, err := queueTestCommands(t).Flush(rec, "primary", "setup")
	if err != nil {
		t.Fatal(err)
	}
	if global != 6 || guild != 1 || len(rec.calls) != 2 {
		t.Fatalf("global=%d guild=%d calls=%+v", global, guild, rec.calls)
	}
	want := []string{"server", "link", "link-status", "unlink", "stats", "leaderboard"}
	g := rec.calls[0]
	if g.appID != "app" || g.guildID != "" || len(g.names) != len(want) {
		t.Fatalf("global list first, without the primary-only command: %+v", g)
	}
	for idx, name := range want {
		if g.names[idx] != name || !g.guildOnly[idx] {
			t.Fatalf("global command %d = %q (servers only %v), want %q: %+v", idx, g.names[idx], g.guildOnly[idx], name, g)
		}
	}
	if p := rec.calls[1]; p.guildID != "primary" || len(p.names) != 1 || p.names[0] != "setup" {
		t.Fatalf("the primary guild keeps only its own commands (the rest are removed there): %+v", p)
	}
}

func TestCommandBatchClearsThePrimaryGuildWhenNothingIsGuildOnly(t *testing.T) {
	rec := &overwriteRecorder{}
	if _, guild, err := queueTestCommands(t).Flush(rec, "primary"); err != nil || guild != 0 {
		t.Fatalf("guild=%d err=%v", guild, err)
	}
	if len(rec.calls) != 2 || rec.calls[1].guildID != "primary" || len(rec.calls[1].names) != 0 {
		t.Fatalf("the old guild copies must be removed: %+v", rec.calls)
	}
}

func TestCommandBatchKeepsTheGuildWhenTheGlobalListFails(t *testing.T) {
	rec := &overwriteRecorder{fail: map[string]error{"": errors.New("discord down")}}
	if _, _, err := queueTestCommands(t).Flush(rec, "primary", "setup"); err == nil {
		t.Fatal("expected an error")
	}
	if len(rec.calls) != 1 {
		t.Fatalf("a failed global overwrite must leave the primary guild's commands alone: %+v", rec.calls)
	}
}

func TestEmptyCommandBatchNeverWipesTheGuild(t *testing.T) {
	rec := &overwriteRecorder{}
	if global, guild, err := NewCommandBatch("app").Flush(rec, "guild"); err != nil || global+guild != 0 || len(rec.calls) != 0 {
		t.Fatalf("empty flush sent a request: global=%d guild=%d err=%v calls=%d", global, guild, err, len(rec.calls))
	}
	if err := RegisterSetupCommand(NewCommandBatch(""), "guild"); err == nil {
		t.Fatal("a batch without an application ID must not queue commands")
	}
}
