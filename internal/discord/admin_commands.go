package discord

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/admin"
)

type AdminCommandHandler struct{ service *admin.Service }

func NewAdminCommandHandler(s *admin.Service) *AdminCommandHandler {
	return &AdminCommandHandler{service: s}
}
func RegisterAdminCommands(session *discordgo.Session, guildID string) error {
	perms := int64(discordgo.PermissionAdministrator | discordgo.PermissionManageServer)
	cmd := &discordgo.ApplicationCommand{Name: "admin", Description: "Champion operations and diagnostics", DefaultMemberPermissions: &perms, Options: []*discordgo.ApplicationCommandOption{{Name: "status", Description: "Show system status", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "diagnostics", Description: "Show sanitized diagnostics", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "pipeline", Description: "Show kill pipeline state", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "workers", Description: "Show worker state", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "permissions", Description: "Show Discord permission state", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "health", Description: "Show component health", Type: discordgo.ApplicationCommandOptionSubCommand}}}
	_, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, cmd)
	return err
}
func (h *AdminCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.service == nil || i == nil {
		respondEphemeral(s, i, "Admin diagnostics unavailable.")
		return
	}
	if !isAdminInteraction(i) {
		respondEphemeral(s, i, "Administrator or Manage Server permission required.")
		return
	}
	data := h.service.Status(context.Background())
	runtime, _ := data["runtime"].(map[string]any)
	var b strings.Builder
	b.WriteString("🏆 **CHAMPION SYSTEM STATUS**\n\n")
	if runtime != nil {
		fmt.Fprintf(&b, "Database: %v\nDiscord: %v\nNitrado: %v\nADM: %v\nOnline Players: %v\nPersistence Queue: %v\n", runtime["database_connected"], runtime["discord_connected"], runtime["nitrado_authenticated"], runtime["log_source_found"], runtime["online_players"], runtime["persistence_queue_depth"])
	}
	respondEphemeral(s, i, b.String())
}
