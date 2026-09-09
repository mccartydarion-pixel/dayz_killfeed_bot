package discord

import (
	"context"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"strings"
)

type ServerCommandHandler struct {
	servers *repository.ServerRepository
	guilds  GuildStore
}

func NewServerCommandHandler(s *repository.ServerRepository, g GuildStore) *ServerCommandHandler {
	return &ServerCommandHandler{servers: s, guilds: g}
}
func RegisterServerCommands(s *discordgo.Session, guildID string) error {
	applicationID, err := ApplicationID(s)
	if err != nil {
		return err
	}
	cmd := &discordgo.ApplicationCommand{Name: "server", Description: "Manage connected game servers", Options: []*discordgo.ApplicationCommandOption{{Name: "status", Description: "Show connected server status", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "diagnostics", Description: "Show server diagnostics", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "services", Description: "Show configured services", Type: discordgo.ApplicationCommandOptionSubCommand}}}
	_, err = s.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}
func (h *ServerCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.servers == nil || h.guilds == nil || i == nil {
		respondEphemeral(s, i, "Server management is unavailable.")
		return
	}
	_, gid, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || gid == 0 {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	rows, err := h.servers.ListActive(context.Background())
	if err != nil {
		respondEphemeral(s, i, "Could not load server connections.")
		return
	}
	var b strings.Builder
	b.WriteString("🏆 **CHAMPION SERVER CONNECTIONS**\n\n")
	count := 0
	for _, row := range rows {
		if row.GuildID != gid {
			continue
		}
		count++
		fmt.Fprintf(&b, "**%s**\n%s • %s\nStatus: %s\n\n", row.DisplayName, row.Game, row.Platform, row.Status)
	}
	if count == 0 {
		b.WriteString("No connected game server.\nUse the secure onboarding flow when enabled.")
	}
	respondEphemeral(s, i, b.String())
}
