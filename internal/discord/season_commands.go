package discord

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type SeasonCommandStore interface {
	GetActiveSeason(context.Context, int64) (*repository.Season, error)
	Start(context.Context, int64, string, time.Time) (*repository.Season, error)
	FinalizeSeason(context.Context, int64, int64, time.Time) (*repository.SeasonResult, error)
	GetSeasonHistory(context.Context, int64, int) ([]repository.Season, error)
}

type SeasonCommandHandler struct {
	seasons SeasonCommandStore
	guilds  GuildStore
}

func NewSeasonCommandHandler(seasons SeasonCommandStore, guilds GuildStore) *SeasonCommandHandler {
	return &SeasonCommandHandler{seasons: seasons, guilds: guilds}
}

func RegisterSeasonCommands(session *discordgo.Session, guildID string) error {
	if session == nil {
		return fmt.Errorf("discord session is nil")
	}
	cmd := &discordgo.ApplicationCommand{Name: "season", Description: "View and manage Champion seasons", Options: []*discordgo.ApplicationCommandOption{
		{Name: "status", Description: "Show the active season", Type: discordgo.ApplicationCommandOptionSubCommand},
		{Name: "start", Description: "Start a new season", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "name", Description: "Season name", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		{Name: "end", Description: "Finalize the active season", Type: discordgo.ApplicationCommandOptionSubCommand},
		{Name: "history", Description: "Show recent seasons", Type: discordgo.ApplicationCommandOptionSubCommand},
	}}
	_, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, cmd)
	return err
}

func (h *SeasonCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.GuildID == "" || h.seasons == nil || h.guilds == nil {
		respondEphemeral(s, i, "Seasons are unavailable until the database is connected.")
		return
	}
	_, guildID, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || guildID == 0 {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		return
	}
	sub := opts[0].Name
	switch sub {
	case "status":
		h.status(s, i, guildID)
	case "history":
		h.history(s, i, guildID)
	case "start":
		if !isAdminInteraction(i) {
			respondEphemeral(s, i, "Administrator or Manage Server permission required.")
			return
		}
		name := opts[0].Options[0].StringValue()
		season, err := h.seasons.Start(context.Background(), guildID, strings.TrimSpace(name), time.Now().UTC())
		if err != nil {
			respondEphemeral(s, i, "Cannot start season: "+err.Error())
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("🏆 **CHAMPION SEASON STARTED**\n\n**%s**\nStarted <t:%d:R>", season.Name, season.StartsAt.Unix()))
	case "end":
		if !isAdminInteraction(i) {
			respondEphemeral(s, i, "Administrator or Manage Server permission required.")
			return
		}
		active, err := h.seasons.GetActiveSeason(context.Background(), guildID)
		if err != nil || active == nil {
			respondEphemeral(s, i, "No active season.")
			return
		}
		result, err := h.seasons.FinalizeSeason(context.Background(), guildID, active.ID, time.Now().UTC())
		if err != nil {
			respondEphemeral(s, i, "Cannot finalize season: "+err.Error())
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("👑 **%s COMPLETE**\n\nTop player kills: %d\nTop faction kills: %d\nLongest kill: %.1fm", active.Name, result.TopPlayerKills, result.TopFactionKills, result.LongestKillValue))
	}
}
func (h *SeasonCommandHandler) status(s *discordgo.Session, i *discordgo.InteractionCreate, guildID int64) {
	active, err := h.seasons.GetActiveSeason(context.Background(), guildID)
	if err != nil || active == nil {
		respondEphemeral(s, i, "No active season.")
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("🏆 **CHAMPION SEASON**\n\n%s\nStatus: **%s**\nStarted: <t:%d:R>", active.Name, active.Status, active.StartsAt.Unix()))
}
func (h *SeasonCommandHandler) history(s *discordgo.Session, i *discordgo.InteractionCreate, guildID int64) {
	rows, err := h.seasons.GetSeasonHistory(context.Background(), guildID, 5)
	if err != nil {
		respondEphemeral(s, i, "Could not load season history.")
		return
	}
	var b strings.Builder
	b.WriteString("🏆 **CHAMPION SEASON HISTORY**\n\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "%s — %s\n", row.Name, row.Status)
	}
	if len(rows) == 0 {
		b.WriteString("No seasons yet.")
	}
	respondEphemeral(s, i, b.String())
}
func isAdminInteraction(i *discordgo.InteractionCreate) bool {
	return i != nil && i.Member != nil && (i.Member.Permissions&(discordgo.PermissionAdministrator|discordgo.PermissionManageServer)) != 0
}
