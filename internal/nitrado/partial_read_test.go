package nitrado

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// resetCapabilityCache clears the process-wide cache between tests, since it is deliberately
// process-wide (shared across every *Client talking to the same service id - see partial_read.go).
func resetCapabilityCache(t *testing.T) {
	t.Helper()
	globalCapabilityCache.mu.Lock()
	globalCapabilityCache.byKey = map[string]*capabilityState{}
	globalCapabilityCache.mu.Unlock()
}

// --- mechanism 1: seek (task section 32) ------------------------------------------------------------

func TestReadLogFromSeekSuccess(t *testing.T) {
	resetCapabilityCache(t)
	const full = "AAAAAAAAAA" + "BBBBBBBBBB" // 20 bytes total
	var sawSeekCall bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "file_server/seek"):
			sawSeekCall = true
			if r.URL.Query().Get("offset") != "10" {
				t.Errorf("expected offset=10, got %q", r.URL.Query().Get("offset"))
			}
			if r.URL.Query().Get("mode") != "raw" {
				t.Errorf("expected mode=raw, got %q", r.URL.Query().Get("mode"))
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"token":{"url":"%s/signed-seek"}}}`, "http://"+r.Host)
		case r.URL.Path == "/signed-seek":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(full[10:])) // "BBBBBBBBBB"
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	result, ok := client.ReadLogFrom(context.Background(), "svc1", "/profile/x.ADM", 10, DeltaModeSeek)
	if !ok {
		t.Fatal("expected ReadLogFrom to succeed")
	}
	if !sawSeekCall {
		t.Fatal("expected the seek endpoint to be called")
	}
	if string(result.Data) != "BBBBBBBBBB" {
		t.Fatalf("got %q", result.Data)
	}
	if result.StartOffset != 10 || result.EndOffset != 20 {
		t.Fatalf("got StartOffset=%d EndOffset=%d", result.StartOffset, result.EndOffset)
	}
	if result.Method != string(CapabilitySeek) {
		t.Fatalf("got method %q", result.Method)
	}
}

func TestReadLogFromSeekUnavailableEndpointFallsThroughInAuto(t *testing.T) {
	resetCapabilityCache(t)
	var offsetQueryCalled bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "file_server/seek"):
			w.WriteHeader(http.StatusNotFound) // this account/service has no seek endpoint
		case strings.Contains(r.URL.Path, "file_server/download"):
			offsetQueryCalled = true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"token":{"url":"http://%s/signed"}}}`, r.Host)
		case r.URL.Path == "/signed":
			if r.URL.Query().Get("offset") != "5" || r.URL.Query().Get("count") == "" {
				t.Errorf("expected offset/count query params, got %q", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("new-bytes"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	result, ok := client.ReadLogFrom(context.Background(), "svc2", "/profile/x.ADM", 5, DeltaModeAuto)
	if !ok {
		t.Fatal("expected auto mode to fall through to offset/count and succeed")
	}
	if !offsetQueryCalled {
		t.Fatal("expected the offset/count mechanism to be tried after seek failed")
	}
	if string(result.Data) != "new-bytes" {
		t.Fatalf("got %q", result.Data)
	}
	if result.Method != string(CapabilityOffsetQuery) {
		t.Fatalf("got method %q", result.Method)
	}
}

// --- mechanism 2: offset/count (task section 33) ----------------------------------------------------

func TestReadLogFromOffsetQueryRejectsFullFileResponse(t *testing.T) {
	resetCapabilityCache(t)
	full := strings.Repeat("X", 500*1024) // far larger than one maxChunkBytes-bounded request

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "file_server/download"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"token":{"url":"http://%s/signed"}}}`, r.Host)
		case r.URL.Path == "/signed":
			// Server ignores offset/count and returns the whole file - must be detected, not trusted.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(full))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	_, err := client.tryOffsetQuery(context.Background(), "svc3", "/profile/x.ADM", 100, 1024)
	if err == nil {
		t.Fatal("expected an error when the server ignores offset/count and returns the whole file")
	}
}

// --- mechanism 3: HTTP Range (task section 34) --------------------------------------------------------

func TestReadLogFromRangeCases(t *testing.T) {
	cases := []struct {
		name     string
		respond  func(w http.ResponseWriter, r *http.Request)
		wantErr  bool
		wantData string
	}{
		{
			name: "206 correct Content-Range",
			respond: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", "bytes 100-109/200")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("0123456789"))
			},
			wantErr:  false,
			wantData: "0123456789",
		},
		{
			name: "200 ignores Range",
			respond: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("whole-file-contents"))
			},
			wantErr: true,
		},
		{
			name: "416 Range Not Satisfiable",
			respond: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			},
			wantErr: true,
		},
		{
			name: "malformed Content-Range",
			respond: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", "not-a-valid-range")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("data"))
			},
			wantErr: true,
		},
		{
			name: "Content-Range start does not match requested offset",
			respond: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", "bytes 0-9/200") // server ignored the requested start
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("0123456789"))
			},
			wantErr: true,
		},
		{
			name: "short body at EOF is still a valid 206",
			respond: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", "bytes 100-102/103")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("abc")) // fewer bytes than requested - legitimate EOF
			},
			wantErr:  false,
			wantData: "abc",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "file_server/download"):
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"data":{"token":{"url":"http://%s/signed"}}}`, r.Host)
				case r.URL.Path == "/signed":
					if r.Header.Get("Range") == "" {
						t.Errorf("expected a Range header to be sent")
					}
					c.respond(w, r)
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
			}))
			defer srv.Close()

			client := NewClient(srv.URL, "token", srv.Client())
			result, err := client.tryRange(context.Background(), "svc-range", "/profile/x.ADM", 100, 10)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got result=%+v", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(result.Data) != c.wantData {
				t.Fatalf("got %q, want %q", result.Data, c.wantData)
			}
		})
	}
}

