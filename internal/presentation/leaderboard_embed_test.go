package presentation

import "testing"

func TestLeaderboardEmbedCards(t *testing.T) {
	e := BuildPlayerLeaderboardEmbed("kills", []RankedEntry{{1, "Player", "12"}, {2, "Other", "9"}}, "Season 3")
	if len(e.Fields) != 2 || e.Fields[0].Name != "#1 Player" {
		t.Fatalf("bad cards: %+v", e.Fields)
	}
	if BuildPlayerLeaderboardEmbed("kills", nil, "Lifetime").Description == "" {
		t.Fatal("empty board")
	}
}
func TestLeaderboardNamesSanitized(t *testing.T) {
	e := BuildPlayerLeaderboardEmbed("kills", []RankedEntry{{1, "@everyone", "1"}}, "")
	if contains := e.Fields[0].Name; contains == "#1 @everyone" {
		t.Fatal("mention leaked")
	}
}
