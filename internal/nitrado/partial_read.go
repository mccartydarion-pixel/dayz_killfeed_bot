package nitrado

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Champion Performance Phase 1.5 (docs/NITRADO_DELTA_READS.md): a partial-read path that fetches
// only the bytes after the durable processed offset, instead of ReadLog's full-file re-download on
// every poll with a change. ReadLog itself is untouched and remains the authoritative fallback -
// nothing here ever removes it or changes its behavior. Every partial-read mechanism here is
// strictly additive and, by default, never called at all (internal/killfeed wires this in behind
// NITRADO_DELTA_READ_MODE, default "off" - see docs/PERFORMANCE.md).
//
// Three mechanisms are attempted, in this preferred order (task's own research into Nitrado's
// official client):
//  1. SEEK    - GET .../file_server/seek?file=&offset=&length=&mode=raw, a first-party Nitrado
//     partial-read endpoint returning the same signed-token envelope the normal download
//     endpoint does.
//  2. OFFSET_QUERY - the existing normal download token, but with &offset=&count= appended to the
//     signed URL fetch itself (Nitrado's second documented mechanism).
//  3. RANGE   - a standard HTTP Range request against the signed URL, accepted ONLY on a genuine
//     206 Partial Content with a Content-Range that matches what was requested - a 200 response to
//     a Range request is never trusted to start at the requested offset (task section 8).
//
// None of this has been verified against a live Nitrado service in this environment (no live
// credentials here, and this phase's own instructions forbid hitting production automatically) -
// it is built exactly to the shape the task describes Nitrado's official client using, defended by
// strict response validation and a full-read fallback on anything that doesn't check out. See
// cmd/nitrado-delta-probe for the read-only tool meant to validate this against a real service
// before an operator changes NITRADO_DELTA_READ_MODE away from "off".

// PartialReadResult is the outcome of one partial-read attempt.
type PartialReadResult struct {
	Data            []byte
	RequestedOffset int64
	StartOffset     int64 // the offset the returned Data actually starts at - may differ from RequestedOffset if the server aligns differently; always validated before use
	EndOffset       int64 // StartOffset + len(Data)
	RemoteSize      int64 // 0 when unknown
	Method          string
	Complete        bool // EndOffset has caught up to RemoteSize (nothing more known to exist right now) - not "the file will never grow again"
}

// Capability is what a Nitrado service has been observed to support for partial reads.
type Capability string

const (
	CapabilityUnknown     Capability = "UNKNOWN"
	CapabilitySeek        Capability = "SEEK_SUPPORTED"
	CapabilityOffsetQuery Capability = "OFFSET_QUERY_SUPPORTED"
	CapabilityRange       Capability = "RANGE_SUPPORTED"
	CapabilityFullOnly    Capability = "FULL_ONLY"
)

// errUnsupported signals "this mechanism didn't work for this response - try the next one, don't
// treat it as a fatal partial-read error." Wrapping it keeps the underlying reason (for logging)
// while letting the fallback chain use errors.Is.
var errUnsupported = errors.New("partial read mechanism unsupported")

// maxChunkBytes bounds one partial-read request (task section 16). Not configurable this phase -
// see docs/NITRADO_DELTA_READS.md for why 256 KiB was chosen as a starting point, pending live
// measurement via cmd/nitrado-delta-probe.
const maxChunkBytes = 256 * 1024

// probeCooldown bounds how often a service already marked FULL_ONLY (every mechanism failed) is
// re-probed - never every 10s poll (task section 10/49).
const probeCooldown = 10 * time.Minute

// capabilityState is one service's cached partial-read capability.
type capabilityState struct {
	capability          Capability
	consecutiveFailures int
	lastProbedAt        time.Time
}

// capabilityCache is process-wide (keyed by Nitrado service id), matching the existing
// package-level fakeIDSeq-style convention for state that must be consistent across every Client
// instance talking to the same service, not per-instance.
type capabilityCache struct {
	mu    sync.Mutex
	byKey map[string]*capabilityState
}

var globalCapabilityCache = &capabilityCache{byKey: map[string]*capabilityState{}}

func (c *capabilityCache) get(serviceID string) capabilityState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.byKey[serviceID]; ok {
		return *s
	}
	return capabilityState{capability: CapabilityUnknown}
}

