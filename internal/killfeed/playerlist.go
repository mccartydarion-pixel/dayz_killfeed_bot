package killfeed

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ADM player-list snapshots (docs/CHAMPION_LIVE_SYNC.md "ADM player lists"). With adminLogPlayerList
// enabled, DayZ writes a block every five minutes:
//
//	04:35:42 | ##### PlayerList log: 1 players
//	04:35:42 | Player "Ceiyxe" (id=... pos=<4621.1, 8397.2, 319.6>)
//	04:35:42 | #####
//
// Each entry is an authoritative position observation (ADM order <x, z, altitude>). These lines are
// observations only: they never produce a killfeed or connection message.
const (
	EventPlayerListHeader EventType = "PLAYER_LIST_HEADER"
	EventPlayerListEntry  EventType = "PLAYER_LIST_ENTRY"
	EventPlayerListFooter EventType = "PLAYER_LIST_FOOTER"
	// EventAdminLogStarted is the ADM file header "AdminLog started on 2026-09-24 at 05:23:05" -
	// server-boot evidence carrying the session's server-local start time.
	EventAdminLogStarted EventType = "ADMIN_LOG_STARTED"
)

var (
	playerListHeaderRe = regexp.MustCompile(`(?i)^\s*(\d{1,2}:\d{2}:\d{2})\s*\|\s*#{5}\s*PlayerList log:\s*(\d+)\s+players?\s*$`)
	playerListFooterRe = regexp.MustCompile(`^\s*(\d{1,2}:\d{2}:\d{2})\s*\|\s*#{5}\s*$`)
	adminLogStartedRe  = regexp.MustCompile(`^\s*AdminLog started on (\d{4}-\d{2}-\d{2}) at (\d{2}:\d{2}:\d{2})\s*$`)
)

func parsePlayerListHeader(line string) (*Event, bool) {
	m := playerListHeaderRe.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n < 0 {
		return nil, false
	}
	return &Event{Type: EventPlayerListHeader, TimeOfDay: m[1], PlayerListCount: &n, Raw: line}, true
}

func parsePlayerListFooter(line string) (*Event, bool) {
	m := playerListFooterRe.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	return &Event{Type: EventPlayerListFooter, TimeOfDay: m[1], Raw: line}, true
}

// parsePlayerListEntry matches a bare player line: `HH:MM:SS | Player "<name>" (id=... pos=<...>)`
// with NOTHING after the metadata block. Any trailing verb (is connected, placed ..., hit by ...)
// makes it a different event, handled by its own sub-parser.
func parsePlayerListEntry(line string) (*Event, bool) {
	bar := strings.Index(line, "|")
	if bar < 0 || timeOfDayRe.FindStringSubmatch(line) == nil {
		return nil, false
	}
	body := strings.TrimSpace(line[bar+1:])
	if !strings.HasPrefix(body, "Player ") || !strings.HasSuffix(body, ")") {
		return nil, false
	}
	head := playerHeadRe.FindStringIndex(body)
	if head == nil || head[0] != 0 {
		return nil, false
	}
	rest := body[head[1]:]
	open := strings.Index(rest, "(")
	if open < 0 || strings.TrimSpace(rest[:open]) != "" {
		return nil, false
	}
	closeIdx := strings.Index(rest[open:], ")")
	if closeIdx < 0 || strings.TrimSpace(rest[open+closeIdx+1:]) != "" {
		return nil, false
	}
	if hasDeadMarker(body) {
		return nil, false
	}
	player, ok := parsePlayer(body)
	if !ok || player.ID == "" {
		return nil, false
	}
	return &Event{Type: EventPlayerListEntry, TimeOfDay: parseTimeOfDay(line), Player: player, Raw: line}, true
}

func parseAdminLogStarted(line string) (*Event, bool) {
	m := adminLogStartedRe.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	return &Event{Type: EventAdminLogStarted, AdminLogStart: m[1] + " " + m[2], Raw: line}, true
}

// PlayerListSnapshot is one assembled player-list block. Complete means the header, every declared
// entry and the footer were all seen - only a complete snapshot may be used to decide who is NOT
// online. A missing, malformed or truncated block is never evidence that anybody left.
type PlayerListSnapshot struct {
	SourceFile   string
	HeaderOffset int64 // stable snapshot identity within the source file
	TimeOfDay    string
	Declared     int
	Entries      []*PlayerRef
	Complete     bool
}

