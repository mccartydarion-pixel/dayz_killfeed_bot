package killfeed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/nitrado/nitradofixture"
)

// What NITRADO_DELTA_READ_MODE does when the server does not honour a partial read
// (docs/PERFORMANCE.md section 21). The production engine and the real Nitrado client run against
// the staging Nitrado fixture, whose download URL ignores offset/count exactly like Nitrado's. The
// wrappers below add the behaviours the fixture does not have on its own: a download host that
// also ignores Range (what production showed), and a seek endpoint that ignores or honours its
// offset.

// ignoreRange makes the download host answer a Range request with the whole file and 200.
func ignoreRange(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_files" {
			r.Header.Del("Range")
		}
		next.ServeHTTP(w, r)
	})
}

// withSeek adds file_server/seek to the fixture. honest=true serves exactly offset..offset+length;
// honest=false hands back the ordinary whole-file download, ignoring offset and length.
func withSeek(next http.Handler, honest bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/gameservers/file_server/seek"):
			q := r.URL.Query()
			signed := "http://" + r.Host + "/_files?" + url.Values{"file": {q.Get("file")}}.Encode()
			if honest {
				signed = "http://" + r.Host + "/_seek?" + url.Values{"file": {q.Get("file")}, "offset": {q.Get("offset")}, "length": {q.Get("length")}}.Encode()
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"token": map[string]any{"url": signed}}})
		case r.URL.Path == "/_seek":
			q := r.URL.Query()
			offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)
			length, _ := strconv.ParseInt(q.Get("length"), 10, 64)
			rec := httptest.NewRecorder()
			whole := httptest.NewRequest(http.MethodGet, "/_files?"+url.Values{"file": {q.Get("file")}}.Encode(), nil)
			next.ServeHTTP(rec, whole)
			body := rec.Body.Bytes()
			if offset+length > int64(len(body)) { // production: a request past the end of the file fails
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(body[offset : offset+length])
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func TestDeltaModesAgainstNitradoFixture(t *testing.T) {
	cases := []struct {
		name string
		mode nitrado.DeltaMode
		wrap func(http.Handler) http.Handler
		// partial: the mode really reads only the new bytes on this server. Otherwise every partial
		// answer must be refused and the whole file downloaded instead.
		partial bool
	}{
		{"offset_query: the download URL ignores offset/count (the fixture as is, and Nitrado)", nitrado.DeltaModeOffsetQuery, nil, false},
		{"range: the download host ignores Range and answers 200 (Nitrado)", nitrado.DeltaModeRange, ignoreRange, false},
		{"range: a host that honours Range with 206 + Content-Range (the fixture as is)", nitrado.DeltaModeRange, nil, true},
		{"seek: no seek endpoint (the fixture as is)", nitrado.DeltaModeSeek, nil, false},
		{"seek: a seek endpoint that ignores its offset", nitrado.DeltaModeSeek, func(h http.Handler) http.Handler { return withSeek(h, false) }, false},
		{"seek: a seek endpoint that honours offset and length (Nitrado)", nitrado.DeltaModeSeek, func(h http.Handler) http.Handler { return withSeek(h, true) }, true},
		{"auto: nothing honoured anywhere", nitrado.DeltaModeAuto, func(h http.Handler) http.Handler { return withSeek(ignoreRange(h), false) }, false},
		{"auto: only seek honoured (Nitrado)", nitrado.DeltaModeAuto, func(h http.Handler) http.Handler { return withSeek(ignoreRange(h), true) }, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			serviceID := int64(90000200 + i) // partial-read capability is remembered per service
			fx := nitradofixture.New(serviceID, "")
			var handler http.Handler = fx
			if tc.wrap != nil {
				handler = tc.wrap(fx)
			}
			srv := httptest.NewServer(handler)
			defer srv.Close()
			svc := strconv.FormatInt(serviceID, 10)

			store := newFakePersistenceStore()
			e := NewEngine(nitrado.NewClient(srv.URL, "fixture-token-not-a-credential", nil), svc, NewADMParser())
			e.deltaMode = tc.mode
			pub := &fixturePublisher{}
			e.SetKillPublisher(pub)
			pq := NewPersistenceQueue(store, 1, "delta-fixture")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go pq.Run(ctx)
			e.SetPersistence(pq)
			pollUntil := func(what string, kills int) {
				t.Helper()
				deadline := time.Now().Add(8 * time.Second)
				for {
					// A fresh client per poll: the client reuses a listing for 750 ms, far longer
					// than the gap between two polls here. What a service's partial reads can do
					// is remembered per service, not per client.
					e.client = nitrado.NewClient(srv.URL, "fixture-token-not-a-credential", nil)
					if err := e.PollOnce(ctx); err != nil {
						t.Fatal(err)
					}
					got, _ := pub.counts()
					if got == kills && e.tracker.LastByteOffset == int64(fx.Snapshot().ADMBytes) {
						return
					}
					if got > kills || time.Now().After(deadline) {
						t.Fatalf("%s: %d kills published (want %d), offset %d of %d bytes", what, got, kills, e.tracker.LastByteOffset, fx.Snapshot().ADMBytes)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			pollUntil("first read", 0)

			var victims []string
			for round := 1; round <= 4; round++ { // four rounds: past the three failures that pause a method
				victims = append(victims, fx.AddKills(3)...)
				pollUntil("round "+strconv.Itoa(round), len(victims))
			}

			pub.mu.Lock()
			for j, ev := range pub.kills {
				if ev.Victim == nil || ev.Victim.Name != victims[j] {
					pub.mu.Unlock()
					t.Fatalf("kill %d is %+v, want %s: order or content differs from the log", j, ev.Victim, victims[j])
				}
			}
			pub.mu.Unlock()
			if len(store.kills) != len(victims) {
				t.Fatalf("stored %d kills, want %d", len(store.kills), len(victims))
			}
			// The checkpoint is exactly the end of the file: bytes from an ignored offset would have
			// pushed it past the end (and re-parsed the file's start as if it were new).
			if got, want := e.tracker.LastByteOffset, int64(fx.Snapshot().ADMBytes); got != want {
				t.Fatalf("checkpoint %d, want the file's size %d", got, want)
			}
			stats := e.DeltaStats()
			if tc.partial && stats.BytesReceived == 0 {
				t.Fatalf("expected real partial reads on this server, got none (%+v)", stats)
			}
			if !tc.partial && stats.BytesReceived != 0 {
				t.Fatalf("%d bytes from a partial read were accepted from a server that does not honour it", stats.BytesReceived)
			}
			if got := e.Metrics().DuplicateEventsDropped; got != 0 {
				t.Fatalf("%d lines were read twice", got)
			}
		})
	}
}
