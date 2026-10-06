package playercard

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/ranked"
)

func sample() Card {
	life, rank := int64(11100), 3
	return Card{PlayerName: "Semillita-azul-_", ServerName: "Chernarus Hardcore PvP", FactionName: "Numbers Unit", FactionTag: "NU", SeasonName: "Season 3",
		Kills: 1284, Deaths: 211, Headshots: 402, LongestKillMeters: 812.4, PlaytimeSeconds: 9*86400 + 4*3600,
		LongestLifeSeconds: &life, Rank: &rank, RankedPlayers: 1520, GeneratedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

// gold is a Gold standing part way to Platinum, with a server position.
func gold() *Ranked {
	position, next := int64(5), int64(1500)
	return &Ranked{Tier: ranked.Gold, RP: 1240, Position: &position, NextTier: ranked.Platinum, Remaining: 260, TierStartRP: 1000, NextTierRP: &next}
}

func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := img.Bounds().Size(); got.X != Width || got.Y != Height {
		t.Fatalf("size = %v", got)
	}
	return img
}

func pixel(img image.Image, x, y int) color.RGBA {
	r, g, b, _ := img.At(x, y).RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 0xFF}
}

func TestRenderProducesDeterministicOpenGraphPNG(t *testing.T) {
	card := sample()
	card.Ranked = gold()
	a, err := Render(card)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(card)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("the same card rendered two different images")
	}
	img := decode(t, a)
	// The gold accent bar and the background gradient are where the layout says they are.
	if got := pixel(img, 5, 300); got != colGold {
		t.Fatalf("accent bar pixel = %v", got)
	}
	if got := pixel(img, Width-5, 0); got != colBackgroundTop {
		t.Fatalf("top background pixel = %v", got)
	}
	if got := pixel(img, Width-5, Height-1); got != colBackgroundBottom {
		t.Fatalf("bottom background pixel = %v", got)
	}
	// A different stat changes the image.
	other := card
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

func TestRenderIsSafeForConcurrentUse(t *testing.T) {
	// The player API, the public share route and /card render at the same time, sharing the parsed
	// fonts and the decoded emblems but nothing else.
	card := sample()
	card.Ranked = gold()
	want, err := Render(card)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan []byte, 8)
	for i := 0; i < cap(results); i++ {
		go func() {
			data, err := Render(card)
			if err != nil {
				t.Error(err)
			}
			results <- data
		}()
	}
	for i := 0; i < cap(results); i++ {
		if got := <-results; !bytes.Equal(got, want) {
			t.Fatal("a concurrent render differs")
		}
	}
}

func TestEveryTierAndSeasonStateRenders(t *testing.T) {
	next := int64(100)
	cases := map[string]*Ranked{
		"no season":          nil,
		"active, no RP yet":  {Tier: ranked.Unranked, NextTier: ranked.Rookie, Remaining: 100, NextTierRP: &next},
		"master":             {Tier: ranked.Master, RP: 9870, Position: ptr(int64(1)), TierStartRP: 2800},
		"unknown tier":       {Tier: "LEGEND", RP: 5, Position: ptr(int64(2)), NextTier: ranked.Rookie, Remaining: 95, NextTierRP: &next},
		"inconsistent range": {Tier: ranked.Rookie, RP: 150, NextTier: ranked.Bronze, Remaining: 150, TierStartRP: 300, NextTierRP: ptr(int64(300))},
	}
	for _, tier := range []ranked.Tier{ranked.Rookie, ranked.Bronze, ranked.Silver, ranked.Gold, ranked.Platinum, ranked.Diamond} {
		r := gold()
		r.Tier = tier
		cases[string(tier)] = r
	}
	for name, r := range cases {
		card := sample()
		card.Ranked = r
		data, err := Render(card)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		decode(t, data)
	}
}

func ptr[T any](v T) *T { return &v }

func TestEmblemAndGlowFollowTheTier(t *testing.T) {
	render := func(r *Ranked) image.Image {
		card := sample()
		card.Ranked = r
		data, err := Render(card)
		if err != nil {
			t.Fatal(err)
		}
		return decode(t, data)
	}
	goldCard := render(gold())
	diamond := gold()
	diamond.Tier = ranked.Diamond
	diamondCard := render(diamond)
	// The emblems differ inside their box, and the glow around them differs too.
	differ := 0
	for y := emblemTop; y < emblemTop+emblemSize; y++ {
		for x := emblemLeft; x < emblemLeft+emblemSize; x++ {
			if pixel(goldCard, x, y) != pixel(diamondCard, x, y) {
				differ++
			}
		}
	}
	if differ < emblemSize*emblemSize/4 {
		t.Fatalf("only %d emblem pixels differ between gold and diamond", differ)
	}
	beside := image.Pt(emblemLeft+emblemSize+8, emblemTop+emblemSize/2)
	if pixel(goldCard, beside.X, beside.Y) == pixel(diamondCard, beside.X, beside.Y) {
		t.Fatal("the glow beside the emblem does not follow the tier")
	}
	// No season: the unranked emblem, no glow.
	none := render(nil)
	if got := pixel(none, beside.X, beside.Y); got != colBackgroundAt(beside.Y) {
		t.Fatalf("a card without a season glows: %v", got)
	}
	if pixel(none, emblemLeft+emblemSize/2, emblemTop+emblemSize/2) == pixel(goldCard, emblemLeft+emblemSize/2, emblemTop+emblemSize/2) {
		t.Fatal("a card without a season shows the gold emblem")
	}
}

