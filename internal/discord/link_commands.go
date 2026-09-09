package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/linking"
)

// LinkCommandHandler exposes the secure pending-only account linking workflow.
type LinkCommandHandler struct {
	service *linking.LinkVerificationService
	guilds  GuildStore
}

func NewLinkCommandHandler(service *linking.LinkVerificationService, guilds GuildStore) *LinkCommandHandler {
	return &LinkCommandHandler{service: service, guilds: guilds}
}

func RegisterLinkCommands(session *discordgo.Session, guildID string) error {
	if session == nil {
		return fmt.Errorf("discord session nil")
	}
	cmds := []*discordgo.ApplicationCommand{
		{Name: "link", Description: "Request PlayStation username verification", Options: []*discordgo.ApplicationCommandOption{{Name: "username", Description: "PlayStation Username", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		{Name: "link-status", Description: "Show your Champion account link status"},
		{Name: "unlink", Description: "Request removal of your Champion account link"},
	}
	for _, cmd := range cmds {
		if _, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, cmd); err != nil {
			return err
		}
	}
	return nil
}

func (h *LinkCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.GuildID == "" || i.Member == nil || i.Member.User == nil {
		return
	}
	if h.service == nil || h.guilds == nil {
		respondEphemeral(s, i, "Account linking requires the database to be configured.")
		return
	}
	_, guildID, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || guildID == 0 {
		respondEphemeral(s, i, "Run `/setup` before linking your account.")
		return
	}
	name := i.ApplicationCommandData().Name
	switch name {
	case "link":
		username := ""
		for _, opt := range i.ApplicationCommandData().Options {
			if opt.Name == "username" {
				username = strings.TrimSpace(opt.StringValue())
			}
		}
		link, err := h.service.Request(context.Background(), guildID, i.Member.User.ID, username)
		if err != nil {
			switch {
			case errors.Is(err, linking.ErrPlayerNotFound):
				respondEphemeral(s, i, "❌ **PLAYER NOT FOUND**\nChampion has not seen that PlayStation username on the DayZ server yet.")
			case errors.Is(err, linking.ErrAlreadyLinked):
				respondEphemeral(s, i, "⚠️ **ACCOUNT ALREADY LINKED**\nUse `/unlink` before linking another account.")
			case errors.Is(err, linking.ErrPlayerClaimed):
				respondEphemeral(s, i, "❌ **ALREADY LINKED**\nThat PlayStation account is already linked to another Discord member.")
			default:
				respondEphemeral(s, i, "❌ Could not create a pending link right now.")
			}
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("🟡 **PENDING VERIFICATION**\n\nPlayStation\n%s\n\nChampion cannot verify ownership from a typed name alone. Your request expires <t:%d:R> and will remain unverified until a real ownership proof is available.", link.RequestedName, link.ExpiresAt.Unix()))
	case "link-status":
		link, err := h.serviceStatus(context.Background(), guildID, i.Member.User.ID)
		if err != nil || link == nil {
			respondEphemeral(s, i, "No Champion account link found.")
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("🏆 **CHAMPION ACCOUNT**\n\nPlayStation\n%s\n\nStatus\n%s", link.RequestedName, statusLabel(link.Status)))
	case "unlink":
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Flags:   discordgo.MessageFlagsEphemeral,
				Content: "⚠️ Confirm unlinking your Champion account?\n\nThis removes account ownership only; historical stats remain.",
				Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.Button{Label: "Unlink Account", Style: discordgo.DangerButton, CustomID: "champion_unlink_confirm:" + i.Member.User.ID},
					discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: "champion_unlink_cancel:" + i.Member.User.ID},
				}}},
			},
		})
	}
}

// HandleComponent handles unlink confirmation without allowing another user to
// confirm someone else's destructive action.
func (h *LinkCommandHandler) HandleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.Member == nil || i.Member.User == nil || i.GuildID == "" {
		return
	}
	parts := strings.SplitN(i.MessageComponentData().CustomID, ":", 2)
	if len(parts) != 2 || parts[1] != i.Member.User.ID {
		respondEphemeral(s, i, "⛔ This confirmation belongs to another member.")
		return
	}
	if parts[0] == "champion_unlink_cancel" {
		respondEphemeral(s, i, "Unlink cancelled.")
		return
	}
	if parts[0] != "champion_unlink_confirm" || h.service == nil || h.guilds == nil {
		return
	}
	_, guildID, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || guildID == 0 {
		respondEphemeral(s, i, "This server is not configured.")
		return
	}
	if err := h.service.Unlink(context.Background(), guildID, i.Member.User.ID); err != nil {
		respondEphemeral(s, i, "❌ Could not unlink your account right now.")
		return
	}
	respondEphemeral(s, i, "✅ Account unlinked. Historical stats were preserved.")
}

func (h *LinkCommandHandler) serviceStatus(ctx context.Context, guildID int64, userID string) (*linking.LinkRecord, error) {
	return h.service.Status(ctx, guildID, userID)
}

func statusLabel(status string) string {
	if status == linking.StatusVerified {
		return "✅ VERIFIED"
	}
	return "🟡 PENDING VERIFICATION"
}
