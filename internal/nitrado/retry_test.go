package nitrado

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRetriesRateLimitedRequestWithBoundedRecovery(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := NewClient(server.URL, "token-not-logged", server.Client())
	if err := client.AuthenticationCheck(context.Background()); err != nil {
		t.Fatalf("expected rate-limited request to recover: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected exactly one bounded retry, got %d calls", calls.Load())
	}
}

func TestRetryAfterClampsLongWaits(t *testing.T) {
	cases := map[string]time.Duration{
		"":      0,
		"abc":   0,
		"-5":    0,
		"0.5":   500 * time.Millisecond,
		"30":    30 * time.Second,
		"60":    60 * time.Second,
		"61":    60 * time.Second,
		"3600":  60 * time.Second,
		"1e9":   60 * time.Second,
		" 120 ": 60 * time.Second,
	}
	for header, want := range cases {
		if got := retryAfter(header); got != want {
			t.Fatalf("retryAfter(%q) = %s, want %s", header, got, want)
		}
	}
}
