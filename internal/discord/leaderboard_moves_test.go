package discord

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestBoardMovesAndWeeklyBoard(t *testing.T) {
	entries := []repository.LeaderboardEntry{{DisplayName: "A", Value: "9"}, {DisplayName: "B", Value: "8"}, {DisplayName: "C", Value: "7"}}
	moves, now := boardMoves(entries, map[string]int{"A": 2, "B": 1})
	if moves["A"] != "▲1" || moves["B"] != "▼1" || moves["C"] != "🆕" || now["C"] != 3 {
		t.Fatalf("moves: %v %v", moves, now)
	}
	if first, _ := boardMoves(entries, nil); len(first) != 0 {
		t.Fatal("no arrows without a day before")
	}
	got := withMoves([]presentation.BoardEntry{{Name: "A", Value: "9 kills"}}, moves)
	if got[0].Value != "9 kills ▲1" {
		t.Fatalf("value: %q", got[0].Value)
	}
	snap := LeaderboardSnapshot{WeekEnabled: true, TopKillsWeek: entries}
	embeds := BuildAutoLeaderboardEmbeds(snap, DefaultLeaderboardConfig())
	if embeds[2].Title != AutoBoardWeekTitle {
		t.Fatalf("weekly board follows the all-time kills: %q", embeds[2].Title)
	}
	if ws := weekStartUTC(time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)); ws.Weekday() != time.Monday || ws.Day() != 28 {
		t.Fatalf("week start: %v", ws)
	}
}