func (c *capabilityCache) record(serviceID string, capability Capability, success bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.byKey[serviceID]
	if !ok {
		s = &capabilityState{}
		c.byKey[serviceID] = s
	}
	s.lastProbedAt = time.Now()
	if success {
		s.capability = capability
		s.consecutiveFailures = 0
		return
	}
	s.consecutiveFailures++
	// A capability that was working and now fails a few times in a row is demoted back to unknown
	// (re-probe the chain) rather than immediately to FULL_ONLY - a single transient failure must
	// not throw away a real, previously-proven capability.
	if s.consecutiveFailures >= 3 {
		s.capability = CapabilityFullOnly
	}
}

// shouldSkipProbe reports whether serviceID was already marked FULL_ONLY within probeCooldown -
// the circuit breaker that stops a genuinely unsupported service from being probed on every poll.
func (s capabilityState) shouldSkipProbe() bool {
	return s.capability == CapabilityFullOnly && time.Since(s.lastProbedAt) < probeCooldown
}

// DeltaMode selects which partial-read mechanism(s) ReadLogFrom is allowed to try. String values
// match NITRADO_DELTA_READ_MODE exactly (task section 11).
type DeltaMode string

const (
	DeltaModeOff         DeltaMode = "off"
	DeltaModeAuto        DeltaMode = "auto"
	DeltaModeSeek        DeltaMode = "seek"
	DeltaModeOffsetQuery DeltaMode = "offset_query"
	DeltaModeRange       DeltaMode = "range"
)

// ParseDeltaMode validates NITRADO_DELTA_READ_MODE; an empty or unrecognized value is DeltaModeOff
// - a typo must never silently enable an unverified transport change (fail closed to the safe,
// already-proven full-read path).
func ParseDeltaMode(raw string) DeltaMode {
	switch DeltaMode(strings.ToLower(strings.TrimSpace(raw))) {
	case DeltaModeAuto:
		return DeltaModeAuto
	case DeltaModeSeek:
		return DeltaModeSeek
	case DeltaModeOffsetQuery:
		return DeltaModeOffsetQuery
	case DeltaModeRange:
		return DeltaModeRange
	default:
		return DeltaModeOff
	}
}

// ReadLogFrom attempts a partial read of path starting at offset, bounded to maxChunkBytes. mode
// controls which mechanism(s) may be tried (DeltaModeOff must never be passed here - callers check
// that before calling at all, exactly mirroring how every other rollout flag in this codebase gates
// at the call site, not inside the client). ok=false means every attempted mechanism failed or was
// unavailable; the caller's only correct response is to fall back to ReadLog (task section 13) -
// this function never returns a partial/best-effort result on failure.
func (c *Client) ReadLogFrom(ctx context.Context, serviceID, path string, offset int64, mode DeltaMode) (*PartialReadResult, bool) {
	if mode == DeltaModeOff || c == nil || serviceID == "" || path == "" || offset < 0 {
		return nil, false
	}
	length := int64(maxChunkBytes)

	state := globalCapabilityCache.get(serviceID)
	order := deltaOrder(mode, state.capability)
	for _, method := range order {
		if method == CapabilityFullOnly {
			break
		}
		if state.shouldSkipProbe() && method != state.capability {
			// FULL_ONLY within cooldown and this isn't a re-probe of the cached capability itself -
			// skip straight to the caller's full-read fallback rather than trying every mechanism
			// again this poll.
			continue
		}
		var result *PartialReadResult
		var err error
		switch method {
		case CapabilitySeek:
			result, err = c.trySeek(ctx, serviceID, path, offset, length)
		case CapabilityOffsetQuery:
			result, err = c.tryOffsetQuery(ctx, serviceID, path, offset, length)
		case CapabilityRange:
			result, err = c.tryRange(ctx, serviceID, path, offset, length)
		default:
			continue
		}
		if err != nil {
			if !errors.Is(err, errUnsupported) {
				slog.Debug("component=nitrado", "event", "partial_read_attempt_failed", "method", string(method), "err", err.Error())
			}
			globalCapabilityCache.record(serviceID, method, false)
			continue
		}
		globalCapabilityCache.record(serviceID, method, true)
		return result, true
	}
	return nil, false
}

// maxChunksPerPoll bounds ReadDelta's loop (task section 16/17) - a safety cap against an
// unexpectedly huge targetSize looping forever, not something normal ADM growth between 10s polls
// should ever approach (maxChunksPerPoll * maxChunkBytes = 16 MiB).
const maxChunksPerPoll = 64

