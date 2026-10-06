// Package playercard renders the Champion Card (docs/CHAMPION_CARD.md): a 1200x630 PNG of one
// player's stats on one server, sized for link previews and Discord attachments, and the animated
// GIF of the same card filling in. Rendering is pure (no I/O, no clock): the same Card always
// produces the same image.
package playercard

import (
	"bytes"
	"fmt"
	"image/color"
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
	return r.progressAt(r.RP)
}

// progressAt is how far rp is through the tier, 0 to 1.
func (r Ranked) progressAt(rp int64) float64 {
	span := *r.NextTierRP - r.TierStartRP
	if span <= 0 {
		return 0
	}
	return clamp01(float64(rp-r.TierStartRP) / float64(span))
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

// --- figures and tiles -----------------------------------------------------------------------------

// figures are the numbers the stat grid shows. The still shows the card's own; a frame of the
// animation shows each part way through its count (Card.figuresAt), formatted by the same
// functions, so a frame mid-count is laid out exactly like the final one. A value the card never
// measured stays a dash throughout.
type figures struct {
	kills, deaths, headshots int
	kd                       float64
	// The PvP/PvE split under the deaths tile and the PvP K/D under the K/D tile, when known.
	split                bool
	pvpDeaths, pveDeaths int
	pvpKD                float64
	hasLongestKill       bool
	longestKill          float64 // metres
	playtime             int64   // seconds
	hasLongestLife       bool
	longestLife          int64 // seconds
	hasRank              bool
	rank, rankedPlayers  int
}

// figures are the card's final numbers.
func (c Card) figures() figures {
	f := figures{kills: c.Kills, deaths: c.Deaths, headshots: c.Headshots, kd: c.KD(), playtime: c.PlaytimeSeconds, rankedPlayers: c.RankedPlayers}
	if pve, ok := c.PvEDeaths(); ok {
		f.split, f.pvpDeaths, f.pveDeaths = true, *c.PvPDeaths, pve
		f.pvpKD, _ = c.PvPKD()
	}
	if c.LongestKillMeters > 0 {
		f.hasLongestKill, f.longestKill = true, c.LongestKillMeters
	}
	if c.LongestLifeSeconds != nil && *c.LongestLifeSeconds > 0 {
		f.hasLongestLife, f.longestLife = true, *c.LongestLifeSeconds
	}
	if c.Rank != nil && *c.Rank > 0 {
		f.hasRank, f.rank = true, *c.Rank
	}
	return f
}

// figuresAt is the card's numbers e of the way (0 to 1) through their count: every figure rises
// from zero to its value, the ratios included; the server rank counts down from the field to the
// player's place.
func (c Card) figuresAt(e float64) figures {
	final := c.figures()
	if e >= 1 {
		return final
	}
	e = clamp01(e)
	count := func(n int) int { return int(math.Round(float64(n) * e)) }
	f := final
	f.kills, f.deaths, f.headshots = count(final.kills), count(final.deaths), count(final.headshots)
	f.kd, f.pvpKD = final.kd*e, final.pvpKD*e
	f.pvpDeaths, f.pveDeaths = count(final.pvpDeaths), count(final.pveDeaths)
	f.longestKill = final.longestKill * e
	f.playtime = int64(math.Round(float64(final.playtime) * e))
	f.longestLife = int64(math.Round(float64(final.longestLife) * e))
	if f.hasRank {
		from := max(final.rankedPlayers, final.rank)
		f.rank = from - count(from-final.rank)
	}
	return f
}

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

func (c Card) tiles() []tile { return c.figures().tiles() }

func (f figures) tiles() []tile {
	const none = "-"
	longestKill, longestLife, rank, rankNote := none, none, none, ""
	if f.hasLongestKill {
		longestKill = thousands(int64(math.Round(f.longestKill))) + " m"
	}
	if f.hasLongestLife {
		longestLife = FormatDuration(f.longestLife)
	}
	if f.hasRank {
		rank = "#" + thousands(int64(f.rank))
		if f.rankedPlayers > 0 {
			rankNote = "of " + thousands(int64(f.rankedPlayers)) + " · by kills"
		}
	}
	var deathsNote, kdNote string
	if f.split {
		deathsNote = "PVP " + thousands(int64(f.pvpDeaths)) + " / PVE " + thousands(int64(f.pveDeaths))
		kdNote = "PVP " + fmt.Sprintf("%.2f", f.pvpKD)
	}
	return []tile{
		{label: "KILLS", value: thousands(int64(f.kills)), gold: true},
		{label: "DEATHS", value: thousands(int64(f.deaths)), note: deathsNote},
		{label: "K/D", value: fmt.Sprintf("%.2f", f.kd), note: kdNote},
		{label: "HEADSHOTS", value: thousands(int64(f.headshots))},
		{label: "LONGEST KILL", value: longestKill},
		{label: "PLAYTIME", value: FormatDuration(f.playtime)},
		{label: "LONGEST LIFE", value: longestLife},
		{label: "SERVER RANK", value: rank, gold: true, note: rankNote},
	}
}

// rankFigures are the numbers of the rank row: the RP, the RP still to climb, and the progress bar's
// fill. The still shows the standing itself; a frame of the animation shows the RP part way up
// from the tier's start (Ranked.figuresAt), the remainder counting down and the bar filling with it.
type rankFigures struct {
	rp, remaining int64
	progress      float64
}

func (r Ranked) figuresAt(e float64) rankFigures {
	if e >= 1 {
		return rankFigures{rp: r.RP, remaining: r.Remaining, progress: r.Progress()}
	}
	e = clamp01(e)
	rp := r.TierStartRP + int64(math.Round(float64(r.RP-r.TierStartRP)*e))
	if r.AtTop() {
		// Nowhere to climb: the bar simply fills.
		return rankFigures{rp: rp, remaining: r.Remaining, progress: e}
	}
	return rankFigures{rp: rp, remaining: r.Remaining + (r.RP - rp), progress: r.progressAt(rp)}
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

	// The stat grid: four by two.
	gridTop = 292
	tileW   = 257
	tileH   = 124
	colGap  = 14
	rowGap  = 12
)

// Render draws the card and returns it PNG-encoded.
func Render(c Card) ([]byte, error) {
	fonts, err := loadFonts()
	if err != nil {
		return nil, fmt.Errorf("load card fonts: %w", err)
	}
	cv := newCanvas(fonts)
	if err := cv.draw(c, stillFrame(c)); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, cv.img); err != nil {
		return nil, fmt.Errorf("encode card: %w", err)
	}
	return buf.Bytes(), nil
}

