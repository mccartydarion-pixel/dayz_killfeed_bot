package nitrado

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRateHeaders(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "15000")
	h.Set("X-RateLimit-Remaining", "1200")
	h.Set("X-RateLimit-Reset", "1790003600")
	b, ok := parseRateHeaders(h, now)
	if !ok || b.Limit != 15000 || b.Remaining != 1200 || !b.Reset.Equal(time.Unix(1790003600, 0)) {
		t.Fatalf("unix reset: %+v %v", b, ok)
	}
	if !b.Low(now) || b.Healthy(now) {
		t.Fatal("8% left is low and not healthy")
	}
	if b.Low(time.Unix(1790003601, 0)) || !b.Healthy(time.Unix(1790003601, 0)) {
		t.Fatal("after the reset time the old reading no longer counts as low")
	}
	h.Set("X-RateLimit-Reset", "120") // seconds until reset
	if b, _ := parseRateHeaders(h, now); !b.Reset.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("relative reset: %v", b.Reset)
	}
	if _, ok := parseRateHeaders(http.Header{}, now); ok {
		t.Fatal("no headers, no budget")
	}
}

func TestOperationLabelDropsIDsAndQuery(t *testing.T) {
	for in, want := range map[string]string{
		"/services/12345/gameservers/file_server/list?dir=/a/b": "gameservers/file_server/list",
		"/services":               "root",
		"/services/7/gameservers": "gameservers",
		"/services/7/gameservers/file_server/seek?x": "gameservers/file_server/seek",
	} {
		if got := operationLabel(in); got != want {
			t.Errorf("%s: %q want %q", in, got, want)
		}
	}
}

func TestClientRecordsBudgetAndSharesListingsWithinATick(t *testing.T) {
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "1000")
		w.Header().Set("X-RateLimit-Remaining", "900")
		if strings.Contains(r.URL.Path, "file_server/list") {
			lists.Add(1)
			_, _ = w.Write([]byte(`{"status":"success","data":{"entries":[{"type":"file","name":"a.ADM","path":"/d/a.ADM","size":10,"modified_at":1}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "secret-token-xyz", nil)
	ctx := context.Background()

	for i := 0; i < 3; i++ { // boot scan, newer-log check and stat in the same tick
		if _, err := c.StatFile(ctx, "77", "/d/a.ADM"); err != nil {
			t.Fatal(err)
		}
	}
	if n := lists.Load(); n != 1 {
		t.Fatalf("expected one list request within the cache window, got %d", n)
	}
	time.Sleep(listCacheTTL + 50*time.Millisecond)
	_, _ = c.StatFile(ctx, "77", "/d/a.ADM")
	if n := lists.Load(); n != 2 {
		t.Fatalf("a later tick must list again, got %d", n)
	}

	b := c.RateBudget()
	if !b.Known || b.Limit != 1000 || b.Remaining != 900 || !b.Healthy(time.Now()) {
		t.Fatalf("budget: %+v", b)
	}
	var found bool
	for _, u := range RateLimitUsage() {
		if u.Token == TokenKey("secret-token-xyz") {
			found = true
			if u.RequestsLastHr != 2 || u.ByOperation["gameservers/file_server/list"] != 2 {
				t.Fatalf("usage: %+v", u)
			}
			if strings.Contains(u.Token, "secret") {
				t.Fatal("token must never appear in usage")
			}
		}
	}
	if !found {
		t.Fatal("token not tracked")
	}
}
