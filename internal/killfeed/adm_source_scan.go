package killfeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// maxScanCandidates bounds how many ADM candidates a source scan will
// direct-download per round, so a scan can never trigger a download storm.
const maxScanCandidates = 5

// ADMSourceCandidate is one ADM file observed across both rounds of a scan.
type ADMSourceCandidate struct {
	Name              string
	ModifiedBefore    time.Time
	ModifiedAfter     time.Time
	SizeBefore        int64
	SizeAfter         int64
	FingerprintBefore string
	FingerprintAfter  string
	Classification    string // ACTIVE_WRITING, STATIC, NEW, UNAVAILABLE
}

// ADMSourceScanResult is the outcome of comparing every candidate across two
// discovery rounds separated by a wait interval.
type ADMSourceScanResult struct {
	Candidates       []ADMSourceCandidate
	CurrentSelected  string
	ActiveSource     string
	ActiveSizeBefore int64
	ActiveSizeAfter  int64
	ContentChanged   bool
	NewADMCreated    bool
	Recommendation   string
	RootCause        string
}

// ScanADMSources discovers ADM candidates, direct-downloads and fingerprints
// each, waits, then repeats to identify which file (if any) is actually being
// written to. It never selects a source based on filename or modified metadata
// alone; only an observed content or size change counts as ACTIVE_WRITING.
func ScanADMSources(ctx context.Context, client LogSource, serviceID, currentSelected string, wait time.Duration) (*ADMSourceScanResult, error) {
	before, err := client.ListLogs(ctx, serviceID)
	if err != nil || len(before) == 0 {
		return &ADMSourceScanResult{
			CurrentSelected: currentSelected,
			Recommendation:  "NO LIVE ADM WRITES DETECTED",
			RootCause:       "NITRADO_SOURCE_UNAVAILABLE",
		}, nil
	}

	beforeLimit := before
	if len(beforeLimit) > maxScanCandidates {
		beforeLimit = beforeLimit[:maxScanCandidates]
	}
	beforeSnap := snapshotCandidates(ctx, client, serviceID, beforeLimit)

	timer := time.NewTimer(wait)
	select {
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
	}

	after, err := client.ListLogs(ctx, serviceID)
	if err != nil {
		after = nil
	}
	afterLimit := after
	if len(afterLimit) > maxScanCandidates {
		afterLimit = afterLimit[:maxScanCandidates]
	}
	afterSnap := snapshotCandidates(ctx, client, serviceID, afterLimit)

	result := mergeScanSnapshots(beforeSnap, afterSnap)
	result.CurrentSelected = currentSelected
	classifyScanResult(result)
	return result, nil
}

type candidateSnapshot struct {
	name        string
	modified    time.Time
	size        int64
	fingerprint string
	available   bool
}

func snapshotCandidates(ctx context.Context, client LogSource, serviceID string, logs []nitrado.LogFile) []candidateSnapshot {
	out := make([]candidateSnapshot, 0, len(logs))
	for _, lf := range logs {
		snap := candidateSnapshot{name: lf.Name, modified: lf.Modified, size: lf.Size}
		content, err := client.ReadLog(ctx, serviceID, lf.Path)
		if err == nil {
			snap.available = true
			snap.size = int64(len(content))
			sum := sha256.Sum256(content)
			snap.fingerprint = hex.EncodeToString(sum[:])
		}
		out = append(out, snap)
	}
	return out
}

func mergeScanSnapshots(before, after []candidateSnapshot) *ADMSourceScanResult {
	beforeByName := make(map[string]candidateSnapshot, len(before))
	for _, c := range before {
		beforeByName[c.name] = c
	}
	afterByName := make(map[string]candidateSnapshot, len(after))
	for _, c := range after {
		afterByName[c.name] = c
	}

	names := make([]string, 0, len(beforeByName)+len(afterByName))
	seen := map[string]struct{}{}
	for _, c := range before {
		if _, ok := seen[c.name]; !ok {
			seen[c.name] = struct{}{}
			names = append(names, c.name)
		}
	}
	for _, c := range after {
		if _, ok := seen[c.name]; !ok {
			seen[c.name] = struct{}{}
			names = append(names, c.name)
		}
	}
	sort.Strings(names)

	candidates := make([]ADMSourceCandidate, 0, len(names))
	for _, name := range names {
		b, hasBefore := beforeByName[name]
		a, hasAfter := afterByName[name]
		candidate := ADMSourceCandidate{Name: name}
		switch {
		case !hasBefore && hasAfter:
			candidate.ModifiedAfter = a.modified
			candidate.SizeAfter = a.size
			candidate.FingerprintAfter = a.fingerprint
			candidate.Classification = "NEW"
		case hasBefore && !hasAfter:
			candidate.ModifiedBefore = b.modified
			candidate.SizeBefore = b.size
			candidate.FingerprintBefore = b.fingerprint
			candidate.Classification = "UNAVAILABLE"
		default:
			candidate.ModifiedBefore = b.modified
			candidate.ModifiedAfter = a.modified
			candidate.SizeBefore = b.size
			candidate.SizeAfter = a.size
			candidate.FingerprintBefore = b.fingerprint
			candidate.FingerprintAfter = a.fingerprint
			switch {
			case !b.available || !a.available:
				candidate.Classification = "UNAVAILABLE"
			case b.fingerprint != a.fingerprint || b.size != a.size:
				candidate.Classification = "ACTIVE_WRITING"
			default:
				candidate.Classification = "STATIC"
			}
		}
		candidates = append(candidates, candidate)
	}
	return &ADMSourceScanResult{Candidates: candidates}
}

// classifyScanResult picks the active source (content-change evidence only)
// and derives the recommendation and root cause.
func classifyScanResult(result *ADMSourceScanResult) {
	for _, c := range result.Candidates {
		if c.Classification != "ACTIVE_WRITING" {
			continue
		}
		result.ActiveSource = c.Name
		result.ActiveSizeBefore = c.SizeBefore
		result.ActiveSizeAfter = c.SizeAfter
		result.ContentChanged = true
		break
	}
	for _, c := range result.Candidates {
		if c.Classification == "NEW" {
			result.NewADMCreated = true
			break
		}
	}

	switch {
	case len(result.Candidates) == 0:
		result.Recommendation = "NO LIVE ADM WRITES DETECTED"
		result.RootCause = "NITRADO_SOURCE_UNAVAILABLE"
	case result.ActiveSource == "":
		result.Recommendation = "NO LIVE ADM WRITES DETECTED"
		result.RootCause = "NO_LIVE_ADM_WRITES"
	case result.ActiveSource == result.CurrentSelected:
		result.Recommendation = "USE ACTIVE SOURCE"
		result.RootCause = "CURRENT_SOURCE_IS_ACTIVE"
	case result.NewADMCreated && isNewCandidate(result.Candidates, result.ActiveSource):
		result.Recommendation = "USE ACTIVE SOURCE"
		result.RootCause = "NEW_ADM_NOT_SELECTED"
	default:
		result.Recommendation = "USE ACTIVE SOURCE"
		result.RootCause = "WRONG_ADM_SELECTED"
	}
}

func isNewCandidate(candidates []ADMSourceCandidate, name string) bool {
	for _, c := range candidates {
		if c.Name == name {
			return c.Classification == "NEW"
		}
	}
	return false
}
