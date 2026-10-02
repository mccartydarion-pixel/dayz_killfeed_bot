package discord

import (
	"fmt"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// CommandRegistrar is where the Register* functions create their slash
// commands: the live session (one request per command), or a CommandBatch.
type CommandRegistrar interface {
	ApplicationCommandCreate(appID, guildID string, cmd *discordgo.ApplicationCommand, options ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error)
}

// CommandBatch collects slash commands and sends them in one bulk overwrite.
// Discord allows about five command creates per 20 s, so creating the ~21
// commands one by one took ~80 s after every deploy; one overwrite takes one
// request. The overwrite also removes guild commands no longer registered.
type CommandBatch struct {
	appID string
	mu    sync.Mutex
	cmds  []*discordgo.ApplicationCommand
}

// NewCommandBatch returns an empty batch for the application.
func NewCommandBatch(appID string) *CommandBatch {
	return &CommandBatch{appID: appID}
}

// ApplicationCommandCreate queues cmd. A later command with the same name
// replaces the earlier one, as a create would at Discord.
func (b *CommandBatch) ApplicationCommandCreate(_, _ string, cmd *discordgo.ApplicationCommand, _ ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error) {
	if cmd == nil {
		return nil, fmt.Errorf("discord command is nil")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for idx, existing := range b.cmds {
		if existing.Name == cmd.Name {
			b.cmds[idx] = cmd
			return cmd, nil
		}
	}
	b.cmds = append(b.cmds, cmd)
	return cmd, nil
}

// Commands returns the queued commands in registration order.
func (b *CommandBatch) Commands() []*discordgo.ApplicationCommand {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*discordgo.ApplicationCommand(nil), b.cmds...)
}

// commandOverwriter is the one call Flush needs from the session.
type commandOverwriter interface {
	ApplicationCommandBulkOverwrite(appID, guildID string, commands []*discordgo.ApplicationCommand, options ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error)
}

// Flush replaces the guild's commands with the batch in one request. An
// empty batch sends nothing, so a misconfigured start never wipes the guild.
func (b *CommandBatch) Flush(s commandOverwriter, guildID string) (int, error) {
	cmds := b.Commands()
	if len(cmds) == 0 {
		return 0, nil
	}
	if b.appID == "" {
		return 0, fmt.Errorf("discord application ID is not available")
	}
	if _, err := s.ApplicationCommandBulkOverwrite(b.appID, guildID, cmds); err != nil {
		return 0, err
	}
	return len(cmds), nil
}
