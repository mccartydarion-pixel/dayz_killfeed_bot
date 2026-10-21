package playercard

import (
	"image"
	"image/color"
	"math"
	"testing"
)

// The GIF palette and dither (quantize.go).

// gradientImage is a w x h image with a smooth sweep of colour: many distinct colours, none flat.
func gradientImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x * 255 / (w - 1)), uint8(y * 255 / (h - 1)), uint8((x + y) * 255 / (w + h - 2)), 255})
		}
	}
	return img
}

func TestMedianCutKeepsEveryColourOfASmallImageAndStaysWithinTheLimit(t *testing.T) {
	// Fewer distinct colours than entries: each is in the palette exactly, so it never dithers.
	small := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			small.SetRGBA(x, y, color.RGBA{uint8(x * 8), uint8(y * 8), 40, 255})
		}
	}
	for x := 0; x < 32; x++ { // one colour that outnumbers all the others
		for y := 0; y < 26; y++ {
			small.SetRGBA(x, y, color.RGBA{0x0F, 0x11, 0x14, 255})
		}
	}
	hist := histogram(small)
	if len(hist) != 6*32+1 {
		t.Fatalf("%d distinct colours", len(hist))
	}
	colors := medianCut(hist, maxPaletteSize)
	if len(colors) != len(hist) {
		t.Fatalf("%d colours for %d distinct", len(colors), len(hist))
	}
	pal := newPalette(colors)
	for _, e := range hist {
		i, ok := pal.index(e.c)
		if !ok || packRGB(colors[i].R, colors[i].G, colors[i].B) != e.c {
			t.Fatalf("colour %06x is not in the palette exactly", e.c)
		}
	}
	// Many more colours than entries: at most the limit, and a mapped image stays close.
	colors = medianCut(histogram(gradientImage(200, 200)), maxPaletteSize)
	if len(colors) > maxPaletteSize || len(colors) < maxPaletteSize-8 {
		t.Fatalf("%d colours from a gradient", len(colors))
	}
	if len(medianCut(nil, 16)) != 1 {
		t.Fatal("an empty histogram has no colour")
	}
}

func TestQuantizeStaysNearTheSourceAndKeepsExactColoursExact(t *testing.T) {
	src := gradientImage(256, 128)
	pal := newPalette(medianCut(histogram(src), maxPaletteSize))
	m := newMapper(pal)
	idx := make([]uint8, 256*128)
	m.quantize(src, idx)
	// Every pixel is within reach of its colour, and over any 8x8 block the dither averages out.
	var worst, total float64
	for by := 0; by < 128; by += 8 {
		for bx := 0; bx < 256; bx += 8 {
			var want, got [3]float64
			for y := by; y < by+8; y++ {
				for x := bx; x < bx+8; x++ {
					s := src.RGBAAt(x, y)
					q := pal.rgb[idx[y*256+x]]
					want[0] += float64(s.R)
					want[1] += float64(s.G)
					want[2] += float64(s.B)
					got[0] += float64(q[0])
					got[1] += float64(q[1])
					got[2] += float64(q[2])
					d := math.Max(math.Abs(float64(s.R)-float64(q[0])), math.Max(math.Abs(float64(s.G)-float64(q[1])), math.Abs(float64(s.B)-float64(q[2]))))
					worst, total = math.Max(worst, d), total+d
				}
			}
			for ch := 0; ch < 3; ch++ {
				if err := math.Abs(want[ch]-got[ch]) / 64; err > 6 {
					t.Fatalf("block (%d,%d) channel %d is off by %.1f on average", bx, by, ch, err)
				}
			}
		}
	}
	if mean := total / (256 * 128); mean > 10 || worst > 48 {
		t.Fatalf("mean error %.2f, worst %v", mean, worst)
	}
	// A colour in the palette maps to itself wherever it is, dither or not.
	flat := image.NewRGBA(image.Rect(0, 0, 16, 16))
	want := pal.rgb[7]
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			flat.SetRGBA(x, y, color.RGBA{uint8(want[0]), uint8(want[1]), uint8(want[2]), 255})
		}
	}
	out := make([]uint8, 16*16)
	m.quantize(flat, out)
	for i, v := range out {
		if v != 7 {
			t.Fatalf("pixel %d of a flat palette colour mapped to %d", i, v)
		}
	}
}

func TestBayerDitherIsStable(t *testing.T) {
	src := gradientImage(96, 64)
	pal := newPalette(medianCut(histogram(src), 64))
	a, b := make([]uint8, 96*64), make([]uint8, 96*64)
	newMapper(pal).quantize(src, a)
	newMapper(pal).quantize(src, b)
	if string(a) != string(b) {
		t.Fatal("two mappers of one palette disagree")
	}
	// A pixel's index depends on its colour and position only: change one region and the rest
	// of the map is untouched, so a frame diff is exactly that region.
	changed := cloneRGBA(src)
	for y := 20; y < 30; y++ {
		for x := 40; x < 50; x++ {
			changed.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
		}
	}
	c := make([]uint8, 96*64)
	newMapper(pal).quantize(changed, c)
	r, ok := changedRect(a, c, 96, 64)
	if !ok || r != image.Rect(40, 20, 50, 30) {
		t.Fatalf("changed rect = %v %v", r, ok)
	}
	// The dither is not a solid: a gradient uses more than one index per block somewhere.
	distinct := map[uint8]bool{}
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			distinct[a[(y+28)*96+x+40]] = true
		}
	}
	if len(distinct) < 2 {
		t.Fatal("a smooth gradient block maps to one colour: no dither")
	}
}
