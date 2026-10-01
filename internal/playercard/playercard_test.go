package playercard

import (
	"bytes"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"
)

func sample() Card {
	life, rank := int64(11100), 3
	return Card{PlayerName: "Semillita-azul-_", ServerName: "Chernarus Hardcore PvP", FactionName: "Numbers Unit", FactionTag: "NU", SeasonName: "Season 3",
		Kills: 1284, Deaths: 211, Headshots: 402, LongestKillMeters: 812.4, PlaytimeSeconds: 9*86400 + 4*3600,
		LongestLifeSeconds: &life, Rank: &rank, RankedPlayers: 1520, GeneratedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

func TestRenderProducesDeterministicOpenGraphPNG(t *testing.T) {
	a, err := Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(sample())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("the same card rendered two different images")
	}
	img, err := png.Decode(bytes.NewReader(a))
	if err != nil {
		t.Fatal(err)
	}
	if got := img.Bounds().Size(); got.X != Width || got.Y != Height {
		t.Fatalf("size = %v", got)
	}
	// The gold accent bar and the background are where the layout says they are.
	at := func(x, y int) color.RGBA {
		r, g, b, _ := img.At(x, y).RGBA()
		return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 0xFF}
	}
	if got := at(5, 300); got != colGold {
		t.Fatalf("accent bar pixel = %v", got)
	}
	if got := at(Width-5, 5); got != colBackground {
		t.Fatalf("background pixel = %v", got)
	}
	// A different stat changes the image.
	other := sample()
	other.Kills++
	c, err := Render(other)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Fatal("changing kills did not change the image")
	}
	if path := os.Getenv("CHAMPION_CARD_SAMPLE_OUT"); path != "" {
		if err := os.WriteFile(path, a, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTilesShowDashForUnmeasuredValues(t *testing.T) {
	c := Card{PlayerName: "New", Kills: 4}
	want := map[string]string{"KILLS": "4", "DEATHS": "0", "K/D": "4.00", "HEADSHOTS": "0", "LONGEST KILL": "-", "PLAYTIME": "0s", "LONGEST LIFE": "-", "SERVER RANK": "-"}
	tiles := c.Tiles()
	if len(tiles) != 8 {
		t.Fatalf("%d tiles", len(tiles))
	}
	for _, tile := range tiles {
		if want[tile[0]] != tile[1] {
			t.Errorf("%s = %q, want %q", tile[0], tile[1], want[tile[0]])
		}
	}
	full := sample().Tiles()
	if full[0][1] != "1,284" || full[4][1] != "812 m" || full[5][1] != "9d 4h" || full[6][1] != "3h 05m" || full[7][1] != "#3" {
		t.Fatalf("tiles = %v", full)
	}
}

func TestRenderSurvivesHostileNames(t *testing.T) {
	for _, name := range []string{"", "   ", "名前のないプレイヤー", "a\x00b\nc", string(make([]rune, 400)), "WWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWW"} {
		c := sample()
		c.PlayerName, c.ServerName, c.FactionName = name, name, name
		if _, err := Render(c); err != nil {
			t.Fatalf("name %q: %v", name, err)
		}
	}
}

func TestPrintableReplacesMissingGlyphs(t *testing.T) {
	if err := loadFonts(); err != nil {
		t.Fatal(err)
	}
	f, err := face(true, 30)
	if err != nil {
		t.Fatal(err)
	}
	if got := printable(f, " Ace名\x07 "); got != "Ace?" {
		t.Fatalf("printable = %q", got)
	}
}
