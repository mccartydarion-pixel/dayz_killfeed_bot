package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// adminPerms is the minimum permission set required to run /setup.
const adminPerms = discordgo.PermissionAdministrator | discordgo.PermissionManageServer

// RegisterSetupCommand registers the admin-only /setup command with subcommands.
func RegisterSetupCommand(session *discordgo.Session, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("discord session is nil")
	}

	defaultMemberPerms := int64(discordgo.PermissionManageServer) // hides it from normal members
	cmd := &discordgo.ApplicationCommand{
		Name:                     "setup",
		Description:              "Configure Champion Killfeed for this server",
		DefaultMemberPermissions: &defaultMemberPerms,
		Options: []*discordgo.ApplicationCommandOption{
			{Name: "run", Description: "Set up Champion's Discord channels", Type: discordgo.ApplicationCommandOptionSubCommand},
			{Name: "status", Description: "Show Champion Killfeed configuration status", Type: discordgo.ApplicationCommandOptionSubCommand},
			{Name: "repair", Description: "Repair Champion's Discord channel layout", Type: discordgo.ApplicationCommandOptionSubCommand},
			{Name: "reset", Description: "Remove Champion Killfeed configuration (requires confirmation)", Type: discordgo.ApplicationCommandOptionSubCommand},
			{Name: "verified-role", Description: "Set the role auto-assigned when a /link request is verified", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "role_id", Description: "Discord role ID", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		},
	}
	_, err = session.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}

// SetupLayoutResult summarizes one /setup run of the Channel System V2
// layout engine across the guild's installations.
type SetupLayoutResult struct {
	Installations int
	Created       []string
	Reused        []string
	Remapped      []string
	Preserved     []string
	Broken        []string
	Blocked       []string
	Retirable     int
}

var (
	// ErrNoInstallation: the guild has no Champion installation, so there is
	// no channel layout to apply.
	ErrNoInstallation = errors.New("no Champion installation for this Discord server")
	// ErrMissingManageChannels: the bot cannot create channels here.
	ErrMissingManageChannels = errors.New("missing Manage Channels permission")
)

// SetupLayoutFunc applies the V2 channel layout for a Discord guild - the
// same engine the website's one-click setup and repair use.
type SetupLayoutFunc func(ctx context.Context, discordGuildID string) (SetupLayoutResult, error)

// SetupHandler processes /setup interactions with admin enforcement.
type SetupHandler struct {
	manager     *SetupManager
	guilds      GuildStore
	welcomeRepo *repository.WelcomeRepository
	layout      SetupLayoutFunc
}

// SetLayout attaches the V2 layout engine. Without it /setup creates nothing.
func (h *SetupHandler) SetLayout(fn SetupLayoutFunc) {
	if h != nil {
		h.layout = fn
	}
}

// NewSetupHandler creates a handler bound to a setup manager.
func NewSetupHandler(manager *SetupManager, extras ...any) *SetupHandler {
	h := &SetupHandler{manager: manager}
	for _, extra := range extras {
		switch value := extra.(type) {
		case GuildStore:
			h.guilds = value
		case *repository.WelcomeRepository:
			h.welcomeRepo = value
		}
	}
	return h
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
	case "run":
		h.handleSetup(s, i, false)
	case "repair":
		h.handleSetup(s, i, true)
	case "reset":
		h.handleReset(s, i)
	case "verified-role":
		h.handleVerifiedRole(s, i)
	default:
		h.handleSetup(s, i, false)
	}
}

// handleVerifiedRole stores the Discord role ID auto-assigned when a /link
// request completes verification (either the ADM disconnect/reconnect
// challenge or an admin's manual approval).
func (h *SetupHandler) handleVerifiedRole(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !isAdmin(s, i) {
		respondEphemeral(s, i, "⛔ You need Administrator or Manage Server permission to run /setup.")
		return
	}
	roleID := strings.TrimSpace(optionString(i.ApplicationCommandData().Options[0], "role_id"))
	if roleID == "" {
		respondEphemeral(s, i, "❌ role_id is required.")
		return
	}
	setup, err := h.manager.store.Get(i.GuildID)
	if err != nil {
		respondEphemeral(s, i, "❌ Could not load configuration.")
		return
	}
	if setup == nil {
		setup = &GuildSetup{GuildID: i.GuildID}
	}
	setup.VerifiedRoleID = roleID
	if err := h.manager.store.Save(*setup); err != nil {
		respondEphemeral(s, i, "❌ Could not save the verified role.")
		return
	}
	respondEphemeral(s, i, "✅ Verified role set. It will be assigned automatically when a /link request is verified.")
}

