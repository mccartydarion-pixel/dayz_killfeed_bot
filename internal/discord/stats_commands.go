package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// StatsReader is the read surface the /stats and /leaderboard commands need.
type StatsReader interface {
	GetPlayerProfile(ctx context.Context, guildID int64, displayName string) (*repository.PlayerProfile, error)
	TopByKills(ctx context.Context, guildID int64, limit int) ([]repository.LeaderboardEntry, error)
	TopByKD(ctx context.Context, guildID int64, limit, minKills int) ([]repository.LeaderboardEntry, error)
	TopByPvPKD(ctx context.Context, guildID int64, limit, minKills int) ([]repository.LeaderboardEntry, error)
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
func RegisterStatsCommands(session CommandRegistrar, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
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
			Description: "kills | kd | pvpkd | longest",
			Required:    false,
		}},
	}
	if _, err := session.ApplicationCommandCreate(applicationID, guildID, statsCmd); err != nil {
		return err
	}
	_, err = session.ApplicationCommandCreate(applicationID, guildID, lbCmd)
	return err
}

// HandleStats processes /stats.
func (h *StatsCommandHandler) HandleStats(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.GuildID == "" || h.stats == nil || h.guilds == nil {
		respondEphemeral(s, i, ReplyAreUnavailable("Stats"))
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
		respondEphemeral(s, i, ReplyNotSetUp)
		return
	}

	prof, err := h.stats.GetPlayerProfile(context.Background(), guildRowID, name)
	if err != nil {
		slog.Warn("component=discord", "msg", "stats query failed", "err", err.Error())
		respondEphemeral(s, i, ReplyCouldNot("load stats"))
		return
	}
	if prof == nil {
		respondEphemeral(s, i, fmt.Sprintf("No record for player `%s`.", name))
		return
	}

	respondEphemeral(s, i, formatPlayerProfile(prof))
}

// formatPlayerProfile is the single authoritative rendering for a player
// profile, shared by /stats and the public "My Stats"/"Search Player" panels.
//
// Deaths is every death, with the PvP/PvE split on the line below it. K/D (PvP) is kills per
// death caused by another player; K/D (overall) is kills per death of any kind, the figure this
// profile has always called K/D (internal/deathstats).
func formatPlayerProfile(prof *repository.PlayerProfile) string {
	longest := "—"
	if prof.LongestKill != nil {
		longest = fmt.Sprintf("%.1fm", *prof.LongestKill)
	}
	return fmt.Sprintf(
		"🏆 **Champion player profile**\n\n"+
			"**Player**\n%s\n\n"+
			"**Kills**\n%d\n\n"+
			"**Deaths**\n%d\n%s\n\n"+
			"**K/D (PvP)**\n%s\n\n"+
			"**K/D (overall)**\n%.2f\n\n"+
			"**Longest Kill**\n%s\n\n"+
			"**Last Seen**\n<t:%d:R>",
		prof.DisplayName, prof.Kills, prof.Deaths, presentation.DeathSplit(prof.PvPDeaths, prof.PvEDeaths()),
		presentation.FormatKD(prof.PvPKD()), prof.KD(), longest, prof.LastSeen.Unix(),
	)
}

// HandleLeaderboard processes /leaderboard.
func (h *StatsCommandHandler) HandleLeaderboard(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.GuildID == "" || h.stats == nil || h.guilds == nil {
		respondEphemeral(s, i, ReplyIsUnavailable("The leaderboard"))
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
		respondEphemeral(s, i, ReplyNotSetUp)
		return
	}

	var entries []repository.LeaderboardEntry
	var title string
	switch kind {
	case "kd":
		title = "K/D"
		entries, err = h.stats.TopByKD(context.Background(), guildRowID, 10, h.minKillsKD)
	case "pvpkd", "pvp-kd", "pvp_kd", "pvp kd", "pvp k/d":
		// Kills per death caused by another player; "kd" stays kills per death of any kind.
		title = "K/D (PvP)"
		entries, err = h.stats.TopByPvPKD(context.Background(), guildRowID, 10, h.minKillsKD)
	case "longest":
		title = "Longest Kill"
		entries, err = h.stats.TopLongestKill(context.Background(), guildRowID, 10)
	default:
		title = "Kills"
		entries, err = h.stats.TopByKills(context.Background(), guildRowID, 10)
	}
	if err != nil {
		slog.Warn("component=discord", "msg", "leaderboard query failed", "err", err.Error())
		respondLeaderboardEmbed(s, i, presentation.BuildLeaderboardErrorEmbed(title))
		return
	}
	ranked := make([]presentation.RankedEntry, 0, len(entries))
	for idx, e := range entries {
		ranked = append(ranked, presentation.RankedEntry{Rank: idx + 1, Name: e.DisplayName, Value: e.Value})
	}
	respondLeaderboardEmbed(s, i, presentation.BuildPlayerLeaderboardEmbed(title, ranked, "Lifetime"))
}

func respondLeaderboardEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) {
	respondPrivate(s, i, &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}})
}