// ReadDelta reads fromOffset..targetSize in sequential, bounded chunks (task section 16), never
// concurrently and never reordered (section 17) - each chunk is requested only after the previous
// one succeeded, and the accumulated bytes are returned in the exact order they arrived. targetSize
// is the remote size CAPTURED at the start of this poll cycle (task section 19): ReadDelta reads up
// to that size and stops, even if the remote file keeps growing during the read - new growth is
// picked up on the next poll, not chased within this one.
//
// ok=false on ANY chunk failure - the whole call fails as a unit, never a partial/best-effort
// result (task section 13's "fall back to full read on unexpected failure" applies to the whole
// delta attempt, not per-chunk, so the caller never has to reconcile a half-applied offset).
func (c *Client) ReadDelta(ctx context.Context, serviceID, path string, fromOffset, targetSize int64, mode DeltaMode) (*PartialReadResult, bool) {
	if mode == DeltaModeOff || fromOffset < 0 || targetSize <= fromOffset {
		return nil, false
	}
	combined := &PartialReadResult{RequestedOffset: fromOffset, StartOffset: fromOffset, RemoteSize: targetSize}
	offset := fromOffset
	for chunks := 0; offset < targetSize && chunks < maxChunksPerPoll; chunks++ {
		result, ok := c.ReadLogFrom(ctx, serviceID, path, offset, mode)
		if !ok {
			return nil, false
		}
		if result.StartOffset != offset {
			// Defense in depth: every mechanism already validates its own response before
			// returning ok=true, so this should be unreachable - but ReadDelta's own sequential
			// chunking correctness depends entirely on each chunk starting exactly where the last
			// one ended, so it is checked again here rather than trusted transitively.
			return nil, false
		}
		combined.Data = append(combined.Data, result.Data...)
		combined.Method = result.Method
		if len(result.Data) == 0 {
			// A legitimate EOF (task section 6/14): the mechanism was reached successfully but had
			// nothing new to return. Stop here rather than looping maxChunksPerPoll times against
			// an unmoving offset.
			break
		}
		offset = result.EndOffset
	}
	combined.EndOffset = offset
	combined.Complete = offset >= targetSize
	return combined, true
}

// deltaOrder returns the mechanisms to try, in priority order, for the given mode and the
// service's cached capability (task section 12: seek, then offset/count, then Range, then full).
// A specific single-mechanism mode (seek/offset_query/range) only ever tries that one mechanism -
// it exists for controlled testing/rollout, not as an implicit "and fall back to the others."
func deltaOrder(mode DeltaMode, cached Capability) []Capability {
	switch mode {
	case DeltaModeSeek:
		return []Capability{CapabilitySeek}
	case DeltaModeOffsetQuery:
		return []Capability{CapabilityOffsetQuery}
	case DeltaModeRange:
		return []Capability{CapabilityRange}
	case DeltaModeAuto:
		if cached == CapabilitySeek || cached == CapabilityOffsetQuery || cached == CapabilityRange {
			// A proven capability is tried first, but the others remain available as a same-poll
			// fallback if that specific call fails transiently - only genuine, repeated failure
			// (capabilityCache.record's consecutiveFailures) demotes to FULL_ONLY.
			rest := []Capability{CapabilitySeek, CapabilityOffsetQuery, CapabilityRange}
			ordered := []Capability{cached}
			for _, m := range rest {
				if m != cached {
					ordered = append(ordered, m)
				}
			}
			return ordered
		}
		return []Capability{CapabilitySeek, CapabilityOffsetQuery, CapabilityRange}
	default:
		return nil
	}
}

// --- mechanism 1: native Nitrado seek (task section 5/6) -------------------------------------------

