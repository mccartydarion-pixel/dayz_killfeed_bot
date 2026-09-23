package presentation

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

func TestNumberFormatting(t *testing.T) {
	cases := map[int64]string{0: "0", 1000: "1,000", 125000: "125,000", -12500: "-12,500", math.MinInt64: "-9,223,372,036,854,775,808"}
	for n, want := range cases {
		if got := FormatThousands(n); got != want {
			t.Errorf("FormatThousands(%d) = %q, want %q", n, got, want)
		}
	}
	if FormatPoints(1) != "1 pt" || FormatPoints(12500) != "12,500 pts" {
		t.Fatal("points units")
	}
	if FormatDistance(11.66) != "11.7m" || FormatKD(9) != "9.00" || FormatKD(1.4166) != "1.42" {
		t.Fatal("distance / K/D precision")
	}
	if CompactStats(9, 0, 9) != "**9 K** • **0 D** • **9.00 K/D**" {
		t.Fatalf("compact stats: %q", CompactStats(9, 0, 9))
	}
}

func TestLeaderboardValueFormatting(t *testing.T) {
	cases := []struct {
		c        RankCategory
		raw, out string
	}{
		{RankKills, "20", "20 Kills"},
		{RankKills, "1", "1 Kill"},
		{RankLongest, "98.3m", "98.3m"},
		{RankLongest, "98.34", "98.3m"},
		{RankKD, "4.25", "4.25 K/D"},
		{RankPoints, "12500", "12,500 pts"},
		{RankStreak, "8", "8 Kills"},
		{RankOther, "odd", "odd"},
	}
	for _, c := range cases {
		if got := FormatLeaderboardValue(c.c, c.raw); got != c.out {
			t.Errorf("%s %q = %q, want %q", c.c, c.raw, got, c.out)
		}
	}
	for label, want := range map[string]RankCategory{"Kills": RankKills, "K/D": RankKD, "kd": RankKD, "Longest Kill": RankLongest, "points": RankPoints, "Best Streak": RankStreak} {
		if got := RankCategoryOf(label); got != want {
			t.Errorf("RankCategoryOf(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestRankMarkers(t *testing.T) {
	want := []string{"🥇", "🥈", "🥉", "`#4`", "`#10`"}
	for i, r := range []int{1, 2, 3, 4, 10} {
		if RankMarker(r) != want[i] {
			t.Errorf("rank %d marker %q", r, RankMarker(r))
		}
	}
}

func TestRankingBlockDropsWholeRowsAtTheFieldLimit(t *testing.T) {
	var entries []RankedEntry
	for i := 0; i < 100; i++ {
		entries = append(entries, RankedEntry{Name: strings.Repeat("N", 40), Value: "1"})
	}
	v := FormatRankingBlock(RankKills, entries, 0)
	if utf8.RuneCountInString(v) > LimitFieldValue {
		t.Fatalf("block over the field limit: %d", utf8.RuneCountInString(v))
	}
	for _, row := range strings.Split(v, "\n") {
		if !strings.HasSuffix(row, "**1 Kill**") {
			t.Fatalf("row cut mid-way: %q", row)
		}
	}
	if FormatRankingBlock(RankKills, nil, 5) != EmptyRanking {
		t.Fatal("empty state")
	}
}

func TestSafeNameKeepsGamerTagsAndBlocksAbuse(t *testing.T) {
	for _, tag := range []string{"WilliamAle--10", "zTonii99", "KikiduritoR2", "Ceiyxe"} {
		if SafeName(tag, MaxRankNameRunes) != tag {
			t.Errorf("valid tag altered: %q", tag)
		}
	}
	if SafeName("Semillita-azul-_", 48) != `Semillita-azul-\_` {
		t.Fatal("underscore must be escaped, not dropped")
	}
	got := SafeName("@everyone <@&1> <#2> **b** a\u202Eb\u200b\x07", 80)
	for _, bad := range []string{"@", "#", "**b**", "\u202E", "\u200b", "\x07"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%q survived in %q", bad, got)
		}
	}
	if CleanName("   ", 10) != "Unknown" || utf8.RuneCountInString(CleanName(strings.Repeat("x", 100), 32)) != 32 {
		t.Fatal("empty / cap")
	}
}

func TestFitEmbedEnforcesEveryLimit(t *testing.T) {
	big := strings.Repeat("x", 5000)
	e := &discordgo.MessageEmbed{Title: big, Description: big, Author: &discordgo.MessageEmbedAuthor{Name: big}, Footer: &discordgo.MessageEmbedFooter{Text: big}}
	for i := 0; i < 40; i++ {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: big, Value: big})
	}
	e.Fields = append(e.Fields, nil, &discordgo.MessageEmbedField{})
	FitEmbed(e)
	if utf8.RuneCountInString(e.Title) > LimitTitle || utf8.RuneCountInString(e.Description) > LimitDescription || len(e.Fields) > LimitFields || EmbedLength(e) > LimitTotal {
		t.Fatalf("limits not enforced: fields=%d total=%d", len(e.Fields), EmbedLength(e))
	}
	for _, f := range e.Fields {
		if f == nil || f.Name == "" || f.Value == "" {
			t.Fatal("nil/empty fields must be dropped or filled")
		}
	}
	if FitEmbed(nil) != nil {
		t.Fatal("nil-safe")
	}
}
