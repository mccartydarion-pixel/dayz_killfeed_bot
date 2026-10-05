package discord

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type SeasonCommandStore interface {
	GetActiveSeason(context.Context, int64) (*repository.Season, error)
	Start(context.Context, int64, string, time.Time) (*repository.Season, error)
	FinalizeSeason(context.Context, int64, int64, time.Time) (*repository.SeasonResult, error)
	GetSeasonHistory(context.Context, int64, int) ([]repository.Season, error)
}

// RankedSeasonStatusReader reports each server's separate Ranked (RP) season.
// Optional: without it /season status only shows the stats season.
type RankedSeasonStatusReader interface {
	ListActiveByGuild(ctx context.Context, guildID int64) ([]repository.GameServer, error)
	ActiveServerSeason(ctx context.Context, guildID, serverID int64) (*repository.ServerRankedSeason, error)
}

// SeasonCommandHandler serves /season. /season manages the guild's STATS
// season (the "seasons" table that labels kill/death history). It never
// starts, ends or resets a server's Ranked (RP) season - those are owner
// controls on the Champion website (Server Admin -> Server Controls -> Server
// Ranked) - but status shows both so the two are never confused.
type SeasonCommandHandler struct {
	seasons SeasonCommandStore
	guilds  GuildStore
	ranked  RankedSeasonStatusReader
}

// SetRankedStatus lets /season status list each server's Ranked season.
func (h *SeasonCommandHandler) SetRankedStatus(r RankedSeasonStatusReader) {
	if h != nil {
		h.ranked = r
	}
}

func NewSeasonCommandHandler(seasons SeasonCommandStore, guilds GuildStore) *SeasonCommandHandler {
	return &SeasonCommandHandler{seasons: seasons, guilds: guilds}
}

func RegisterSeasonCommands(session CommandRegistrar, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("discord session is nil")
	}
	cmd := &discordgo.ApplicationCommand{Name: "season", Description: "Stats season (kill/death history) and each server's Ranked RP season status", Options: []*discordgo.ApplicationCommandOption{
		{Name: "status", Description: "Show the stats season and each server's Ranked (RP) season", Type: discordgo.ApplicationCommandOptionSubCommand},
		{Name: "start", Description: "Start a new stats season (does not start Ranked RP)", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "name", Description: "Stats season name", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		{Name: "end", Description: "Finalize the active stats season (Ranked RP is unaffected)", Type: discordgo.ApplicationCommandOptionSubCommand},
		{Name: "history", Description: "Show recent stats seasons", Type: discordgo.ApplicationCommandOptionSubCommand},
	}}
	_, err = session.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}

func (h *SeasonCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || i.GuildID == "" || h.seasons == nil || h.guilds == nil {
		respondEphemeral(s, i, ReplyAreUnavailable("Seasons"))
		return
	}
	_, guildID, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || guildID == 0 {
		respondEphemeral(s, i, ReplyNotSetUp)
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
			respondEphemeral(s, i, ReplyNeedsManageServer)
			return
		}
		name := opts[0].Options[0].StringValue()
		season, err := h.seasons.Start(context.Background(), guildID, strings.TrimSpace(name), time.Now().UTC())
		if err != nil {
			respondEphemeral(s, i, ReplyCouldNotBecause("start the season", err.Error()))
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("📊 **Stats season started**\n\n**%s**\nStarted <t:%d:R>\n\n%s", season.Name, season.StartsAt.Unix(), rankedIsSeparateNote))
	case "end":
		if !isAdminInteraction(i) {
			respondEphemeral(s, i, ReplyNeedsManageServer)
			return
		}
		active, err := h.seasons.GetActiveSeason(context.Background(), guildID)
		if err != nil || active == nil {
			respondEphemeral(s, i, "No active stats season.")
			return
		}
		result, err := h.seasons.FinalizeSeason(context.Background(), guildID, active.ID, time.Now().UTC())
		if err != nil {
			respondEphemeral(s, i, ReplyCouldNotBecause("finalize the season", err.Error()))
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("👑 **%s complete** (stats season)\n\nTop player kills: %d\nTop faction kills: %d\nLongest kill: %.1fm\n\n%s", active.Name, result.TopPlayerKills, result.TopFactionKills, result.LongestKillValue, rankedIsSeparateNote))
	}
}