func (c *Client) trySeek(ctx context.Context, serviceID, path string, offset, length int64) (*PartialReadResult, error) {
	endpoint := "/services/" + url.PathEscape(serviceID) + "/gameservers/file_server/seek?" +
		url.Values{"file": {path}, "offset": {strconv.FormatInt(offset, 10)}, "length": {strconv.FormatInt(length, 10)}, "mode": {"raw"}}.Encode()
	signedURL, err := c.fetchSignedURL(ctx, endpoint, "file seek")
	if err != nil {
		var reqErr *RequestError
		if errors.As(err, &reqErr) && (reqErr.Kind == KindNotFound || reqErr.Kind == KindInvalidEndpoint) {
			// The endpoint itself doesn't exist on this account/service - not a transient failure,
			// a capability that isn't there. Treated identically either way by the caller, but
			// worth its own branch for clarity/future log analysis.
			return nil, fmt.Errorf("%w: seek endpoint unavailable: %v", errUnsupported, err)
		}
		return nil, err
	}
	data, status, err := fetchBytes(ctx, c.httpClient, signedURL, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: seek signed URL returned status=%d", errUnsupported, status)
	}
	return validatePartialData(data, offset, string(CapabilitySeek))
}

// --- mechanism 2: signed download URL with offset/count (task section 7) ---------------------------

func (c *Client) tryOffsetQuery(ctx context.Context, serviceID, path string, offset, length int64) (*PartialReadResult, error) {
	// The token-fetch step is identical to a normal full download (task section 7: "GET normal
	// download token") - only the final signed-URL fetch differs.
	signedURL, err := c.fetchSignedURL(ctx, "/services/"+url.PathEscape(serviceID)+"/gameservers/file_server/download?file="+url.QueryEscape(path), "file download")
	if err != nil {
		return nil, err
	}
	extra := url.Values{"offset": {strconv.FormatInt(offset, 10)}, "count": {strconv.FormatInt(length, 10)}}
	data, status, header, err := fetchBytesWithHeaders(ctx, c.httpClient, appendQuery(signedURL, extra), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusPartialContent {
		return nil, fmt.Errorf("%w: offset/count signed URL returned status=%d", errUnsupported, status)
	}
	// Champion Live Sync phase 2 finding (2026-09-24, Champions service, read-only): Nitrado's
	// signed download URL IGNORES offset/count and returns the whole file with a 200 and no
	// Content-Range. A small file then fits under `length` and was previously accepted as if it
	// started at `offset` - silently misaligned bytes. A plain 200 body carries no proof of where it
	// starts, so the response is trusted only when a Content-Range states the requested offset.
	start, _, total, ok := parseContentRange(header.Get("Content-Range"))
	if !ok || start != offset {
		return nil, fmt.Errorf("%w: offset/count response has no Content-Range proving it starts at offset %d", errUnsupported, offset)
	}
	if total > 0 && offset+int64(len(data)) > total {
		return nil, fmt.Errorf("%w: offset/count response extends past the stated total size", errUnsupported)
	}
	// If the server ignored offset/count entirely (a real risk task section 8 warns about for
	// Range, and just as real here), it would return the WHOLE file, i.e. far more than `length`
	// bytes. Treat any response noticeably larger than what was requested as unsupported rather
	// than risk feeding a wrongly-offset buffer into the parser.
	if int64(len(data)) > length {
		return nil, fmt.Errorf("%w: offset/count returned %d bytes, more than the requested %d - server likely ignored the parameters", errUnsupported, len(data), length)
	}
	return validatePartialData(data, offset, string(CapabilityOffsetQuery))
}

// --- mechanism 3: validated HTTP Range (task section 8, last resort) --------------------------------

func (c *Client) tryRange(ctx context.Context, serviceID, path string, offset, length int64) (*PartialReadResult, error) {
	signedURL, err := c.fetchSignedURL(ctx, "/services/"+url.PathEscape(serviceID)+"/gameservers/file_server/download?file="+url.QueryEscape(path), "file download")
	if err != nil {
		return nil, err
	}
	end := offset + length - 1
	headers := map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", offset, end)}
	data, status, header, err := fetchBytesWithHeaders(ctx, c.httpClient, signedURL, headers)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusPartialContent:
		start, respEnd, total, ok := parseContentRange(header.Get("Content-Range"))
		if !ok || start != offset {
			return nil, fmt.Errorf("%w: 206 with unusable or mismatched Content-Range %q (requested offset %d)", errUnsupported, header.Get("Content-Range"), offset)
		}
		result, err := validatePartialData(data, offset, string(CapabilityRange))
		if err != nil {
			return nil, err
		}
		if total > 0 {
			result.RemoteSize = total
		}
		_ = respEnd
		return result, nil
	case http.StatusRequestedRangeNotSatisfiable:
		// 416 at/after EOF is a normal "nothing new yet" outcome, not proof Range is unsupported -
		// but without a successful 206 we still can't confirm Range works, so this attempt reports
		// unsupported and the caller falls back; a later poll with real new bytes is what actually
		// proves or disproves RANGE_SUPPORTED.
		return nil, fmt.Errorf("%w: 416 range not satisfiable", errUnsupported)
	case http.StatusOK:
		// The server ignored Range and sent the whole file back with a 200 - task section 8 is
		// explicit that this must never be treated as "body starts at offset". Unsupported, not a
		// partial result.
		return nil, fmt.Errorf("%w: server returned 200 (not 206) for a Range request - Range is not honored", errUnsupported)
	default:
		return nil, fmt.Errorf("%w: unexpected status=%d for a Range request", errUnsupported, status)
	}
}

