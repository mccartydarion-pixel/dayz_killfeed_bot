package killfeed

import (
	"regexp"
	"strconv"
	"strings"
)

// Parser defines the contract for incremental log parsing.
type Parser interface {
	ParseLine(line string) (*Event, error)
}

// ADMParser parses real PlayStation DayZ ADM log lines using ordered dedicated
// sub-parsers. It never fabricates fields and treats unknown lines as ignored.
type ADMParser struct{}

// NewADMParser constructs the real ADM parser.
func NewADMParser() *ADMParser { return &ADMParser{} }

var (
	// timeOfDayRe captures the leading "HH:MM:SS |" clock.
	timeOfDayRe = regexp.MustCompile(`^\s*(\d{1,2}:\d{2}:\d{2})\s*\|`)
	// playerHeadRe captures Player "<name>" with optional (DEAD) marker.
	playerHeadRe = regexp.MustCompile(`Player\s+"([^"]+)"\s*(\(DEAD\))?`)
	// idRe captures id=<value> from a parenthesized metadata block.
	idRe = regexp.MustCompile(`\bid=([^\s)]+)`)
	// posRe captures pos=<x, y, z> with variable precision and sign.
	posRe = regexp.MustCompile(`\bpos=<\s*(-?\d+(?:\.\d+)?)\s*,\s*(-?\d+(?:\.\d+)?)\s*,\s*(-?\d+(?:\.\d+)?)\s*>`)
	// hpRe captures [HP: <value>].
	hpRe = regexp.MustCompile(`\[HP:\s*(-?\d+(?:\.\d+)?)\]`)
	// zoneRe captures into <zone>(<id>).
	zoneRe = regexp.MustCompile(`\binto\s+([A-Za-z]+)\(([^)]*)\)`)
	// damageRe captures for <n> damage.
	damageRe = regexp.MustCompile(`\bfor\s+(-?\d+(?:\.\d+)?)\s+damage`)
	// ammoRe captures the parenthesized ammo after the damage clause.
	ammoRe = regexp.MustCompile(`\((Bullet_[^)]+)\)`)
	// distanceRe captures from <n> meters.
	distanceRe = regexp.MustCompile(`\bfrom\s+(-?\d+(?:\.\d+)?)\s+meters`)
)

// ParseLine classifies and parses one ADM line. Unknown or unsupported lines
// return (nil, nil); only genuinely malformed expected events return an error.
func (p *ADMParser) ParseLine(line string) (*Event, error) {
	if strings.TrimSpace(line) == "" {
		return nil, nil
	}

	// Order matters: explicit kill is authoritative and must be checked before
	// generic death and before hit (a hit line never becomes a kill here).
	if ev, ok := parseExplicitKill(line); ok {
		return ev, nil
	}
	if ev, ok := parseHit(line); ok {
		return ev, nil
	}
	if ev, ok := parseDeath(line); ok {
		return ev, nil
	}
	if ev, ok := parseSuicideAction(line); ok {
		return ev, nil
	}
	if ev, ok := parseConnecting(line); ok {
		return ev, nil
	}
	if ev, ok := parseConnected(line); ok {
		return ev, nil
	}
	if ev, ok := parseDisconnected(line); ok {
		return ev, nil
	}
	if ev, ok := parseUnconscious(line); ok {
		return ev, nil
	}
	if ev, ok := parseConscious(line); ok {
		return ev, nil
	}
	if ev, ok := parseRespawn(line); ok {
		return ev, nil
	}
	return nil, nil
}

// parseTimeOfDay extracts the HH:MM:SS clock token.
func parseTimeOfDay(line string) string {
	m := timeOfDayRe.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	return m[1]
}

