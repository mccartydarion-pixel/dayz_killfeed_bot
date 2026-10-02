package nitrado

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"time"
)

// Verified tail reads (docs/NITRADO_POLLING.md, "Tail reads"). Reading only the bytes after a
// checkpoint is trusted per Nitrado service only after partial reads have matched full downloads
// byte for byte in production. The trust is shared: Live Sync (RPT/restart/script/crash logs) and
// the killfeed ADM engine read through the same file server, so either one can earn it.
//
//   - verifying: callers do their usual full download plus a tail read and report the comparison.
//     TailTrustAfter matches that covered new bytes make the service trusted.
//   - trusted: callers read the tail only; every TailRecheckEvery-th read is a verifying read.
//   - disabled: one mismatch turns tail reads off for the service until restart.
//
// Trust and match counts are saved through a TailTrustStore when one is set, so a restart does
// not repeat the verification. A restored trusted service verifies its first read again.

const (
	TailTrustAfter   = 3
	TailRecheckEvery = 20
	tailMaxChunks    = 64 // x maxChunkBytes = 16 MiB per read
)

// TailReader is the partial-read surface (*Client implements it): up to length bytes from offset.
type TailReader interface {
	ReadLogRange(ctx context.Context, serviceID, path string, offset, length int64) (*PartialReadResult, bool)
}

var _ TailReader = (*Client)(nil)

// TailTrustState is one service's tail-read state.
type TailTrustState struct {
	Matches  int  `json:"matches"`
	Trusted  bool `json:"trusted"`
	Disabled bool `json:"disabled"`
	reads    int
}

var tailTrust = struct {
	mu       sync.Mutex
	services map[string]*TailTrustState
	store    TailTrustStore
}{services: map[string]*TailTrustState{}}

// TailTrustStore persists tail-read trust per service. Disabled is never stored: after a restart
// a disabled service starts verifying again.
type TailTrustStore interface {
	LoadTailTrust(ctx context.Context) (map[string]TailTrustState, error)
	SaveTailTrust(ctx context.Context, serviceID string, s TailTrustState) error
}

// RestoreTailTrust loads saved trust and keeps saving changes to store from now on.
func RestoreTailTrust(ctx context.Context, store TailTrustStore) {
	if store == nil {
		return
	}
	saved, err := store.LoadTailTrust(ctx)
	tailTrust.mu.Lock()
	defer tailTrust.mu.Unlock()
	tailTrust.store = store
	if err != nil {
		slog.Warn("component=nitrado", "event", "tail_trust_load_failed", "err", err.Error())
		return
	}
	restored := 0
	for id, st := range saved {
		if _, seen := tailTrust.services[id]; seen {
			continue
		}
		st.Disabled = false
		st.reads = 0
		if st.Trusted {
			st.reads = TailRecheckEvery - 1 // the first read after a restart verifies again
			restored++
		}
		cp := st
		tailTrust.services[id] = &cp
	}
	if restored > 0 {
		slog.Info("component=nitrado", "event", "tail_trust_restored", "trusted_services", restored)
	}
}

// saveTailState writes s in the background. Callers hold tailTrust.mu.
func saveTailState(serviceID string, s TailTrustState) {
	store := tailTrust.store
	if store == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.SaveTailTrust(ctx, serviceID, s); err != nil {
			slog.Warn("component=nitrado", "event", "tail_trust_save_failed", "service_id", serviceID, "err", err.Error())
		}
	}()
}

func tailState(serviceID string) *TailTrustState {
	s := tailTrust.services[serviceID]
	if s == nil {
		s = &TailTrustState{}
		tailTrust.services[serviceID] = s
	}
	return s
}

// TailPlan says how the next read of serviceID should be done: tail only (trusted), or a full
// download plus a comparison tail read (verify). Both false = full download only (disabled).
func TailPlan(serviceID string) (tailOnly, verify bool) {
	tailTrust.mu.Lock()
	defer tailTrust.mu.Unlock()
	s := tailState(serviceID)
	switch {
	case s.Disabled:
		return false, false
	case s.Trusted:
		s.reads++
		if s.reads%TailRecheckEvery == 0 {
			return false, true
		}
		return true, false
	default:
		return false, true
	}
}

// TailVerified records one comparison (see TailMatches). Only matches that covered new bytes
// count toward trust; any mismatch disables the service.
func TailVerified(serviceID string, match bool, newBytes int, method, source string) {
	tailTrust.mu.Lock()
	defer tailTrust.mu.Unlock()
	s := tailState(serviceID)
	if s.Disabled {
		return
	}
	if !match {
		s.Disabled, s.Trusted, s.Matches = true, false, 0
		saveTailState(serviceID, TailTrustState{})
		slog.Warn("component=nitrado", "event", "tail_read_disabled", "service_id", serviceID, "source", source, "reason", "partial read did not match the full download")
		return
	}
	if newBytes <= 0 {
		return
	}
	if s.Trusted {
		saveTailState(serviceID, *s) // a passed recheck keeps the saved trust fresh
		return
	}
	s.Matches++
	if s.Matches >= TailTrustAfter {
		s.Trusted = true
		slog.Info("component=nitrado", "event", "tail_read_trusted", "service_id", serviceID, "source", source, "method", method, "matches", s.Matches)
	}
	saveTailState(serviceID, *s)
}

// TailFailed records a tail read that could not be done; a trusted service goes back to verifying.
func TailFailed(serviceID string) {
	tailTrust.mu.Lock()
	defer tailTrust.mu.Unlock()
	s := tailState(serviceID)
	if s.Trusted {
		s.Trusted, s.Matches, s.reads = false, 0, 0
		saveTailState(serviceID, *s)
	}
}

// TailTrust returns a copy of every service's tail-read state.
func TailTrust() map[string]TailTrustState {
	tailTrust.mu.Lock()
	defer tailTrust.mu.Unlock()
	out := make(map[string]TailTrustState, len(tailTrust.services))
	for k, v := range tailTrust.services {
		out[k] = *v
	}
	return out
}

// ReadTail reads path from offset in bounded chunks up to until (exclusive), the file size the
// caller already knows (from the listing or a full download). Each request asks only for bytes
// that exist: Nitrado's seek answered 500 when asked past the end of the file. With until <= 0
// (size unknown) it reads full-size chunks to the end. ok=false if any chunk fails or the file is
// more than 16 MiB behind.
func ReadTail(ctx context.Context, tr TailReader, serviceID, path string, offset, until int64) ([]byte, string, bool) {
	var out []byte
	method := ""
	for i := 0; i < tailMaxChunks; i++ {
		if until > 0 && offset >= until {
			return out, method, true
		}
		length := int64(maxChunkBytes)
		if until > 0 && until-offset < length {
			length = until - offset
		}
		res, ok := tr.ReadLogRange(ctx, serviceID, path, offset, length)
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
	return nil, "", false
}

// TailMatches compares a tail read starting at offset with a full download. The file may grow
// between the two reads, so only the bytes both saw are compared; the comparison must cover at
// least one byte. newBytes is how many of those compared bytes lie past the checkpoint
// (offset+1, since tail reads start one byte early).
func TailMatches(full []byte, offset int64, tail []byte) (match bool, newBytes int) {
	if offset < 0 || offset >= int64(len(full)) || len(tail) == 0 {
		return false, 0
	}
	a := full[offset:]
	n := len(a)
	if len(tail) < n {
		n = len(tail)
	}
	return bytes.Equal(a[:n], tail[:n]), n - 1
}
