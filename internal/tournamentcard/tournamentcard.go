// Package tournamentcard renders the tournament bracket as a PNG (docs/TOURNAMENTS.md): every
// match of a single-elimination bracket with names, seeds, scores and small tier emblems, the
// match in play highlighted, and the champion card posted when the final is decided. It draws
// with the Champion Card's kit (internal/playercard.Surface), so the images share one look.
// Rendering is pure: the same input always gives the same bytes.
package tournamentcard

import (
	"fmt"
	"image/color"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/playercard"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/tournament"
)

// Width is the image width; the height grows with the bracket (never below MinHeight).
const (
	Width     = 1200
	MinHeight = 630
)

// Player is one name on the bracket.
type Player struct {
	Name string
	Tier ranked.Tier
}

// Side is one side of a match: a player or a team, with its seed and score.
type Side struct {
	Players []Player
	Seed    int
	Score   int
	Winner  bool
	// DQ marks a disqualified side.
	DQ bool
}

// Label is the side's name line ("Ceiyxe", "Tusk & Lynx").
func (s *Side) Label() string {
	if s == nil {
		return ""
	}
	names := make([]string, 0, len(s.Players))
	for _, p := range s.Players {
		names = append(names, p.Name)
	}
	return strings.Join(names, " & ")
}

// Match is one node of the bracket.
type Match struct {
	Round, Position int
	RoundName       string
	Status          string // tournament.MatchPending ... MatchForfeit
	A, B            *Side  // nil: no entry (a bye, or not decided yet)
	Arena           string // the arena's name when assigned
}

// Bracket is everything the image shows.
type Bracket struct {
	Name        string
	ServerName  string
	Status      string // the tournament's status
	BracketSize int
	TeamSize    int
	BestOf      int
	Matches     []Match
	// SiteHost is the website's host for the footer ("" shows nothing there).
	SiteHost string
}

// Champion is the winner card.
type Champion struct {
	TournamentName string
	ServerName     string
	Players        []Player
	Title          string
	Points         int64
	Wins, Losses   int
	RoundsWon      int
	RoundsLost     int
	SiteHost       string
}

// --- from the engine ------------------------------------------------------------------------------

// FromTournament builds the bracket image's input from a tournament. tiers may be nil.
func FromTournament(t *tournament.Tournament, tiers map[int64]ranked.Tier, serverName, siteHost string) Bracket {
	b := Bracket{Name: t.Name, ServerName: serverName, Status: t.Status, BracketSize: t.BracketSize, TeamSize: t.TeamSize, BestOf: t.BestOf, SiteHost: siteHost}
	side := func(id *int64, score int, winner *int64) *Side {
		if id == nil {
			return nil
		}
		e := t.Entry(*id)
		if e == nil {
			return nil
		}
		s := &Side{Score: score, Winner: winner != nil && *winner == e.ID, DQ: e.Status == tournament.EntryDQ}
		if e.Seed != nil {
			s.Seed = *e.Seed
		}
		for _, p := range e.Players {
			s.Players = append(s.Players, Player{Name: p.Name, Tier: tiers[p.PlayerID]})
		}
		return s
	}
	for _, m := range t.Matches {
		mm := Match{Round: m.Round, Position: m.Position, RoundName: m.RoundName, Status: m.Status, A: side(m.EntryA, m.ScoreA, m.WinnerEntry), B: side(m.EntryB, m.ScoreB, m.WinnerEntry)}
		if a := t.Arena(m); a != nil {
			mm.Arena = a.Name
		}
		b.Matches = append(b.Matches, mm)
	}
	return b
}

