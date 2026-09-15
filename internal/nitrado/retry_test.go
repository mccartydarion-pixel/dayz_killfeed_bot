package nitrado

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
