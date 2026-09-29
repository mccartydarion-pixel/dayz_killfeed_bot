package discord

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// ServerRanksSnapshot is a single server's active local Ranked season. The
// global platform board is a different view and must not be passed here.
type ServerRanksSnapshot struct {
	ServerName string
	SeasonName string
	UpdatedAt  time.Time
	Standings  []repository.ServerStanding
}

// BuildServerRanksEmbed renders the separate #server-ranks panel. This builder
// has no publishing path; callers must first have an active, verified season.
func BuildServerRanksEmbed(s ServerRanksSnapshot) *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{
		Author: presentation.ChampionAuthor(),
		Title:  "🎖️ SERVER RANKS 🎖️",
		Color:  presentation.ChampionGold,
	}
	var lines []string
	if name := strings.TrimSpace(s.ServerName); name != "" {
		lines = append(lines, "**"+presentation.SafeName(name, 100)+"**")
	}
	if name := strings.TrimSpace(s.SeasonName); name != "" {
		lines = append(lines, "Season: "+presentation.SafeName(name, 64))
	}
	if !s.UpdatedAt.IsZero() {
		lines = append(lines, fmt.Sprintf("Last Updated <t:%d:R>", s.UpdatedAt.Unix()))
	}
	embed.Description = strings.Join(lines, "\n")
	entries := make([]presentation.BoardEntry, 0, len(s.Standings))
	for _, standing := range s.Standings {
		value := fmt.Sprintf("%s • %s RP", standing.Tier, presentation.FormatThousands(standing.RP))
		entries = append(entries, presentation.BoardEntry{Name: standing.Name, Value: value})
	}
	embed.Fields = presentation.BoardGridFields(entries, presentation.MaxBoardEntries, presentation.MaxRankNameRunes)
	if len(embed.Fields) == 0 {
		embed.Description += "\n" + presentation.EmptyBoard
	}
	return presentation.FitEmbed(embed)
}
