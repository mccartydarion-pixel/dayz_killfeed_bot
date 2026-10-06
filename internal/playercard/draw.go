package playercard

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Drawing helpers for the card. Everything here is deterministic: no clock, no randomness, no map
// iteration order in the output. Shapes are anti-aliased with golang.org/x/image/vector and
// composited through their coverage mask, so a fill can be a flat colour, a gradient or a glow.

// canvas is the card being drawn. The first drawing error is kept and reported by Render.
type canvas struct {
	img   *image.RGBA
	fonts *fontSet
	// faces are built per canvas: a font.Face is not safe for concurrent use, and cards render
	// concurrently (the player API, the public share route and /card).
	faces map[faceKey]*face
	buf   sfnt.Buffer
	err   error
}

func newCanvas(fonts *fontSet) *canvas {
	return &canvas{img: image.NewRGBA(image.Rect(0, 0, Width, Height)), fonts: fonts, faces: map[faceKey]*face{}}
}

func (c *canvas) fail(err error) {
	if c.err == nil {
		c.err = err
	}
}

// --- colour ----------------------------------------------------------------------------------------

func rgb(hex uint32) color.RGBA {
	return color.RGBA{uint8(hex >> 16), uint8(hex >> 8), uint8(hex), 0xFF}
}

// alpha is col at an opacity of a (0 to 1), as a straight (non-premultiplied) colour.
func alpha(col color.RGBA, a float64) color.NRGBA {
	return color.NRGBA{col.R, col.G, col.B, uint8(math.Round(255 * clamp01(a)))}
}

func opaque(col color.RGBA) color.NRGBA { return color.NRGBA{col.R, col.G, col.B, 0xFF} }

// lerp is the colour t of the way from a to b.
func lerp(a, b color.RGBA, t float64) color.RGBA {
	t = clamp01(t)
	mix := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t)) }
	return color.RGBA{mix(a.R, b.R), mix(a.G, b.G), mix(a.B, b.B), 0xFF}
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

// blend composites col over the pixel at (x, y) through a coverage of cov (0 to 255). The canvas is
// opaque (the background is painted first), so the result keeps full alpha. Integer arithmetic:
// the same inputs always give the same pixel.
func (c *canvas) blend(x, y int, col color.NRGBA, cov uint8) {
	if cov == 0 || col.A == 0 || !(image.Point{x, y}.In(c.img.Rect)) {
		return
	}
	const full = 255 * 255
	a := uint32(cov) * uint32(col.A)
	i := c.img.PixOffset(x, y)
	p := c.img.Pix[i : i+4 : i+4]
	p[0] = uint8((uint32(p[0])*(full-a) + uint32(col.R)*a + full/2) / full)
	p[1] = uint8((uint32(p[1])*(full-a) + uint32(col.G)*a + full/2) / full)
	p[2] = uint8((uint32(p[2])*(full-a) + uint32(col.B)*a + full/2) / full)
	p[3] = 0xFF
}

// --- flat areas ------------------------------------------------------------------------------------

// rect fills an axis-aligned rectangle; integer edges need no anti-aliasing.
func (c *canvas) rect(x, y, w, h int, col color.Color) {
	draw.Draw(c.img, image.Rect(x, y, x+w, y+h), image.NewUniform(col), image.Point{}, draw.Src)
}

// background paints the whole canvas with a vertical gradient from top to bottom.
func (c *canvas) background(top, bottom color.RGBA) {
	for y := 0; y < Height; y++ {
		row := lerp(top, bottom, float64(y)/float64(Height-1))
		c.rect(0, y, Width, 1, row)
	}
}

// glow is a soft radial light: alpha peak at the centre fading linearly to nothing at radius r.
func (c *canvas) glow(cx, cy, r float64, col color.RGBA, peak float64) {
	x0, x1 := int(math.Floor(cx-r)), int(math.Ceil(cx+r))
	y0, y1 := int(math.Floor(cy-r)), int(math.Ceil(cy+r))
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			if d >= r {
				continue
			}
			c.blend(x, y, alpha(col, peak*(1-d/r)), 0xFF)
		}
	}
}

// --- vector shapes ---------------------------------------------------------------------------------

// kappa is the control-point distance, as a fraction of the radius, of a cubic Bézier quarter circle.
const kappa = 0.5522847498

// shape is a path being built over the part of the canvas it covers.
type shape struct {
	z      *vector.Rasterizer
	ox, oy int // canvas position of the rasterizer's origin
}

// newShape makes a rasterizer for a path within the box (x, y, w, h), with a pixel of margin for
// anti-aliased edges.
func newShape(x, y, w, h float64) *shape {
	ox, oy := int(math.Floor(x))-1, int(math.Floor(y))-1
	return &shape{z: vector.NewRasterizer(int(math.Ceil(x+w))+1-ox, int(math.Ceil(y+h))+1-oy), ox: ox, oy: oy}
}

