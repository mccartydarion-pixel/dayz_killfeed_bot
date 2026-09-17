package killfeed

import (
	"sort"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// candidateActivityState classifies one discovered ADM candidate's evidence
// of real activity, independent of the overall pipeline health classification
// in runtime_diagnostics.go (which describes the SELECTED source's read/parse
// pipeline, not candidate selection itself).
type candidateActivityState string

const (
	// candidateActive has direct evidence of recent activity: its size grew or
	// its modified time advanced since the last discovery pass, or it was
	// previously marked stale but has since shown new evidence.
	candidateActive candidateActivityState = "ACTIVE"
	// candidateUnknown has never been observed in a prior discovery pass -
	// no evidence either way, but also no evidence AGAINST it. This is the
	// only state a genuinely new/freshly rotated file can have on its first
	// appearance, so it still gets picked over a proven-stale source. Ranks
	// above a known-stale candidate (section 4).
	candidateUnknown candidateActivityState = "UNKNOWN"
	// candidateStale covers two equally non-promising cases, deliberately
	// merged: a source explicitly given up on (see Engine.markSelectedStale)
	// with no new evidence since, AND a candidate that has been observed
	// before but shows no growth or modified-time advancement since that
	// observation. The second case used to be its own optimistic
	// "RECENTLY_ACTIVE" state simply for having appeared in history before -
	// which let discovery walk backward through old inactive ADM files one
	// at a time, each new "least-bad" pick looking falsely promising. An
	// unchanged history entry is not activity evidence; it is exactly the
	// same signal a proven-stale source gives, so it is scored the same way.
	candidateStale candidateActivityState = "STALE"
)

// candidateObservation is the cheap metadata (no extra remote reads) recorded
// for every discovered candidate on each discovery pass, so growth/advancement
// can be detected across passes even for candidates that were never selected.
type candidateObservation struct {
	Size       int64
	Modified   time.Time
	ObservedAt time.Time
}

// staleMark records the metadata Champion observed for a path at the exact
// moment it gave up on it as the active source (see Engine.markSelectedStale).
// It is a demotion, never a permanent blacklist: a later discovery pass that
// finds this same path with a different size or modified time treats it as
// having new evidence and clears the mark (section 5).
type staleMark struct {
	At       time.Time
	Size     int64
	Modified time.Time
}

// candidateRank is one candidate's ranking outcome, kept for both selection
// and diagnostic logging (section 13).
type candidateRank struct {
	File   nitrado.LogFile
	State  candidateActivityState
	Reason string
	Score  int64
}

// Score bands, spaced far apart so a higher-priority signal always outranks
// every lower one regardless of magnitude (section 9's precedence). baseScore
// (see rankCandidates) is bounded by the candidate count, so it never
// approaches these bands - it only decides ties when no other signal applies,
// which reproduces today's "newest remote modified" behavior unchanged.
const (
	scoreSizeGrew         int64 = 1_000_000
	scoreModifiedAdvanced int64 = 500_000
	scoreStaleDemotion    int64 = -10_000_000
)

// rankCandidates orders discovered ADM candidates by evidence of real
// activity instead of raw "newest modified timestamp" alone - the root cause
// of Champion repeatedly reselecting a known-dead file solely because Nitrado
// still reported it as newest by metadata. Highest Score wins; ties preserve
// Nitrado's own newest-first ListLogs ordering exactly as before.
//
// This only ever reads cheap, already-fetched ListLogs metadata (path, size,
// modified) plus in-memory history - it never issues extra remote reads.
// Direct-probe evidence (content changed / unread bytes past checkpoint) is
// handled entirely upstream by probeStaleSource: by the time pollSelected
// gives up and discovery runs, the probe has already conclusively ruled out
// life in the selected file (see the staleGiveUpAfter branch in pollSelected), so
// there is nothing stronger left to fold in here for that candidate.
func (e *Engine) rankCandidates(logs []nitrado.LogFile, now time.Time) []candidateRank {
	ranks := make([]candidateRank, len(logs))
	for i, lf := range logs {
		ranks[i] = e.rankOneCandidate(lf, len(logs)-i, now)
	}
	sort.SliceStable(ranks, func(a, b int) bool { return ranks[a].Score > ranks[b].Score })
	return ranks
}

func (e *Engine) rankOneCandidate(lf nitrado.LogFile, baseScore int, now time.Time) candidateRank {
	rank := candidateRank{File: lf, State: candidateUnknown, Reason: "newest_remote_modified", Score: int64(baseScore)}

	prev, hasHistory := e.candidateHistory[lf.Path]
	grew := hasHistory && lf.Size > prev.Size
	modifiedAdvanced := hasHistory && !grew && lf.Modified.After(prev.Modified)
	staleByHistory := hasHistory && !grew && !modifiedAdvanced

	mark, hasMark := e.staleMarks[lf.Path]
	newEvidenceVsMark := hasMark && (lf.Size != mark.Size || !lf.Modified.Equal(mark.Modified))

	switch {
	case hasMark && !newEvidenceVsMark:
		// Explicitly proven stale (direct probe or the hard give-up) and
		// nothing has changed since - the strongest, most authoritative
		// demotion available.
		rank.State, rank.Reason = candidateStale, "known_stale_no_new_evidence"
		rank.Score += scoreStaleDemotion
	case hasMark && newEvidenceVsMark:
		// New evidence since it was marked stale: never a permanent
		// blacklist, it is immediately eligible again.
		rank.State, rank.Reason = candidateActive, "stale_source_shows_new_evidence"
		delete(e.staleMarks, lf.Path)
	case grew:
		rank.State, rank.Reason = candidateActive, "size_growth_since_last_discovery"
		rank.Score += scoreSizeGrew
	case modifiedAdvanced:
		rank.State, rank.Reason = candidateActive, "modified_advanced_since_last_discovery"
		rank.Score += scoreModifiedAdvanced
	case staleByHistory:
		// Seen before, shows no growth or modified advancement since - not
		// "recently active" just because it happened to appear in a prior
		// discovery pass (section 6). Demoted exactly like an explicitly
		// proven-stale source, so discovery never treats an old unchanged
		// file as a promising fallback pick.
		rank.State, rank.Reason = candidateStale, "no_growth_since_last_discovery"
		rank.Score += scoreStaleDemotion
	}
	return rank
}

// updateCandidateHistory records this pass's cheap metadata for every
// discovered candidate, so the NEXT discovery pass can detect growth or a
// modified-time advance. Must run after rankCandidates has consulted the
// previous pass's history, not before.
func (e *Engine) updateCandidateHistory(logs []nitrado.LogFile, now time.Time) {
	if e.candidateHistory == nil {
		e.candidateHistory = make(map[string]candidateObservation, len(logs))
	}
	for _, lf := range logs {
		e.candidateHistory[lf.Path] = candidateObservation{Size: lf.Size, Modified: lf.Modified, ObservedAt: now}
	}
}

// markSelectedStale records the metadata observed for the given path at the
// exact moment Champion gave up on it (see pollSelected's staleGiveUpAfter branch),
// demoting it in future candidate ranking until new evidence appears. A fresh
// stat is attempted so the recorded snapshot is as current as possible; if
// that fails, the last known selected metadata is used instead - still a
// useful demotion baseline.
func (e *Engine) markSelectedStale(current *nitrado.LogFile) {
	if e == nil || current == nil {
		return
	}
	if e.staleMarks == nil {
		e.staleMarks = make(map[string]staleMark)
	}
	e.staleMarks[current.Path] = staleMark{At: time.Now(), Size: current.Size, Modified: current.Modified}
}
