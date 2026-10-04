// Package heatmapimage draws the Discord heatmap picture (docs/HEATMAPS.md "Picture"): the map, a
// dot for every busy cell sized and coloured by how many kills it saw, and the three busiest
// numbered to match the card's "Hot Zones" list. The satellite tiles come from the website
// (public/maps/<map>/<level>/<x>_<y>.webp) and are cached for the life of the process; without
// them the picture is a dark grid.
package heatmapimage

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/webp" // registers the tile format
)

// Size is the picture's edge in pixels.
const Size = 768

// tileLevel is the tile zoom used for the background: 4×4 tiles of 256 px (1024 px across).
const tileLevel = 2

// Point is one busy cell: its centre in map metres and its kill count.
type Point struct {
	X, Z  float64
	Count int64
}

var (
	bgColor   = color.RGBA{14, 15, 18, 255}
	gridColor = color.RGBA{38, 38, 44, 255}
	cool      = color.RGBA{240, 198, 94, 255} // gold
	hot       = color.RGBA{255, 23, 38, 255}  // crimson
	ink       = color.RGBA{242, 242, 243, 255}
	shadow    = color.RGBA{10, 10, 12, 255}
)

// Tiles fetches and caches a map's background from the website.
type Tiles struct {
	BaseURL string // e.g. https://championshp.vip
	Client  *http.Client

	mu    sync.Mutex
	cache map[string]image.Image
	fails map[string]time.Time
}

// Background returns the map's satellite picture at Size×Size, or nil when it cannot be fetched (a
// failure is not retried for ten minutes).
func (t *Tiles) Background(ctx context.Context, mapKey string) image.Image {
	if t == nil || strings.TrimSpace(t.BaseURL) == "" || mapKey == "" {
		return nil
	}
	t.mu.Lock()
	if t.cache == nil {
		t.cache, t.fails = map[string]image.Image{}, map[string]time.Time{}
	}
	if img, ok := t.cache[mapKey]; ok {
		t.mu.Unlock()
		return img
	}
	if at, ok := t.fails[mapKey]; ok && time.Since(at) < 10*time.Minute {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()
	img, err := t.fetch(ctx, mapKey)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.fails[mapKey] = time.Now()
		return nil
	}
	t.cache[mapKey] = img
	return img
}

func (t *Tiles) fetch(ctx context.Context, mapKey string) (image.Image, error) {
	client := t.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	n := 1 << tileLevel
	full := image.NewRGBA(image.Rect(0, 0, n*256, n*256))
	for x := 0; x < n; x++ {
		for y := 0; y < n; y++ {
			url := fmt.Sprintf("%s/maps/%s/%d/%d_%d.webp", strings.TrimRight(t.BaseURL, "/"), mapKey, tileLevel, x, y)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return nil, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			tile, _, err := image.Decode(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("tile %s: status %d: %v", url, resp.StatusCode, err)
			}
			r := image.Rect(x*256, y*256, (x+1)*256, (y+1)*256)
			xdraw.CatmullRom.Scale(full, r, tile, tile.Bounds(), xdraw.Src, nil)
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, Size, Size))
	xdraw.CatmullRom.Scale(out, out.Bounds(), full, full.Bounds(), xdraw.Src, nil)
	return out, nil
}

// Render draws the picture: background (darkened) or a dark grid, then the cells. mapSize is the
// map's edge in metres.
func Render(bg image.Image, mapSize float64, points []Point) ([]byte, error) {
	if mapSize <= 0 {
		return nil, fmt.Errorf("map size must be positive")
	}
	img := image.NewRGBA(image.Rect(0, 0, Size, Size))
	if bg != nil {
		xdraw.Draw(img, img.Bounds(), bg, bg.Bounds().Min, xdraw.Src)
		darken(img, 0.3)
	} else {
		xdraw.Draw(img, img.Bounds(), &image.Uniform{bgColor}, image.Point{}, xdraw.Src)
		step := float64(Size) * 1000 / mapSize // a line every kilometre
		for v := step; v < Size; v += step {
			for i := 0; i < Size; i++ {
				img.SetRGBA(int(v), i, gridColor)
				img.SetRGBA(i, int(v), gridColor)
			}
		}
	}
	if len(points) == 0 {
		return encode(img)
	}
	sorted := append([]Point(nil), points...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Count != sorted[j].Count {
			return sorted[i].Count > sorted[j].Count
		}
		if sorted[i].X != sorted[j].X {
			return sorted[i].X < sorted[j].X
		}
		return sorted[i].Z < sorted[j].Z
	})
	maxCount := float64(sorted[0].Count)
	scale := float64(Size) / mapSize
	// Quietest first, so the busiest paint on top.
	for i := len(sorted) - 1; i >= 0; i-- {
		p := sorted[i]
		t := 0.0
		if maxCount > 1 {
			t = math.Log1p(float64(p.Count-1)) / math.Log1p(maxCount-1)
		}
		cx, cy := p.X*scale, (mapSize-p.Z)*scale
		r := 5 + 9*t
		disc(img, cx, cy, r+2.5, shadow, 1)
		disc(img, cx, cy, r, lerp(cool, hot, t), 0.92)
	}
	face := basicfont.Face7x13
	for i, p := range sorted {
		if i == 3 {
			break
		}
		cx, cy := p.X*scale, (mapSize-p.Z)*scale
		label(img, face, fmt.Sprintf("#%d", i+1), int(cx)+14, int(cy)+5)
	}
	return encode(img)
}

func encode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func darken(img *image.RGBA, by float64) {
	for i := 0; i < len(img.Pix); i += 4 {
		for c := 0; c < 3; c++ {
			img.Pix[i+c] = uint8(float64(img.Pix[i+c]) * (1 - by))
		}
	}
}

func lerp(a, b color.RGBA, t float64) color.RGBA {
	m := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.RGBA{m(a.R, b.R), m(a.G, b.G), m(a.B, b.B), 255}
}

// disc paints a filled, anti-aliased circle blended at alpha.
func disc(img *image.RGBA, cx, cy, r float64, c color.RGBA, alpha float64) {
	b := img.Bounds()
	for y := int(cy - r - 1); y <= int(cy+r+1); y++ {
		for x := int(cx - r - 1); x <= int(cx+r+1); x++ {
			if !(image.Point{x, y}).In(b) {
				continue
			}
			d := math.Hypot(float64(x)+0.5-cx, float64(y)+0.5-cy)
			cover := math.Max(0, math.Min(1, r-d+0.5)) * alpha
			if cover <= 0 {
				continue
			}
			o := img.PixOffset(x, y)
			img.Pix[o] = uint8(float64(img.Pix[o])*(1-cover) + float64(c.R)*cover)
			img.Pix[o+1] = uint8(float64(img.Pix[o+1])*(1-cover) + float64(c.G)*cover)
			img.Pix[o+2] = uint8(float64(img.Pix[o+2])*(1-cover) + float64(c.B)*cover)
			img.Pix[o+3] = 255
		}
	}
}

// label draws text with a dark outline so it reads on any background.
func label(img *image.RGBA, face font.Face, text string, x, y int) {
	draw := func(dx, dy int, c color.RGBA) {
		d := &font.Drawer{Dst: img, Src: image.NewUniform(c), Face: face, Dot: fixed.P(x+dx, y+dy)}
		d.DrawString(text)
	}
	for _, o := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}, {-1, -1}, {1, 1}, {-1, 1}, {1, -1}} {
		draw(o[0], o[1], shadow)
	}
	draw(0, 0, ink)
}