// rankedIsSeparateNote is appended wherever a stats season changes, so an
// owner never believes /season started or reset Ranked RP.
const rankedIsSeparateNote = "Ranked (RP) seasons are separate and per server: owners start or reset them in Champion Server Admin → Server Controls → Server Ranked."

func (h *SeasonCommandHandler) status(s *discordgo.Session, i *discordgo.InteractionCreate, guildID int64) {
	respondEphemeral(s, i, h.statusText(context.Background(), guildID, time.Now().UTC()))
}

// statusText renders both seasons, clearly separated:
//
//	📊 STATS SEASON   - the guild's kill/death history label (/season start|end)
//	🎖️ RANKED (RP)     - each server's own RP season (website owner controls)
//
// All-time kills, deaths, streaks and longest kills are never reset by either.
func (h *SeasonCommandHandler) statusText(ctx context.Context, guildID int64, now time.Time) string {
	var b strings.Builder
	b.WriteString("📊 **Stats season**\n")
	active, err := h.seasons.GetActiveSeason(ctx, guildID)
	switch {
	case err != nil:
		b.WriteString("Stats season is temporarily unavailable.\n")
	case active == nil:
		b.WriteString("No active stats season.\n")
	default:
		fmt.Fprintf(&b, "**%s** — %s since <t:%d:R>\n", active.Name, strings.ToLower(string(active.Status)), active.StartsAt.Unix())
	}
	b.WriteString("Labels kill/death history. All-time leaderboards are never reset.\n\n")
	b.WriteString("🎖️ **Ranked (RP) season** — per server\n")
	b.WriteString(h.rankedStatusLines(ctx, guildID))
	return b.String()
}

func (h *SeasonCommandHandler) rankedStatusLines(ctx context.Context, guildID int64) string {
	if h.ranked == nil {
		return "Ranked status is unavailable.\n"
	}
	servers, err := h.ranked.ListActiveByGuild(ctx, guildID)
	if err != nil {
		return "Ranked status is temporarily unavailable.\n"
	}
	if len(servers) == 0 {
		return "No active game server.\n"
	}
	var b strings.Builder
	anyInactive := false
	for _, srv := range servers {
		name := presentation.SafeName(srv.DisplayName, 60)
		if strings.TrimSpace(srv.DisplayName) == "" {
			name = fmt.Sprintf("Server %d", srv.ID)
		}
		season, err := h.ranked.ActiveServerSeason(ctx, guildID, srv.ID)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "**%s** — status temporarily unavailable\n", name)
		case season == nil:
			anyInactive = true
			fmt.Fprintf(&b, "**%s** — not started. Players are Unranked and no RP is awarded.\n", name)
		default:
			fmt.Fprintf(&b, "**%s** — active since <t:%d:R> · %s RP per eligible kill\n", name, season.StartsAt.Unix(), presentation.FormatThousands(season.RPPerKill))
		}
	}
	if anyInactive {
		b.WriteString("Owners start a Ranked season in Champion Server Admin → Server Controls → Server Ranked.\n")
	}
	return b.String()
}
func (h *SeasonCommandHandler) history(s *discordgo.Session, i *discordgo.InteractionCreate, guildID int64) {
	rows, err := h.seasons.GetSeasonHistory(context.Background(), guildID, 5)
	if err != nil {
		respondEphemeral(s, i, ReplyCouldNot("load the season history"))
		return
	}
	var b strings.Builder
	b.WriteString("📊 **Stats season history**\n\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "%s — %s\n", row.Name, row.Status)
	}
	if len(rows) == 0 {
		b.WriteString("No stats seasons yet.")
	}
	respondEphemeral(s, i, b.String())
}
func isAdminInteraction(i *discordgo.InteractionCreate) bool {
	return i != nil && i.Member != nil && (i.Member.Permissions&(discordgo.PermissionAdministrator|discordgo.PermissionManageServer)) != 0
}
