package nitrado

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSafeReasonNeverContainsURLs(t *testing.T) {
	signed := "https://files.example.net/dl/abc?token=SECRET123&expires=9"
	transport := fmt.Errorf("partial read request failed: %w", &url.Error{Op: "Get", URL: signed, Err: errors.New("connection reset by peer")})
	if got := safeReason(transport); strings.Contains(got, "SECRET123") || strings.Contains(got, "https://") || got != "transport: connection reset by peer" {
		t.Fatalf("transport reason = %q", got)
	}
	api := fmt.Errorf("wrapped: %w", &RequestError{Op: "file seek", Kind: KindNotFound, StatusCode: 404, Message: "file not found"})
	if got := safeReason(api); got != "file seek status=404 kind="+string(KindNotFound)+" message=file not found" {
		t.Fatalf("api reason = %q", got)
	}
	plain := fmt.Errorf("%w: seek signed URL %s returned status=403", errUnsupported, signed)
	if got := safeReason(plain); strings.Contains(got, "SECRET123") || !strings.Contains(got, "<url>") {
		t.Fatalf("plain reason = %q", got)
	}
}

func TestPartialFailureLogIsRateLimited(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	partialFailLog.mu.Lock()
	partialFailLog.now = func() time.Time { return now }
	partialFailLog.mu.Unlock()
	t.Cleanup(func() {
		partialFailLog.mu.Lock()
		partialFailLog.now = time.Now
		partialFailLog.mu.Unlock()
	})
	logged := func() time.Time {
		partialFailLog.mu.Lock()
		defer partialFailLog.mu.Unlock()
		return partialFailLog.last["svc-log|"+string(CapabilitySeek)]
	}
	logPartialFailure("svc-log", CapabilitySeek, errors.New("x"))
	if !logged().Equal(now) {
		t.Fatal("first failure is logged")
	}
	first := now
	now = now.Add(10 * time.Minute)
	logPartialFailure("svc-log", CapabilitySeek, errors.New("x"))
	if !logged().Equal(first) {
		t.Fatal("a second failure within 30 minutes is not logged")
	}
	now = now.Add(25 * time.Minute)
	logPartialFailure("svc-log", CapabilitySeek, errors.New("x"))
	if !logged().Equal(now) {
		t.Fatal("logged again after 30 minutes")
	}
}

func TestRecordReportsThePauseOnce(t *testing.T) {
	c := &capabilityCache{byKey: map[string]*capabilityState{}}
	if c.record("svc", CapabilitySeek, false) || c.record("svc", CapabilityOffsetQuery, false) {
		t.Fatal("not paused before three failures")
	}
	if !c.record("svc", CapabilityRange, false) {
		t.Fatal("the third failure pauses partial reads")
	}
	if c.record("svc", CapabilitySeek, false) {
		t.Fatal("an already paused service is not reported again")
	}
}
