package presentation

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// RankCategory is the semantic kind of a ranking; it decides how raw values
// are displayed ("20 Kills", "98.3m", "4.25 K/D", "12,500 pts").
type RankCategory string

const (
	RankKills   RankCategory = "kills"
	RankKD      RankCategory = "kd"
	RankLongest RankCategory = "longest"
	RankPoints  RankCategory = "points"
	RankStreak  RankCategory = "streak"
	RankOther   RankCategory = ""
)

// RankCategoryOf maps the labels used across commands and panels ("Kills",
// "K/D", "Longest Kill", "points", ...) onto a RankCategory.
func RankCategoryOf(label string) RankCategory {
	l := strings.ToLower(strings.TrimSpace(label))
	switch {
	case l == "kd" || l == "k/d" || strings.Contains(l, "k/d") || l == "kdr":
		return RankKD
	case strings.Contains(l, "longest") || strings.Contains(l, "distance"):
		return RankLongest
	case strings.Contains(l, "point") || l == "pts":
		return RankPoints
	case strings.Contains(l, "streak"):
		return RankStreak
	case strings.Contains(l, "kill"):
		return RankKills
	default:
		return RankOther
	}
}

// RankHeading is the one heading a category's ranking block carries.
func RankHeading(c RankCategory) string {
	switch c {
	case RankKills:
		return "⚔️ TOP KILLERS"
	case RankKD:
		return "📈 BEST K/D"
	case RankLongest:
		return "🎯 LONGEST KILLS"
	case RankPoints:
		return "🏆 CHAMPION POINTS"
	case RankStreak:
		return "🔥 BEST STREAKS"
	default:
		return "🏆 RANKINGS"
	}
}

// FormatLeaderboardValue turns a stored ranking value into its display form.
// Values come pre-rendered from the repositories ("20", "4.25", "98.3m");
// anything that does not parse is shown cleaned, never labelled "Value".
func FormatLeaderboardValue(c RankCategory, raw string) string {
	raw = strings.TrimSpace(raw)
	switch c {
	case RankKills, RankStreak:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return Plural(n, "Kill", "Kills")
		}
	case RankPoints:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return FormatPoints(n)
		}
	case RankKD:
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return FormatKD(f) + " K/D"
		}
	case RankLongest:
		if f, err := strconv.ParseFloat(strings.TrimSuffix(raw, "m"), 64); err == nil {
			return FormatDistance(f)
		}
	}
	return SafeName(raw, 40)
}

// RankMarker is 🥇🥈🥉 for the podium and `#N` below it.
func RankMarker(rank int) string {
	switch rank {
	case 1:
		return "🥇"
	case 2:
		return "🥈"
	case 3:
		return "🥉"
	default:
		return fmt.Sprintf("`#%d`", rank)
	}
}

// RankLine is one scoreboard row: "🥇 PlayerOne • **20 Kills**". Plain
// rank • name • value survives mobile and desktop widths alike; no fake
// whitespace columns.
func RankLine(rank int, name, value string) string {
	return fmt.Sprintf("%s %s • **%s**", RankMarker(rank), SafeName(name, MaxRankNameRunes), value)
}

// RankedEntry is one row of a ranking. Rank <= 0 means "use the position".
type RankedEntry struct {
	Rank        int
	Name, Value string
}

// EmptyRanking is the single empty-state line for a category.
const EmptyRanking = "_No qualifying data yet._"

// FormatRankingBlock renders a whole category into one field value: at most
// limit rows (limit <= 0 = all), each value formatted for the category, and
// whole rows dropped - never cut mid-row - to stay within Discord's field
// limit. An empty ranking yields EmptyRanking.
func FormatRankingBlock(c RankCategory, entries []RankedEntry, limit int) string {
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	var b strings.Builder
	for i, e := range entries {
		rank := e.Rank
		if rank <= 0 {
			rank = i + 1
		}
		line := RankLine(rank, e.Name, FormatLeaderboardValue(c, e.Value))
		if b.Len() > 0 {
			line = "\n" + line
		}
		if utf8.RuneCountInString(b.String())+utf8.RuneCountInString(line) > LimitFieldValue {
			break
		}
		b.WriteString(line)
	}
	if b.Len() == 0 {
		return EmptyRanking
	}
	return b.String()
}

// RankingField is a category as exactly one embed field, or nil when the
// category has no rows (callers decide how to show an all-empty board).
func RankingField(c RankCategory, entries []RankedEntry, limit int) *discordgo.MessageEmbedField {
	if len(entries) == 0 {
		return nil
	}
	return &discordgo.MessageEmbedField{Name: RankHeading(c), Value: FormatRankingBlock(c, entries, limit), Inline: false}
}

// BulletBlock renders stored strings (live events, most-wanted lines) as one
// compact bullet list within the field limit. Empty input yields "".
func BulletBlock(lines []string, limit int) string {
	if limit > 0 && len(lines) > limit {
		lines = lines[:limit]
	}
	var b strings.Builder
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		l = SafeName(l, 100)
		row := "• " + l
		if b.Len() > 0 {
			row = "\n" + row
		}
		if utf8.RuneCountInString(b.String())+utf8.RuneCountInString(row) > LimitFieldValue {
			break
		}
		b.WriteString(row)
	}
	return b.String()
}
