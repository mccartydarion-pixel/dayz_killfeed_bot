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
	"math"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/deathstats"
	"github.com/yourname/dayz-killfeed/internal/ranked"
)

// Width and Height are the Open Graph image size.
const (
	Width  = 1200
	Height = 630
)

// Card is everything the image shows. Pointer fields are optional: a nil value's tile shows a dash
// rather than a made-up number.
type Card struct {
	PlayerName  string
	ServerName  string
	FactionName string
	FactionTag  string
	SeasonName  string
	// SiteHost is the website's host name ("championshp.vip") for the footer; empty shows the date
	// the card was generated instead.
	SiteHost string

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

	// Ranked is the player's standing in the server's Ranked Points season; nil when the server has
	// no active season.
	Ranked *Ranked

	GeneratedAt time.Time
}

// Ranked is one player's place in a server's active Ranked Points season, as
// repository.ServerRankedProgress reports it.
type Ranked struct {
	Tier ranked.Tier
	RP   int64
	// Position is the player's place on the server by RP; nil until they have any.
	Position *int64
	// NextTier, Remaining and NextTierRP describe the climb out of the current tier, which starts
	// at TierStartRP. At Master there is nowhere to climb: NextTier is "" and NextTierRP nil.
	NextTier    ranked.Tier
	Remaining   int64
	TierStartRP int64
	NextTierRP  *int64
}

// AtTop reports whether there is no tier above the player's.
func (r Ranked) AtTop() bool { return r.NextTier == "" || r.NextTierRP == nil }

