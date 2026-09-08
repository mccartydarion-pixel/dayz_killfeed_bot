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
