package discord

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type WarCommandHandler struct {
	wars    *repository.PostgresWarRepository
	guilds  GuildStore
	seasons *repository.SeasonRepository
}

func NewWarCommandHandler(wars *repository.PostgresWarRepository, guilds GuildStore, seasons *repository.SeasonRepository) *WarCommandHandler {
	return &WarCommandHandler{wars: wars, guilds: guilds, seasons: seasons}
}
func RegisterWarCommands(session *discordgo.Session, guildID string) error {
	war := &discordgo.ApplicationCommandOption{Name: "war", Description: "Manage faction wars", Type: discordgo.ApplicationCommandOptionSubCommandGroup, Options: []*discordgo.ApplicationCommandOption{{Name: "challenge", Description: "Challenge two factions", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "faction_a", Description: "First faction ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}, {Name: "faction_b", Description: "Second faction ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "accept", Description: "Accept a challenge", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "War ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "decline", Description: "Decline a challenge", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "War ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "end", Description: "End an active war", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "War ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "cancel", Description: "Cancel a war", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "War ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "history", Description: "Show war history", Type: discordgo.ApplicationCommandOptionSubCommand}}}
	cmd := &discordgo.ApplicationCommand{Name: "faction", Description: "Faction systems", Options: []*discordgo.ApplicationCommandOption{war}}
	_, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, cmd)
	return err
}
func (h *WarCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.wars == nil || h.guilds == nil || i == nil {
		respondEphemeral(s, i, "Faction wars are unavailable.")
		return
	}
	if !isAdminInteraction(i) {
		respondEphemeral(s, i, "Administrator or Manage Server permission required.")
		return
	}
	_, gid, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || gid == 0 {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	group := i.ApplicationCommandData().Options[0]
	if len(group.Options) == 0 {
		return
	}
	sub := group.Options[0]
	ctx := context.Background()
	switch sub.Name {
	case "challenge":
		a, b := optionInt(sub, "faction_a"), optionInt(sub, "faction_b")
		seasonID := int64(0)
		if h.seasons != nil {
			if season, _ := h.seasons.GetActiveSeason(ctx, gid); season != nil {
				seasonID = season.ID
			}
		}
		war, err := h.wars.CreateChallenge(ctx, gid, seasonID, a, b, 0)
		if err != nil {
			respondEphemeral(s, i, "Could not create war: "+err.Error())
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("⚔️ War challenge created: **%d** (%d vs %d)", war.ID, war.FactionAID, war.FactionBID))
	case "accept":
		err = h.wars.AcceptWar(ctx, gid, optionInt(sub, "id"))
	case "decline":
		err = h.wars.DeclineWar(ctx, gid, optionInt(sub, "id"))
	case "end":
		err = h.wars.EndWar(ctx, gid, optionInt(sub, "id"))
	case "cancel":
		err = h.wars.CancelWar(ctx, gid, optionInt(sub, "id"))
	case "history":
		rows, historyErr := h.wars.GetWarHistory(ctx, gid, 10)
		if historyErr != nil {
			respondEphemeral(s, i, "Could not load war history.")
			return
		}
		var b strings.Builder
		b.WriteString("⚔️ **FACTION WAR HISTORY**\n\n")
		for _, war := range rows {
			fmt.Fprintf(&b, "%d. %d vs %d — %s\n", war.ID, war.FactionAID, war.FactionBID, war.Status)
		}
		respondEphemeral(s, i, b.String())
		return
	}
	if err != nil {
		respondEphemeral(s, i, "War operation failed: "+err.Error())
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("⚔️ War %s.", strings.ToLower(sub.Name)))
}
