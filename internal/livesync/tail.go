package livesync

import (
	"bytes"
	"context"
	"log/slog"
	"sync"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Tail reads (docs/NITRADO_POLLING.md, "Live Sync tail reads"): reading only the bytes after the
// checkpoint instead of downloading the whole file every probe. Nitrado's partial-read endpoints
// were never verified against a live server, so each Nitrado service earns trust in production:
//
//   - verifying: every read is a full download (exactly as before) PLUS a tail read from the
//     checkpoint; the tail must equal full[checkpoint:] byte for byte. tailTrustAfter matches in a
//     row make the service trusted. One mismatch disables tail reads for the service until restart.
//   - trusted: reads are tail reads only. Every tailRecheckEvery-th read is a verifying read again,
//     so a service whose partial reads later go wrong is caught and disabled.
//
// A tail read is never used for a source's first read, when the checkpoint is 0, or when the
// listing says the file is now smaller than the checkpoint (replaced or truncated).

const (
	tailTrustAfter   = 3
	tailRecheckEvery = 20
	tailMaxChunks    = 64 // x 256 KiB per chunk (nitrado.maxChunkBytes) = 16 MiB per read
)

// TailReader is the optional partial-read surface (*nitrado.Client implements it).
type TailReader interface {
	ReadLogFrom(ctx context.Context, serviceID, path string, offset int64, mode nitrado.DeltaMode) (*nitrado.PartialReadResult, bool)
}

type tailState struct {
	matches  int
	trusted  bool
	disabled bool
	reads    int // reads since trust, for the periodic recheck
}

type tailRegistry struct {
	mu       sync.Mutex
	services map[string]*tailState
}

var tailTrust = &tailRegistry{services: map[string]*tailState{}}

func (r *tailRegistry) state(serviceID string) *tailState {
	s := r.services[serviceID]
	if s == nil {
		s = &tailState{}
		r.services[serviceID] = s
	}
	return s
}

// plan decides how the next read of serviceID works: tailOnly (trusted, no full download) or
// verify (full download plus a comparison tail read). Both false = full download only.
func (r *tailRegistry) plan(serviceID string) (tailOnly, verify bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.state(serviceID)
	switch {
	case s.disabled:
		return false, false
	case s.trusted:
		s.reads++
		if s.reads%tailRecheckEvery == 0 {
			return false, true
		}
		return true, false
	default:
		return false, true
	}
}

// verified records one comparison. Only comparisons that covered new bytes count toward trust.
func (r *tailRegistry) verified(serviceID string, match bool, newBytes int, method string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.state(serviceID)
	if s.disabled {
		return
	}
	if !match {
		s.disabled, s.trusted = true, false
		slog.Warn("component=livesync", "event", "tail_read_disabled", "service_id", serviceID, "reason", "partial read did not match the full download")
		return
	}
	if newBytes == 0 || s.trusted {
		return
	}
	s.matches++
	if s.matches >= tailTrustAfter {
		s.trusted = true
		slog.Info("component=livesync", "event", "tail_read_trusted", "service_id", serviceID, "method", method, "matches", s.matches)
	}
}

// failed records a tail read that could not be done (the caller falls back to a full download).
func (r *tailRegistry) failed(serviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.state(serviceID)
	if s.trusted {
		s.trusted, s.matches, s.reads = false, 0, 0 // re-verify before trusting again
	}
}

// readTail reads path from offset to the current end of the file in bounded chunks.
func readTail(ctx context.Context, tr TailReader, serviceID, path string, offset int64) ([]byte, string, bool) {
	var out []byte
	method := ""
	for i := 0; i < tailMaxChunks; i++ {
		res, ok := tr.ReadLogFrom(ctx, serviceID, path, offset, nitrado.DeltaModeAuto)
		if !ok || res == nil || res.StartOffset != offset {
			return nil, "", false
		}
		method = res.Method
		if len(res.Data) == 0 {
			return out, method, true
		}
		out = append(out, res.Data...)
		offset = res.EndOffset
	}
	return nil, "", false // more than tailMaxChunks behind: let a full read catch up
}

// tailMatches reports whether a tail read equals the full download from offset.
func tailMatches(full []byte, offset int64, tail []byte) bool {
	if offset > int64(len(full)) {
		return false
	}
	return bytes.Equal(full[offset:], tail)
}

var _ TailReader = (*nitrado.Client)(nil)
