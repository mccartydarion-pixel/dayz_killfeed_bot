package livesync

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Parse results. Stats counts records per category so noisy categories (localization, missing
// models) can be monitored without shipping every line.
type ParseResult struct {
	Records []Record
	Stats   map[string]int
	// BootLocal is the in-file startup time when the file states one (RPT "Current time:", script /
	// crash log header).
	BootLocal *time.Time
	// Version is the server build stated in an RPT header.
	Version string
}

func (p *ParseResult) add(r Record) {
	if p.Stats == nil {
		p.Stats = map[string]int{}
	}
	if r.Status == "" {
		r.Status = StatusParsed
	}
	p.Stats[r.Category]++
	p.Records = append(p.Records, r)
}

// lines splits content into complete lines with their end offsets (base + bytes through the "\n").
// A trailing partial line (no "\n") is NOT returned: the caller keeps it for the next read.
func lines(content []byte, base int64) (out []struct {
	text string
	end  int64
}, consumed int64) {
	start := 0
	for i, b := range content {
		if b == '\n' {
			out = append(out, struct {
				text string
				end  int64
			}{strings.TrimRight(string(content[start:i]), "\r"), base + int64(i) + 1})
			start = i + 1
		}
	}
	return out, int64(start)
}

// localClock turns an in-file time of day into a full server-local time, counting midnight
// rollovers from a known start (no timezone is invented).
type localClock struct {
	base    time.Time
	lastSec int
	days    int
	ok      bool
}

func (c *localClock) set(t time.Time) {
	c.base, c.ok, c.days = t, true, 0
	c.lastSec = t.Hour()*3600 + t.Minute()*60 + t.Second()
}

func (c *localClock) at(h, m, s int, nanos int) *time.Time {
	if !c.ok {
		return nil
	}
	sec := h*3600 + m*60 + s
	if sec < c.lastSec-3600 {
		c.days++
	}
	if sec > c.lastSec || sec < c.lastSec-3600 {
		c.lastSec = sec
	}
	day := time.Date(c.base.Year(), c.base.Month(), c.base.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, c.days)
	t := day.Add(time.Duration(sec)*time.Second + time.Duration(nanos))
	return &t
}

// --- RPT ----------------------------------------------------------------------------------------

var (
	rptCurrentTimeRe = regexp.MustCompile(`^Current time:\s+(\d{4})/(\d{2})/(\d{2}) (\d{2}):(\d{2}):(\d{2})`)
	rptVersionRe     = regexp.MustCompile(`^Version (\d+\.\d+\.\d+)`)
	rptClockRe       = regexp.MustCompile(`^\s*(\d{1,2}):(\d{2}):(\d{2})\.(\d{3})(?:\s+(.*))?$`)
	terminationRe    = regexp.MustCompile(`\[Server\] :: termination in: (\d+)`)
	missingModelRe   = regexp.MustCompile(`^Warning Message: Cannot open object (.+)$`)
	playerRemovedRe  = regexp.MustCompile(`Server: Player info removed - name (.+?), id \S+`)
	ceComponentRe    = regexp.MustCompile(`^\[CE\]\[([^\]]+)\]`)
	reasonRe         = regexp.MustCompile(`^Reason: \[::([A-Za-z0-9_]+)\] :: \[([A-Z]+)\] :: (.+)$`)
	spawnerFileRe    = regexp.MustCompile(`File "\$mission:([^"]+)" does not exist`)
	stackFrameRe     = regexp.MustCompile(`^(scripts/[^\s:]+:\d+) Function (\S+)`)
	functionRe       = regexp.MustCompile(`^Function: '([^']+)'`)
	modelPathRe      = regexp.MustCompile(`(dz\\[^\s:()']+\.p3d)`)
	spawnerNameRe    = regexp.MustCompile(`^Primary Spawner: "([^"]+)"`)
)

// isModelWarning matches the engine's model/geometry load warnings (real Champions RPT lines).
func isModelWarning(msg string) bool {
	switch {
	case strings.HasPrefix(msg, "Warning: No components in "),
		strings.HasPrefix(msg, "PerfWarning: Way too much components"),
		strings.HasPrefix(msg, `Convex "`) && strings.Contains(msg, "selection faces are less then"),
		strings.HasPrefix(msg, "ENTITY") && strings.Contains(msg, "(W): Unknown object class"),
		strings.HasPrefix(msg, "ENTITY") && strings.Contains(msg, "is missing geometry components"):
		return true
	}
	return false
}

// isEngineStartup matches start-up chatter: package loading, config inheritance, stats manager.
func isEngineStartup(msg string) bool {
	return (strings.HasPrefix(msg, "ENGINE") && strings.Contains(msg, "FileSystem: Adding package")) ||
		strings.HasPrefix(msg, "Updating base class ") ||
		msg == "Initializing stats manager." || msg == "Stats config disabled."
}

// ParseRPT parses the complete lines of an RPT slice starting at byte base. clockStart seeds the
// time-of-day clock when the slice does not contain the header (a delta read); pass nil otherwise.
func ParseRPT(content []byte, base int64, clockStart *time.Time) (ParseResult, int64) {
	var res ParseResult
	var clock localClock
	if clockStart != nil {
		clock.set(*clockStart)
	}
	ls, consumed := lines(content, base)
	var exc *Record // an exception block being assembled from Reason + stack lines
	flush := func() {
		if exc != nil {
			res.add(*exc)
			exc = nil
		}
	}
	for _, l := range ls {
		text := strings.TrimSpace(l.text)
		if text == "" {
			continue
		}
		if m := rptCurrentTimeRe.FindStringSubmatch(text); m != nil {
			t, err := time.Parse("2006/01/02 15:04:05", m[1]+"/"+m[2]+"/"+m[3]+" "+m[4]+":"+m[5]+":"+m[6])
			if err == nil {
				clock.set(t)
				tt := t
				res.BootLocal = &tt
				res.add(Record{Offset: l.end, Category: CategoryBootStarted, SourceLocalTime: &tt, Payload: map[string]string{"evidence": "rpt_current_time"}, Evidence: Redact(text)})
			}
			continue
		}
		if m := rptVersionRe.FindStringSubmatch(text); m != nil {
			res.Version = m[1]
			res.add(Record{Offset: l.end, Category: CategoryLogHeader, Payload: map[string]string{"version": m[1]}, Evidence: Redact(text)})
			continue
		}
		if strings.HasPrefix(text, "==") || strings.HasPrefix(text, "Exe timestamp:") {
			res.add(Record{Offset: l.end, Category: CategoryLogHeader, Evidence: Redact(text)})
			continue
		}
		// Exception blocks: RPT prints Reason / stack lines without a clock prefix.
		if m := reasonRe.FindStringSubmatch(text); m != nil {
			flush()
			exc = exceptionRecord(l.end, m, text, nil)
			continue
		}
		if m := stackFrameRe.FindStringSubmatch(text); m != nil && exc != nil {
			addFrame(exc, m[1]+" "+m[2])
			exc.Offset = l.end
			continue
		}
		flush()
		m := rptClockRe.FindStringSubmatch(l.text)
		if m == nil {
			res.add(Record{Offset: l.end, Category: CategoryUnknown, Status: StatusUnknown, Evidence: Redact(text)})
			continue
		}
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		s, _ := strconv.Atoi(m[3])
		ms, _ := strconv.Atoi(m[4])
		at := clock.at(h, mi, s, ms*int(time.Millisecond))
		msg := strings.TrimSpace(m[5])
		if msg == "" {
			continue // a bare time-of-day line carries no observation
		}
		r := Record{Offset: l.end, SourceLocalTime: at, Evidence: Redact(text)}
		if at == nil {
			r.Status = StatusPartial // time of day without a known date
		}
		switch {
		case terminationRe.MatchString(msg):
			r.Category, r.Payload = CategoryShutdownCountdown, map[string]string{"secondsRemaining": terminationRe.FindStringSubmatch(msg)[1]}
		case strings.Contains(msg, "--- Termination successfully completed ---"):
			r.Category = CategoryShutdownComplete
		case strings.HasPrefix(msg, "ENGINE") && strings.Contains(msg, "Destroying game"):
			r.Category, r.Payload = CategoryEngineDestroy, map[string]string{"stage": "engine"}
		case strings.Contains(msg, "~DayZGame()"):
			r.Category, r.Payload = CategoryEngineDestroy, map[string]string{"stage": "script_teardown"}
		case missingModelRe.MatchString(msg):
			r.Category, r.Payload = CategoryMissingModel, map[string]string{"object": missingModelRe.FindStringSubmatch(msg)[1]}
		case strings.HasPrefix(msg, "Warning Message:"):
			r.Category, r.Payload = CategoryConfigWarning, map[string]string{"message": strings.TrimSpace(strings.TrimPrefix(msg, "Warning Message:"))}
		case strings.Contains(msg, "Localization not present") || strings.Contains(msg, "not in localization table") || strings.Contains(msg, "listed twice in"):
			r.Category = CategoryLocalization
		case ceComponentRe.MatchString(msg):
			r.Category, r.Payload = CategoryCentralEconomy, map[string]string{"component": ceComponentRe.FindStringSubmatch(msg)[1]}
		case playerRemovedRe.MatchString(msg):
			r.Category, r.Payload = CategoryNetworkPlayerLeft, map[string]string{"gamertag": playerRemovedRe.FindStringSubmatch(msg)[1]}
		case strings.HasPrefix(msg, "RESOURCES (E):"):
			r.Category = CategoryResourceLeak
		case strings.HasPrefix(msg, "[A2S]"):
			r.Category = CategoryQuery
		case isModelWarning(msg):
			r.Category = CategoryModelWarning
			if mm := modelPathRe.FindStringSubmatch(msg); mm != nil {
				r.Payload = map[string]string{"model": mm[1]}
			}
		case isEngineStartup(msg):
			r.Category = CategoryEngineStartup
		case spawnerNameRe.MatchString(msg):
			r.Category, r.Payload = CategorySpawnerConfig, map[string]string{"spawner": spawnerNameRe.FindStringSubmatch(msg)[1]}
		default:
			r.Category, r.Status = CategoryUnknown, StatusUnknown
		}
		res.add(r)
	}
	flush()
	return res, consumed
}

func exceptionRecord(offset int64, m []string, text string, at *time.Time) *Record {
	r := &Record{Offset: offset, SourceLocalTime: at, Evidence: Redact(text),
		Payload: map[string]string{"function": m[1], "level": m[2], "message": Redact(m[3])}}
	r.Category = CategoryScriptException
	if f := spawnerFileRe.FindStringSubmatch(m[3]); f != nil && m[1] == "SpawnObjects" {
		r.Category = CategoryObjectSpawnerError
		r.Payload["missingFile"] = f[1]
	}
	return r
}

func addFrame(r *Record, frame string) {
	if r.Payload == nil {
		r.Payload = map[string]string{}
	}
	if n := strings.Count(r.Payload["stack"], "\n"); r.Payload["stack"] != "" && n >= 7 {
		return // keep at most 8 frames
	}
	if r.Payload["stack"] != "" {
		r.Payload["stack"] += "\n"
	}
	r.Payload["stack"] += frame
}

// --- script_*.log and crash_*.log ------------------------------------------------------------------

var (
	logStartedRe = regexp.MustCompile(`^Log .* started at (\d{2})\.(\d{2})\. (\d{2}):(\d{2}):(\d{2})`)
	crashStampRe = regexp.MustCompile(`^[A-Z0-9]+, (\d{2})\.(\d{2}) (\d{4}) (\d{2}):(\d{2}):(\d{2})$`)
	moduleRe     = regexp.MustCompile(`^SCRIPT\s*:\s*Module: ([^;]+);`)
)

// ParseScriptLog parses a script_*.log or crash_*.log. year comes from the filename (the in-file
// header states day and month only); pass 0 when unknown.
func ParseScriptLog(content []byte, base int64, year int) (ParseResult, int64) {
	var res ParseResult
	ls, consumed := lines(content, base)
	var exc *Record
	var blockAt *time.Time
	pendingVM := false
	flush := func() {
		if exc != nil {
			res.add(*exc)
			exc = nil
		}
	}
	for _, l := range ls {
		text := strings.TrimSpace(l.text)
		switch {
		case text == "" || strings.HasPrefix(text, "----") || text == "Runtime mode" || strings.HasPrefix(text, "CLI params:"):
			continue // separators; CLI params carry the server address and are never kept
		case logStartedRe.MatchString(text):
			m := logStartedRe.FindStringSubmatch(text)
			r := Record{Offset: l.end, Category: CategoryLogHeader, Evidence: Redact(text), Payload: map[string]string{}}
			if year > 0 {
				if t, err := time.Parse("2006-01-02 15:04:05", strconv.Itoa(year)+"-"+m[2]+"-"+m[1]+" "+m[3]+":"+m[4]+":"+m[5]); err == nil {
					r.SourceLocalTime = &t
					res.BootLocal = &t
					blockAt = &t
				}
			} else {
				r.Status = StatusPartial
			}
			res.add(r)
		case crashStampRe.MatchString(text):
			m := crashStampRe.FindStringSubmatch(text)
			if t, err := time.Parse("2006-01-02 15:04:05", m[3]+"-"+m[2]+"-"+m[1]+" "+m[4]+":"+m[5]+":"+m[6]); err == nil {
				blockAt = &t
			}
		case strings.HasSuffix(text, "Virtual Machine Exception"):
			flush()
			pendingVM = true
		case reasonRe.MatchString(text):
			flush()
			exc = exceptionRecord(l.end, reasonRe.FindStringSubmatch(text), text, blockAt)
			if !pendingVM {
				exc.Payload["block"] = "reason_without_header"
			}
			pendingVM = false
		case functionRe.MatchString(text) && exc != nil:
			exc.Payload["function"] = functionRe.FindStringSubmatch(text)[1]
			exc.Offset = l.end
		case text == "Stack trace:" && exc != nil:
			exc.Offset = l.end
		case stackFrameRe.MatchString(text) && exc != nil:
			m := stackFrameRe.FindStringSubmatch(text)
			addFrame(exc, m[1]+" "+m[2])
			exc.Offset = l.end
		case moduleRe.MatchString(text):
			flush()
			res.add(Record{Offset: l.end, Category: CategoryScriptModule, SourceLocalTime: blockAt, Payload: map[string]string{"module": strings.TrimSpace(moduleRe.FindStringSubmatch(text)[1])}, Evidence: Redact(text)})
		case strings.Contains(text, "~DayZGame()"):
			flush()
			res.add(Record{Offset: l.end, Category: CategoryEngineDestroy, Payload: map[string]string{"stage": "script_teardown"}, Evidence: Redact(text)})
		case strings.HasPrefix(text, "SCRIPT"):
			flush()
			res.add(Record{Offset: l.end, Category: CategoryUnknown, Status: StatusUnknown, Evidence: Redact(text)})
		default:
			flush()
			res.add(Record{Offset: l.end, Category: CategoryUnknown, Status: StatusUnknown, Evidence: Redact(text)})
		}
	}
	flush()
	return res, consumed
}

// --- restart.log ----------------------------------------------------------------------------------

var (
	restartRFCRe  = regexp.MustCompile(`^([A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} [-+]\d{4}) (.*)$`)
	restartISORe  = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) (.*)$`)
	restartReqRe  = regexp.MustCompile(`^Server restart requested \(([^)]+)\)$`)
	stopReqRe     = regexp.MustCompile(`^Server stop requested \(([^)]+)\)$`)
	automatedRe   = regexp.MustCompile(`^Automated server restart in progress`)
	preStartRe    = regexp.MustCompile(`^\[[^\]]+\] \[([A-Za-z]+)\] (.+)$`)
	hostRebootRe  = regexp.MustCompile(`rebooting windows host system\.?$`)
	clientAdminRe = regexp.MustCompile(`^Website Client Admin request$`)
)

// ParseRestartLog parses Nitrado's restart.log. Lines come in two observed formats:
// "Thu, 24 Sep 2026 04:13:32 -0400 msg" (states its UTC offset, so both server-local and UTC time
// are known) and "2026-09-24 08:13:29 msg" (no zone stated; observed to be UTC because every such
// line pairs with an offset line written seconds later - recorded as clock=utc_inferred).
func ParseRestartLog(content []byte, base int64) (ParseResult, int64) {
	var res ParseResult
	ls, consumed := lines(content, base)
	for _, l := range ls {
		text := strings.TrimSpace(l.text)
		if text == "" {
			continue
		}
		r := Record{Offset: l.end, Evidence: Redact(text), Payload: map[string]string{}}
		msg := ""
		if m := restartRFCRe.FindStringSubmatch(text); m != nil {
			if t, err := time.Parse("Mon, 02 Jan 2006 15:04:05 -0700", m[1]); err == nil {
				u := t.UTC()
				local := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
				r.SourceUTC, r.SourceLocalTime = &u, &local
				r.Payload["clock"] = "stated_offset"
			}
			msg = m[2]
		} else if m := restartISORe.FindStringSubmatch(text); m != nil {
			if t, err := time.Parse("2006-01-02 15:04:05", m[1]); err == nil {
				r.SourceUTC = &t
				r.Payload["clock"] = "utc_inferred"
			}
			msg = m[2]
		} else {
			r.Category, r.Status = CategoryUnknown, StatusUnknown
			res.add(r)
			continue
		}
		switch {
		case restartReqRe.MatchString(msg):
			r.Category = CategoryRestartRequested
			r.Payload["requestedVia"] = restartReqRe.FindStringSubmatch(msg)[1]
		case stopReqRe.MatchString(msg):
			r.Category = CategoryStopRequested
			r.Payload["requestedVia"] = stopReqRe.FindStringSubmatch(msg)[1]
		case automatedRe.MatchString(msg):
			r.Category = CategoryAutomatedRestart
		case hostRebootRe.MatchString(msg):
			r.Category = CategoryHostReboot
		case clientAdminRe.MatchString(msg):
			r.Category = CategoryClientAdminRequest
		case preStartRe.MatchString(msg):
			m := preStartRe.FindStringSubmatch(msg)
			r.Category = CategoryPreStartCheck
			r.Payload["component"], r.Payload["message"] = m[1], m[2]
		default:
			r.Category, r.Status = CategoryUnknown, StatusUnknown
		}
		res.add(r)
	}
	return res, consumed
}