func colBackgroundAt(y int) color.RGBA {
	return lerp(colBackgroundTop, colBackgroundBottom, float64(y)/float64(Height-1))
}

func TestRankRowNeverReachesTheProgressBar(t *testing.T) {
	card := sample()
	card.Ranked = &Ranked{Tier: ranked.Unranked, RP: 999_999_999, Position: ptr(int64(999_999)), NextTier: ranked.Rookie, Remaining: 1, NextTierRP: ptr(int64(1_000_000_000))}
	data, err := Render(card)
	if err != nil {
		t.Fatal(err)
	}
	img := decode(t, data)
	// Between the row and the bar nothing is drawn: the position was dropped and the RP shrunk.
	for y := pillTop - 4; y < pillTop+pillHeight+4; y++ {
		for x := rowLimit + 1; x < barLeft; x++ {
			if got := pixel(img, x, y); got != colBackgroundAt(y) {
				t.Fatalf("pixel (%d,%d) = %v: the rank row reached the bar", x, y, got)
			}
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
	for i, n := range c.TileNotes() {
		if n != "" {
			t.Errorf("tile %d of an unranked card has a note: %q", i, n)
		}
	}
	full := sample().Tiles()
	if full[0][1] != "1,284" || full[4][1] != "812 m" || full[5][1] != "9d 4h" || full[6][1] != "3h 05m" || full[7][1] != "#3" {
		t.Fatalf("tiles = %v", full)
	}
	far := sample()
	far.LongestKillMeters = 1012.4
	if got := far.Tiles()[4][1]; got != "1,012 m" {
		t.Fatalf("longest kill = %q", got)
	}
}

func TestServerRankTileSaysWhatItCounts(t *testing.T) {
	if n := sample().TileNotes(); n[7] != "of 1,520 · by kills" {
		t.Fatalf("server rank note = %q", n[7])
	}
	// The count alone, without a rank, is nothing to caption.
	c := sample()
	c.Rank = nil
	if n := c.TileNotes(); n[7] != "" {
		t.Fatalf("unranked server rank note = %q", n[7])
	}
	c = sample()
	c.RankedPlayers = 0
	if n := c.TileNotes(); n[7] != "" {
		t.Fatalf("server rank note without a count = %q", n[7])
	}
}

func TestRenderSurvivesHostileNames(t *testing.T) {
	for _, name := range []string{"", "   ", "名前のないプレイヤー", "a\x00b\nc", string(make([]rune, 400)), "WWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWWW", "​‮"} {
		c := sample()
		c.Ranked = gold()
		c.PlayerName, c.ServerName, c.FactionName, c.SeasonName = name, name, name, name
		if _, err := Render(c); err != nil {
			t.Fatalf("name %q: %v", name, err)
		}
	}
}

func TestPrintableReplacesMissingGlyphs(t *testing.T) {
	fonts, err := loadFonts()
	if err != nil {
		t.Fatal(err)
	}
	f := newCanvas(fonts).face(soraExtraBold, false, 30)
	if got := printable(f, " Ace名\x07 "); got != "Ace?" {
		t.Fatalf("printable = %q", got)
	}
}

func TestLatinStringsUseTheSiteFontsAndOthersFallBackToTheGoFonts(t *testing.T) {
	fonts, err := loadFonts()
	if err != nil {
		t.Fatal(err)
	}
	cv := newCanvas(fonts)
	f, s := cv.resolve(stName, "Ceiyxe")
	if f.src != fonts.primary[soraExtraBold] || s != "Ceiyxe" {
		t.Fatalf("a Latin name resolved to %v, %q", f.src == fonts.goBold, s)
	}
	// Cyrillic is not in the Sora subset: the whole name moves to Go Bold, and stays legible.
	f, s = cv.resolve(stName, "Иван_Грозный")
	if f.src != fonts.goBold || s != "Иван_Грозный" {
		t.Fatalf("a Cyrillic name resolved to goBold=%v, %q", f.src == fonts.goBold, s)
	}
	f, s = cv.resolve(stSubline, "Иван")
	if f.src != fonts.goRegular || s != "Иван" {
		t.Fatalf("a Cyrillic sub line resolved to goRegular=%v, %q", f.src == fonts.goRegular, s)
	}
	// A script no face has still renders, as question marks.
	if _, s = cv.resolve(stName, "名前"); s != "??" {
		t.Fatalf("CJK resolved to %q", s)
	}
	// The rendered image of a Cyrillic name is not the image of its question marks.
	cyrillic, latin := sample(), sample()
	cyrillic.PlayerName, latin.PlayerName = "Иван_Грозный", "????_???????"
	a, err := Render(cyrillic)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(latin)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("a Cyrillic name rendered as question marks")
	}
}

func TestFitShrinksThenCutsWithAnEllipsis(t *testing.T) {
	fonts, err := loadFonts()
	if err != nil {
		t.Fatal(err)
	}
	cv := newCanvas(fonts)
	st, s := cv.fit(stName, []float64{80, 68, 58, 48}, 868, "Ace")
	if st.size != 80 || s != "Ace" {
		t.Fatalf("short name fit = %v %q", st.size, s)
	}
	long := "xX_SniperWolf_Xx_2026"
	st, s = cv.fit(stName, []float64{80, 68, 58, 48}, 868, long)
	if st.size == 80 || s != long {
		t.Fatalf("long name fit = %v %q", st.size, s)
	}
	st, s = cv.fit(stServer, []float64{22}, 560, "Champions® 1v1 Tournament | M4 Only | PvP | Discord: ASD43MQs7M")
	if st.size != 22 || s[len(s)-len("…"):] != "…" || cv.measure(st, s) > 560 {
		t.Fatalf("server name fit = %q (%d px)", s, cv.measure(st, s))
	}
	if cv.err != nil {
		t.Fatal(cv.err)
	}
}

func TestRankedProgress(t *testing.T) {
	if p := gold().Progress(); p < 0.47 || p > 0.49 {
		t.Fatalf("gold progress = %v", p)
	}
	if p := (Ranked{Tier: ranked.Master, RP: 5000}).Progress(); p != 1 {
		t.Fatalf("master progress = %v", p)
	}
	over := gold()
	over.RP = 9000
	if p := over.Progress(); p != 1 {
		t.Fatalf("progress past the next tier = %v", p)
	}
	if TierName(ranked.Platinum) != "Platinum" || TierName("") != "" {
		t.Fatal("tier names")
	}
}

func TestTilesCarryThePvPSplitOnlyWhenItIsKnown(t *testing.T) {
	// Not known: only the server rank tile has a caption.
	plain := sample()
	for i, n := range plain.TileNotes() {
		if n != "" && i != 7 {
			t.Fatalf("tile %d has a note without a split: %q", i, n)
		}
	}
	if _, ok := plain.PvEDeaths(); ok {
		t.Fatal("PvEDeaths reported a split that is not known")
	}
	if _, ok := plain.PvPKD(); ok {
		t.Fatal("PvPKD reported a split that is not known")
	}

	pvp := 160
	split := sample() // 1,284 kills, 211 deaths
	split.PvPDeaths = &pvp
	if pve, ok := split.PvEDeaths(); !ok || pve != 51 {
		t.Fatalf("PvEDeaths = %d, %v", pve, ok)
	}
	if kd, ok := split.PvPKD(); !ok || kd != 1284.0/160 {
		t.Fatalf("PvPKD = %v, %v", kd, ok)
	}
	tiles, notes := split.Tiles(), split.TileNotes()
	if len(notes) != len(tiles) {
		t.Fatalf("%d notes for %d tiles", len(notes), len(tiles))
	}
	// The tiles themselves keep their meaning: every death, the overall K/D.
	if tiles[1] != [2]string{"DEATHS", "211"} || tiles[2] != [2]string{"K/D", "6.09"} {
		t.Fatalf("tiles = %v", tiles)
	}
	for i, n := range notes {
		want := map[int]string{1: "PVP 160 / PVE 51", 2: "PVP 8.03", 7: "of 1,520 · by kills"}[i]
		if n != want {
			t.Errorf("note %d = %q, want %q", i, n, want)
		}
	}

	// Zero rules: no PvP deaths means the PvP K/D is the kill count; only PvP deaths means both agree.
	none, all := 0, 211
	onlyPvE, onlyPvP := sample(), sample()
	onlyPvE.PvPDeaths, onlyPvP.PvPDeaths = &none, &all
	if n := onlyPvE.TileNotes(); n[1] != "PVP 0 / PVE 211" || n[2] != "PVP 1284.00" {
		t.Fatalf("only PvE notes = %v", n)
	}
	if n := onlyPvP.TileNotes(); n[1] != "PVP 211 / PVE 0" || n[2] != "PVP 6.09" {
		t.Fatalf("only PvP notes = %v", n)
	}

	a, err := Render(plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(split)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("the split did not change the image")
	}
	if path := os.Getenv("CHAMPION_CARD_SPLIT_SAMPLE_OUT"); path != "" {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