// roundedRect adds a rectangle with corners of radius r (clamped to half the shorter side) to the
// path. It runs clockwise; reverse runs it anticlockwise, so a reversed inner outline cut from an
// outer one leaves a ring (the rasterizer fills by the nonzero winding rule).
func (s *shape) roundedRect(x, y, w, h, r float64, reverse bool) {
	r = math.Min(r, math.Min(w, h)/2)
	x -= float64(s.ox)
	y -= float64(s.oy)
	k := r * kappa
	type pt struct{ x, y float64 }
	// Each corner's arc: its start, two control points and end, clockwise from the top right.
	arcs := [4][4]pt{
		{{x + w - r, y}, {x + w - r + k, y}, {x + w, y + r - k}, {x + w, y + r}},
		{{x + w, y + h - r}, {x + w, y + h - r + k}, {x + w - r + k, y + h}, {x + w - r, y + h}},
		{{x + r, y + h}, {x + r - k, y + h}, {x, y + h - r + k}, {x, y + h - r}},
		{{x, y + r}, {x, y + r - k}, {x + r - k, y}, {x + r, y}},
	}
	line := func(p pt) { s.z.LineTo(float32(p.x), float32(p.y)) }
	curve := func(a, b, p pt) {
		s.z.CubeTo(float32(a.x), float32(a.y), float32(b.x), float32(b.y), float32(p.x), float32(p.y))
	}
	s.z.MoveTo(float32(x+r), float32(y))
	if !reverse {
		for _, arc := range arcs {
			line(arc[0])
			curve(arc[1], arc[2], arc[3])
		}
	} else {
		for i := len(arcs) - 1; i >= 0; i-- {
			line(arcs[i][3])
			curve(arcs[i][2], arcs[i][1], arcs[i][0])
		}
	}
	s.z.ClosePath()
}

// paint composites the shape onto the canvas, colouring each covered pixel with paint(x, y).
func (c *canvas) paint(s *shape, paint func(x, y int) color.NRGBA) {
	size := s.z.Size()
	mask := image.NewAlpha(image.Rect(0, 0, size.X, size.Y))
	s.z.DrawOp = draw.Src
	s.z.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	for y := 0; y < size.Y; y++ {
		for x := 0; x < size.X; x++ {
			if cov := mask.Pix[y*mask.Stride+x]; cov != 0 {
				c.blend(s.ox+x, s.oy+y, paint(s.ox+x, s.oy+y), cov)
			}
		}
	}
}

// fill composites the shape in one colour.
func (c *canvas) fill(s *shape, col color.NRGBA) {
	c.paint(s, func(int, int) color.NRGBA { return col })
}

// roundRect fills a rounded rectangle.
func (c *canvas) roundRect(x, y, w, h, r float64, col color.NRGBA) {
	s := newShape(x, y, w, h)
	s.roundedRect(x, y, w, h, r, false)
	c.fill(s, col)
}

// roundRectBorder strokes a rounded rectangle with a border of the given width, inside its box.
func (c *canvas) roundRectBorder(x, y, w, h, r, width float64, col color.NRGBA) {
	s := newShape(x, y, w, h)
	s.roundedRect(x, y, w, h, r, false)
	s.roundedRect(x+width, y+width, w-2*width, h-2*width, r-width, true)
	c.fill(s, col)
}

// bar draws a progress bar with fully rounded ends: the track, then the filled fraction painted
// left to right from lo to hi. A fraction above zero is never narrower than the bar is tall.
func (c *canvas) bar(x, y, w, h float64, frac float64, track color.RGBA, lo, hi color.RGBA) {
	c.roundRect(x, y, w, h, h/2, opaque(track))
	frac = clamp01(frac)
	if frac == 0 {
		return
	}
	fw := math.Max(h, math.Round(frac*w))
	s := newShape(x, y, fw, h)
	s.roundedRect(x, y, fw, h, h/2, false)
	c.paint(s, func(px, _ int) color.NRGBA {
		return opaque(lerp(lo, hi, (float64(px)+0.5-x)/fw))
	})
}

// --- text ------------------------------------------------------------------------------------------

// style is a run of text: its typeface, size in pixels and tracking (extra advance after every
// glyph as a fraction of the size: CSS letter-spacing in em).
type style struct {
	tf    typeface
	size  float64
	track float64
}

func (st style) at(size float64) style {
	st.size = size
	return st
}

type faceKey struct {
	tf       typeface
	fallback bool
	size     float64
}

// face is one font at one size. opentype.Face.Kern ignores the size, so kerning pairs are read from
// the font itself at the face's scale.
type face struct {
	font.Face
	src   *sfnt.Font
	scale fixed.Int26_6
}

