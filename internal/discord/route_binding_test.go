package discord

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestEnvRouteLookupTimeoutDefaultsAndOverrides(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset uses the measured default", "", defaultRouteLookupTimeout},
		{"valid override wins", "750ms", 750 * time.Millisecond},
		{"malformed value falls back to default", "not-a-duration", defaultRouteLookupTimeout},
		{"too small a floor falls back to default (would defeat the resolver)", "1ms", defaultRouteLookupTimeout},
		{"exactly the floor is accepted", "50ms", 50 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.env == "" {
				os.Unsetenv("ROUTE_LOOKUP_TIMEOUT")
			} else {
				t.Setenv("ROUTE_LOOKUP_TIMEOUT", c.env)
			}
			if got := envRouteLookupTimeout(); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// blockingResolver never returns until its context is done - a stand-in for
// a genuinely unresponsive database, so ChannelID() must not hang past the
// configured timeout.
type blockingResolver struct{}

func (blockingResolver) Resolve(ctx context.Context, _, _ int64, _ string) (string, bool, error) {
	<-ctx.Done()
	return "", false, ctx.Err()
}

// TestChannelIDBoundedByConfiguredTimeout proves a hanging resolver can never
// stall a live publisher past routeLookupTimeout, and that the bound is the
// package var (so ROUTE_LOOKUP_TIMEOUT actually takes effect), not a
// hardcoded value that measurement/configuration can't move.
func TestChannelIDBoundedByConfiguredTimeout(t *testing.T) {
	original := routeLookupTimeout
	routeLookupTimeout = 40 * time.Millisecond
	t.Cleanup(func() { routeLookupTimeout = original })

	b := NewRouteBinding(blockingResolver{}, 1, 1, routeKeyAdminLogs)
	start := time.Now()
	got := b.ChannelID()
	elapsed := time.Since(start)

	if got != "" {
		t.Fatalf("expected the legacy fallback (empty string), got %q", got)
	}
	// Generous upper bound (10x the configured timeout) to stay robust on a
	// loaded CI machine while still proving it did NOT wait out some much
	// larger bound (e.g. the old hardcoded 2s).
	if elapsed > 400*time.Millisecond {
		t.Fatalf("ChannelID() took %v, expected it to return within roughly routeLookupTimeout (%v)", elapsed, routeLookupTimeout)
	}
}

// A cache-hit-shaped resolver (returns instantly, no context wait) must
// never be slowed down by the timeout machinery - confirms the timeout only
// bounds a genuinely slow/miss lookup, never a fast one.
func TestChannelIDFastResolverReturnsImmediately(t *testing.T) {
	original := routeLookupTimeout
	routeLookupTimeout = time.Hour // would fail the test if ChannelID() actually waited
	t.Cleanup(func() { routeLookupTimeout = original })

	b := NewRouteBinding(fastResolver{channelID: "chan-fast", found: true}, 1, 1, routeKeyAdminLogs)
	start := time.Now()
	got := b.ChannelID()
	if got != "chan-fast" {
		t.Fatalf("got %q", got)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("a fast resolver must return immediately, took %v", elapsed)
	}
}

type fastResolver struct {
	channelID string
	found     bool
	err       error
}

func (f fastResolver) Resolve(context.Context, int64, int64, string) (string, bool, error) {
	return f.channelID, f.found, f.err
}

func TestChannelIDPropagatesLookupError(t *testing.T) {
	b := NewRouteBinding(fastResolver{err: errors.New("boom")}, 1, 1, routeKeyAdminLogs)
	if got := b.ChannelID(); got != "" {
		t.Fatalf("expected fallback on lookup error, got %q", got)
	}
}
