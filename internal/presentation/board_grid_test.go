package presentation

import (
	"strings"
	"testing"
)

func TestBoardValueFormats(t *testing.T) {
	cases := map[string]string{
		FormatBoardKills("1"):         "1 Kill",
		FormatBoardKills("2"):         "2 Kills",
		FormatBoardKills("6053"):      "6,053 Kills",
		FormatBoardDeaths("1"):        "1 Death",
		FormatBoardDeaths("5012"):     "5,012 Deaths",
		FormatBoardStreak("27"):       "27 Kill Streak",
		FormatBoardDistance("98.3m"):  "98.3m",
		FormatBoardDistance("215"):    "215.0m",
		FormatBoardDistance("1104.2"): "1,104.2m",
		FormatBoardDistance("99.96"):  "100.0m",
		FormatBoardKills("@everyone"): "everyone",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestBoardGridFields(t *testing.T) {
	var entries []BoardEntry
	for i := 0; i < 20; i++ {
		entries = append(entries, BoardEntry{Name: "P", Value: "1 Kill"})
	}
	f := BoardGridFields(entries, 0, 0)
	if len(f) != MaxBoardEntries {
		t.Fatalf("capped at %d, got %d", MaxBoardEntries, len(f))
	}
	if f[0].Name != "🥇 P" || f[2].Name != "🥉 P" || f[3].Name != "#4 P" || f[14].Name != "#15 P" {
		t.Fatalf("markers: %q %q %q %q", f[0].Name, f[2].Name, f[3].Name, f[14].Name)
	}
	for _, x := range f {
		if !x.Inline {
			t.Fatal("every cell is inline")
		}
	}
	if n := len(BoardGridFields(entries[:7], 0, 0)); n != 7 {
		t.Fatalf("7 entries -> 7 cells, got %d", n)
	}
	if got := BoardFieldName(4, strings.Repeat("x", 50), 16); got != "#4 "+strings.Repeat("x", 15)+"…" {
		t.Fatalf("cap: %q", got)
	}
}
