package discord

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// adminPerms is the minimum permission set required to run /setup.
const adminPerms = discordgo.PermissionAdministrator | discordgo.PermissionManageServer

// RegisterSetupCommand registers the admin-only /setup command with subcommands.
func RegisterSetupCommand(session *discordgo.Session, guildID string) error {
	if session == nil {
		return fmt.Errorf("discord session is nil")
	}

	defaultMemberPerms := int64(discordgo.PermissionManageServer) // hides it from normal members
	cmd := &discordgo.ApplicationCommand{
		Name:                     "setup",
		Description:              "Configure Champion Killfeed for this server",
		DefaultMemberPermissions: &defaultMemberPerms,
		Options: []*discordgo.ApplicationCommandOption{
			{Name: "status", Description: "Show Champion Killfeed configuration status", Type: discordgo.ApplicationCommandOptionSubCommand},
			{Name: "repair", Description: "Recreate any missing Champion Killfeed resources", Type: discordgo.ApplicationCommandOptionSubCommand},
			{Name: "reset", Description: "Remove Champion Killfeed configuration (requires confirmation)", Type: discordgo.ApplicationCommandOptionSubCommand},
		},
	}
	_, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, cmd)
	return err
}

// SetupHandler processes /setup interactions with admin enforcement.
type SetupHandler struct {
	manager *SetupManager
}

// NewSetupHandler creates a handler bound to a setup manager.
func NewSetupHandler(manager *SetupManager) *SetupHandler {
	return &SetupHandler{manager: manager}
}

// Handle processes an incoming /setup interaction.
func (h *SetupHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.GuildID == "" {
		return
	}

	if !isAdmin(s, i) {
		respondEphemeral(s, i, "⛔ You need Administrator or Manage Server permission to run /setup.")
		return
	}

	sub := ""
	if len(i.ApplicationCommandData().Options) > 0 {
		sub = i.ApplicationCommandData().Options[0].Name
	}

	slog.Info("component=discord", "msg", "setup started", "guild_id", i.GuildID, "sub", sub)

	switch sub {
	case "status":
		h.handleStatus(s, i)
	case "repair":
		h.handleSetup(s, i, true)
	case "reset":
		h.handleReset(s, i)
	default:
		h.handleSetup(s, i, false)
	}
}

func (h *SetupHandler) handleSetup(s *discordgo.Session, i *discordgo.InteractionCreate, repair bool) {
	setup, report, err := h.manager.EnsureConfigured(i.GuildID)
	if err != nil && report == nil {
		respondEphemeral(s, i, "❌ Setup failed: "+err.Error())
		return
	}
	if setup == nil {
		respondEphemeral(s, i, "❌ Setup failed.")
		return
	}

	var b strings.Builder
	if repair {
		b.WriteString("🔧 **Champion Killfeed Repair**\n\n")
	} else {
		b.WriteString("🏆 **Champion Killfeed Setup Complete**\n\n")
	}
	if h.manager.IsConfigured(i.GuildID) && len(report.Created) == 0 && len(report.Repaired) == 0 {
		b.WriteString("Champion Killfeed is already configured.\n\n")
	}
	fmt.Fprintf(&b, "Category: `%s`\n", CategoryName)
	writeLine(&b, "Server Status", setup.ServerStatusChannelID)
	writeLine(&b, "Killfeed", setup.KillfeedChannelID)
	writeLine(&b, "Online Players", setup.OnlinePlayersChannelID)
	writeLine(&b, "Leaderboards", setup.LeaderboardsChannelID)
	writeLine(&b, "Player Stats", setup.PlayerStatsChannelID)
	for name, reason := range report.Failed {
		fmt.Fprintf(&b, "❌ %s: %s\n", name, reason)
	}
	if len(report.Failed) > 0 {
		b.WriteString("\nFix permissions and run `/setup repair`.")
	} else {
		b.WriteString("\nChampion Killfeed is ready.")
	}

	respondEphemeral(s, i, b.String())
	slog.Info("component=discord", "msg", "setup complete", "guild_id", i.GuildID, "created", len(report.Created), "repaired", len(report.Repaired), "failed", len(report.Failed))
}

func (h *SetupHandler) handleStatus(s *discordgo.Session, i *discordgo.InteractionCreate) {
	setup, err := h.manager.store.Get(i.GuildID)
	if err != nil || setup == nil {
		respondEphemeral(s, i, "Champion Killfeed is not configured. Run `/setup`.")
		return
	}

	mark := func(id string) string {
		if id != "" {
			return "✅"
		}
		return "❌"
	}
	msg := fmt.Sprintf(
		"🏆 **Champion Killfeed Status**\n\n"+
			"Category: %s\nKillfeed: %s\nOnline Players: %s\nServer Status: %s\nLeaderboards: %s\nPlayer Stats: %s\n",
		mark(setup.CategoryID),
		mark(setup.KillfeedChannelID),
		mark(setup.OnlinePlayersChannelID),
		mark(setup.ServerStatusChannelID),
		mark(setup.LeaderboardsChannelID),
		mark(setup.PlayerStatsChannelID),
	)
	respondEphemeral(s, i, msg)
}

// handleReset requires an explicit confirmation button click before any deletion.
// Without a confirmed interaction, nothing is removed.
func (h *SetupHandler) handleReset(s *discordgo.Session, i *discordgo.InteractionCreate) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags:   discordgo.MessageFlagsEphemeral,
			Content: "⚠️ **Reset Champion Killfeed?**\nThis will remove the bot-created Champion Killfeed configuration.",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.Button{Label: "Confirm", Style: discordgo.DangerButton, CustomID: "champion_reset_confirm"},
					discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: "champion_reset_cancel"},
				}},
			},
		},
	})
	if err != nil {
		slog.Warn("component=discord", "msg", "reset confirmation prompt failed", "err", err.Error())
	}
}

// HandleResetConfirm processes the reset confirmation button. Only a confirmed
// click removes the stored configuration.
func (h *SetupHandler) HandleResetConfirm(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.GuildID == "" {
		return
	}
	if !isAdmin(s, i) {
		respondEphemeral(s, i, "⛔ Admin only.")
		return
	}
	customID := i.MessageComponentData().CustomID
	if customID == "champion_reset_cancel" {
		respondEphemeral(s, i, "Reset cancelled.")
		return
	}
	if customID != "champion_reset_confirm" {
		return
	}
	if err := h.manager.store.Delete(i.GuildID); err != nil {
		respondEphemeral(s, i, "❌ Reset failed: "+err.Error())
		return
	}
	slog.Info("component=discord", "msg", "setup reset confirmed", "guild_id", i.GuildID)
	respondEphemeral(s, i, "✅ Champion Killfeed configuration cleared. Run `/setup` to reconfigure.")
}

// isAdmin reports whether the invoking member has Administrator or Manage Server.
func isAdmin(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	if i.Member != nil && i.Member.Permissions&adminPerms != 0 {
		return true
	}
	if i.Member == nil || i.GuildID == "" || s == nil {
		return false
	}
	perms, err := s.UserChannelPermissions(i.Member.User.ID, i.ChannelID)
	if err != nil {
		return false
	}
	return perms&adminPerms != 0
}

func writeLine(b *strings.Builder, label, id string) {
	status := "✅"
	if id == "" {
		status = "❌"
	}
	fmt.Fprintf(b, "%s %s: <#%s>\n", status, label, id)
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: content},
	})
}

// RespondEphemeral is the exported ephemeral response helper for app-level routing.
func RespondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	respondEphemeral(s, i, content)
}