// draw lays out the card in the state fr describes: every element at its opacity, rise and scale,
// every figure at its count. The still is the frame where everything has arrived (stillFrame).
func (cv *canvas) draw(c Card, fr frame) error {
	cv.clear()

	tier := ranked.Unranked
	if c.Ranked != nil {
		tier = c.Ranked.Tier
	}
	pal := paletteFor(tier)
	size := int(math.Round(emblemSize * fr.emblemScale))
	emblem, err := tierEmblemAt(tier, size)
	if err != nil {
		return fmt.Errorf("load card emblem: %w", err)
	}
	if c.Ranked != nil && tier != ranked.Unranked && fr.glow > 0 {
		cv.glow(pal.glow, fr.glow)
	}
	cv.rect(0, 0, 10, Height, colGold)

	// Header.
	if fr.header > 0 {
		cv.text(left, 66, stEyebrow, fade(colGold, fr.header), -1, "CHAMPIONS KILLFEED")
		if c.ServerName != "" {
			st, server := cv.fit(stServer, []float64{stServer.size}, 560, c.ServerName)
			cv.text(right, 66, st, fade(colMuted, fr.header), 1, server)
		}
		cv.fillRect(left, 92, right-left, 1, colHairline, fr.header)
	}

	// Identity band: the emblem, the name, the sub line and the Ranked row.
	if fr.emblem > 0 {
		cx, cy := float64(emblemLeft+emblemSize/2), float64(emblemTop+emblemSize/2)
		cv.drawImage(emblem, int(math.Round(cx-float64(size)/2)), int(math.Round(cy-float64(size)/2)), fr.emblem)
	}
	if fr.name > 0 {
		name := c.PlayerName
		if strings.TrimSpace(name) == "" {
			name = "Unknown"
		}
		st, name := cv.fit(stName, []float64{80, 68, 58, 48}, right-textLeft, name)
		cv.text(textLeft, 172+fr.nameRise, st, fade(colWhite, fr.name), -1, name)
		if sub := c.subline(); sub != "" {
			st, line := cv.fit(stSubline, []float64{stSubline.size}, right-textLeft, sub)
			cv.text(textLeft, 214+fr.nameRise, st, fade(colMuted, fr.name), -1, line)
		}
	}
	if fr.row > 0 {
		if c.Ranked == nil {
			cv.text(textLeft, rowBaseline+fr.rowRise, stNoSeason, fade(colMuted, fr.row), -1, "Ranked season not started")
		} else {
			cv.rankRow(*c.Ranked, pal, fr)
		}
	}

	// Stat grid. Each tile's figure is set at the size its final value fits, so a count-up never
	// changes size part way.
	final, current := c.figures().tiles(), fr.figures.tiles()
	for i, t := range current {
		ts := fr.tiles[i]
		if ts.alpha <= 0 {
			continue
		}
		x := left + (i%4)*(tileW+colGap)
		y := gridTop + (i/4)*(tileH+rowGap) + ts.rise
		cv.roundRect(float64(x), float64(y), tileW, tileH, 12, alpha(colTile, ts.alpha))
		cv.roundRectBorder(float64(x), float64(y), tileW, tileH, 12, 1, alpha(colTileBorder, ts.alpha))
		cv.text(x+22, y+34, stLabel, fade(colMuted, ts.alpha), -1, t.label)
		col := colWhite
		if t.gold {
			col = colGold
		}
		st, value := cv.fit(stValue, []float64{46, 40, 34}, tileW-44, final[i].value)
		if t.value != final[i].value {
			value = t.value
		}
		cv.text(x+22, y+86, st, fade(col, ts.alpha), -1, value)
		if t.note != "" {
			st, note := cv.fit(stNote, []float64{stNote.size}, tileW-44, t.note)
			cv.text(x+22, y+108, st, fade(colMuted, ts.alpha), -1, note)
		}
	}

	// Footer.
	if fr.footer > 0 {
		cv.text(left, 598, stFooter, fade(colMuted, fr.footer), -1, "EVERY KILL TELLS A STORY")
		switch {
		case c.SiteHost != "":
			cv.text(right, 598, stFooterRight, fade(colMuted, fr.footer), 1, c.SiteHost)
		case !c.GeneratedAt.IsZero():
			cv.text(right, 598, stFooterRight, fade(colMuted, fr.footer), 1, c.GeneratedAt.UTC().Format("2 Jan 2006"))
		}
	}

	if fr.sweepOn {
		cv.sweep(fr.sweep)
	}
	return cv.err
}

