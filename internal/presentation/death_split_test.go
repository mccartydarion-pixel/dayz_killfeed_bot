package presentation

import "testing"

func TestDeathSplitFormats(t *testing.T) {
	if got := CompactPvPStats(262, 4.9007); got != "PvP **262 D** • **4.90 K/D**" {
		t.Errorf("CompactPvPStats = %q", got)
	}
	if got := CompactPvPStats(1250, 9); got != "PvP **1,250 D** • **9.00 K/D**" {
		t.Errorf("CompactPvPStats thousands = %q", got)
	}
	if got := DeathSplit(1250, 60); got != "PvP 1,250 • PvE 60" {
		t.Errorf("DeathSplit = %q", got)
	}
	if got := DeathSplit(0, 0); got != "PvP 0 • PvE 0" {
		t.Errorf("DeathSplit zero = %q", got)
	}
}

func TestPvPKDIsItsOwnRanking(t *testing.T) {
	for _, label := range []string{"K/D (PvP)", "pvpkd", "PvP K/D", "pvp kd"} {
		if got := RankCategoryOf(label); got != RankPvPKD {
			t.Errorf("RankCategoryOf(%q) = %q, want %q", label, got, RankPvPKD)
		}
	}
	// The existing K/D board keeps its category and heading.
	for _, label := range []string{"K/D", "kd", "kdr", "Best K/D"} {
		if got := RankCategoryOf(label); got != RankKD {
			t.Errorf("RankCategoryOf(%q) = %q, want %q", label, got, RankKD)
		}
	}
	if RankHeading(RankKD) != "📈 Best K/D" || RankHeading(RankPvPKD) != "📈 Best K/D (PvP)" {
		t.Errorf("headings: %q / %q", RankHeading(RankKD), RankHeading(RankPvPKD))
	}
	if got := FormatLeaderboardValue(RankPvPKD, "4.9"); got != "4.90 K/D" {
		t.Errorf("PvP K/D value = %q", got)
	}
	if v := CheckLabel(RankHeading(RankPvPKD)); v != "" {
		t.Errorf("the PvP K/D heading breaks the label rules: %s", v)
	}
}