// ChampionOf builds the winner card's input from a finished tournament (nil when it has no champion).
func ChampionOf(t *tournament.Tournament, tiers map[int64]ranked.Tier, serverName, siteHost string) *Champion {
	e := t.Champion()
	if e == nil {
		return nil
	}
	c := &Champion{TournamentName: t.Name, ServerName: serverName, SiteHost: siteHost}
	for _, p := range e.Players {
		c.Players = append(c.Players, Player{Name: p.Name, Tier: tiers[p.PlayerID]})
	}
	for _, p := range t.Prizes {
		if p.Place == 1 {
			c.Points = p.Points
			if p.Title != nil {
				c.Title = *p.Title
			}
		}
	}
	r := t.RecordOf(e.ID)
	c.Wins, c.Losses, c.RoundsWon, c.RoundsLost = r.Wins, r.Losses, r.RoundsWon, r.RoundsLost
	return c
}

// --- layout ---------------------------------------------------------------------------------------

const (
	margin     = 40
	headerH    = 150
	footerH    = 56
	boxH       = 76
	pitch      = boxH + 22
	captionH   = 26 // room under the last row of boxes for a caption
	rowH       = 34
	colGap     = 28
	emblemPx   = 22
	seedW      = 24
	cornerR    = 10.0
	captionGap = 14
)

var (
	colAmber  = playercard.RGB(0xE08A1E)
	colGreen  = playercard.RGB(0x38C987)
	colRed    = playercard.RGB(0xFF3344)
	colShadow = playercard.RGB(0x0B0D10)
)

var (
	stEyebrow   = playercard.Style{Face: playercard.FaceLabel, Size: 18, Track: 0.16}
	stTitle     = playercard.Style{Face: playercard.FaceDisplay, Size: 44}
	stSubtitle  = playercard.Style{Face: playercard.FaceBody, Size: 20}
	stRound     = playercard.Style{Face: playercard.FaceLabel, Size: 14, Track: 0.14}
	stName      = playercard.Style{Face: playercard.FaceDisplay, Size: 17}
	stScore     = playercard.Style{Face: playercard.FaceDisplay, Size: 20}
	stSeed      = playercard.Style{Face: playercard.FaceLabel, Size: 12}
	stCaption   = playercard.Style{Face: playercard.FaceLabel, Size: 12, Track: 0.1}
	stFooter    = playercard.Style{Face: playercard.FaceLabel, Size: 15, Track: 0.14}
	stFooterR   = playercard.Style{Face: playercard.FaceBody, Size: 15}
	stChampName = playercard.Style{Face: playercard.FaceDisplay, Size: 84}
	stChampSub  = playercard.Style{Face: playercard.FaceBody, Size: 26}
	stPill      = playercard.Style{Face: playercard.FaceLabel, Size: 18, Track: 0.12}
	stChampStat = playercard.Style{Face: playercard.FaceDisplay, Size: 40}
	stChampLbl  = playercard.Style{Face: playercard.FaceLabel, Size: 15, Track: 0.14}
)

// Height is the image height for a bracket of size n.
func Height(n int) int {
	h := headerH + 26 + (n/2)*pitch + captionH + footerH
	if h < MinHeight {
		return MinHeight
	}
	return h
}

