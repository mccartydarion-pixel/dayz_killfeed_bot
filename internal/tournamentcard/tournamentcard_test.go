package tournamentcard

import (
	"bytes"
	"image/png"
	"math/rand"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/tournament"
)

func sample(t *testing.T, n, size int) *tournament.Tournament {
	t.Helper()
	t0 := time.Date(2026, 10, 10, 19, 0, 0, 0, time.UTC)
	tr := &tournament.Tournament{ID: 1, Name: "Friday Night", Status: tournament.StatusCheckin, TeamSize: 1, BracketSize: size, BestOf: 3, Seeding: tournament.SeedingRanked, StartsAt: t0,
		Rules: tournament.Rules{MatchTimerMinutes: 10, ReadyMinutes: 2, Arenas: []tournament.Arena{{No: 1, Name: "Arena 1", X: 1, Z: 1, Radius: 100}}}}
	for i := 1; i <= n; i++ {
		at := t0
		tr.Entries = append(tr.Entries, &tournament.Entry{ID: int64(i), TeamNo: i, Status: tournament.EntryActive, CheckedInAt: &at, Players: []tournament.EntryPlayer{{PlayerID: int64(i), Name: "Player " + string(rune('A'+i-1))}}})
	}
	if _, err := tr.Start(t0, nil, rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	for i, m := range tr.Matches {
		m.ID = int64(100 + i)
	}
	tr.LinkNext()
	m := tr.Current()
	a, b := tr.Entry(*m.EntryA), tr.Entry(*m.EntryB)
	d := tr.Attribute(tournament.Kill{KillID: 1, KillerPlayerID: a.Players[0].PlayerID, VictimPlayerID: b.Players[0].PlayerID, Weapon: "M4-A1", At: t0})
	if _, err := tr.RecordRound(m, d.Round, t0); err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestRenderSizesAndDeterminism(t *testing.T) {
	for _, c := range []struct{ n, size, height int }{{4, 4, MinHeight}, {8, 8, Height(8)}, {13, 16, Height(16)}, {32, 32, Height(32)}} {
		tr := sample(t, c.n, c.size)
		b := FromTournament(tr, map[int64]ranked.Tier{1: ranked.Gold}, "Server", "championshp.vip")
		first, err := Render(b)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(first))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != Width || img.Bounds().Dy() != c.height {
			t.Fatalf("size %d: %v", c.size, img.Bounds())
		}
		second, _ := Render(b)
		if !bytes.Equal(first, second) {
			t.Fatalf("size %d: rendering is not deterministic", c.size)
		}
	}
	if h := Height(32); h <= Height(16) || Height(16) <= MinHeight {
		t.Fatalf("heights: %d %d", Height(16), h)
	}
}

func TestChampionCard(t *testing.T) {
	tr := sample(t, 2, 4)
	m := tr.Current()
	if _, err := tr.AdminResult(m.ID, *m.EntryA, "admin", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	title := "Friday Champion"
	tr.Prizes = []tournament.Prize{{Place: 1, Points: 1500, Title: &title}}
	c := ChampionOf(tr, nil, "Server", "")
	if c == nil || c.Title != title || c.Points != 1500 || c.Wins != 1 || len(c.Players) != 1 {
		t.Fatalf("champion: %+v", c)
	}
	data, err := RenderChampion(*c)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() != Width || img.Bounds().Dy() != MinHeight {
		t.Fatalf("champion image: %v %v", err, img.Bounds())
	}
	if ChampionOf(sample(t, 4, 4), nil, "", "") != nil {
		t.Fatal("an unfinished tournament has no champion")
	}
	if _, err := Render(Bracket{BracketSize: 1}); err == nil {
		t.Fatal("a bracket needs at least two slots")
	}
}
