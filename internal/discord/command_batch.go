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

// Flush registers the batch in at most two requests. Every command is global
// and usable in servers only (never in DMs), so every Discord server the bot
// is in gets it; the commands named in primaryOnly become guild commands of
// primaryGuildID instead, and that guild's list is replaced with just them,
// which also removes the copies registered there when every command was a
// guild command. Without a primary guild nothing is guild-only. The global
// list goes first: if it fails, the primary guild keeps the commands it has.
// An empty batch sends nothing, so a misconfigured start never wipes anything.
func (b *CommandBatch) Flush(s commandOverwriter, primaryGuildID string, primaryOnly ...string) (global, guild int, err error) {
	cmds := b.Commands()
	if len(cmds) == 0 {
		return 0, 0, nil
	}
	if b.appID == "" {
		return 0, 0, fmt.Errorf("discord application ID is not available")
	}
	only := make(map[string]bool, len(primaryOnly))
	if primaryGuildID != "" {
		for _, name := range primaryOnly {
			only[name] = true
		}
	}
	serversOnly := []discordgo.InteractionContextType{discordgo.InteractionContextGuild}
	everywhere := []*discordgo.ApplicationCommand{}
	primary := []*discordgo.ApplicationCommand{}
	for _, cmd := range cmds {
		if only[cmd.Name] {
			primary = append(primary, cmd)
			continue
		}
		cmd.Contexts = &serversOnly
		everywhere = append(everywhere, cmd)
	}
	if _, err := s.ApplicationCommandBulkOverwrite(b.appID, "", everywhere); err != nil {
		return 0, 0, fmt.Errorf("global commands: %w", err)
	}
	if primaryGuildID == "" {
		return len(everywhere), 0, nil
	}
	if _, err := s.ApplicationCommandBulkOverwrite(b.appID, primaryGuildID, primary); err != nil {
		return len(everywhere), 0, fmt.Errorf("commands of guild %s: %w", primaryGuildID, err)
	}
	return len(everywhere), len(primary), nil
}