// parsePlayer extracts a PlayerRef from the region of text starting at a
// "Player " occurrence. It reads the name, the (DEAD) flag, and the id/pos from
// the immediately following parenthesized metadata block.
func parsePlayer(segment string) (*PlayerRef, bool) {
	head := playerHeadRe.FindStringSubmatchIndex(segment)
	if head == nil {
		return nil, false
	}
	name := segment[head[2]:head[3]]
	dead := head[4] >= 0 // (DEAD) group present

	ref := &PlayerRef{Name: name}

	// Metadata block: the first (...) after the name.
	rest := segment[head[1]:]
	if open := strings.Index(rest, "("); open >= 0 {
		if close := strings.Index(rest[open:], ")"); close >= 0 {
			meta := rest[open : open+close+1]
			if m := idRe.FindStringSubmatch(meta); m != nil {
				ref.ID = m[1]
			}
			if pm := posRe.FindStringSubmatch(meta); pm != nil {
				ref.Position = &Position{
					X: mustFloat(pm[1]),
					Y: mustFloat(pm[2]),
					Z: mustFloat(pm[3]),
				}
			}
		}
	}
	_ = dead
	return ref, true
}

// isDead reports whether the (DEAD) marker appears right after a player name in segment.
func hasDeadMarker(segment string) bool {
	head := playerHeadRe.FindStringSubmatchIndex(segment)
	return head != nil && head[4] >= 0
}

// parseBetweenWeapon returns the weapon text between " with " and " from ... meters".
func parseWeapon(line string) string {
	wi := strings.LastIndex(line, " with ")
	if wi < 0 {
		return ""
	}
	rest := line[wi+len(" with "):]
	fi := strings.Index(rest, " from ")
	if fi < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:fi])
}

// parseExplicitKill matches: Player "<victim>" (DEAD) (...) killed by Player "<killer>" (...) with <weapon> from <d> meters
func parseExplicitKill(line string) (*Event, bool) {
	if !strings.Contains(line, "killed by Player") {
		return nil, false
	}
	ki := strings.Index(line, "killed by Player")
	victimSeg := line[:ki]
	killerSeg := line[ki+len("killed by "):]

	victim, ok := parsePlayer(victimSeg)
	if !ok {
		return nil, false
	}
	killer, ok := parsePlayer(killerSeg)
	if !ok {
		return nil, false
	}

	ev := &Event{
		Type:      EventPlayerKill,
		TimeOfDay: parseTimeOfDay(line),
		Victim:    victim,
		Killer:    killer,
		Weapon:    parseWeapon(line),
		Dead:      true,
		Raw:       line,
	}
	if m := distanceRe.FindStringSubmatch(line); m != nil {
		d := mustFloat(m[1])
		ev.Distance = &d
	}
	return ev, true
}

// parseHit matches a damage line: Player "<victim>" ... hit by Player "<attacker>" ... into <zone>(<id>) for <n> damage (<ammo>) with <weapon> from <d> meters
func parseHit(line string) (*Event, bool) {
	if !strings.Contains(line, "hit by Player") {
		return nil, false
	}
	hi := strings.Index(line, "hit by Player")
	victimSeg := line[:hi]
	attackerSeg := line[hi+len("hit by "):]

	victim, ok := parsePlayer(victimSeg)
	if !ok {
		return nil, false
	}
	attacker, ok := parsePlayer(attackerSeg)
	if !ok {
		return nil, false
	}

	ev := &Event{
		Type:      EventPlayerHit,
		TimeOfDay: parseTimeOfDay(line),
		Victim:    victim,
		Attacker:  attacker,
		Weapon:    parseWeapon(line),
		Dead:      hasDeadMarker(victimSeg),
		Raw:       line,
	}
	if m := hpRe.FindStringSubmatch(line); m != nil {
		hp := mustFloat(m[1])
		ev.HP = &hp
	}
	if m := zoneRe.FindStringSubmatch(line); m != nil {
		ev.HitZone = m[1]
		ev.HitZoneID = m[2]
	}
	if m := damageRe.FindStringSubmatch(line); m != nil {
		d := mustFloat(m[1])
		ev.Damage = &d
	}
	if m := ammoRe.FindStringSubmatch(line); m != nil {
		ev.Ammo = m[1]
	}
	if m := distanceRe.FindStringSubmatch(line); m != nil {
		d := mustFloat(m[1])
		ev.Distance = &d
	}
	return ev, true
}

