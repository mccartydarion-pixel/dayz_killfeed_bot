package playercard

import (
	"image"
	"image/color"
	"slices"
)

// Colour for the GIF. One global palette serves the whole animation: a median cut over the exact
// colours of a few sample frames, so the flat areas (the background's gradient steps, the tiles)
// get their own entries and never dither. Pixels map to it through a 64x64x64 lookup table and an
// 8x8 Bayer ordered dither, a pure function of the pixel's colour and position: a region that does
// not change between frames maps to the same indices, which is what lets a frame carry only the
// rectangle that changed (error diffusion would scatter the difference everywhere).

const maxPaletteSize = 256

// colorCount is an exact colour (0xRRGGBB) and how many sample pixels have it.
type colorCount struct {
	c uint32
	n uint32
}

func packRGB(r, g, b uint8) uint32 { return uint32(r)<<16 | uint32(g)<<8 | uint32(b) }

// histogram counts the colours of opaque RGBA images, sorted by colour so the cut is deterministic.
func histogram(imgs ...*image.RGBA) []colorCount {
	counts := map[uint32]uint32{}
	for _, img := range imgs {
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			row := img.Pix[img.PixOffset(b.Min.X, y):img.PixOffset(b.Max.X, y)]
			// Runs of one colour are the common case: count them in one map write.
			last, run := packRGB(row[0], row[1], row[2]), uint32(0)
			for i := 0; i < len(row); i += 4 {
				if c := packRGB(row[i], row[i+1], row[i+2]); c != last {
					counts[last] += run
					last, run = c, 0
				}
				run++
			}
			counts[last] += run
		}
	}
	out := make([]colorCount, 0, len(counts))
	for c, n := range counts {
		out = append(out, colorCount{c, n})
	}
	slices.SortFunc(out, func(a, b colorCount) int { return int(a.c) - int(b.c) })
	return out
}

// box is a group of histogram entries (entries[lo:hi] of the shared slice) with their bounds.
type box struct {
	lo, hi   int
	count    uint64
	min, max [3]uint8
}

func channel(c uint32, ch int) uint8 { return uint8(c >> (16 - 8*ch)) }

func newBox(entries []colorCount, lo, hi int) box {
	b := box{lo: lo, hi: hi, min: [3]uint8{255, 255, 255}}
	for _, e := range entries[lo:hi] {
		b.count += uint64(e.n)
		for ch := 0; ch < 3; ch++ {
			v := channel(e.c, ch)
			b.min[ch], b.max[ch] = min(b.min[ch], v), max(b.max[ch], v)
		}
	}
	return b
}

// longest is the channel with the widest range, and the range.
func (b box) longest() (ch int, span int) {
	for i := 0; i < 3; i++ {
		if s := int(b.max[i]) - int(b.min[i]); s > span {
			ch, span = i, s
		}
	}
	return ch, span
}

// medianCut picks up to n colours for the histogram: boxes split at their median along their
// longest side, the most populous wide box first, until there are n or nothing is left to split.
// Each colour is the mean of its box, so a box of one colour is that colour exactly.
func medianCut(entries []colorCount, n int) []color.RGBA {
	if len(entries) == 0 {
		return []color.RGBA{{0, 0, 0, 255}}
	}
	entries = slices.Clone(entries)
	boxes := []box{newBox(entries, 0, len(entries))}
	for len(boxes) < n {
		// The box to split: the widest, weighted by its population, so the busy dark gradient
		// gets as many entries as the colourful emblem.
		best, bestScore := -1, uint64(0)
		for i, b := range boxes {
			if _, span := b.longest(); span > 0 {
				if score := b.count * uint64(span); score > bestScore {
					best, bestScore = i, score
				}
			}
		}
		if best < 0 {
			break
		}
		b := boxes[best]
		ch, _ := b.longest()
		part := entries[b.lo:b.hi]
		slices.SortFunc(part, func(x, y colorCount) int {
			if d := int(channel(x.c, ch)) - int(channel(y.c, ch)); d != 0 {
				return d
			}
			return int(x.c) - int(y.c)
		})
		// Split where the count passes the middle, keeping both halves non-empty.
		mid, acc := b.lo, uint64(0)
		for mid < b.hi-1 && acc+uint64(entries[mid].n) < b.count/2 {
			acc += uint64(entries[mid].n)
			mid++
		}
		mid = max(mid, b.lo+1)
		boxes[best] = newBox(entries, b.lo, mid)
		boxes = append(boxes, newBox(entries, mid, b.hi))
	}
	colors := make([]color.RGBA, 0, len(boxes))
	for _, b := range boxes {
		var sum [3]uint64
		for _, e := range entries[b.lo:b.hi] {
			for ch := 0; ch < 3; ch++ {
				sum[ch] += uint64(channel(e.c, ch)) * uint64(e.n)
			}
		}
		mean := func(ch int) uint8 { return uint8((sum[ch] + b.count/2) / b.count) }
		colors = append(colors, color.RGBA{mean(0), mean(1), mean(2), 255})
	}
	return colors
}