func TestReadLogFromRangeNetworkFailure(t *testing.T) {
	resetCapabilityCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"token":{"url":"http://127.0.0.1:1/unreachable"}}}`) // nothing listens here
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	_, err := client.tryRange(context.Background(), "svc-fail", "/profile/x.ADM", 0, 10)
	if err == nil {
		t.Fatal("expected a network failure error")
	}
}

// --- fallback chain (task section 35) -----------------------------------------------------------------

func TestReadLogFromFallbackChainAllThreeAttempted(t *testing.T) {
	resetCapabilityCache(t)
	var seekCalls, downloadCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "file_server/seek"):
			atomic.AddInt32(&seekCalls, 1)
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "file_server/download"):
			atomic.AddInt32(&downloadCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"token":{"url":"http://%s/signed"}}}`, r.Host)
		case r.URL.Path == "/signed":
			// Neither offset/count nor Range work either - both return the full file untouched.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strings.Repeat("Z", 2*maxChunkBytes)))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	_, ok := client.ReadLogFrom(context.Background(), "svc-allfail", "/profile/x.ADM", 0, DeltaModeAuto)
	if ok {
		t.Fatal("expected every mechanism to fail and ReadLogFrom to report ok=false, requiring the caller to fall back to ReadLog")
	}
	if seekCalls < 1 {
		t.Error("expected seek to have been attempted")
	}
	if downloadCalls < 1 {
		t.Error("expected the offset/count and/or Range mechanisms (both use the download token) to have been attempted")
	}
}

func TestReadLogFromModeOffNeverCallsNetwork(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	_, ok := client.ReadLogFrom(context.Background(), "svc-off", "/profile/x.ADM", 0, DeltaModeOff)
	if ok {
		t.Fatal("DeltaModeOff must never succeed")
	}
	if called {
		t.Fatal("DeltaModeOff must never make a network call")
	}
}

// --- capability caching (task section 36) ---------------------------------------------------------

func TestCapabilityCachingSkipsOtherMechanismsOnceSeekProven(t *testing.T) {
	resetCapabilityCache(t)
	var seekCalls, downloadCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "file_server/seek"):
			atomic.AddInt32(&seekCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"token":{"url":"http://%s/signed"}}}`, r.Host)
		case strings.Contains(r.URL.Path, "file_server/download"):
			atomic.AddInt32(&downloadCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"token":{"url":"http://%s/signed"}}}`, r.Host)
		case r.URL.Path == "/signed":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	for i := 0; i < 5; i++ {
		if _, ok := client.ReadLogFrom(context.Background(), "svc-cached", "/profile/x.ADM", int64(i), DeltaModeAuto); !ok {
			t.Fatalf("call %d: expected success", i)
		}
	}
	if seekCalls != 5 {
		t.Fatalf("expected seek to be tried on every call (it's the cached, working capability), got %d", seekCalls)
	}
	if downloadCalls != 0 {
		t.Fatalf("expected offset/count and Range to never be tried once seek is proven, got %d download calls", downloadCalls)
	}
}

