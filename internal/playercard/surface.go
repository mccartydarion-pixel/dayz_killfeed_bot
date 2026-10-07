package playercard

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"

	"github.com/yourname/dayz-killfeed/internal/ranked"
)

// Surface is the card's drawing kit on a canvas of any size, for the other Champion images
// (the tournament bracket, docs/TOURNAMENTS.md): the same fonts, shapes, text fitting and tier
// emblems as the Champion Card, so every image looks like one family. Deterministic like the
// card: no clock, no randomness.
type Surface struct {
	c    *canvas
	w, h int
}

// Face is one of the card's typefaces.
type Face int

const (
	FaceDisplay Face = iota // Sora 800: names and figures
	FaceLabel               // Manrope 700: eyebrows, labels, pills
	FaceBody                // Manrope 500: everything else
)

func (f Face) typeface() typeface {
	switch f {
	case FaceDisplay:
		return soraExtraBold
	case FaceLabel:
		return manropeBold
	}
	return manropeMedium
}

// Style is a run of text: a face, a size in pixels and tracking in em.
type Style struct {
	Face  Face
	Size  float64
	Track float64
}

func (s Style) style() style { return style{s.Face.typeface(), s.Size, s.Track} }

// NewSurface makes a canvas of w by h pixels filled with the card's background gradient.
func NewSurface(w, h int) (*Surface, error) {
	fonts, err := loadFonts()
	if err != nil {
		return nil, err
	}
	c := &canvas{img: image.NewRGBA(image.Rect(0, 0, w, h)), fonts: fonts, faces: map[faceKey]*face{}, glyphs: map[glyphKey]*glyph{}}
	for y := 0; y < h; y++ {
		row := lerp(colBackgroundTop, colBackgroundBottom, float64(y)/float64(max(h-1, 1)))
		draw.Draw(c.img, image.Rect(0, y, w, y+1), image.NewUniform(row), image.Point{}, draw.Src)
	}
	return &Surface{c: c, w: w, h: h}, nil
}

// Size is the canvas size.
func (s *Surface) Size() (w, h int) { return s.w, s.h }

// Image is the canvas (read it after drawing).
func (s *Surface) Image() *image.RGBA { return s.c.img }

// PNG encodes the canvas.
func (s *Surface) PNG() ([]byte, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, s.c.img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Err is the first drawing error.
func (s *Surface) Err() error { return s.c.err }

// RGB is a colour from a hex value.
func RGB(hex uint32) color.RGBA { return rgb(hex) }

// Alpha is col at an opacity of a.
func Alpha(col color.RGBA, a float64) color.NRGBA { return alpha(col, a) }

// Lerp is the colour t of the way from a to b.
func Lerp(a, b color.RGBA, t float64) color.RGBA { return lerp(a, b, t) }

// Palette colours the card uses, for the other images.
var (
	ColGold       = colGold
	ColWhite      = colWhite
	ColMuted      = colMuted
	ColTile       = colTile
	ColTileBorder = colTileBorder
	ColHairline   = colHairline
)

// TierColor is a tier's mid tone and highlight, as the card paints its pill.
func TierColor(t ranked.Tier) (mid, hi color.RGBA) {
	p := paletteFor(t)
	return p.mid, p.hi
}

// FillRect paints an axis-aligned rectangle at an opacity of a.
func (s *Surface) FillRect(x, y, w, h int, col color.RGBA, a float64) {
	s.c.fillRect(x, y, w, h, col, a)
}

// RoundRect fills a rounded rectangle.
func (s *Surface) RoundRect(x, y, w, h, r float64, col color.NRGBA) {
	s.c.roundRect(x, y, w, h, r, col)
}

// RoundRectBorder strokes a rounded rectangle inside its box.
func (s *Surface) RoundRectBorder(x, y, w, h, r, width float64, col color.NRGBA) {
	s.c.roundRectBorder(x, y, w, h, r, width, col)
}

// Text draws text with its baseline at y and returns its width; align is -1 left, 0 centre, 1 right.
func (s *Surface) Text(x, y int, st Style, col color.Color, align int, text string) int {
	return s.c.text(x, y, st.style(), col, align, text)
}

// Measure is the width of text in st.
func (s *Surface) Measure(st Style, text string) int { return s.c.measure(st.style(), text) }

// Fit returns the largest of sizes at which text fits maxWidth, and the text cut with an
// ellipsis when even the smallest does not.
func (s *Surface) Fit(st Style, sizes []float64, maxWidth int, text string) (Style, string) {
	got, out := s.c.fit(st.style(), sizes, maxWidth, text)
	st.Size = got.size
	return st, out
}

// CapHeight is the height of a capital letter in st.
func (s *Surface) CapHeight(st Style) int { return s.c.capHeight(st.style()) }

// DrawImage composites a premultiplied image at (x, y) with an opacity of a.
func (s *Surface) DrawImage(src *image.RGBA, x, y int, a float64) { s.c.drawImage(src, x, y, a) }

// TierEmblem is a tier's emblem scaled to size pixels.
func TierEmblem(t ranked.Tier, size int) (*image.RGBA, error) { return tierEmblemAt(t, size) }
