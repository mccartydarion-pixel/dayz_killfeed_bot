// Package playercard renders the Champion Card (docs/CHAMPION_CARD.md): a 1200x630 PNG of one
// player's stats on one server, sized for link previews and Discord attachments. Rendering is pure
// (no I/O, no clock): the same Card always produces the same image.
package playercard

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/deathstats"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Width and Height are the Open Graph image size.
const (
	Width  = 1200
	Height = 630
)

// Card is everything the image shows. Pointer fields are optional: a nil value's tile shows an em
// dash rather than a made-up number.
type Card struct {
	PlayerName  string
	ServerName  string
	FactionName string
	FactionTag  string
	SeasonName  string

	Kills  int
	Deaths int // every death
	// PvPDeaths are the deaths caused by another player (internal/deathstats); nil = the split is
	// not known, and the card then shows deaths and K/D alone, as it always has.
	PvPDeaths         *int
	Headshots         int
	LongestKillMeters float64
	PlaytimeSeconds   int64
	// LongestLifeSeconds is the longest recorded life by observed playtime.
	LongestLifeSeconds *int64
	// Rank is the player's position by kills among RankedPlayers on the server.
	Rank          *int
	RankedPlayers int

	GeneratedAt time.Time
}

// KD follows the convention used everywhere else: kills when there are no deaths.
func (c Card) KD() float64 {
	if c.Deaths == 0 {
		return float64(c.Kills)
	}
	return float64(c.Kills) / float64(c.Deaths)
}

// PvEDeaths are the deaths not caused by another player; ok is false when the split is not known.
func (c Card) PvEDeaths() (n int, ok bool) {
	if c.PvPDeaths == nil {
		return 0, false
	}
	return int(deathstats.PvE(int64(c.Deaths), int64(*c.PvPDeaths))), true
}

// PvPKD is kills per PvP death, by the same zero rule as KD; ok is false when the split is not known.
func (c Card) PvPKD() (kd float64, ok bool) {
	if c.PvPDeaths == nil {
		return 0, false
	}
	return deathstats.KD(int64(c.Kills), int64(*c.PvPDeaths)), true
}

var (
	colBackground = color.RGBA{0x14, 0x17, 0x1A, 0xFF}
	colTile       = color.RGBA{0x1E, 0x23, 0x27, 0xFF}
	colGold       = color.RGBA{0xC9, 0xA2, 0x27, 0xFF} // presentation.ChampionGold
	colWhite      = color.RGBA{0xF2, 0xF2, 0xF0, 0xFF}
	colMuted      = color.RGBA{0x8B, 0x93, 0x9B, 0xFF}
)

var (
	fontsOnce sync.Once
	fontsErr  error
	boldFont  *opentype.Font
	plainFont *opentype.Font
	facesMu   sync.Mutex
	faces     = map[faceKey]font.Face{}
)

type faceKey struct {
	bold bool
	size float64
}

func loadFonts() error {
	fontsOnce.Do(func() {
		if boldFont, fontsErr = opentype.Parse(gobold.TTF); fontsErr != nil {
			return
		}
		plainFont, fontsErr = opentype.Parse(goregular.TTF)
	})
	return fontsErr
}

func face(bold bool, size float64) (font.Face, error) {
	facesMu.Lock()
	defer facesMu.Unlock()
	key := faceKey{bold, size}
	if f, ok := faces[key]; ok {
		return f, nil
	}
	src := plainFont
	if bold {
		src = boldFont
	}
	f, err := opentype.NewFace(src, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	faces[key] = f
	return f, nil
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

type canvas struct {
	img *image.RGBA
	err error
}

func (c *canvas) rect(x, y, w, h int, col color.Color) {
	draw.Draw(c.img, image.Rect(x, y, x+w, y+h), image.NewUniform(col), image.Point{}, draw.Src)
}

// roundRect fills a rectangle with corners of radius r.
func (c *canvas) roundRect(x, y, w, h, r int, col color.RGBA) {
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			dx, dy := 0, 0
			switch {
			case px < r:
				dx = r - px
			case px >= w-r:
				dx = px - (w - r - 1)
			}
			switch {
			case py < r:
				dy = r - py
			case py >= h-r:
				dy = py - (h - r - 1)
			}
			if dx > 0 && dy > 0 && dx*dx+dy*dy > r*r {
				continue
			}
			c.img.SetRGBA(x+px, y+py, col)
		}
	}
}