func (c *canvas) face(tf typeface, fallback bool, size float64) *face {
	key := faceKey{tf, fallback, size}
	if f, ok := c.faces[key]; ok {
		return f
	}
	src := c.fonts.primary[tf]
	if fallback {
		src = c.fonts.fallback(tf)
	}
	f, err := opentype.NewFace(src, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		c.fail(err)
		return nil
	}
	out := &face{Face: f, src: src, scale: fixed.Int26_6(0.5 + size*64)}
	c.faces[key] = out
	return out
}

func (c *canvas) kern(f *face, r0, r1 rune) fixed.Int26_6 {
	x0, err0 := f.src.GlyphIndex(&c.buf, r0)
	x1, err1 := f.src.GlyphIndex(&c.buf, r1)
	if err0 != nil || err1 != nil || x0 == 0 || x1 == 0 {
		return 0
	}
	k, err := f.src.Kern(&c.buf, x0, x1, f.scale, font.HintingNone)
	if err != nil {
		return 0
	}
	return k
}

// covers reports whether f has a glyph for every rune of s.
func covers(f font.Face, s string) bool {
	for _, r := range s {
		if _, ok := f.GlyphAdvance(r); !ok {
			return false
		}
	}
	return true
}

// printable replaces every rune the face has no glyph for (and control characters) with '?', so a
// gamertag in an unsupported script never renders as a row of empty boxes of unknown width.
func printable(f font.Face, s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r < 0x20 || r == 0x7F {
			continue
		}
		if _, ok := f.GlyphAdvance(r); !ok {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// resolve picks the face that draws s: the style's own typeface when it has every glyph, else the
// Go fallback of the same weight. The string comes back printable in that face.
func (c *canvas) resolve(st style, s string) (*face, string) {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	f := c.face(st.tf, false, st.size)
	if f == nil {
		return nil, ""
	}
	if !covers(f, s) {
		if f = c.face(st.tf, true, st.size); f == nil {
			return nil, ""
		}
	}
	return f, printable(f, s)
}

// layout places the glyphs of s: the x offset of each from the start of the run, and the run's
// advance (tracking after the last glyph excluded, so right-aligned text ends where it should).
func (c *canvas) layout(f *face, st style, s string) ([]fixed.Int26_6, fixed.Int26_6) {
	track := fixed.Int26_6(math.Round(st.track * st.size * 64))
	runes := []rune(s)
	xs := make([]fixed.Int26_6, len(runes))
	var x fixed.Int26_6
	for i, r := range runes {
		if i > 0 {
			x += c.kern(f, runes[i-1], r) + track
		}
		xs[i] = x
		adv, _ := f.GlyphAdvance(r)
		x += adv
	}
	return xs, x
}

// measure is the width of s in st, in whole pixels.
func (c *canvas) measure(st style, s string) int {
	f, s := c.resolve(st, s)
	if f == nil {
		return 0
	}
	_, w := c.layout(f, st, s)
	return w.Ceil()
}

// text draws s with its baseline at y and returns its width. align is -1 (x is the left edge), 0
// (centre) or 1 (right edge).
func (c *canvas) text(x, y int, st style, col color.Color, align int, s string) int {
	f, s := c.resolve(st, s)
	if f == nil {
		return 0
	}
	xs, w := c.layout(f, st, s)
	origin := fixed.I(x)
	switch align {
	case 0:
		origin -= w / 2
	case 1:
		origin -= w
	}
	src := image.NewUniform(col)
	for i, r := range []rune(s) {
		dr, mask, maskp, _, ok := f.Glyph(fixed.Point26_6{X: origin + xs[i], Y: fixed.I(y)}, r)
		if !ok {
			continue
		}
		draw.DrawMask(c.img, dr, src, image.Point{}, mask, maskp, draw.Over)
	}
	return w.Ceil()
}

// fit returns the largest of sizes at which s fits maxWidth, and s cut with an ellipsis when even
// the smallest does not.
func (c *canvas) fit(st style, sizes []float64, maxWidth int, s string) (style, string) {
	for _, size := range sizes {
		if c.measure(st.at(size), s) <= maxWidth {
			return st.at(size), s
		}
	}
	st = st.at(sizes[len(sizes)-1])
	r := []rune(strings.TrimSpace(s))
	for len(r) > 1 && c.measure(st, strings.TrimRight(string(r), " ")+"…") > maxWidth {
		r = r[:len(r)-1]
	}
	return st, strings.TrimRight(string(r), " ") + "…"
}

// capHeight is the height of a capital letter in st, for centring a label on a shape.
func (c *canvas) capHeight(st style) int {
	f := c.face(st.tf, false, st.size)
	if f == nil {
		return int(st.size * 0.7)
	}
	if b, _, ok := f.GlyphBounds('H'); ok {
		return (-b.Min.Y).Round()
	}
	return int(st.size * 0.7)
}
