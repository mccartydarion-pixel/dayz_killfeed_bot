package presentation

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Board grid: the Auto Leaderboard V3 presentation. Each ranked player is ONE
// inline field (name = marker + player, value = the category value), so
// Discord lays a Top 15 out as 3 columns x 5 rows. This is deliberately
// different from FormatRankingBlock (one field per category), which the
// interactive /leaderboard keeps.

// MaxBoardEntries is the Top-N every Auto Leaderboard category renders.
const MaxBoardEntries = 15

// EmptyBoard is the one line a category embed shows when nobody qualifies. The
// embed itself is kept, so the board's order never shifts.
const EmptyBoard = "_No qualifying players yet._"

// BoardEntry is one ranked row with its value already in display form.
type BoardEntry struct {
	Name, Value string
}

// BoardFieldName is the grid cell heading: 🥇/🥈/🥉 for the podium and
// "#N" below it, then the safe (cleaned, capped, markdown-escaped) name.
func BoardFieldName(rank int, name string, nameCap int) string {
	if nameCap <= 0 {
		nameCap = MaxRankNameRunes
	}
	marker := fmt.Sprintf("#%d", rank)
	if rank >= 1 && rank <= 3 {
		marker = RankMarker(rank)
	}
	return marker + " " + SafeName(name, nameCap)
}

// BoardGridFields renders at most limit entries (limit <= 0 = MaxBoardEntries)
// as inline fields, one per player, ranked by position. Entries past the
// limit are dropped; nothing is padded, so 7 qualifying players give 7 cells.
func BoardGridFields(entries []BoardEntry, limit, nameCap int) []*discordgo.MessageEmbedField {
	if limit <= 0 || limit > MaxBoardEntries {
		limit = MaxBoardEntries
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]*discordgo.MessageEmbedField, 0, len(entries))
	for i, e := range entries {
		value := strings.TrimSpace(e.Value)
		if value == "" {
			value = "​"
		}
		out = append(out, &discordgo.MessageEmbedField{
			Name:   Truncate(BoardFieldName(i+1, e.Name, nameCap), LimitFieldName),
			Value:  Truncate(value, LimitFieldValue),
			Inline: true,
		})
	}
	return out
}

// FormatBoardKills renders an all-time kill count: "1 kill", "6,053 kills".
func FormatBoardKills(raw string) string { return formatBoardCount(raw, "kill", "kills") }

// FormatBoardDeaths renders an all-time death count: "1 death", "5,012 deaths".
func FormatBoardDeaths(raw string) string { return formatBoardCount(raw, "death", "deaths") }

// FormatBoardStreak renders a record streak: "27 kill streak".
func FormatBoardStreak(raw string) string {
	if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
		return FormatThousands(n) + " kill streak"
	}
	return SafeName(raw, 40)
}

// FormatBoardDistance renders a longest kill at one decimal with thousands
// separators: "98.3m", "215.0m", "1,104.2m".
func FormatBoardDistance(raw string) string {
	f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(raw), "m"), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return SafeName(raw, 40)
	}
	tenths := int64(math.Round(math.Abs(f) * 10))
	sign := ""
	if f < 0 && tenths != 0 {
		sign = "-"
	}
	return fmt.Sprintf("%s%s.%dm", sign, FormatThousands(tenths/10), tenths%10)
}

func formatBoardCount(raw, singular, plural string) string {
	if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
		return Plural(n, singular, plural)
	}
	return SafeName(raw, 40)
}