func (c *canvas) width(bold bool, size float64, s string) int {
	f, err := face(bold, size)
	if err != nil {
		c.err = err
		return 0
	}
	return font.MeasureString(f, printable(f, s)).Ceil()
}

// text draws s with its baseline at y. align is -1 (x is the left edge), 0 (centre) or 1 (right edge).
func (c *canvas) text(x, y int, bold bool, size float64, col color.Color, align int, s string) {
	f, err := face(bold, size)
	if err != nil {
		c.err = err
		return
	}
	s = printable(f, s)
	w := font.MeasureString(f, s).Ceil()
	switch align {
	case 0:
		x -= w / 2
	case 1:
		x -= w
	}
	d := font.Drawer{Dst: c.img, Src: image.NewUniform(col), Face: f, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

// fit returns the largest size from sizes at which s fits maxWidth, and s truncated with an
// ellipsis if even the smallest does not.
func (c *canvas) fit(bold bool, sizes []float64, maxWidth int, s string) (float64, string) {
	for _, size := range sizes {
		if c.width(bold, size, s) <= maxWidth {
			return size, s
		}
	}
	size := sizes[len(sizes)-1]
	r := []rune(s)
	for len(r) > 1 && c.width(bold, size, string(r)+"...") > maxWidth {
		r = r[:len(r)-1]
	}
	return size, string(r) + "..."
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// FormatDuration renders playtime for a tile: "47m", "12h 05m", "9d 4h".
func FormatDuration(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh %02dm", seconds/3600, (seconds%3600)/60)
	default:
		return fmt.Sprintf("%dd %dh", seconds/86400, (seconds%86400)/3600)
	}
}

type tile struct {
	label string
	value string
	gold  bool
	// note is a small caption under the figure: the PvP/PvE split of the tile's figure.
	note string
}

// Tiles is the stat grid, row by row. Exposed so the JSON card and the image never disagree.
func (c Card) Tiles() [][2]string {
	out := make([][2]string, 0, 8)
	for _, t := range c.tiles() {
		out = append(out, [2]string{t.label, t.value})
	}
	return out
}

// TileNotes is the small caption of each tile, in the order of Tiles ("" for a tile without one).
// Only the deaths and K/D tiles carry one, and only when the PvP/PvE split is known.
func (c Card) TileNotes() []string {
	out := make([]string, 0, 8)
	for _, t := range c.tiles() {
		out = append(out, t.note)
	}
	return out
}

func (c Card) tiles() []tile {
	const none = "-"
	longestKill, longestLife, rank := none, none, none
	if c.LongestKillMeters > 0 {
		longestKill = fmt.Sprintf("%.0f m", c.LongestKillMeters)
	}
	if c.LongestLifeSeconds != nil && *c.LongestLifeSeconds > 0 {
		longestLife = FormatDuration(*c.LongestLifeSeconds)
	}
	if c.Rank != nil && *c.Rank > 0 {
		rank = "#" + thousands(*c.Rank)
	}
	var deathsNote, kdNote string
	if pve, ok := c.PvEDeaths(); ok {
		pvpKD, _ := c.PvPKD()
		deathsNote = "PVP " + thousands(*c.PvPDeaths) + " / PVE " + thousands(pve)
		kdNote = "PVP " + fmt.Sprintf("%.2f", pvpKD)
	}
	return []tile{
		{label: "KILLS", value: thousands(c.Kills), gold: true},
		{label: "DEATHS", value: thousands(c.Deaths), note: deathsNote},
		{label: "K/D", value: fmt.Sprintf("%.2f", c.KD()), note: kdNote},
		{label: "HEADSHOTS", value: thousands(c.Headshots)},
		{label: "LONGEST KILL", value: longestKill},
		{label: "PLAYTIME", value: FormatDuration(c.PlaytimeSeconds)},
		{label: "LONGEST LIFE", value: longestLife},
		{label: "SERVER RANK", value: rank, gold: true},
	}
}

// Render draws the card and returns it PNG-encoded.
func Render(c Card) ([]byte, error) {
	if err := loadFonts(); err != nil {
		return nil, fmt.Errorf("load card fonts: %w", err)
	}
	cv := &canvas{img: image.NewRGBA(image.Rect(0, 0, Width, Height))}
	cv.rect(0, 0, Width, Height, colBackground)
	cv.rect(0, 0, 14, Height, colGold)

	const left, right = 64, Width - 56
	cv.text(left, 76, true, 26, colGold, -1, "CHAMPIONS KILLFEED")
	if c.ServerName != "" {
		_, server := cv.fit(false, []float64{26}, 560, c.ServerName)
		cv.text(right, 76, false, 26, colMuted, 1, server)
	}
	cv.rect(left, 96, right-left, 2, colTile)

	name := c.PlayerName
	if strings.TrimSpace(name) == "" {
		name = "Unknown"
	}
	size, name := cv.fit(true, []float64{84, 72, 60, 50}, right-left, name)
	cv.text(left, 196, true, size, colWhite, -1, name)

	var sub []string
	switch {
	case c.FactionTag != "" && c.FactionName != "":
		sub = append(sub, "["+c.FactionTag+"] "+c.FactionName)
	case c.FactionName != "":
		sub = append(sub, c.FactionName)
	}
	if c.SeasonName != "" {
		sub = append(sub, c.SeasonName)
	}
	if len(sub) > 0 {
		_, line := cv.fit(false, []float64{30}, right-left, strings.Join(sub, "  /  "))
		cv.text(left, 244, false, 30, colMuted, -1, line)
	}

	const (
		gridTop = 276
		tileW   = 258
		tileH   = 132 // room under the figure for a tile's caption
		gap     = 16
		rowGap  = 14
	)
	for i, t := range c.tiles() {
		x := left + (i%4)*(tileW+gap)
		y := gridTop + (i/4)*(tileH+rowGap)
		cv.roundRect(x, y, tileW, tileH, 14, colTile)
		cv.text(x+22, y+38, true, 19, colMuted, -1, t.label)
		col := colWhite
		if t.gold {
			col = colGold
		}
		valueSize, value := cv.fit(true, []float64{52, 44, 36}, tileW-44, t.value)
		cv.text(x+22, y+98, true, valueSize, col, -1, value)
		if t.note != "" {
			// Under the figure, in the strip the tile leaves below it.
			noteSize, note := cv.fit(false, []float64{17, 15, 13}, tileW-44, t.note)
			cv.text(x+22, y+120, false, noteSize, colMuted, -1, note)
		}
	}

	footer := "EVERY KILL TELLS A STORY"
	cv.text(left, Height-34, true, 20, colMuted, -1, footer)
	if c.Rank != nil && c.RankedPlayers > 0 {
		cv.text(right, Height-34, false, 20, colMuted, 1, "of "+thousands(c.RankedPlayers)+" ranked players")
	} else if !c.GeneratedAt.IsZero() {
		cv.text(right, Height-34, false, 20, colMuted, 1, c.GeneratedAt.UTC().Format("2 Jan 2006"))
	}
	if cv.err != nil {
		return nil, cv.err
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, cv.img); err != nil {
		return nil, fmt.Errorf("encode card: %w", err)
	}
	return buf.Bytes(), nil
}