// rankRow draws the Ranked standing: the tier pill, the RP, the server position, and the progress
// bar to the next tier with its caption, at the frame's opacity and rise, with the frame's figures.
func (cv *canvas) rankRow(r Ranked, pal tierPalette, fr frame) {
	a, dy := fr.row, fr.rowRise
	// The pill, with the tier name centred on it.
	label := strings.ToUpper(strings.TrimSpace(string(r.Tier)))
	if label == "" {
		label = string(ranked.Unranked)
	}
	pillW := cv.measure(stPill, label) + 36
	cv.roundRect(textLeft, pillTop+float64(dy), float64(pillW), pillHeight, pillHeight/2, alpha(pal.mid, 0.18*a))
	cv.roundRectBorder(textLeft, pillTop+float64(dy), float64(pillW), pillHeight, pillHeight/2, 1.5, alpha(pal.mid, 0.70*a))
	cv.text(textLeft+18, pillTop+dy+pillHeight/2+cv.capHeight(stPill)/2, stPill, fade(pal.hi, a), -1, label)

	// The RP and the position. The row must end before the bar: a long one loses the position
	// first, then the RP shrinks. The row is laid out by the final RP, so it does not shift while
	// the figure counts up.
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
	cv.text(x, rowBaseline+dy, rpStyle, fade(colWhite, a), -1, thousands(fr.rank.rp)+" RP")
	if position != "" {
		cv.text(x+cv.measure(rpStyle, rp)+sepGap, rowBaseline+dy, stPosition, fade(colMuted, a), -1, position)
	}

	// The progress bar and its caption.
	if r.AtTop() {
		cv.bar(barLeft, barTop+float64(dy), right-barLeft, barHeight, fr.rank.progress, colHairline, colGold, colGold, a)
		cv.text(right, 240+dy, stCaption, fade(colMuted, a), 1, "Top tier")
		return
	}
	cv.bar(barLeft, barTop+float64(dy), right-barLeft, barHeight, fr.rank.progress, colHairline, pal.lo, pal.hi, a)
	cv.text(right, 240+dy, stCaption, fade(colMuted, a), 1, thousands(fr.rank.remaining)+" RP to "+TierName(r.NextTier))
}