// palette is the GIF's colour table with the lookups that map pixels to it.
type palette struct {
	colors color.Palette // the global colour table, as the encoder takes it
	rgb    [][3]int32
	// exact finds a colour that is in the palette outright (open addressing, colour → index+1):
	// such a pixel keeps its colour and takes no dither.
	exact [1024]uint64
}

func newPalette(colors []color.RGBA) *palette {
	p := &palette{}
	for i, c := range colors {
		p.colors = append(p.colors, c)
		p.rgb = append(p.rgb, [3]int32{int32(c.R), int32(c.G), int32(c.B)})
		key := packRGB(c.R, c.G, c.B)
		for h := exactSlot(key); ; h = (h + 1) % len(p.exact) {
			if p.exact[h] == 0 {
				p.exact[h] = uint64(key)<<16 | uint64(i+1)
				break
			}
			if uint32(p.exact[h]>>16) == key {
				break // the same colour twice: the first index wins
			}
		}
	}
	return p
}

func exactSlot(c uint32) int { return int((c * 2654435761) >> 22) }

// index is the palette index of c when c is in the palette.
func (p *palette) index(c uint32) (uint8, bool) {
	for h := exactSlot(c); ; h = (h + 1) % len(p.exact) {
		e := p.exact[h]
		if e == 0 {
			return 0, false
		}
		if uint32(e>>16) == c {
			return uint8(e&0xFFFF) - 1, true
		}
	}
}

// nearest is the palette index closest to a colour (the lowest on a tie).
func (p *palette) nearest(r, g, b int32) uint8 {
	best, bestD := 0, int32(1<<30)
	for i, q := range p.rgb {
		dr, dg, db := r-q[0], g-q[1], b-q[2]
		if d := dr*dr + dg*dg + db*db; d < bestD {
			best, bestD = i, d
		}
	}
	return uint8(best)
}

// bayer8 is the 8x8 ordered dither threshold matrix (0..63).
var bayer8 = [8][8]uint8{
	{0, 32, 8, 40, 2, 34, 10, 42},
	{48, 16, 56, 24, 50, 18, 58, 26},
	{12, 44, 4, 36, 14, 46, 6, 38},
	{60, 28, 52, 20, 62, 30, 54, 22},
	{3, 35, 11, 43, 1, 33, 9, 41},
	{51, 19, 59, 27, 49, 17, 57, 25},
	{15, 47, 7, 39, 13, 45, 5, 37},
	{63, 31, 55, 23, 61, 29, 53, 21},
}

// mapper maps pixels to a palette through a 64x64x64 lookup table of nearest colours, one cell per
// 4x4x4 block of colour space, filled as cells are first met. A mapper belongs to one goroutine;
// every mapper of a palette gives the same answers.
type mapper struct {
	p   *palette
	lut []int16 // nearest palette index per cell, -1 until computed
}

func newMapper(p *palette) *mapper {
	m := &mapper{p: p, lut: make([]int16, 64*64*64)}
	for i := range m.lut {
		m.lut[i] = -1
	}
	return m
}

// cell is the palette index nearest the centre of the cell holding (r, g, b).
func (m *mapper) cell(r, g, b int32) uint8 {
	i := (r>>2)<<12 | (g>>2)<<6 | b>>2
	if v := m.lut[i]; v >= 0 {
		return uint8(v)
	}
	v := m.p.nearest(r>>2<<2+2, g>>2<<2+2, b>>2<<2+2)
	m.lut[i] = int16(v)
	return v
}

func clampByte(v int32) int32 { return max(0, min(255, v)) }

// quantize maps an opaque RGBA image to palette indices, row-major, into dst. A colour that is in
// the palette maps to itself. Any other maps to the nearest cell's colour or to the colour on the
// far side of it, the Bayer threshold at the pixel's position deciding, in the proportion that
// averages back to the pixel's colour: a stable dither that varies only where colours do.
func (m *mapper) quantize(img *image.RGBA, dst []uint8) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	for y := 0; y < h; y++ {
		thresholds := &bayer8[y&7]
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		out := dst[y*w : (y+1)*w]
		for x := 0; x < w; x++ {
			pr, pg, pb := row[x*4], row[x*4+1], row[x*4+2]
			if i, ok := m.p.index(packRGB(pr, pg, pb)); ok {
				out[x] = i
				continue
			}
			r, g, b := int32(pr), int32(pg), int32(pb)
			p0 := m.cell(r, g, b)
			q0 := m.p.rgb[p0]
			p1 := m.cell(clampByte(2*r-q0[0]), clampByte(2*g-q0[1]), clampByte(2*b-q0[2]))
			if p1 == p0 {
				out[x] = p0
				continue
			}
			q1 := m.p.rgb[p1]
			dr, dg, db := q1[0]-q0[0], q1[1]-q0[1], q1[2]-q0[2]
			num := (r-q0[0])*dr + (g-q0[1])*dg + (b-q0[2])*db
			den := dr*dr + dg*dg + db*db
			switch {
			case num <= 0:
				out[x] = p0
			case num >= den:
				out[x] = p1
			case int32(thresholds[x&7]) < (num*64+den/2)/den:
				out[x] = p1
			default:
				out[x] = p0
			}
		}
	}
}
