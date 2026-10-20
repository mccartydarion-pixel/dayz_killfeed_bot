package app

import (
	"testing"
	"time"
)

// Keys whose hits have all expired are dropped, so a limiter keyed by remote address (one key
// per connection) does not grow without bound.
func TestRateLimiterDropsExpiredKeys(t *testing.T) {
	l := newSaaSRateLimiter(20*time.Millisecond, 5)
	for _, k := range []string{"a", "b", "c"} {
		if !l.Allow(k) {
			t.Fatal("first hit allowed")
		}
	}
	time.Sleep(30 * time.Millisecond)
	if !l.Allow("d") {
		t.Fatal("hit allowed")
	}
	l.mu.Lock()
	n := len(l.hits)
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("expired keys must be dropped, %d left", n)
	}
	for i := 0; i < 5; i++ {
		l.Allow("e")
	}
	if l.Allow("e") {
		t.Fatal("the limit still applies")
	}
}
