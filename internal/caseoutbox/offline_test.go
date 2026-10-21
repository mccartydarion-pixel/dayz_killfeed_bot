package caseoutbox

import (
	"strings"
	"testing"
	"time"
)

func start(t *testing.T) (Item, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 27, 16, 0, 0, 0, time.UTC)
	i, err := NewSynthetic(strings.Repeat("a", 64), Scope{GuildID: 1, InstallationID: 2, ServerID: 3}, now)
	if err != nil {
		t.Fatal(err)
	}
	return i, now
}

func TestSyntheticDeliveryAckAndReplayFailClosed(t *testing.T) {
	item, now := start(t)
	claimed, lease, err := Acquire(item, now)
	if err != nil || claimed.Status != Leased || claimed.Attempts != 1 {
		t.Fatalf("claim: %+v %+v %v", claimed, lease, err)
	}
	if claimed.Key != item.Key || claimed.Scope != item.Scope {
		t.Fatal("claim changed immutable delivery identity")
	}
	if _, _, err := Acquire(claimed, now.Add(time.Second)); err == nil {
		t.Fatal("active lease double claimed")
	}
	sent, err := Ack(claimed, lease, now.Add(time.Second))
	if err != nil || sent.Status != Sent || sent.SentAt.IsZero() {
		t.Fatalf("ack: %+v %v", sent, err)
	}
	if _, err := Ack(sent, lease, now.Add(2*time.Second)); err == nil {
		t.Fatal("replayed receipt accepted")
	}
	if _, _, err := Acquire(sent, now.Add(time.Hour)); err == nil {
		t.Fatal("terminal delivery reclaimed")
	}
	if _, err := Suppress(sent, "STAFF_DISMISSED"); err == nil {
		t.Fatal("sent item suppressed retroactively")
	}
}

func TestBoundedRetriesAndTerminalFailures(t *testing.T) {
	item, now := start(t)
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		claimed, lease, err := Acquire(item, now)
		if err != nil || claimed.Attempts != attempt {
			t.Fatalf("claim %d: %+v %v", attempt, claimed, err)
		}
		next, err := Failure(claimed, lease, now.Add(time.Second), "TEMPORARY_TRANSPORT")
		if err != nil {
			t.Fatal(err)
		}
		if attempt == MaxAttempts {
			if next.Status != Dead || !next.NextAt.IsZero() {
				t.Fatalf("attempt budget not terminal: %+v", next)
			}
			if _, _, err := Acquire(next, now.Add(time.Hour)); err == nil {
				t.Fatal("dead item reclaimed")
			}
		} else {
			expected := 30 * time.Second * time.Duration(1<<uint(attempt-1))
			if next.Status != RetryWait || !next.NextAt.Equal(now.Add(time.Second).Add(expected)) {
				t.Fatalf("retry %d: %+v", attempt, next)
			}
			if _, _, err := Acquire(next, next.NextAt.Add(-time.Nanosecond)); err == nil {
				t.Fatal("early retry admitted")
			}
			now = next.NextAt
		}
		item = next
	}
}

func TestUnsafeDestinationOrPermissionDoesNotReroute(t *testing.T) {
	for _, code := range []string{"DESTINATION_UNAVAILABLE", "PERMISSION_REVOKED"} {
		item, now := start(t)
		leased, token, err := Acquire(item, now)
		if err != nil {
			t.Fatal(err)
		}
		next, err := Failure(leased, token, now.Add(time.Second), code)
		if err != nil || next.Status != Dead || next.Scope != item.Scope || next.Key != item.Key {
			t.Fatalf("unsafe route handling %s: %+v %v", code, next, err)
		}
	}
}

func TestExpiredOrSuppressedLeasesCannotBecomeSent(t *testing.T) {
	item, now := start(t)
	leased, token, err := Acquire(item, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Acquire(leased, token.Until.Add(time.Second)); err == nil {
		t.Fatal("expired lease was blindly retried")
	}
	if _, err := Ack(leased, token, token.Until.Add(time.Second)); err == nil {
		t.Fatal("late receipt accepted")
	}
	suppressed, err := Suppress(leased, "OWNER_DISABLED")
	if err != nil {
		t.Fatal(err)
	}
	if suppressed.Status != Suppressed {
		t.Fatal("suppression failed")
	}
	if _, err := Ack(suppressed, token, now.Add(time.Second)); err == nil {
		t.Fatal("in-flight ack bypassed suppression")
	}
	if _, _, err := Acquire(suppressed, now.Add(time.Hour)); err == nil {
		t.Fatal("suppressed item reclaimed")
	}
}

func TestInvalidSyntheticIdentityAndFailureCodes(t *testing.T) {
	item, now := start(t)
	for _, s := range []string{"", "abc", strings.Repeat("G", 64)} {
		if _, err := NewSynthetic(s, item.Scope, now); err == nil {
			t.Fatalf("accepted key %q", s)
		}
	}
	if _, err := NewSynthetic(item.Key, Scope{GuildID: 1, InstallationID: 0, ServerID: 3}, now); err == nil {
		t.Fatal("accepted missing scope")
	}
	if _, err := NewSynthetic(item.Key, item.Scope, time.Time{}); err == nil {
		t.Fatal("accepted missing timestamp")
	}
	claimed, lease, err := Acquire(item, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"", "raw HTTP 500 @everyone", "UNKNOWN"} {
		if next, err := Failure(claimed, lease, now.Add(time.Second), code); err == nil || next.Status != Leased {
			t.Fatal("accepted unsafe raw error")
		}
	}
	if _, err := Ack(claimed, Lease{Version: lease.Version + 1, Until: lease.Until}, now.Add(time.Second)); err == nil {
		t.Fatal("stale token accepted")
	}
	if _, err := Ack(claimed, lease, now.Add(-time.Second)); err == nil {
		t.Fatal("ack before lease issue accepted")
	}
	if _, err := Failure(claimed, lease, now.Add(-time.Second), "RATE_LIMIT"); err == nil {
		t.Fatal("failure before lease issue accepted")
	}
	if _, err := Suppress(claimed, "unreviewed reason"); err == nil {
		t.Fatal("unapproved suppression accepted")
	}
}
