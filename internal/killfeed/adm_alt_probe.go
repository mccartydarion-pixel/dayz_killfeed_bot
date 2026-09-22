package killfeed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

const (
	// maxAlternativeProbes bounds how many non-selected ADM candidates are
	// direct-read per probe round, so recovery can never become a download storm.
	maxAlternativeProbes = 3
	// alternativeProbeInterval spaces probe rounds. Shorter than the stale
	// rediscovery cadence (staleProbeInterval) so consecutive discovery passes
	// are never skipped by rounding, but still far apart enough to observe growth.
	alternativeProbeInterval = staleProbeInterval / 2
)

// directProbeObservation is what a direct read of a candidate showed. Unlike
// the listing metadata in candidateObservation it comes from the file's real
// content, so it stays truthful when Nitrado's directory metadata lags.
type directProbeObservation struct {
	Size          int64
	Fingerprint   string
	AlignedOffset int64 // just past the last complete line at observation time
	Modified      time.Time
	ObservedAt    time.Time
}

// probeSwitch is an alternative candidate proven active by direct reads.
type probeSwitch struct {
	File        nitrado.LogFile // Size is the direct size
	Content     []byte
	StartOffset int64 // first byte not yet seen (end of the baseline's last complete line)
	SizeBefore  int64
}

// directSizeHint is the last size a direct read proved for a path.
type directSizeHint struct {
	Path string
	Size int64
}

// probeAlternatives direct-reads the top non-selected candidates when nothing
// is ranked ACTIVE/UNKNOWN by metadata. Ranking only sees listing size and
// modified time; when those lag (the reason probeStaleSource exists for the
// selected file) the genuinely live file looks static, every candidate is
// demoted, and the stale selection is retained forever. A candidate is only
// returned once two reads, at least one probe interval apart, show its size
// grew and the new bytes contain a complete line. The first read only records
// a baseline, so pre-existing history is never treated as new activity.
func (e *Engine) probeAlternatives(ctx context.Context, ranked []candidateRank) *probeSwitch {
	if e == nil || e.client == nil || e.selected == nil {
		return nil
	}
	if !e.lastAltProbeAt.IsZero() && time.Since(e.lastAltProbeAt) < alternativeProbeInterval {
		return nil
	}
	e.lastAltProbeAt = time.Now()
	if e.altProbeHistory == nil {
		e.altProbeHistory = make(map[string]directProbeObservation)
	}
	selectedID := canonicalADMID(e.selected.Path)

	probed := 0
	for _, r := range ranked {
		if probed >= maxAlternativeProbes {
			break
		}
		lf := r.File
		if lf.Path == "" || canonicalADMID(lf.Path) == selectedID {
			continue
		}
		probed++
		content, err := e.client.ReadLog(ctx, e.serviceID, lf.Path)
		if err != nil {
			slog.Warn("component=adm_discovery", "event", "alt_probe_failed", "server_id", e.serverID, "file", lf.Name, "error_class", safeDownloadErrorClass(err))
			continue
		}
		size := int64(len(content))
		sum := sha256.Sum256(content)
		obs := directProbeObservation{
			Size:          size,
			Fingerprint:   hex.EncodeToString(sum[:]),
			AlignedOffset: int64(bytes.LastIndexByte(content, '\n') + 1),
			Modified:      lf.Modified,
			ObservedAt:    time.Now(),
		}
		prev, seen := e.altProbeHistory[lf.Path]
		e.altProbeHistory[lf.Path] = obs

		grew := seen && size > prev.Size && prev.AlignedOffset <= size &&
			bytes.IndexByte(content[prev.AlignedOffset:], '\n') >= 0
		// Debug, not Info (Champion Performance Phase 1, section 32/33): this
		// is a per-candidate, per-probe-round diagnostic that repeats while the
		// engine sits in the stale-metadata fallback path - the meaningful
		// outcome (an actual source switch) is already logged at Info
		// separately ("component=adm","event","rotation") when grew leads to
		// finishProbeSwitch; this raw attempt log carries no business meaning
		// on its own.
		slog.Debug("component=adm_discovery", "event", "alt_probe",
			"server_id", e.serverID, "path", lf.Path, "modified_at", lf.Modified.UTC().Format(time.RFC3339),
			"metadata_size", lf.Size, "size_a", prev.Size, "size_b", size, "baseline_only", !seen, "growth", grew)
		if !grew {
			continue
		}
		active := lf
		active.Size = size
		return &probeSwitch{File: active, Content: content, StartOffset: prev.AlignedOffset, SizeBefore: prev.Size}
	}
	return nil
}

// seedProbeCheckpoint makes the switch resume where the baseline probe stopped
// (never byte 0, so the file's history is not replayed). A path the tracker
// already has its own checkpoint for keeps it: selectLog resumes from that.
func (e *Engine) seedProbeCheckpoint(sw *probeSwitch) {
	if e.tracker == nil {
		e.tracker = NewTracker(e.serviceID)
	}
	if e.tracker.Checkpoints == nil {
		e.tracker.Checkpoints = make(map[string]LogCheckpoint)
	}
	if _, ok := e.tracker.Checkpoints[sw.File.Path]; ok {
		return
	}
	e.tracker.Checkpoints[sw.File.Path] = LogCheckpoint{
		ServiceID:    e.serviceID,
		FilePath:     sw.File.Path,
		Offset:       sw.StartOffset,
		FileSize:     sw.SizeBefore,
		LastModified: sw.File.Modified,
	}
}

// finishProbeSwitch runs after selectLog: it processes the bytes written
// between the two probes right away (through the normal acknowledged
// persistence path), and records the direct size so lagging directory
// metadata is not later mistaken for a truncation.
func (e *Engine) finishProbeSwitch(ctx context.Context, sw *probeSwitch) {
	delete(e.staleMarks, sw.File.Path)
	e.lastStaleProbeFingerprint = ""
	if e.tracker.CurrentLogFile != sw.File.Path {
		e.tracker.UpdateCheckpoint(e.serviceID, sw.File.Path, sw.SizeBefore, sw.File.Modified, sw.StartOffset)
	}
	e.directSizeHint = directSizeHint{Path: sw.File.Path, Size: sw.File.Size}
	start := e.tracker.LastByteOffset
	if start < 0 || start >= int64(len(sw.Content)) || e.selected == nil {
		return
	}
	e.processProbeTail(ctx, e.selected, sw.Content, start)
}

// metadataLagsDirect reports whether listing metadata claims a size below the
// checkpoint even though a direct read already proved the file is at least
// that large - lagging metadata, not a truncation.
func (e *Engine) metadataLagsDirect(path string, offset int64) bool {
	return e.directSizeHint.Path == path && e.directSizeHint.Size >= offset
}
