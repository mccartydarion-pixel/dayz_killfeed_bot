package playercard

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"math"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/ranked"
)

// The animated card (docs/CHAMPION_CARD.md, "Animated card").

func TestRenderAnimationIsALoopingGIFOfTheRightShape(t *testing.T) {
	card := sample()
	card.Ranked = gold()
	data, err := RenderAnimation(card)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if g.Config.Width != Width || g.Config.Height != Height || g.LoopCount != 0 {
		t.Fatalf("screen %dx%d, loop %d", g.Config.Width, g.Config.Height, g.LoopCount)
	}
	if n := len(g.Image); n < 2 || n > MotionFrames+1 || len(g.Delay) != n || len(g.Disposal) != n {
		t.Fatalf("%d frames, %d delays, %d disposals", n, len(g.Delay), len(g.Disposal))
	}
	total := 0
	for i, d := range g.Delay {
		total += d
		if d%frameDelay != 0 || d <= 0 {
			t.Fatalf("frame %d delay %d is not whole motion frames", i, d)
		}
		if g.Disposal[i] != gif.DisposalNone {
			t.Fatalf("frame %d disposal %d", i, g.Disposal[i])
		}
		if !g.Image[i].Bounds().In(image.Rect(0, 0, Width, Height)) {
			t.Fatalf("frame %d bounds %v", i, g.Image[i].Bounds())
		}
		if len(g.Image[i].Palette) > maxPaletteSize {
			t.Fatalf("frame %d has %d colours", i, len(g.Image[i].Palette))
		}
	}
	if want := MotionFrames*frameDelay + HoldDelay; total != want {
		t.Fatalf("the animation lasts %d/100 s, want %d", total, want)
	}
	if last := g.Delay[len(g.Delay)-1]; last < HoldDelay {
		t.Fatalf("the still holds for %d/100 s", last)
	}
	// The first frame is the whole screen; later ones carry only what changed, with the rest
	// transparent, so the file stays small.
	if g.Image[0].Bounds() != image.Rect(0, 0, Width, Height) {
		t.Fatalf("first frame bounds %v", g.Image[0].Bounds())
	}
	if len(data) >= 2<<20 {
		t.Fatalf("the Gold sample is %d bytes", len(data))
	}
	if g.Image[1].Bounds() == g.Image[0].Bounds() {
		t.Fatalf("second frame is not a diff: %v", g.Image[1].Bounds())
	}
}

// Playing the GIF to its end gives the still, mapped to the palette: the diffs and the
// transparency reproduce every pixel, and the hold frame is Render's image.
func TestAnimationEndsOnTheStill(t *testing.T) {
	card := sample()
	card.Ranked = gold()
	data, err := RenderAnimation(card)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	screen := image.NewRGBA(image.Rect(0, 0, Width, Height))
	for _, fr := range g.Image {
		draw.Draw(screen, fr.Bounds(), fr, fr.Bounds().Min, draw.Over)
	}
	// The frame the hold is drawn from is, before quantisation, exactly the still.
	fonts, err := loadFonts()
	if err != nil {
		t.Fatal(err)
	}
	cv := newCanvas(fonts)
	times := frameTimes()
	if err := cv.draw(card, frameAt(card, times[len(times)-1])); err != nil {
		t.Fatal(err)
	}
	stillPNG, err := Render(card)
	if err != nil {
		t.Fatal(err)
	}
	still := decode(t, stillPNG)
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			if got, want := pixel(cv.img, x, y), pixel(still, x, y); got != want {
				t.Fatalf("final frame pixel (%d,%d) = %v, still has %v", x, y, got, want)
			}
		}
	}
	// And what the GIF shows at the end is that frame through the palette.
	pal := g.Image[0].Palette
	m := newMapper(newPalette(decodedColors(pal)))
	want := make([]uint8, Width*Height)
	m.quantize(cv.img, want)
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			if got, w := screen.RGBAAt(x, y), pal[want[y*Width+x]]; got != w {
				t.Fatalf("played to the end, pixel (%d,%d) = %v, want %v", x, y, got, w)
			}
		}
	}
}

// decodedColors are the opaque colours of a decoded GIF palette, in order; the transparent entry
// is the last one, so the indices of the others hold.
func decodedColors(p color.Palette) []color.RGBA {
	var out []color.RGBA
	for _, c := range p {
		if rgba := color.RGBAModel.Convert(c).(color.RGBA); rgba.A != 0 {
			out = append(out, rgba)
		}
	}
	return out
}