// Render draws the bracket.
func Render(b Bracket) ([]byte, error) {
	if b.BracketSize < 2 {
		return nil, fmt.Errorf("tournamentcard: bracket size %d", b.BracketSize)
	}
	rounds := tournament.Rounds(b.BracketSize)
	h := Height(b.BracketSize)
	s, err := playercard.NewSurface(Width, h)
	if err != nil {
		return nil, err
	}
	s.FillRect(0, 0, 10, h, playercard.ColGold, 1) // the brand's accent bar, as on the card
	drawHeader(s, b)

	colW := (Width - 2*margin - 10) / rounds
	boxW := colW - colGap
	top := headerH + 26
	stack := h - footerH - captionH - top // the vertical room for the first column
	firstPitch := float64(stack) / float64(b.BracketSize/2)
	if firstPitch > pitch*1.4 {
		firstPitch = pitch * 1.4
	}
	// Centres: round 1 evenly, later rounds between their feeders.
	centre := map[[2]int]float64{}
	for p := 1; p <= b.BracketSize/2; p++ {
		centre[[2]int{1, p}] = float64(top) + (float64(p)-0.5)*firstPitch
	}
	for r := 2; r <= rounds; r++ {
		for p := 1; p <= b.BracketSize>>r; p++ {
			centre[[2]int{r, p}] = (centre[[2]int{r - 1, 2*p - 1}] + centre[[2]int{r - 1, 2 * p}]) / 2
		}
	}
	byPos := map[[2]int]*Match{}
	for i := range b.Matches {
		m := &b.Matches[i]
		byPos[[2]int{m.Round, m.Position}] = m
	}
	for r := 1; r <= rounds; r++ {
		x := margin + 10 + (r-1)*colW
		s.Text(x, top-12, stRound, playercard.ColMuted, -1, strings.ToUpper(tournament.RoundName(r, rounds)))
		for p := 1; p <= b.BracketSize>>r; p++ {
			cy := centre[[2]int{r, p}]
			y := int(cy) - boxH/2
			// Connector to the next match: out of the box, across the gap, into the next.
			if r < rounds {
				ny := int(centre[[2]int{r + 1, (p + 1) / 2}])
				midX := x + boxW + colGap/2
				s.FillRect(x+boxW, int(cy), colGap/2, 2, playercard.ColHairline, 1)
				y0, y1 := int(cy), ny
				if y0 > y1 {
					y0, y1 = y1, y0
				}
				s.FillRect(midX, y0, 2, y1-y0+2, playercard.ColHairline, 1)
				if p%2 == 1 {
					s.FillRect(midX, ny, colGap/2, 2, playercard.ColHairline, 1)
				}
			}
			m := byPos[[2]int{r, p}]
			if m == nil {
				m = &Match{Round: r, Position: p, Status: tournament.MatchPending}
			}
			drawMatch(s, m, b, x, y, boxW)
		}
	}
	drawFooter(s, b.SiteHost, statusLine(b), h)
	return s.PNG()
}

func drawHeader(s *playercard.Surface, b Bracket) {
	x := margin + 10
	s.Text(x, 48, stEyebrow, playercard.ColGold, -1, "CHAMPIONS® TOURNAMENT")
	st, name := s.Fit(stTitle, []float64{44, 38, 32, 26}, Width-2*margin-10, b.Name)
	s.Text(x, 96, st, playercard.ColWhite, -1, name)
	s.Text(x, 124, stSubtitle, playercard.ColMuted, -1, subtitle(b))
}

func subtitle(b Bracket) string {
	parts := []string{}
	if b.ServerName != "" {
		parts = append(parts, b.ServerName)
	}
	format := "1v1"
	if b.TeamSize == 2 {
		format = "2v2"
	}
	parts = append(parts, fmt.Sprintf("%s · %d slots · best of %d", format, b.BracketSize, b.BestOf))
	return strings.Join(parts, " · ")
}

func statusLine(b Bracket) string {
	switch b.Status {
	case tournament.StatusLive:
		return "Live now"
	case tournament.StatusPaused:
		return "Paused"
	case tournament.StatusFinished:
		return "Finished"
	case tournament.StatusCancelled:
		return "Cancelled"
	}
	return "Bracket"
}

