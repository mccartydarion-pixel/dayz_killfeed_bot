package discord

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type PointsCommandHandler struct {
	points  *repository.PointsRepository
	players *repository.PlayerRepository
	guilds  GuildStore
}

func NewPointsCommandHandler(points *repository.PointsRepository, players *repository.PlayerRepository, guilds GuildStore) *PointsCommandHandler {
	return &PointsCommandHandler{points: points, players: players, guilds: guilds}
}
func RegisterPointsCommands(session *discordgo.Session, guildID string) error {
	cmd := &discordgo.ApplicationCommand{Name: "points", Description: "Show Champion Points", Options: []*discordgo.ApplicationCommandOption{{Name: "player", Description: "Player name", Type: discordgo.ApplicationCommandOptionString}}}
	_, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, cmd)
	return err
}
func (h *PointsCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.points == nil || h.players == nil || h.guilds == nil || i == nil {
		respondEphemeral(s, i, "Champion Points are unavailable.")
		return
	}
	_, gid, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || gid == 0 {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	name := ""
	if len(i.ApplicationCommandData().Options) > 0 {
		name = strings.TrimSpace(i.ApplicationCommandData().Options[0].StringValue())
	}
	if name == "" {
		respondEphemeral(s, i, "Provide a player name until account linking is configured for this command.")
		return
	}
	pid, err := h.players.FindByDisplayName(context.Background(), gid, name)
	if err != nil {
		respondEphemeral(s, i, "Player not found.")
		return
	}
	p, err := h.points.Get(context.Background(), gid, pid)
	if err != nil {
		respondEphemeral(s, i, "No Champion Points recorded.")
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("🏆 **CHAMPION POINTS**\n\n%s\n\nSeason: **%d**\nLifetime: **%d**", name, p.Season, p.Lifetime))
}