// Progress is how far the player is through their tier, 0 to 1; 1 at the top tier.
func (r Ranked) Progress() float64 {
	if r.AtTop() {
		return 1
	}
	span := *r.NextTierRP - r.TierStartRP
	if span <= 0 {
		return 0
	}
	return clamp01(float64(r.RP-r.TierStartRP) / float64(span))
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

// --- palette ---------------------------------------------------------------------------------------

var (
	colBackgroundTop    = rgb(0x0F1114)
	colBackgroundBottom = rgb(0x171B20)
	colGold             = rgb(0xC9A227) // presentation.ChampionGold
	colWhite            = rgb(0xF2F2F0)
	colMuted            = rgb(0x8B939B)
	colTile             = rgb(0x1B1F24)
	colTileBorder       = rgb(0x2A3036)
	colHairline         = rgb(0x272D33)
)

// tierPalette is a tier's colours, taken from its emblem: a highlight, a mid tone, a shadow and the
// glow behind the emblem.
type tierPalette struct{ hi, mid, lo, glow color.RGBA }

var tierPalettes = map[ranked.Tier]tierPalette{
	ranked.Unranked: {rgb(0xa9aeba), rgb(0x6b707c), rgb(0x3a3d45), rgb(0x8a8f9b)},
	ranked.Rookie:   {rgb(0xb7dcc0), rgb(0x5f8f6f), rgb(0x2f4a3a), rgb(0x7fc797)},
	ranked.Bronze:   {rgb(0xf0b47a), rgb(0xb0692f), rgb(0x5a2f14), rgb(0xe08a3c)},
	ranked.Silver:   {rgb(0xf4f7fb), rgb(0xa7b1c0), rgb(0x4c5563), rgb(0xcfd8e6)},
	ranked.Gold:     {rgb(0xfff0a8), rgb(0xe0a81e), rgb(0x7a4d05), rgb(0xffcf3f)},
	ranked.Platinum: {rgb(0xe4fffb), rgb(0x57c7c9), rgb(0x1f5b63), rgb(0x6ff0e6)},
	ranked.Diamond:  {rgb(0xe3f0ff), rgb(0x4f8dff), rgb(0x1b3a8f), rgb(0x6fb0ff)},
	ranked.Master:   {rgb(0xffc2b8), rgb(0xe0162a), rgb(0x6d0710), rgb(0xff3b4a)},
}

// paletteFor is a tier's colours; a tier this build does not know is drawn as Unranked, like its emblem.
func paletteFor(t ranked.Tier) tierPalette {
	if p, ok := tierPalettes[t]; ok {
		return p
	}
	return tierPalettes[ranked.Unranked]
}

// The type styles of the card (docs/CHAMPION_CARD.md, "Layout").
var (
	stEyebrow     = style{manropeBold, 22, 0.16}
	stServer      = style{manropeMedium, 22, 0}
	stName        = style{soraExtraBold, 80, 0}
	stSubline     = style{manropeMedium, 26, 0}
	stPill        = style{manropeBold, 18, 0.12}
	stRP          = style{soraExtraBold, 26, 0}
	stPosition    = style{manropeMedium, 22, 0}
	stCaption     = style{manropeMedium, 18, 0}
	stNoSeason    = style{manropeMedium, 22, 0}
	stLabel       = style{manropeBold, 16, 0.14}
	stValue       = style{soraExtraBold, 46, 0}
	stNote        = style{manropeMedium, 15, 0}
	stFooter      = style{manropeBold, 16, 0.14}
	stFooterRight = style{manropeMedium, 16, 0}
)

// --- formatting ------------------------------------------------------------------------------------

func thousands(n int64) string {
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

// TierName is a tier in title case: "Platinum" for PLATINUM.
func TierName(t ranked.Tier) string {
	s := strings.ToLower(strings.TrimSpace(string(t)))
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// subline is the line under the name: the faction (with its tag) and the season, whichever exist.
func (c Card) subline() string {
	var parts []string
	switch {
	case c.FactionTag != "" && c.FactionName != "":
		parts = append(parts, "["+c.FactionTag+"] "+c.FactionName)
	case c.FactionName != "":
		parts = append(parts, c.FactionName)
	}
	if c.SeasonName != "" {
		parts = append(parts, c.SeasonName)
	}
	return strings.Join(parts, "  ·  ")
}

// --- tiles -----------------------------------------------------------------------------------------

type tile struct {
	label string
	value string
	gold  bool
	// note is a small caption under the figure: the PvP/PvE split, or what the server rank counts.
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
// The deaths and K/D tiles carry one when the PvP/PvE split is known; the server rank tile when the
// player is ranked ("of 123 · by kills": the Ranked Points position is on the rank row instead).
func (c Card) TileNotes() []string {
	out := make([]string, 0, 8)
	for _, t := range c.tiles() {
		out = append(out, t.note)
	}
	return out
}

func (c Card) tiles() []tile {
	const none = "-"
	longestKill, longestLife, rank, rankNote := none, none, none, ""
	if c.LongestKillMeters > 0 {
		longestKill = thousands(int64(math.Round(c.LongestKillMeters))) + " m"
	}
	if c.LongestLifeSeconds != nil && *c.LongestLifeSeconds > 0 {
		longestLife = FormatDuration(*c.LongestLifeSeconds)
	}
	if c.Rank != nil && *c.Rank > 0 {
		rank = "#" + thousands(int64(*c.Rank))
		if c.RankedPlayers > 0 {
			rankNote = "of " + thousands(int64(c.RankedPlayers)) + " · by kills"
		}
	}
	var deathsNote, kdNote string
	if pve, ok := c.PvEDeaths(); ok {
		pvpKD, _ := c.PvPKD()
		deathsNote = "PVP " + thousands(int64(*c.PvPDeaths)) + " / PVE " + thousands(int64(pve))
		kdNote = "PVP " + fmt.Sprintf("%.2f", pvpKD)
	}
	return []tile{
		{label: "KILLS", value: thousands(int64(c.Kills)), gold: true},
		{label: "DEATHS", value: thousands(int64(c.Deaths)), note: deathsNote},
		{label: "K/D", value: fmt.Sprintf("%.2f", c.KD()), note: kdNote},
		{label: "HEADSHOTS", value: thousands(int64(c.Headshots))},
		{label: "LONGEST KILL", value: longestKill},
		{label: "PLAYTIME", value: FormatDuration(c.PlaytimeSeconds)},
		{label: "LONGEST LIFE", value: longestLife},
		{label: "SERVER RANK", value: rank, gold: true, note: rankNote},
	}
}

// --- layout ----------------------------------------------------------------------------------------

// The card's columns and the identity band (docs/CHAMPION_CARD.md, "Layout").
const (
	left, right = 64, Width - 64
	textLeft    = 268 // the name, the sub line and the rank row start right of the emblem
	emblemLeft  = 64
	emblemTop   = 100
	rowBaseline = 264 // the rank row: pill, RP and position share this baseline
	pillTop     = 238
	pillHeight  = 36
	barLeft     = 820
	barTop      = 252
	barHeight   = 10
	rowLimit    = 800 // the rank row must end before the progress bar
)

// Render draws the card and returns it PNG-encoded.
func Render(c Card) ([]byte, error) {
	fonts, err := loadFonts()
	if err != nil {
		return nil, fmt.Errorf("load card fonts: %w", err)
	}
	cv := newCanvas(fonts)
	cv.background(colBackgroundTop, colBackgroundBottom)

	tier := ranked.Unranked
	if c.Ranked != nil {
		tier = c.Ranked.Tier
	}
	pal := paletteFor(tier)
	emblem, err := tierEmblem(tier)
	if err != nil {
		return nil, fmt.Errorf("load card emblem: %w", err)
	}
	cx, cy := float64(emblemLeft+emblemSize/2), float64(emblemTop+emblemSize/2)
	if c.Ranked != nil && tier != ranked.Unranked {
		cv.glow(cx, cy, 150, pal.glow, 0.30)
	}
	cv.rect(0, 0, 10, Height, colGold)

	// Header.
	cv.text(left, 66, stEyebrow, colGold, -1, "CHAMPIONS KILLFEED")
	if c.ServerName != "" {
		st, server := cv.fit(stServer, []float64{stServer.size}, 560, c.ServerName)
		cv.text(right, 66, st, colMuted, 1, server)
	}
	cv.rect(left, 92, right-left, 1, colHairline)

	// Identity band: the emblem, the name, the sub line and the Ranked row.
	draw.Draw(cv.img, image.Rect(emblemLeft, emblemTop, emblemLeft+emblemSize, emblemTop+emblemSize), emblem, image.Point{}, draw.Over)
	name := c.PlayerName
	if strings.TrimSpace(name) == "" {
		name = "Unknown"
	}
	st, name := cv.fit(stName, []float64{80, 68, 58, 48}, right-textLeft, name)
	cv.text(textLeft, 172, st, colWhite, -1, name)
	if sub := c.subline(); sub != "" {
		st, line := cv.fit(stSubline, []float64{stSubline.size}, right-textLeft, sub)
		cv.text(textLeft, 214, st, colMuted, -1, line)
	}
	if c.Ranked == nil {
		cv.text(textLeft, rowBaseline, stNoSeason, colMuted, -1, "Ranked season not started")
	} else {
		cv.rankRow(*c.Ranked, pal)
	}

	// Stat grid: four by two.
	const (
		gridTop = 292
		tileW   = 257
		tileH   = 124
		colGap  = 14
		rowGap  = 12
	)
	for i, t := range c.tiles() {
		x := left + (i%4)*(tileW+colGap)
		y := gridTop + (i/4)*(tileH+rowGap)
		cv.roundRect(float64(x), float64(y), tileW, tileH, 12, opaque(colTile))
		cv.roundRectBorder(float64(x), float64(y), tileW, tileH, 12, 1, opaque(colTileBorder))
		cv.text(x+22, y+34, stLabel, colMuted, -1, t.label)
		col := colWhite
		if t.gold {
			col = colGold
		}
		st, value := cv.fit(stValue, []float64{46, 40, 34}, tileW-44, t.value)
		cv.text(x+22, y+86, st, col, -1, value)
		if t.note != "" {
			st, note := cv.fit(stNote, []float64{stNote.size}, tileW-44, t.note)
			cv.text(x+22, y+108, st, colMuted, -1, note)
		}
	}

	// Footer.
	cv.text(left, 598, stFooter, colMuted, -1, "EVERY KILL TELLS A STORY")
	switch {
	case c.SiteHost != "":
		cv.text(right, 598, stFooterRight, colMuted, 1, c.SiteHost)
	case !c.GeneratedAt.IsZero():
		cv.text(right, 598, stFooterRight, colMuted, 1, c.GeneratedAt.UTC().Format("2 Jan 2006"))
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

// rankRow draws the Ranked standing: the tier pill, the RP, the server position, and the progress
// bar to the next tier with its caption.
func (cv *canvas) rankRow(r Ranked, pal tierPalette) {
	// The pill, with the tier name centred on it.
	label := strings.ToUpper(strings.TrimSpace(string(r.Tier)))
	if label == "" {
		label = string(ranked.Unranked)
	}
	pillW := cv.measure(stPill, label) + 36
	cv.roundRect(textLeft, pillTop, float64(pillW), pillHeight, pillHeight/2, alpha(pal.mid, 0.18))
	cv.roundRectBorder(textLeft, pillTop, float64(pillW), pillHeight, pillHeight/2, 1.5, alpha(pal.mid, 0.70))
	cv.text(textLeft+18, pillTop+pillHeight/2+cv.capHeight(stPill)/2, stPill, pal.hi, -1, label)

	// The RP and the position. The row must end before the bar: a long one loses the position
	// first, then the RP shrinks.
	const gap, sepGap = 18, 10
	rp := thousands(r.RP) + " RP"
	position := ""
	if r.Position != nil {
		position = "·  #" + thousands(*r.Position) + " in Ranked"
	}
	x := textLeft + pillW + gap
	rpStyle := stRP
	width := func() int {
		w := x + cv.measure(rpStyle, rp)
		if position != "" {
			w += sepGap + cv.measure(stPosition, position)
		}
		return w
	}
	if width() > rowLimit {
		position = ""
	}
	if width() > rowLimit {
		rpStyle = stRP.at(22)
	}
	x += cv.text(x, rowBaseline, rpStyle, colWhite, -1, rp)
	if position != "" {
		cv.text(x+sepGap, rowBaseline, stPosition, colMuted, -1, position)
	}

	// The progress bar and its caption.
	if r.AtTop() {
		cv.bar(barLeft, barTop, right-barLeft, barHeight, 1, colHairline, colGold, colGold)
		cv.text(right, 240, stCaption, colMuted, 1, "Top tier")
		return
	}
	cv.bar(barLeft, barTop, right-barLeft, barHeight, r.Progress(), colHairline, pal.lo, pal.hi)
	cv.text(right, 240, stCaption, colMuted, 1, thousands(r.Remaining)+" RP to "+TierName(r.NextTier))
}