func TestRenderAnimationIsDeterministicAndSafeForConcurrentUse(t *testing.T) {
	card := sample()
	card.Ranked = gold()
	want, err := RenderAnimation(card)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan []byte, 4)
	for i := 0; i < cap(results); i++ {
		go func() {
			data, err := RenderAnimation(card)
			if err != nil {
				t.Error(err)
			}
			results <- data
		}()
	}
	for i := 0; i < cap(results); i++ {
		if got := <-results; !bytes.Equal(got, want) {
			t.Fatal("a concurrent render of the animation differs")
		}
	}
}

func TestEveryTierAndSeasonStateAnimates(t *testing.T) {
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
		if name == "no season" {
			card.Rank, card.LongestLifeSeconds, card.LongestKillMeters = nil, nil, 0
		}
		data, err := RenderAnimation(card)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := gif.DecodeAll(bytes.NewReader(data)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The frame state is where the counting happens: part way through the count window every figure
// is strictly between zero and its value, the server rank is on its way down, the RP on its way
// up, and a value the card never measured stays a dash.
func TestFramesCountTheFiguresUp(t *testing.T) {
	card := sample()
	card.Ranked = gold()
	pvp := 160
	card.PvPDeaths = &pvp
	mid := frameAt(card, 1.0)
	f := mid.figures
	between := func(name string, v, hi float64) {
		t.Helper()
		if !(v > 0 && v < hi) {
			t.Fatalf("%s at t=1.0 is %v, want strictly between 0 and %v", name, v, hi)
		}
	}
	between("kills", float64(f.kills), float64(card.Kills))
	between("deaths", float64(f.deaths), float64(card.Deaths))
	between("headshots", float64(f.headshots), float64(card.Headshots))
	between("K/D", f.kd, card.KD())
	between("PvP deaths", float64(f.pvpDeaths), float64(pvp))
	between("longest kill", f.longestKill, card.LongestKillMeters)
	between("playtime", float64(f.playtime), float64(card.PlaytimeSeconds))
	between("longest life", float64(f.longestLife), float64(*card.LongestLifeSeconds))
	if !(f.rank > *card.Rank && f.rank < card.RankedPlayers) {
		t.Fatalf("server rank at t=1.0 is #%d, want between #%d and #%d counting down", f.rank, card.RankedPlayers, *card.Rank)
	}
	r := mid.rank
	if !(r.rp > card.Ranked.TierStartRP && r.rp < card.Ranked.RP) || r.remaining != card.Ranked.Remaining+card.Ranked.RP-r.rp || !(r.progress > 0 && r.progress < card.Ranked.Progress()) {
		t.Fatalf("rank row at t=1.0 = %+v", r)
	}
	tiles := f.tiles()
	if tiles[0].value == "0" || tiles[0].value == "1,284" || tiles[7].value == "#3" || tiles[7].note != "of 1,520 · by kills" {
		t.Fatalf("tiles at t=1.0 = %v", tiles)
	}

	// Before the count: everything at zero, the rank at the field, the RP at the tier's start.
	start := frameAt(card, 0.5)
	if start.figures.kills != 0 || start.figures.kd != 0 || start.figures.rank != card.RankedPlayers || start.rank.rp != 1000 || start.rank.remaining != 500 || start.rank.progress != 0 {
		t.Fatalf("figures before the count = %+v, rank %+v", start.figures, start.rank)
	}
	// After it: the card's own numbers, and the still's frame has everything in place.
	done := frameAt(card, 1.8)
	if done.figures != card.figures() || done.rank != (rankFigures{rp: 1240, remaining: 260, progress: card.Ranked.Progress()}) {
		t.Fatalf("figures after the count = %+v, rank %+v", done.figures, done.rank)
	}
	still := stillFrame(card)
	if still.header != 1 || still.name != 1 || still.row != 1 || still.footer != 1 || still.emblem != 1 || still.emblemScale != 1 || still.glow != glowRest ||
		still.nameRise != 0 || still.rowRise != 0 || still.sweepOn || still.figures != card.figures() {
		t.Fatalf("still frame = %+v", still)
	}
	for i, tile := range still.tiles {
		if tile.alpha != 1 || tile.rise != 0 {
			t.Fatalf("still tile %d = %+v", i, tile)
		}
	}

	// A dash stays a dash, and Master's bar fills over the count.
	bare := Card{PlayerName: "New", Kills: 4}
	for _, tm := range []float64{0, 0.7, 1.0, 1.8, math.Inf(1)} {
		tiles := frameAt(bare, tm).figures.tiles()
		if tiles[4].value != "-" || tiles[6].value != "-" || tiles[7].value != "-" {
			t.Fatalf("unmeasured tiles at t=%v = %v", tm, tiles)
		}
	}
	master := sample()
	master.Ranked = &Ranked{Tier: ranked.Master, RP: 9870, TierStartRP: 2800}
	if p := frameAt(master, 1.0).rank.progress; !(p > 0 && p < 1) {
		t.Fatalf("master bar at t=1.0 = %v", p)
	}
	if p := frameAt(master, 2.0).rank.progress; p != 1 {
		t.Fatalf("master bar after the count = %v", p)
	}
}

func TestSequenceWindows(t *testing.T) {
	card := sample()
	card.Ranked = gold()
	at := func(tm float64) frame { return frameAt(card, tm) }
	// Before a window the element is at its start state; after it, at its end state.
	if f := at(0); f.header != 0 || f.emblem != 0 || f.name != 0 || f.row != 0 || f.footer != 0 || f.glow != 0 || f.emblemScale != emblemStartScale || f.nameRise != nameRise || f.rowRise != rowRise || f.tiles[7].rise != tileRise {
		t.Fatalf("frame at t=0 = %+v", f)
	}
	if f := at(0.3); f.header != 1 || f.name <= 0 || f.name >= 1 || f.nameRise <= 0 || f.nameRise >= nameRise || f.emblemScale <= 1 || f.emblemScale >= emblemStartScale {
		t.Fatalf("frame at t=0.3 = %+v", f)
	}
	if f := at(1.2); f.emblem != 1 || f.name != 1 || f.row != 1 || f.footer != 1 || f.tiles[0].alpha != 1 || f.tiles[7].alpha != 1 || f.tiles[7].rise != 0 {
		t.Fatalf("frame at t=1.2 = %+v", f)
	}
	// Tiles start one after another.
	if f := at(0.5); !(f.tiles[0].alpha > f.tiles[1].alpha && f.tiles[1].alpha > 0 && f.tiles[2].alpha == 0) {
		t.Fatalf("tiles at t=0.5 = %+v", f.tiles)
	}
	// The glow overshoots as the emblem lands, settles, breathes once, and rests.
	for _, c := range []struct{ t, want float64 }{{0.05, 0}, {0.6, glowPeak}, {1.1, glowRest}, {1.5, glowRest}, {2.2, glowRest + glowBreathDepth}, {2.5, glowRest}, {3, glowRest}} {
		if got := glowAt(c.t); math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("glow at t=%v = %v, want %v", c.t, got, c.want)
		}
	}
	// The sweep runs only in its window, from the left edge to the right.
	if f := at(1.99); f.sweepOn {
		t.Fatal("sweep before its window")
	}
	if f := at(2.3); !f.sweepOn || math.Abs(f.sweep-0.5) > 1e-9 {
		t.Fatalf("sweep at t=2.3 = %v %v", f.sweepOn, f.sweep)
	}
	if f := at(2.61); f.sweepOn {
		t.Fatal("sweep after its window")
	}
	// The easing: fast first, settling into place.
	if easeOut(0) != 0 || easeOut(1) != 1 || easeOut(0.5) <= 0.5 || easeOut(2) != 1 || easeOut(-1) != 0 {
		t.Fatal("easeOut")
	}
	if breath(0) != 0 || math.Abs(breath(0.5)-1) > 1e-12 || math.Abs(breath(1)) > 1e-12 {
		t.Fatal("breath")
	}
	if times := frameTimes(); len(times) != MotionFrames+1 || times[0] != 0 || times[1] != 0.04 || times[MotionFrames-1] != 2.56 || !math.IsInf(times[MotionFrames], 1) {
		t.Fatalf("frame times = %v", times)
	}
}

func TestChangedRect(t *testing.T) {
	a := make([]uint8, 20*10)
	b := make([]uint8, 20*10)
	if _, ok := changedRect(a, b, 20, 10); ok {
		t.Fatal("identical maps differ")
	}
	b[3*20+7] = 1
	if r, ok := changedRect(a, b, 20, 10); !ok || r != image.Rect(7, 3, 8, 4) {
		t.Fatalf("one pixel: %v %v", r, ok)
	}
	b[8*20+2] = 1
	if r, ok := changedRect(a, b, 20, 10); !ok || r != image.Rect(2, 3, 8, 9) {
		t.Fatalf("two pixels: %v %v", r, ok)
	}
}