// drawMatch paints one match box with its two rows and a caption for its state.
func drawMatch(s *playercard.Surface, m *Match, b Bracket, x, y, w int) {
	fx, fy, fw, fh := float64(x), float64(y), float64(w), float64(boxH)
	live := m.Status == tournament.MatchLive
	called := m.Status == tournament.MatchCalled
	if live {
		s.RoundRect(fx-6, fy-6, fw+12, fh+12, cornerR+6, playercard.Alpha(playercard.ColGold, 0.16))
	}
	s.RoundRect(fx+2, fy+3, fw, fh, cornerR, playercard.Alpha(colShadow, 0.6))
	s.RoundRect(fx, fy, fw, fh, cornerR, playercard.Alpha(playercard.ColTile, 1))
	border, bw := playercard.ColTileBorder, 1.5
	switch {
	case live:
		border, bw = playercard.ColGold, 2.5
	case called:
		border, bw = colAmber, 2
	}
	s.RoundRectBorder(fx, fy, fw, fh, cornerR, bw, playercard.Alpha(border, 1))
	s.FillRect(x+10, y+boxH/2, w-20, 1, playercard.ColHairline, 1)
	decided := m.Status == tournament.MatchDone || m.Status == tournament.MatchForfeit
	drawSide(s, m.A, m, b, x, y+4, w, decided, m.B == nil)
	drawSide(s, m.B, m, b, x, y+4+rowH+4, w, decided, m.A == nil)

	var caption string
	var capCol color.RGBA
	switch m.Status {
	case tournament.MatchLive:
		caption, capCol = "LIVE", playercard.ColGold
		if m.Arena != "" {
			caption += " · " + strings.ToUpper(m.Arena)
		}
	case tournament.MatchCalled:
		caption, capCol = "CALLED", colAmber
		if m.Arena != "" {
			caption += " · " + strings.ToUpper(m.Arena)
		}
	case tournament.MatchForfeit:
		caption, capCol = "FORFEIT", playercard.ColMuted
	}
	if caption != "" {
		s.Text(x+w, y+boxH+captionGap, stCaption, capCol, 1, caption)
	}
}

// drawSide paints one row: seed pill, emblem, name and score.
func drawSide(s *playercard.Surface, side *Side, m *Match, b Bracket, x, y, w int, decided, otherEmpty bool) {
	base := y + rowH/2 + s.CapHeight(stName)/2
	if side == nil {
		text := "TBD"
		if m.Round == 1 && otherEmpty {
			text = "—"
		} else if m.Round == 1 || (decided && otherEmpty) {
			text = "Bye"
		}
		s.Text(x+14, base, stName, playercard.Alpha(playercard.ColMuted, 0.6), -1, text)
		return
	}
	col := playercard.ColWhite
	if decided && !side.Winner {
		col = playercard.ColMuted
	}
	if side.DQ {
		col = colRed
	}
	nx := x + 14
	if side.Seed > 0 {
		s.RoundRect(float64(nx), float64(y+rowH/2-9), seedW, 18, 5, playercard.Alpha(playercard.ColTileBorder, 1))
		s.Text(nx+seedW/2, y+rowH/2+s.CapHeight(stSeed)/2, stSeed, playercard.ColMuted, 0, fmt.Sprint(side.Seed))
		nx += seedW + 8
	}
	if len(side.Players) > 0 {
		if img, err := playercard.TierEmblem(side.Players[0].Tier, emblemPx); err == nil {
			s.DrawImage(img, nx, y+rowH/2-emblemPx/2, 1)
		}
		nx += emblemPx + 8
	}
	scoreW := 0
	if (m.Status != tournament.MatchPending && !otherEmpty) || side.Score > 0 {
		scoreW = s.Measure(stScore, fmt.Sprint(side.Score)) + 12
		scol := col
		if side.Winner {
			scol = playercard.ColGold
		}
		s.Text(x+w-12, y+rowH/2+s.CapHeight(stScore)/2, stScore, scol, 1, fmt.Sprint(side.Score))
	}
	label := side.Label()
	if side.DQ {
		label += " (DQ)"
	}
	st, text := s.Fit(stName, []float64{17, 15, 13}, x+w-12-scoreW-nx, label)
	s.Text(nx, base, st, col, -1, text)
	if side.Winner && decided {
		s.FillRect(x, y+6, 3, rowH-12, playercard.ColGold, 1)
	}
}