// handleSetup defers the interaction immediately so Discord's ~3 second ack
// window can never expire while EnsureConfigured does Discord/DB work; the
// result is delivered later via an edit to the deferred response.
func (h *SetupHandler) handleSetup(s *discordgo.Session, i *discordgo.InteractionCreate, repair bool) {
	action := "run"
	if repair {
		action = "repair"
	}
	slog.Info("component=setup", "action", action, "stage", "received", "guild_id", i.GuildID)

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		slog.Warn("component=setup", "action", action, "stage", "deferred", "error_class", "discord_unavailable", "err", err.Error())
		return
	}
	slog.Info("component=setup", "action", action, "stage", "deferred", "guild_id", i.GuildID)

	if h.layout == nil {
		h.editEphemeral(s, i, "❌ Channel setup is unavailable right now. Try again later or use Setup on the Champion website.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := h.layout(ctx, i.GuildID)
	h.editEphemeral(s, i, FormatSetupLayoutResult(result, err, repair))
	slog.Info("component=setup", "action", action, "stage", "completed", "guild_id", i.GuildID, "installations", result.Installations,
		"created", len(result.Created), "broken", len(result.Broken), "error", err != nil)
}

// FormatSetupLayoutResult renders the /setup reply.
func FormatSetupLayoutResult(r SetupLayoutResult, err error, repair bool) string {
	switch {
	case errors.Is(err, ErrNoInstallation):
		return "ℹ️ This Discord server is not connected to Champion yet.\nConnect it in **Setup** on the Champion website, then run `/setup` again."
	case errors.Is(err, ErrMissingManageChannels):
		return "❌ Champion needs the **Manage Channels** permission to set up its channels. Grant it and run `/setup repair`."
	case err != nil:
		return "❌ Setup failed. Try `/setup repair` again in a moment."
	}
	var b strings.Builder
	if repair {
		b.WriteString("🔧 **Champion Discord Layout Repaired**\n\n")
	} else {
		b.WriteString("🏆 **Champion Discord Layout Ready**\n\n")
	}
	line := func(label string, items []string) {
		if len(items) > 0 {
			fmt.Fprintf(&b, "**%s:** %s\n", label, strings.Join(items, ", "))
		}
	}
	line("Created", r.Created)
	line("Reused", r.Reused)
	line("Remapped", r.Remapped)
	line("Preserved (your channels)", r.Preserved)
	line("Needs attention", r.Broken)
	line("Not available yet", r.Blocked)
	if len(r.Created) == 0 && len(r.Remapped) == 0 && len(r.Broken) == 0 {
		b.WriteString("Everything was already in place.\n")
	}
	if r.Retirable > 0 {
		fmt.Fprintf(&b, "\n%d old Champion channel(s) are no longer used. Review and remove them in **Setup → Discord Channels** on the website.\n", r.Retirable)
	}
	return strings.TrimRight(b.String(), "\n")
}

// editEphemeral edits a previously deferred ephemeral interaction response.
func (h *SetupHandler) editEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
		slog.Warn("component=setup", "msg", "response edit failed", "err", err.Error())
	}
}

// syncWelcomeChannel is kept for the welcome system's own configuration.
func (h *SetupHandler) syncWelcomeChannel(discordGuildID, channelID string) {
	if h == nil || h.guilds == nil || h.welcomeRepo == nil || channelID == "" {
		return
	}
	_, guildID, err := h.guilds.GetGuild(context.Background(), discordGuildID)
	if err != nil || guildID == 0 {
		return
	}
	cfg, err := h.welcomeRepo.Get(context.Background(), guildID)
	if err != nil || cfg == nil {
		cfg = &repository.WelcomeConfig{GuildID: guildID, Enabled: true}
	}
	if cfg.ChannelID == channelID {
		return
	}
	cfg.ChannelID = channelID
	if err := h.welcomeRepo.Upsert(context.Background(), *cfg); err != nil {
		slog.Warn("component=discord", "msg", "welcome channel persistence sync failed", "guild_id", discordGuildID, "error_class", "database_unavailable", "err", err.Error())
	}
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
			"Category: %s\nWelcome: %s\nKillfeed: %s\nOnline Players: %s\nServer Status: %s\nLeaderboards: %s\nPlayer Stats: %s\nLink Username: %s\nADM Monitor: %s\nDeath Feed: %s\nVerified Role: %s\n",
		mark(setup.CategoryID),
		mark(setup.WelcomeChannelID),
		mark(setup.KillfeedChannelID),
		mark(setup.OnlinePlayersChannelID),
		mark(setup.ServerStatusChannelID),
		mark(setup.LeaderboardsChannelID),
		mark(setup.PlayerStatsChannelID),
		mark(setup.LinkPanelChannelID),
		mark(setup.ADMMonitorChannelID),
		mark(setup.DeathChannelID),
		mark(setup.VerifiedRoleID),
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