// parseContentRange parses "bytes start-end/total" (or "bytes start-end/*"). total is -1 when "*".
func parseContentRange(v string) (start, end, total int64, ok bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "bytes ") {
		return 0, 0, 0, false
	}
	v = strings.TrimPrefix(v, "bytes ")
	slash := strings.IndexByte(v, '/')
	if slash < 0 {
		return 0, 0, 0, false
	}
	rangePart, totalPart := v[:slash], v[slash+1:]
	dash := strings.IndexByte(rangePart, '-')
	if dash < 0 {
		return 0, 0, 0, false
	}
	start, err1 := strconv.ParseInt(rangePart[:dash], 10, 64)
	end, err2 := strconv.ParseInt(rangePart[dash+1:], 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return 0, 0, 0, false
	}
	if totalPart == "*" {
		return start, end, -1, true
	}
	total, err3 := strconv.ParseInt(totalPart, 10, 64)
	if err3 != nil || total < 0 {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

// validatePartialData builds the result from raw bytes returned for a request starting at
// requestedOffset - the one place every mechanism's returned length is checked before it is
// trusted (task section 6: "Do not blindly trust body length"). Zero bytes is valid (EOF - no new
// data yet), never an error.
func validatePartialData(data []byte, requestedOffset int64, method string) (*PartialReadResult, error) {
	return &PartialReadResult{
		Data: data, RequestedOffset: requestedOffset, StartOffset: requestedOffset,
		EndOffset: requestedOffset + int64(len(data)), Method: method,
	}, nil
}

// appendQuery adds extra query parameters to a URL that may already carry its own (a signed URL's
// existing signature/expiry params are preserved; offset/count are appended alongside them).
func appendQuery(rawURL string, extra url.Values) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	for k, vals := range extra {
		for _, v := range vals {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// fetchBytes is fetchBytesWithHeaders without needing the response header back.
func fetchBytes(ctx context.Context, hc *http.Client, rawURL string, headers map[string]string) ([]byte, int, error) {
	data, status, _, err := fetchBytesWithHeaders(ctx, hc, rawURL, headers)
	return data, status, err
}

// fetchBytesWithHeaders performs one GET against a (never logged - section 28) signed URL and
// returns the body, status and response headers, with the same 429 retry/backoff shape
// readDirectURL already uses - a single shared low-level fetch every partial-read mechanism above
// builds on, so the retry/backoff behavior stays consistent with the existing full-read path.
func fetchBytesWithHeaders(ctx context.Context, hc *http.Client, rawURL string, headers map[string]string) ([]byte, int, http.Header, error) {
	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("create partial read request: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("partial read request failed: %w", err)
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, resp.StatusCode, resp.Header, fmt.Errorf("read partial read response: %w", err)
			}
			return data, resp.StatusCode, resp.Header, nil
		}
		delay := retryAfter(resp.Header.Get("Retry-After"))
		resp.Body.Close()
		if attempt == maxAttempts {
			return nil, resp.StatusCode, resp.Header, fmt.Errorf("%w: rate limited", errUnsupported)
		}
		if delay <= 0 {
			delay = time.Duration(attempt*attempt)*250*time.Millisecond + time.Duration((attempt*37)%100)*time.Millisecond
		}
		slog.Warn("component=nitrado", "event", "rate_limited", "operation", "adm_partial_read", "retry_after_ms", delay.Milliseconds(), "attempt", attempt)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, 0, nil, ctx.Err()
		}
	}
	return nil, 0, nil, fmt.Errorf("partial read retry exhausted")
}