func drawFooter(s *playercard.Surface, host, right string, h int) {
	y := h - footerH + 32
	s.FillRect(margin+10, h-footerH+6, Width-2*margin-10, 1, playercard.ColHairline, 1)
	left := "CHAMPIONS® KILLFEED"
	if host != "" {
		left = strings.ToUpper(host)
	}
	s.Text(margin+10, y, stFooter, playercard.ColMuted, -1, left)
	s.Text(Width-margin, y, stFooterR, playercard.ColMuted, 1, right)
}

// RenderChampion draws the winner card (1200 x 630).
func RenderChampion(c Champion) ([]byte, error) {
	s, err := playercard.NewSurface(Width, MinHeight)
	if err != nil {
		return nil, err
	}
	s.FillRect(0, 0, 10, MinHeight, playercard.ColGold, 1)
	x := margin + 10
	s.Text(x, 64, stEyebrow, playercard.ColGold, -1, "CHAMPIONS® TOURNAMENT · CHAMPION")
	names := make([]string, 0, len(c.Players))
	for _, p := range c.Players {
		names = append(names, p.Name)
	}
	tier := ranked.Unranked
	if len(c.Players) > 0 && c.Players[0].Tier != "" {
		tier = c.Players[0].Tier
	}
	const emblem = 200
	ex, ey := Width-margin-emblem, 110
	if img, err := playercard.TierEmblem(tier, emblem); err == nil {
		mid, _ := playercard.TierColor(tier)
		s.RoundRect(float64(ex-24), float64(ey-24), emblem+48, emblem+48, 40, playercard.Alpha(mid, 0.12))
		s.DrawImage(img, ex, ey, 1)
	}
	st, name := s.Fit(stChampName, []float64{84, 72, 60, 48, 40}, ex-48-x, strings.Join(names, " & "))
	s.Text(x, 190, st, playercard.ColWhite, -1, name)
	sub := "Won " + c.TournamentName
	if c.ServerName != "" {
		sub += " on " + c.ServerName
	}
	st2, sub := s.Fit(stChampSub, []float64{26, 22, 18}, ex-48-x, sub)
	s.Text(x, 236, st2, playercard.ColMuted, -1, sub)
	y := 296
	if c.Title != "" {
		pw := s.Measure(stPill, strings.ToUpper(c.Title)) + 36
		s.RoundRect(float64(x), float64(y-28), float64(pw), 40, 20, playercard.Alpha(playercard.ColGold, 0.18))
		s.RoundRectBorder(float64(x), float64(y-28), float64(pw), 40, 20, 1.5, playercard.Alpha(playercard.ColGold, 0.9))
		s.Text(x+pw/2, y+s.CapHeight(stPill)/2-8, stPill, playercard.ColGold, 0, strings.ToUpper(c.Title))
		y += 40
	}
	tiles := []struct{ label, value string }{
		{"MATCHES", fmt.Sprintf("%d–%d", c.Wins, c.Losses)},
		{"ROUNDS", fmt.Sprintf("%d–%d", c.RoundsWon, c.RoundsLost)},
	}
	if c.Points > 0 {
		tiles = append(tiles, struct{ label, value string }{"PRIZE", fmt.Sprintf("%s pts", thousands(c.Points))})
	}
	ty := y + 40
	tw := 230
	for i, t := range tiles {
		tx := x + i*(tw+16)
		s.RoundRect(float64(tx), float64(ty), float64(tw), 110, 14, playercard.Alpha(playercard.ColTile, 1))
		s.RoundRectBorder(float64(tx), float64(ty), float64(tw), 110, 14, 1.5, playercard.Alpha(playercard.ColTileBorder, 1))
		s.Text(tx+20, ty+34, stChampLbl, playercard.ColMuted, -1, t.label)
		s.Text(tx+20, ty+86, stChampStat, playercard.ColWhite, -1, t.value)
	}
	drawFooter(s, c.SiteHost, "Tournament champion", MinHeight)
	return s.PNG()
}

func thousands(n int64) string {
	str := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range str {
		if i > 0 && (len(str)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