// parseDeath matches a generic death line: Player "<player>" (DEAD) (...) died. ...
func parseDeath(line string) (*Event, bool) {
	if !strings.Contains(line, " died") && !strings.Contains(line, " died.") {
		return nil, false
	}
	if strings.Contains(line, "killed by") {
		return nil, false // explicit kill handled earlier
	}
	player, ok := parsePlayer(line)
	if !ok {
		return nil, false
	}
	return &Event{
		Type:      EventPlayerDeath,
		TimeOfDay: parseTimeOfDay(line),
		Player:    player,
		Dead:      hasDeadMarker(line),
		Raw:       line,
	}, true
}

// parseSuicideAction matches: Player "<player>" (...) performed EmoteSuicide with <weapon>
func parseSuicideAction(line string) (*Event, bool) {
	if !strings.Contains(line, "performed EmoteSuicide") {
		return nil, false
	}
	player, ok := parsePlayer(line)
	if !ok {
		return nil, false
	}
	ev := &Event{
		Type:      EventSuicideAction,
		TimeOfDay: parseTimeOfDay(line),
		Player:    player,
		Dead:      hasDeadMarker(line),
		Raw:       line,
	}
	// Weapon follows " with " to end of line (no "from ... meters" clause).
	if wi := strings.LastIndex(line, " with "); wi >= 0 {
		ev.Weapon = strings.TrimSpace(line[wi+len(" with "):])
	}
	return ev, true
}

// parseConnecting matches: Player "<name>" (id=...) is connecting
func parseConnecting(line string) (*Event, bool) {
	if !strings.Contains(line, "is connecting") {
		return nil, false
	}
	player, ok := parsePlayer(line)
	if !ok {
		return nil, false
	}
	return &Event{Type: EventPlayerConnecting, TimeOfDay: parseTimeOfDay(line), Player: player, Raw: line}, true
}

// parseConnected matches: Player "<name>" (id=... pos=...) is connected
func parseConnected(line string) (*Event, bool) {
	if !strings.Contains(line, "is connected") {
		return nil, false
	}
	player, ok := parsePlayer(line)
	if !ok {
		return nil, false
	}
	return &Event{Type: EventPlayerConnect, TimeOfDay: parseTimeOfDay(line), Player: player, Raw: line}, true
}

// parseDisconnected matches: Player "<name>" (...) has been disconnected
func parseDisconnected(line string) (*Event, bool) {
	if !strings.Contains(line, "has been disconnected") {
		return nil, false
	}
	player, ok := parsePlayer(line)
	if !ok {
		return nil, false
	}
	return &Event{Type: EventPlayerDisconnect, TimeOfDay: parseTimeOfDay(line), Player: player, Raw: line}, true
}

// parseUnconscious matches: Player "<name>" (...) is unconscious
func parseUnconscious(line string) (*Event, bool) {
	if !strings.Contains(line, "is unconscious") {
		return nil, false
	}
	player, ok := parsePlayer(line)
	if !ok {
		return nil, false
	}
	return &Event{Type: EventPlayerUnconscious, TimeOfDay: parseTimeOfDay(line), Player: player, Dead: hasDeadMarker(line), Raw: line}, true
}

// parseConscious matches: Player "<name>" (...) regained consciousness
func parseConscious(line string) (*Event, bool) {
	if !strings.Contains(line, "regained consciousness") {
		return nil, false
	}
	player, ok := parsePlayer(line)
	if !ok {
		return nil, false
	}
	return &Event{Type: EventPlayerConscious, TimeOfDay: parseTimeOfDay(line), Player: player, Raw: line}, true
}

// parseRespawn matches: Player "<name>" [(DEAD)] (...) is choosing to respawn
func parseRespawn(line string) (*Event, bool) {
	if !strings.Contains(line, "choosing to respawn") {
		return nil, false
	}
	player, ok := parsePlayer(line)
	if !ok {
		return nil, false
	}
	return &Event{Type: EventPlayerRespawn, TimeOfDay: parseTimeOfDay(line), Player: player, Dead: hasDeadMarker(line), Raw: line}, true
}

func mustFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}