func TestCapabilityCircuitBreakerStopsProbingAfterRepeatedFailure(t *testing.T) {
	resetCapabilityCache(t)
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	for i := 0; i < 5; i++ {
		client.ReadLogFrom(context.Background(), "svc-breaker", "/profile/x.ADM", 0, DeltaModeAuto)
	}
	afterFiveFailures := atomic.LoadInt32(&calls)

	// One more call: with the circuit breaker engaged (3 consecutive failures -> FULL_ONLY, cooldown
	// not yet elapsed), this must not make three MORE network calls (seek+download+download for
	// offset/count and range) the way an unbounded retry-every-poll would.
	client.ReadLogFrom(context.Background(), "svc-breaker", "/profile/x.ADM", 0, DeltaModeAuto)
	afterSixthCall := atomic.LoadInt32(&calls)

	if afterSixthCall-afterFiveFailures >= 3 {
		t.Fatalf("expected the circuit breaker to suppress repeated full-chain probing once FULL_ONLY, calls went from %d to %d", afterFiveFailures, afterSixthCall)
	}
}

// --- chunking (task section 16/17) ------------------------------------------------------------------

func TestReadDeltaSingleChunk(t *testing.T) {
	resetCapabilityCache(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "file_server/seek"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"token":{"url":"http://%s/signed"}}}`, r.Host)
		case r.URL.Path == "/signed":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("new-growth"))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	result, ok := client.ReadDelta(context.Background(), "svc-delta1", "/profile/x.ADM", 1000, 1010, DeltaModeSeek)
	if !ok {
		t.Fatal("expected success")
	}
	if string(result.Data) != "new-growth" {
		t.Fatalf("got %q", result.Data)
	}
	if result.StartOffset != 1000 {
		t.Fatalf("got StartOffset=%d", result.StartOffset)
	}
	if !result.Complete {
		t.Fatal("expected Complete=true (reached targetSize)")
	}
}

// TestReadDeltaMultipleChunksSequential forces growth larger than one maxChunkBytes-bounded request,
// and asserts chunks are fetched in strict sequential order (never concurrently, never reordered -
// task section 17) with each chunk's offset exactly continuing where the previous one ended.
func TestReadDeltaMultipleChunksSequential(t *testing.T) {
	resetCapabilityCache(t)
	const fromOffset = int64(0)
	targetSize := int64(maxChunkBytes)*2 + 500 // forces 3 chunks
	full := make([]byte, targetSize)
	for i := range full {
		full[i] = byte('A' + (i % 26))
	}

	var seenOffsets []int64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "file_server/seek"):
			off := r.URL.Query().Get("offset")
			var offset int64
			fmt.Sscanf(off, "%d", &offset)
			mu.Lock()
			seenOffsets = append(seenOffsets, offset)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"token":{"url":"http://%s/signed?offset=%d"}}}`, r.Host, offset)
		case r.URL.Path == "/signed":
			var offset int64
			fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
			end := offset + maxChunkBytes
			if end > int64(len(full)) {
				end = int64(len(full))
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(full[offset:end])
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	result, ok := client.ReadDelta(context.Background(), "svc-delta2", "/profile/x.ADM", fromOffset, targetSize, DeltaModeSeek)
	if !ok {
		t.Fatal("expected success")
	}
	if !bytesEqual(result.Data, full) {
		t.Fatalf("expected concatenated chunks to exactly equal the full growth region (len got=%d want=%d)", len(result.Data), len(full))
	}
	wantOffsets := []int64{0, maxChunkBytes, maxChunkBytes * 2}
	if len(seenOffsets) != len(wantOffsets) {
		t.Fatalf("expected %d sequential chunk requests, got %d: %v", len(wantOffsets), len(seenOffsets), seenOffsets)
	}
	for i, want := range wantOffsets {
		if seenOffsets[i] != want {
			t.Fatalf("chunk %d: got offset %d, want %d (chunks must be strictly sequential, never reordered)", i, seenOffsets[i], want)
		}
	}
}

