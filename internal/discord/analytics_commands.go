package discord

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/analytics"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type AnalyticsCommandHandler struct {
	repo   *repository.AnalyticsRepository
	guilds GuildStore
}

func NewAnalyticsCommandHandler(r *repository.AnalyticsRepository, g GuildStore) *AnalyticsCommandHandler {
	return &AnalyticsCommandHandler{repo: r, guilds: g}
}
func RegisterAnalyticsCommands(s *discordgo.Session, guildID string) error {
	applicationID, err := ApplicationID(s)
	if err != nil {
		return err
	}
	match := &discordgo.ApplicationCommand{Name: "matchup", Description: "Show player kill exchange", Options: []*discordgo.ApplicationCommandOption{{Name: "player", Description: "Player A", Type: discordgo.ApplicationCommandOptionString, Required: true}, {Name: "opponent", Description: "Player B", Type: discordgo.ApplicationCommandOptionString, Required: true}}}
	weapon := &discordgo.ApplicationCommand{Name: "weapon", Description: "Weapon analytics", Options: []*discordgo.ApplicationCommandOption{{Name: "stats", Description: "Show weapon stats", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "weapon", Description: "Weapon name", Type: discordgo.ApplicationCommandOptionString, Required: true}}}}}
	if _, err := s.ApplicationCommandCreate(applicationID, guildID, match); err != nil {
		return err
	}
	_, err = s.ApplicationCommandCreate(applicationID, guildID, weapon)
	return err
}
func (h *AnalyticsCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.repo == nil || h.guilds == nil || i == nil {
		respondEphemeral(s, i, "Analytics unavailable.")
		return
	}
	_, gid, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	data := i.ApplicationCommandData()
	if data.Name == "matchup" {
		a, b := data.Options[0].StringValue(), data.Options[1].StringValue()
		m, err := h.repo.Matchup(context.Background(), gid, a, b, analytics.ScopeLifetime, 0)
		if err != nil {
			respondEphemeral(s, i, "Could not load matchup.")
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("⚔️ **KILL EXCHANGE**\n\n%s — %d\n%s — %d\n\nTotal encounters: %d", m.PlayerA, m.AKills, m.PlayerB, m.BKills, m.Total))
		return
	}
	if data.Name == "weapon" && len(data.Options) > 0 {
		weapon := data.Options[0].Options[0].StringValue()
		w, err := h.repo.Weapon(context.Background(), gid, weapon, analytics.ScopeLifetime, 0)
		if err != nil {
			respondEphemeral(s, i, "Could not load weapon stats.")
			return
		}
		avg := "—"
		if w.AverageDistance != nil {
			avg = fmt.Sprintf("%.1fm", *w.AverageDistance)
		}
		respondEphemeral(s, i, fmt.Sprintf("🔫 **WEAPON STATS**\n\n%s\nPvP kills: %d\nUnique users: %d\nAverage distance: %s\nHeadshots: %d (%.1f%%)", strings.TrimSpace(w.Weapon), w.Kills, w.UniqueUsers, avg, w.Headshots, w.HeadshotRate))
	}
}
