package livesync

import (
	"sort"
	"time"
)

// Boot evidence kinds, strongest first.
const (
	EvidenceRPTHeader     = "RPT_CURRENT_TIME"   // "Current time:" in the RPT header
	EvidenceADMHeader     = "ADM_LOG_STARTED"    // "AdminLog started on ..." in the ADM
	EvidenceScriptHeader  = "SCRIPT_LOG_STARTED" // script_*.log header
	EvidenceCrashHeader   = "CRASH_LOG_STARTED"  // crash_*.log header
	EvidencePreStartCheck = "PRE_START_CHECK"    // restart.log pre-start component line
	EvidenceFilename      = "FILENAME_STAMP"     // a timestamp in a filename - weakest
	EvidenceRestartReq    = "RESTART_REQUESTED"  // restart.log request - a cause, not a boot
)

// BootEvidence is one source's claim that a boot happened at a server-local time.
type BootEvidence struct {
	Kind     string
	SourceID string
	Local    time.Time
	Detail   string // e.g. restart requestedVia
}

// BootObservation is one server boot, counted once however many sources report it.
type BootObservation struct {
	BootID     string // "boot:<server-local anchor time>"
	AnchorKind string
	Local      time.Time
	Evidence   []BootEvidence
	// RestartCause is the restart request that preceded this boot, when restart.log shows one.
	RestartCause string
	// Confirmed is true when in-file startup evidence (not only a filename) supports the boot.
	Confirmed bool
}

// Correlation windows.
const (
	bootJoinWindow  = 2 * time.Minute // evidence this close to an anchor is the same boot
	restartCauseMax = 5 * time.Minute // a request at most this long before a boot caused it
)

var anchorRank = map[string]int{EvidenceRPTHeader: 0, EvidenceADMHeader: 1, EvidenceScriptHeader: 2, EvidenceCrashHeader: 3, EvidenceFilename: 4, EvidencePreStartCheck: 5}

// CorrelateBoots groups evidence into boots. Anchors are in-file startup headers (filenames only
// when nothing better exists); every other item joins the nearest anchor within bootJoinWindow.
// Restart requests never create a boot: they attach as the cause of the next boot within
// restartCauseMax. Unmatched pre-start checks are dropped from boots but remain in the caller's
// records. All times are server-local (restart.log's stated local wall time is used).
func CorrelateBoots(ev []BootEvidence) []BootObservation {
	items := append([]BootEvidence(nil), ev...)
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].Local.Equal(items[j].Local) {
			return items[i].Local.Before(items[j].Local)
		}
		return anchorRank[items[i].Kind] < anchorRank[items[j].Kind]
	})
	var boots []BootObservation
	nearest := func(t time.Time) int {
		best, bestD := -1, bootJoinWindow+1
		for i := range boots {
			d := t.Sub(boots[i].Local)
			if d < 0 {
				d = -d
			}
			if d <= bootJoinWindow && d < bestD {
				best, bestD = i, d
			}
		}
		return best
	}
	// Pass 1: strong anchors (in-file headers), strongest first so the best anchor defines the boot.
	for rank := 0; rank <= 3; rank++ {
		for _, e := range items {
			if anchorRank[e.Kind] != rank || e.Kind == EvidenceRestartReq {
				continue
			}
			if i := nearest(e.Local); i >= 0 {
				boots[i].Evidence = append(boots[i].Evidence, e)
				continue
			}
			boots = append(boots, BootObservation{AnchorKind: e.Kind, Local: e.Local, Evidence: []BootEvidence{e}, Confirmed: true})
		}
	}
	// Pass 2: filenames anchor only where no header did; pre-start checks only join.
	for _, e := range items {
		switch e.Kind {
		case EvidenceFilename:
			if i := nearest(e.Local); i >= 0 {
				boots[i].Evidence = append(boots[i].Evidence, e)
			} else {
				boots = append(boots, BootObservation{AnchorKind: e.Kind, Local: e.Local, Evidence: []BootEvidence{e}})
			}
		case EvidencePreStartCheck:
			if i := nearest(e.Local); i >= 0 {
				boots[i].Evidence = append(boots[i].Evidence, e)
			}
		}
	}
	sort.Slice(boots, func(i, j int) bool { return boots[i].Local.Before(boots[j].Local) })
	// Pass 3: restart causes.
	for _, e := range items {
		if e.Kind != EvidenceRestartReq {
			continue
		}
		for i := range boots {
			d := boots[i].Local.Sub(e.Local)
			if d >= 0 && d <= restartCauseMax && boots[i].RestartCause == "" {
				boots[i].RestartCause = e.Detail
				boots[i].Evidence = append(boots[i].Evidence, e)
				break
			}
		}
	}
	for i := range boots {
		boots[i].BootID = "boot:" + boots[i].Local.Format("2006-01-02T15:04:05")
	}
	return boots
}
