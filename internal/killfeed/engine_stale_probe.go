package killfeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// probeStaleSource downloads the selected ADM even when directory metadata
// claims it is unchanged, because Nitrado listings can go stale while the file
// is still being written. It is rate limited and only reads the unread tail.
func (e *Engine) probeStaleSource(ctx context.Context, current *nitrado.LogFile) {
	if e == nil || current == nil || e.tracker == nil || e.client == nil {
		return
	}
	if e.lastLogChange.IsZero() || time.Since(e.lastLogChange) < staleProbeAfter {
		return
	}
	if !e.lastStaleProbeAt.IsZero() && time.Since(e.lastStaleProbeAt) < staleProbeInterval {
		return
	}
	e.lastStaleProbeAt = time.Now()

	content, err := e.client.ReadLog(ctx, e.serviceID, current.Path)
	if err != nil {
		if e.diagnostics != nil {
			e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
				s.LastProbeAt = e.lastStaleProbeAt
				s.ProbeResult = "FAILURE"
				s.ProbeClassification = "LIVE_SOURCE_STALE"
			})
		}
		slog.Warn("component=adm", "event", "stale_probe_failed", "server_id", e.serverID, "file", current.Name, "error_class", safeDownloadErrorClass(err))
		return
	}

	directSize := int64(len(content))
	sum := sha256.Sum256(content)
	fingerprint := hex.EncodeToString(sum[:])
	contentChanged := e.lastStaleProbeFingerprint != "" && e.lastStaleProbeFingerprint != fingerprint
	e.lastStaleProbeFingerprint = fingerprint
	e.directSizeHint = directSizeHint{Path: current.Path, Size: directSize}

	checkpointOffset := e.tracker.LastByteOffset
	unread := directSize - checkpointOffset
	if unread < 0 {
		unread = 0
	}
	// No growth by direct read is only proof of a wrong/inactive source once
	// it has lasted past staleGiveUpAfter; before that it is a quiet server
	// (an old modified time alone never proves the source is broken).
	classification := ProbeSourceQuiet
	if !e.lastLogChange.IsZero() && time.Since(e.lastLogChange) > staleGiveUpAfter {
		classification = "WRONG_OR_INACTIVE_ADM_SOURCE"
	}
	if directSize > current.Size || contentChanged || unread > 0 {
		classification = "NITRADO_METADATA_STALE"
	}

	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.LastProbeAt = e.lastStaleProbeAt
			s.ProbeResult = "SUCCESS"
			s.ProbeMetadataSize = current.Size
			s.ProbeDirectSize = directSize
			s.ProbeContentChanged = contentChanged
			s.ProbeUnreadBytes = unread
			s.ProbeClassification = classification
		})
		e.diagnostics.Event("stale probe " + classification)
	}
	slog.Info("component=adm", "event", "stale_probe", "server_id", e.serverID, "file", current.Name, "metadata_size", current.Size, "direct_size", directSize, "checkpoint", checkpointOffset, "unread_bytes", unread, "content_changed", contentChanged, "classification", classification)

	if unread > 0 {
		e.processProbeTail(ctx, current, content, checkpointOffset)
	}
}

// noteSourceGrowth records that a normal metadata-driven read consumed new
// bytes from the selected ADM. That is live evidence the source is writing and
// that listing metadata has caught up, so a quiet/stale/wrong probe label
// from an earlier quiet window no longer describes it and is cleared. (A
// stale source merely re-selected by discovery never reaches here, so it is
// never falsely healed.)
func (e *Engine) noteSourceGrowth(bytes int64) {
	if e == nil || bytes <= 0 || e.diagnostics == nil {
		return
	}
	now := time.Now()
	e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
		s.LastSourceGrowthAt = now
		if s.ProbeClassification != "" {
			s.ProbeClassification = ""
		}
	})
}

// processProbeTail processes only the unread tail discovered by a stale probe,
// using the same acknowledged persistence path as normal polling.
func (e *Engine) processProbeTail(ctx context.Context, current *nitrado.LogFile, content []byte, startOffset int64) {
	if startOffset < 0 || startOffset > int64(len(content)) {
		return
	}
	e.tracker.LineBuffer = string(content[startOffset:])
	lineChunks := e.tracker.DrainCompleteLinesWithOffsets(startOffset)
	safeOffset := startOffset
	eventsParsed := 0
	for _, chunk := range lineChunks {
		parsed, processErr := e.processLineAt(chunk.Text, current.Path, chunk.EndOffset)
		if parsed {
			eventsParsed++
		}
		if processErr != nil {
			break
		}
		safeOffset = chunk.EndOffset
	}
	e.tracker.LineBuffer = string(content[safeOffset:])
	e.bytesProcessed += safeOffset - startOffset
	e.linesDiscovered += int64(len(lineChunks))
	e.lastLogChange = time.Now()
	e.lastDownloadAt = time.Now()
	e.tracker.UpdateCheckpoint(e.serviceID, current.Path, int64(len(content)), current.Modified, safeOffset)
	checkpointOK := e.saveDurableCheckpoint(ctx, current, safeOffset)
	if safeOffset > startOffset && e.diagnostics != nil {
		// Growth proven by direct read; the NITRADO_METADATA_STALE label
		// stays, because the listing is still what lags.
		now := time.Now()
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) { s.LastSourceGrowthAt = now })
	}
	e.emitDownloadReport(DownloadReport{ServerID: e.serverID, File: current.Name, RemoteSize: current.Size, DownloadedBytes: int64(len(content)), PreviousOffset: startOffset, NewOffset: safeOffset, NewBytes: safeOffset - startOffset, EventsParsed: eventsParsed, Result: "success", CheckpointCurrent: checkpointOK, At: time.Now()})
	slog.Info("component=adm", "event", "stale_probe_processed", "server_id", e.serverID, "file", current.Name, "previous_offset", startOffset, "new_offset", safeOffset, "events_parsed", eventsParsed)
}
