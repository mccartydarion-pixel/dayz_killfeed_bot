package nitrado

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignedDownloadRetries429ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ADM line\n"))
	}))
	defer server.Close()

	client := NewClient(server.URL, "token", server.Client())
	content, err := client.ReadLog(context.Background(), "service", server.URL+"/signed")
	if err != nil || string(content) != "ADM line\n" {
		t.Fatalf("expected signed download recovery, content=%q err=%v", content, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected two signed requests, got %d", calls.Load())
	}
}

func TestSignedDownloadPersistent429IsBounded(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := NewClient(server.URL, "token", server.Client())
	_, err := client.ReadLog(context.Background(), "service", server.URL+"/signed")
	if err == nil {
		t.Fatal("expected persistent 429 failure")
	}
	if calls.Load() != 3 {
		t.Fatalf("expected three bounded attempts, got %d", calls.Load())
	}
}

func TestSignedDownloadRetryHonorsContextCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	client := NewClient(server.URL, "token", server.Client())
	_, err := client.ReadLog(ctx, "service", server.URL+"/signed")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected retry to stop during backoff, got %d calls", calls.Load())
	}
}