func TestReadDeltaStopsAtTargetSizeEvenIfMoreDataAvailable(t *testing.T) {
	resetCapabilityCache(t)
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "file_server/seek"):
			callCount++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"token":{"url":"http://%s/signed"}}}`, r.Host)
		case r.URL.Path == "/signed":
			// The remote file kept growing mid-download (task section 19) - always has more than
			// targetSize available, but ReadDelta must still stop at the captured target.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("0123456789")) // 10 bytes, targetSize below asks for only 5
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	result, ok := client.ReadDelta(context.Background(), "svc-delta3", "/profile/x.ADM", 100, 105, DeltaModeSeek)
	if !ok {
		t.Fatal("expected success")
	}
	if result.EndOffset < 105 {
		t.Fatalf("expected to reach at least targetSize 105, got EndOffset=%d", result.EndOffset)
	}
	if callCount != 1 {
		t.Fatalf("expected exactly one chunk request (targetSize-fromOffset=5 fits in one chunk), got %d", callCount)
	}
}

func TestReadDeltaChunkFailureIsAllOrNothing(t *testing.T) {
	resetCapabilityCache(t)
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "file_server/seek"):
			callCount++
			if callCount == 1 {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"data":{"token":{"url":"http://%s/signed"}}}`, r.Host)
				return
			}
			// Second chunk fails outright.
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/signed":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(make([]byte, maxChunkBytes)) // exactly one full chunk, forcing a second request
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	_, ok := client.ReadDelta(context.Background(), "svc-delta4", "/profile/x.ADM", 0, maxChunkBytes*2, DeltaModeSeek)
	if ok {
		t.Fatal("expected the whole ReadDelta call to fail when any chunk fails - never a partial/best-effort result")
	}
}

func TestReadDeltaRejectsInvalidParams(t *testing.T) {
	resetCapabilityCache(t)
	client := NewClient("http://example.invalid", "token", nil)
	if _, ok := client.ReadDelta(context.Background(), "svc", "/x.ADM", 0, 100, DeltaModeOff); ok {
		t.Fatal("DeltaModeOff must never succeed")
	}
	if _, ok := client.ReadDelta(context.Background(), "svc", "/x.ADM", 100, 100, DeltaModeSeek); ok {
		t.Fatal("targetSize == fromOffset (no growth) must not attempt anything")
	}
	if _, ok := client.ReadDelta(context.Background(), "svc", "/x.ADM", 100, 50, DeltaModeSeek); ok {
		t.Fatal("targetSize < fromOffset must not attempt anything")
	}
	if _, ok := client.ReadDelta(context.Background(), "svc", "/x.ADM", -1, 100, DeltaModeSeek); ok {
		t.Fatal("negative fromOffset must not attempt anything")
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- ParseDeltaMode -------------------------------------------------------------------------------

func TestParseDeltaMode(t *testing.T) {
	cases := map[string]DeltaMode{
		"":             DeltaModeOff,
		"off":          DeltaModeOff,
		"OFF":          DeltaModeOff,
		"auto":         DeltaModeAuto,
		"AUTO":         DeltaModeAuto,
		"seek":         DeltaModeSeek,
		"offset_query": DeltaModeOffsetQuery,
		"range":        DeltaModeRange,
		"garbage":      DeltaModeOff,
		"  auto  ":     DeltaModeAuto,
	}
	for raw, want := range cases {
		if got := ParseDeltaMode(raw); got != want {
			t.Errorf("ParseDeltaMode(%q) = %q, want %q", raw, got, want)
		}
	}
}

// --- parseContentRange -----------------------------------------------------------------------------

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in                            string
		wantStart, wantEnd, wantTotal int64
		wantOK                        bool
	}{
		{"bytes 100-109/200", 100, 109, 200, true},
		{"bytes 0-0/1", 0, 0, 1, true},
		{"bytes 100-109/*", 100, 109, -1, true},
		{"not-bytes 1-2/3", 0, 0, 0, false},
		{"bytes 1-2", 0, 0, 0, false},
		{"bytes abc-def/3", 0, 0, 0, false},
		{"", 0, 0, 0, false},
	}
	for _, c := range cases {
		start, end, total, ok := parseContentRange(c.in)
		if ok != c.wantOK {
			t.Errorf("parseContentRange(%q) ok=%v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if start != c.wantStart || end != c.wantEnd || total != c.wantTotal {
			t.Errorf("parseContentRange(%q) = (%d,%d,%d), want (%d,%d,%d)", c.in, start, end, total, c.wantStart, c.wantEnd, c.wantTotal)
		}
	}
}
