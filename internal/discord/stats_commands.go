package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// StatsReader is the read surface the /stats and /leaderboard commands need.
type StatsReader interface {
	GetPlayerProfile(ctx context.Context, guildID int64, displayName string) (*repository.PlayerProfile, error)
	TopByKills(ctx context.Context, guildID int64, limit int) ([]repository.LeaderboardEntry, error)
	TopByKD(ctx context.Context, guildID int64, limit, minKills int) ([]repository.LeaderboardEntry, error)
	TopLongestKill(ctx context.Context, guildID int64, limit int) ([]repository.LeaderboardEntry, error)
}

// StatsCommandHandler serves /stats and /leaderboard.
type StatsCommandHandler struct {
	stats      StatsReader
	guilds     GuildStore
	guildID    string
	minKillsKD int
}

// NewStatsCommandHandler creates the handler. minKillsKD gates the K/D board.
func NewStatsCommandHandler(stats StatsReader, guilds GuildStore, guildID string) *StatsCommandHandler {
	return &StatsCommandHandler{stats: stats, guilds: guilds, guildID: guildID, minKillsKD: 5}
}

// RegisterStatsCommands registers /stats and /leaderboard.
func RegisterStatsCommands(session *discordgo.Session, guildID string) error {
	if session == nil {
		return fmt.Errorf("discord session is nil")
	}
	statsCmd := &discordgo.ApplicationCommand{
		Name:        "stats",
		Description: "Show a player's Champion profile",
		Options: []*discordgo.ApplicationCommandOption{{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "player",
			Description: "DayZ display name",
			Required:    true,
		}},
	}
	lbCmd := &discordgo.ApplicationCommand{
		Name:        "leaderboard",
		Description: "Show the Champion leaderboard",
		Options: []*discordgo.ApplicationCommandOption{{
			Type:        discordgo.ApplicationCommandOptionString,
			Name:        "type",
			Description: "kills | kd | longest",
			Required:    false,
		}},
	}
	if _, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, statsCmd); err != nil {
		return err
	}
	_, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, lbCmd)
	return err
}

// HandleStats processes /stats.
func (h *StatsCommandHandler) HandleStats(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.GuildID == "" || h.stats == nil || h.guilds == nil {
		respondEphemeral(s, i, "Stats are unavailable until the database is connected.")
		return
	}
	name := ""
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "player" {
			name = strings.TrimSpace(opt.StringValue())
		}
	}
	if name == "" {
		respondEphemeral(s, i, "Provide a player name.")
		return
	}

	_, guildRowID, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || guildRowID == 0 {
		respondEphemeral(s, i, "This server is not configured. Run `/setup` first.")
		return
	}

	prof, err := h.stats.GetPlayerProfile(context.Background(), guildRowID, name)
	if err != nil {
		slog.Warn("component=discord", "msg", "stats query failed", "err", err.Error())
		respondEphemeral(s, i, "Could not load stats right now.")
		return
	}
	if prof == nil {
		respondEphemeral(s, i, fmt.Sprintf("No record for player `%s`.", name))
		return
	}

	longest := "—"
	if prof.LongestKill != nil {
		longest = fmt.Sprintf("%.1fm", *prof.LongestKill)
	}
	msg := fmt.Sprintf(
		"🏆 **CHAMPION PLAYER PROFILE**\n\n"+
			"**Player**\n%s\n\n"+
			"**Kills**\n%d\n\n"+
			"**Deaths**\n%d\n\n"+
			"**K/D**\n%.2f\n\n"+
			"**Longest Kill**\n%s\n\n"+
			"**Last Seen**\n<t:%d:R>",
		prof.DisplayName, prof.Kills, prof.Deaths, prof.KD(), longest, prof.LastSeen.Unix(),
	)
	respondEphemeral(s, i, msg)
}

// HandleLeaderboard processes /leaderboard.
func (h *StatsCommandHandler) HandleLeaderboard(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.GuildID == "" || h.stats == nil || h.guilds == nil {
		respondEphemeral(s, i, "Leaderboard is unavailable until the database is connected.")
		return
	}
	kind := "kills"
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "type" {
			kind = strings.ToLower(strings.TrimSpace(opt.StringValue()))
		}
	}

	_, guildRowID, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || guildRowID == 0 {
		respondEphemeral(s, i, "This server is not configured. Run `/setup` first.")
		return
	}

	var entries []repository.LeaderboardEntry
	var title string
	switch kind {
	case "kd":
		title = "K/D"
		entries, err = h.stats.TopByKD(context.Background(), guildRowID, 10, h.minKillsKD)
	case "longest":
		title = "Longest Kill"
		entries, err = h.stats.TopLongestKill(context.Background(), guildRowID, 10)
	default:
		title = "Kills"
		entries, err = h.stats.TopByKills(context.Background(), guildRowID, 10)
	}
	if err != nil {
		slog.Warn("component=discord", "msg", "leaderboard query failed", "err", err.Error())
		respondEphemeral(s, i, "Could not load the leaderboard right now.")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🏆 **CHAMPION LEADERBOARD — %s**\n\n", strings.ToUpper(title))
	if len(entries) == 0 {
		b.WriteString("_No data yet._")
	}
	for idx, e := range entries {
		fmt.Fprintf(&b, "%d. %s — %s\n", idx+1, e.DisplayName, e.Value)
	}
	if kind == "kd" {
		fmt.Fprintf(&b, "\n_min %d kills to qualify_", h.minKillsKD)
	}
	respondEphemeral(s, i, b.String())
}
