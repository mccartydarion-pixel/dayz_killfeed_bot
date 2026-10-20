package caseintel

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Trusted event time for retained ADM evidence.
//
// DayZ names every boot's ADM after its server-local start time
// (DayZServer_PS4_x64_2026-09-24_05-23-05.ADM) and prefixes each line with a
// server-local HH:MM:SS. Within one boot file, with lines ordered by byte
// offset, the date is the boot date plus the midnight rollovers seen so far.
// The zone is not in the file: UTC needs the offset the live-sync layer
// learned from a restart.log line that states it. Without that offset no
// time is trusted.
//
// Retained evidence is a sparse per-player subset of the file. A midnight
// rollover between two sparse rows is resolved with the real UTC ingestion
// gap between them. When the two clocks disagree the row is untrusted, not
// guessed.

var bootStampRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})_(\d{2})-(\d{2})-(\d{2})\.ADM$`)

// BootStamp returns the server-local boot time encoded in an ADM source name.
func BootStamp(sourceID string) (time.Time, bool) {
	m := bootStampRe.FindStringSubmatch(path.Base(strings.ReplaceAll(sourceID, "\\", "/")))
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02 15:04:05", m[1]+" "+m[2]+":"+m[3]+":"+m[4])
	return t, err == nil
}

func clockSeconds(clock string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(clock), ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, e1 := strconv.Atoi(parts[0])
	m, e2 := strconv.Atoi(parts[1])
	s, e3 := strconv.Atoi(parts[2])
	if e1 != nil || e2 != nil || e3 != nil || h < 0 || h > 23 || m < 0 || m > 59 || s < 0 || s > 59 {
		return 0, false
	}
	return h*3600 + m*60 + s, true
}

// SourceTime is the resolved event time of one evidence row.
type SourceTime struct {
	EventAt time.Time // UTC; zero when untrusted
	Trusted bool
	Reason  string // why untrusted; empty when trusted
}

// ingestTolerance bounds how far ingestion may lag or compress relative to
// source time (backfill reads many lines at once) before a rollover call
// becomes ambiguous.
const ingestTolerance = 2 * time.Hour

// ResolveSourceTimes returns a trusted UTC event time per evidence ID, or the
// reason it cannot be trusted. utcOffsetMinutes is nil when the server's
// offset has not been learned.
func ResolveSourceTimes(events []Event, utcOffsetMinutes *int) map[int64]SourceTime {
	out := make(map[int64]SourceTime, len(events))
	bySource := map[string][]Event{}
	for _, e := range events {
		bySource[e.SourceID] = append(bySource[e.SourceID], e)
	}
	for source, rows := range bySource {
		sort.Slice(rows, func(i, j int) bool { return rows[i].SourceEndOffset < rows[j].SourceEndOffset })
		boot, ok := BootStamp(source)
		untrust := func(reason string) {
			for _, e := range rows {
				out[e.ID] = SourceTime{Reason: reason}
			}
		}
		if !ok {
			untrust("BOOT_STAMP_UNAVAILABLE")
			continue
		}
		if utcOffsetMinutes == nil {
			untrust("UTC_OFFSET_UNKNOWN")
			continue
		}
		offset := time.Duration(*utcOffsetMinutes) * time.Minute
		day := time.Date(boot.Year(), boot.Month(), boot.Day(), 0, 0, 0, 0, time.UTC)
		prevLocal, prevIngest := boot, time.Time{}
		broken := ""
		for _, e := range rows {
			if broken != "" {
				out[e.ID] = SourceTime{Reason: broken}
				continue
			}
			sec, ok := clockSeconds(e.ADMClock)
			if !ok {
				out[e.ID] = SourceTime{Reason: "CLOCK_MALFORMED"}
				continue
			}
			local := day.Add(time.Duration(sec) * time.Second)
			for local.Before(prevLocal.Add(-time.Hour)) {
				// Clock went back by more than an hour: a midnight rollover.
				day = day.AddDate(0, 0, 1)
				local = local.AddDate(0, 0, 1)
			}
			if local.Before(prevLocal) {
				// A small backwards step contradicts byte order.
				out[e.ID] = SourceTime{Reason: "CLOCK_OUT_OF_ORDER"}
				continue
			}
			if !prevIngest.IsZero() && !e.IngestedAt.IsZero() {
				ingestGap, sourceGap := e.IngestedAt.Sub(prevIngest), local.Sub(prevLocal)
				// A day more ingestion time than source time means an
				// unobserved extra rollover; the order of lines is then
				// no longer enough, so stop trusting this source.
				if ingestGap-sourceGap > 24*time.Hour-ingestTolerance {
					broken = "ROLLOVER_AMBIGUOUS"
					out[e.ID] = SourceTime{Reason: broken}
					continue
				}
			}
			prevLocal, prevIngest = local, e.IngestedAt
			out[e.ID] = SourceTime{EventAt: local.Add(-offset), Trusted: true}
		}
	}
	return out
}

// BootRestartWindows derives restart windows from boot boundaries: a newer
// boot file is written evidence that the server restarted. A window runs from
// the last trusted event of the older boot to the newer boot's start (UTC).
func BootRestartWindows(events []Event, times map[int64]SourceTime, utcOffsetMinutes *int) []RestartWindow {
	if utcOffsetMinutes == nil {
		return nil
	}
	offset := time.Duration(*utcOffsetMinutes) * time.Minute
	type bootInfo struct{ start, last time.Time }
	boots := map[string]*bootInfo{}
	for _, e := range events {
		start, ok := BootStamp(e.SourceID)
		if !ok {
			continue
		}
		b := boots[e.SourceID]
		if b == nil {
			b = &bootInfo{start: start.Add(-offset), last: start.Add(-offset)}
			boots[e.SourceID] = b
		}
		if t := times[e.ID]; t.Trusted && t.EventAt.After(b.last) {
			b.last = t.EventAt
		}
	}
	list := make([]*bootInfo, 0, len(boots))
	for _, b := range boots {
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].start.Before(list[j].start) })
	var out []RestartWindow
	for i := 1; i < len(list); i++ {
		if list[i].start.After(list[i-1].last) {
			out = append(out, RestartWindow{Start: list[i-1].last, End: list[i].start})
		}
	}
	return out
}
