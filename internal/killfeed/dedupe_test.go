package killfeed

import (
	"testing"
	"time"
)

func killEvent(victim, killer, weapon string, dist float64, tod string) *Event {
	d := dist
	return &Event{
		Type:      EventPlayerKill,
		TimeOfDay: tod,
		Victim:    &PlayerRef{Name: victim, ID: victim + "-id"},
		Killer:    &PlayerRef{Name: killer, ID: killer + "-id"},
		Weapon:    weapon,
		Distance:  &d,
	}
}

func TestDeduplicatorDropsRepeatedKill(t *testing.T) {
	d := NewDeduplicator(60*time.Second, 100)
	ev := killEvent("ookylianoo", "MmeyAFK_7", "M4-A1", 62.1978, "16:40:12")

	if d.IsDuplicate(ev) {
		t.Fatal("first occurrence must not be a duplicate")
	}
	if !d.IsDuplicate(ev) {
		t.Fatal("replayed identical kill must be dropped as duplicate")
	}
}

func TestDeduplicatorDistinguishesDifferentKills(t *testing.T) {
	d := NewDeduplicator(60*time.Second, 100)
	a := killEvent("ookylianoo", "MmeyAFK_7", "M4-A1", 62.1978, "16:40:12")
	b := killEvent("ookylianoo", "MmeyAFK_7", "M4-A1", 62.1978, "16:41:55")  // different time
	c := killEvent("ookylianoo", "MmeyAFK_7", "SCR 17", 62.1978, "16:40:12") // different weapon

	if d.IsDuplicate(a) {
		t.Fatal("first kill must pass")
	}
	if d.IsDuplicate(b) {
		t.Fatal("different timestamp must not be a duplicate")
	}
	if d.IsDuplicate(c) {
		t.Fatal("different weapon must not be a duplicate")
	}
}

func TestDeduplicatorExpiresAfterTTL(t *testing.T) {
	d := NewDeduplicator(1*time.Millisecond, 100)
	ev := killEvent("v", "k", "M4-A1", 10, "16:40:12")
	if d.IsDuplicate(ev) {
		t.Fatal("first occurrence must pass")
	}
	time.Sleep(5 * time.Millisecond)
	if d.IsDuplicate(ev) {
		t.Fatal("entry should expire after TTL and be allowed again")
	}
}

// A Timestamp the engine derives from the line's clock (event_time.go) must not change a line's
// fingerprint: every kill and death ever stored was fingerprinted with a zero Timestamp, and the
// durable dedupe would otherwise see all of history as new after a deploy. A Timestamp read from
// a full date on the line (not derived) still takes part, as before.
func TestFingerprintUnchangedByDerivedTimestamp(t *testing.T) {
	plain := killEvent("ookylianoo", "MmeyAFK_7", "M4-A1", 62.1978, "16:40:12")
	want := fingerprint(plain)

	derived := killEvent("ookylianoo", "MmeyAFK_7", "M4-A1", 62.1978, "16:40:12")
	derived.Timestamp = time.Date(2026, 10, 15, 20, 40, 12, 0, time.UTC)
	derived.TimestampDerived = true
	if got := fingerprint(derived); got != want {
		t.Fatalf("derived Timestamp changed the fingerprint: %s vs %s", got, want)
	}
	otherDay := killEvent("ookylianoo", "MmeyAFK_7", "M4-A1", 62.1978, "16:40:12")
	otherDay.Timestamp = time.Date(2026, 10, 16, 20, 40, 12, 0, time.UTC)
	otherDay.TimestampDerived = true
	if got := fingerprint(otherDay); got != want {
		t.Fatalf("derived Timestamp on another day changed the fingerprint: %s vs %s", got, want)
	}
	// The "session date when known" part is still honoured for a Timestamp that was not derived.
	absolute := killEvent("ookylianoo", "MmeyAFK_7", "M4-A1", 62.1978, "16:40:12")
	absolute.Timestamp = time.Date(2026, 10, 15, 20, 40, 12, 0, time.UTC)
	if got := fingerprint(absolute); got == want {
		t.Fatal("a Timestamp read from the line itself must still be part of the fingerprint")
	}
	// Dedupe: the stamped line is the same line.
	d := NewDeduplicator(60*time.Second, 100)
	if d.IsDuplicate(plain) {
		t.Fatal("first occurrence must not be a duplicate")
	}
	if !d.IsDuplicate(derived) {
		t.Fatal("the same line with a derived Timestamp must be the same line")
	}
}
