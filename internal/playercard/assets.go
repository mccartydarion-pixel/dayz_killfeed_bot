package playercard

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/png"
	"strings"
	"sync"

	"github.com/yourname/dayz-killfeed/internal/ranked"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
)

// The card's assets ship inside the binary: the website's own typefaces (Sora for display, Manrope
// for labels; static Latin-subset instances of the variable fonts, SIL OFL 1.1, licence in
// assets/fonts/LICENSE.txt) and the Ranked tier emblems (256 px, transparent). Everything is parsed
// or decoded once, on first use, and shared by every render.

//go:embed assets/fonts/*.ttf
var fontFS embed.FS

//go:embed assets/ranks/*.png
var rankFS embed.FS

// typeface is one of the card's faces. The Sora and Manrope subsets cover Latin only, so a string
// that needs a glyph they lack is drawn whole with the Go font of the matching weight instead
// (gobold for the bold faces, goregular for Manrope 500): a Cyrillic gamertag still reads as a
// name, in a face that is at least the right colour and weight.
type typeface int

const (
	soraExtraBold typeface = iota // Sora 800: the player name and every figure
	manropeBold                   // Manrope 700: eyebrows, labels and the tier pill
	manropeMedium                 // Manrope 500: every other line
)

var typefaceFiles = [...]string{
	soraExtraBold: "assets/fonts/Sora-800.ttf",
	manropeBold:   "assets/fonts/Manrope-700.ttf",
	manropeMedium: "assets/fonts/Manrope-500.ttf",
}

// fontSet is every parsed font. An *sfnt.Font is safe for concurrent use (each caller brings its own
// sfnt.Buffer); the font.Face values built from them are not, so a canvas makes its own.
type fontSet struct {
	primary   [len(typefaceFiles)]*sfnt.Font
	goBold    *sfnt.Font
	goRegular *sfnt.Font
}

// fallback is the Go font that stands in for a typeface.
func (fs *fontSet) fallback(tf typeface) *sfnt.Font {
	if tf == manropeMedium {
		return fs.goRegular
	}
	return fs.goBold
}

var (
	fontsOnce sync.Once
	fonts     fontSet
	fontsErr  error
)

func loadFonts() (*fontSet, error) {
	fontsOnce.Do(func() {
		for tf, path := range typefaceFiles {
			data, err := fontFS.ReadFile(path)
			if err != nil {
				fontsErr = err
				return
			}
			if fonts.primary[tf], err = opentype.Parse(data); err != nil {
				fontsErr = fmt.Errorf("parse %s: %w", path, err)
				return
			}
		}
		if fonts.goBold, fontsErr = opentype.Parse(gobold.TTF); fontsErr != nil {
			return
		}
		fonts.goRegular, fontsErr = opentype.Parse(goregular.TTF)
	})
	if fontsErr != nil {
		return nil, fontsErr
	}
	return &fonts, nil
}

// emblemSize is the side, in pixels, of the tier emblem on the card.
const emblemSize = 180

// tierSlug is the emblem file name of a tier ("gold" for GOLD), the same rule as the website's
// /ranks/<tier>.png and presentation.RankTierIconSlug. A tier this build does not know shows the
// Unranked emblem rather than nothing.
func tierSlug(t ranked.Tier) string {
	slug := strings.ToLower(strings.TrimSpace(string(t)))
	if _, ok := emblems[slug]; ok {
		return slug
	}
	return "unranked"
}

type emblem struct {
	once sync.Once
	img  *image.RGBA // the PNG decoded and scaled to emblemSize with Catmull-Rom, premultiplied
	err  error
}

var emblems = map[string]*emblem{
	"unranked": {}, "rookie": {}, "bronze": {}, "silver": {},
	"gold": {}, "platinum": {}, "diamond": {}, "master": {},
}

// tierEmblem is the emblem of a tier at the card's size. The decode and the (slow, high quality)
// scale happen once per tier per process.
func tierEmblem(t ranked.Tier) (*image.RGBA, error) {
	e := emblems[tierSlug(t)]
	e.once.Do(func() {
		data, err := rankFS.ReadFile("assets/ranks/" + tierSlug(t) + ".png")
		if err != nil {
			e.err = err
			return
		}
		src, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			e.err = fmt.Errorf("decode %s emblem: %w", tierSlug(t), err)
			return
		}
		dst := image.NewRGBA(image.Rect(0, 0, emblemSize, emblemSize))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
		e.img = dst
	})
	return e.img, e.err
}
