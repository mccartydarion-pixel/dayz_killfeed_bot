package killfeed

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Deduplicator drops logically-duplicate events using a bounded TTL cache keyed
// by a stable fingerprint. It protects against replayed bytes, rotation overlap,
// duplicate explicit kills, and reprocessing after transient read failures.
type Deduplicator struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	ttl     time.Duration
	maxKeys int
}

// NewDeduplicator creates a dedupe cache with the given TTL and key bound.
func NewDeduplicator(ttl time.Duration, maxKeys int) *Deduplicator {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	if maxKeys <= 0 {
		maxKeys = 4096
	}
	return &Deduplicator{seen: make(map[string]time.Time), ttl: ttl, maxKeys: maxKeys}
}

// IsDuplicate reports whether the event was already seen within the TTL.
// The first occurrence returns false and records the event.
func (d *Deduplicator) IsDuplicate(ev *Event) bool {
	if d == nil || ev == nil {
		return false
	}
	key := fingerprint(ev)

	d.mu.Lock()
	defer d.mu.Unlock()

	d.pruneLocked(time.Now())
	if _, exists := d.seen[key]; exists {
		return true
	}
	d.seen[key] = time.Now()
	return false
}

// pruneLocked removes expired entries and evicts oldest if over capacity.
func (d *Deduplicator) pruneLocked(now time.Time) {
	for k, ts := range d.seen {
		if now.Sub(ts) > d.ttl {
			delete(d.seen, k)
		}
	}
	if len(d.seen) <= d.maxKeys {
		return
	}
	// Evict arbitrary excess keys; bounded to keep memory flat.
	excess := len(d.seen) - d.maxKeys
	for k := range d.seen {
		if excess <= 0 {
			break
		}
		delete(d.seen, k)
		excess--
	}
}

// fingerprint builds a stable identity for an event from its durable fields.
func fingerprint(ev *Event) string {
	var victimID, killerID, attackerID string
	if ev.Victim != nil {
		victimID = ev.Victim.ID
	}
	if ev.Killer != nil {
		killerID = ev.Killer.ID
	}
	if ev.Attacker != nil {
		attackerID = ev.Attacker.ID
	}
	dist := ""
	if ev.Distance != nil {
		dist = fmt.Sprintf("%.4f", *ev.Distance)
	}
	parts := []string{
		string(ev.Type),
		ev.TimeOfDay,
		ev.Timestamp.UTC().Format("2006-01-02"), // session date when known
		victimID,
		killerID,
		attackerID,
		ev.Weapon,
		dist,
	}
	sum := sha1.Sum([]byte(joinParts(parts)))
	return hex.EncodeToString(sum[:])
}

func joinParts(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "|"
		}
		out += p
	}
	return out
}