// ID is the snapshot's stable identity: canonical source file + header byte offset.
func (s PlayerListSnapshot) ID() string {
	return s.SourceFile + "@" + strconv.FormatInt(s.HeaderOffset, 10)
}

// playerListAssembler groups header/entry/footer events into snapshots.
type playerListAssembler struct {
	open *PlayerListSnapshot
}

// header starts a new snapshot; a snapshot left open by a missing footer is returned incomplete.
func (a *playerListAssembler) header(file string, offset int64, ev *Event) (abandoned *PlayerListSnapshot) {
	abandoned = a.abandon()
	a.open = &PlayerListSnapshot{SourceFile: file, HeaderOffset: offset, TimeOfDay: ev.TimeOfDay, Declared: *ev.PlayerListCount}
	return abandoned
}

// entry adds a player to the open snapshot. An entry with no open snapshot (or from another
// timestamp) is still a valid position observation but belongs to no complete snapshot.
func (a *playerListAssembler) entry(file string, ev *Event) (snapshotID string) {
	if a.open == nil || a.open.SourceFile != file || a.open.TimeOfDay != ev.TimeOfDay {
		return ""
	}
	a.open.Entries = append(a.open.Entries, ev.Player)
	return a.open.ID()
}

// footer closes the open snapshot. It is complete only if the entry count matches the header.
func (a *playerListAssembler) footer(file string, ev *Event) *PlayerListSnapshot {
	if a.open == nil || a.open.SourceFile != file || a.open.TimeOfDay != ev.TimeOfDay {
		return nil // a bare "#####" with no open snapshot is not a snapshot
	}
	s := a.open
	a.open = nil
	s.Complete = len(s.Entries) == s.Declared
	return s
}

func (a *playerListAssembler) abandon() *PlayerListSnapshot {
	if a.open == nil {
		return nil
	}
	s := a.open
	a.open = nil
	s.Complete = false
	return s
}

// admClock converts an ADM line's server-local HH:MM:SS into a full server-local timestamp for one
// ADM file, starting from the file's session start (filename or "AdminLog started" header) and
// counting midnight rollovers. The result has no timezone: DayZ writes server-local wall time and
// the ADM file never states the zone, so Champion stores it as a server-local source time and never
// invents a UTC conversion.
type admClock struct {
	file    string
	base    time.Time // session start, server-local, zone-less (stored as UTC-typed)
	lastSec int
	days    int
}

var admFileStartRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})_(\d{2})-(\d{2})-(\d{2})\.ADM$`)

// newADMClock derives the session start from an ADM filename such as
// DayZServer_PS4_x64_2026-09-24_05-23-05.ADM. ok=false when the name carries no timestamp.
func newADMClock(file string) (*admClock, bool) {
	m := admFileStartRe.FindStringSubmatch(file)
	if m == nil {
		return nil, false
	}
	t, err := time.Parse("2006-01-02 15:04:05", m[1]+" "+m[2]+":"+m[3]+":"+m[4])
	if err != nil {
		return nil, false
	}
	return &admClock{file: file, base: t, lastSec: t.Hour()*3600 + t.Minute()*60 + t.Second()}, true
}

// at returns the server-local time of a line's HH:MM:SS within this file. A clock that goes back by
// more than an hour is a midnight rollover; smaller backwards steps (reordered lines) are not.
func (c *admClock) at(tod string) *time.Time {
	if c == nil || tod == "" {
		return nil
	}
	parts := strings.Split(tod, ":")
	if len(parts) != 3 {
		return nil
	}
	h, e1 := strconv.Atoi(parts[0])
	mi, e2 := strconv.Atoi(parts[1])
	s, e3 := strconv.Atoi(parts[2])
	if e1 != nil || e2 != nil || e3 != nil || h > 23 || mi > 59 || s > 59 {
		return nil
	}
	sec := h*3600 + mi*60 + s
	if sec < c.lastSec-3600 {
		c.days++
	}
	if sec > c.lastSec || sec < c.lastSec-3600 {
		c.lastSec = sec
	}
	day := time.Date(c.base.Year(), c.base.Month(), c.base.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, c.days)
	t := day.Add(time.Duration(sec) * time.Second)
	return &t
}
