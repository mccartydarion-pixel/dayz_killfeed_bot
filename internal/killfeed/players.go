package killfeed

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// OnlinePlayer is one tracked connected player. ID is the stable DayZ identity
// (never shown publicly); Name is the current display name.
type OnlinePlayer struct {
	ID          string
	Name        string
	ConnectedAt time.Time
}

// PlayerTracker tracks online players keyed by DayZ player ID when available.
// It is concurrency-safe and rebuilt from "is connected" events each session.
type PlayerTracker struct {
	mu      sync.RWMutex
	players map[string]OnlinePlayer
	now     func() time.Time
}

// NewPlayerTracker creates an empty tracker.
func NewPlayerTracker() *PlayerTracker {
	return &PlayerTracker{players: make(map[string]OnlinePlayer), now: time.Now}
}

// playerKey returns the stable tracking key: the DayZ ID when present, else the
// lowercased display name as a fallback so a player without an ID is still tracked.
func playerKey(p *PlayerRef) string {
	if p == nil {
		return ""
	}
	if p.ID != "" {
		return "id:" + p.ID
	}
	return "name:" + strings.ToLower(strings.TrimSpace(p.Name))
}

// PlayerConnected records an authoritative "is connected" event. Duplicate
// connections for the same player refresh the record rather than double-counting.
// Returns true only when the player was not already tracked as online.
func (t *PlayerTracker) PlayerConnected(p *PlayerRef) bool {
	if t == nil || p == nil {
		return false
	}
	key := playerKey(p)
	if key == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, alreadyOnline := t.players[key]
	connectedAt := t.now()
	if alreadyOnline && !prev.ConnectedAt.IsZero() {
		// A duplicate "is connected" refreshes the record but must not restart
		// the session clock, or the observed session length would be wrong.
		connectedAt = prev.ConnectedAt
	}
	t.players[key] = OnlinePlayer{ID: p.ID, Name: p.Name, ConnectedAt: connectedAt}
	return !alreadyOnline
}

// PlayerDisconnected removes a player on "has been disconnected". Unknown
// disconnects are ignored safely. Returns true only when a tracked player was
// actually removed (never goes negative).
func (t *PlayerTracker) PlayerDisconnected(p *PlayerRef) bool {
	_, removed := t.DisconnectSession(p)
	return removed
}

// DisconnectSession is PlayerDisconnected that also reports how long Champion
// observed the player online: from when it processed their connect to now. It
// is only known for a player this tracker saw connect (a player who joined
// before Champion started, or before a tracker reset, has no known start), in
// which case removal still succeeds but the duration is 0.
func (t *PlayerTracker) DisconnectSession(p *PlayerRef) (session time.Duration, removed bool) {
	if t == nil || p == nil {
		return 0, false
	}
	key := playerKey(p)
	if key == "" {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	tracked, ok := t.players[key]
	if !ok {
		return 0, false
	}
	delete(t.players, key)
	if !tracked.ConnectedAt.IsZero() {
		if d := t.now().Sub(tracked.ConnectedAt); d > 0 {
			session = d
		}
	}
	return session, true
}

// GetOnlinePlayers returns a copy of current online players sorted by name.
func (t *PlayerTracker) GetOnlinePlayers() []OnlinePlayer {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]OnlinePlayer, 0, len(t.players))
	for _, p := range t.players {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// OnlineCount returns the number of tracked online players.
func (t *PlayerTracker) OnlineCount() int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.players)
}

// Reset clears all tracked players. Called when a new ADM session/log is selected
// (server restart) so stale players are not carried across sessions.
func (t *PlayerTracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.players = make(map[string]OnlinePlayer)
}

// ReconcileSnapshot applies a COMPLETE ADM player-list snapshot: every listed player is online and
// every tracked player not listed is not. Callers must pass only complete snapshots - an incomplete
// or missing snapshot is never evidence that a player left. A player added here has no known
// connect time (ConnectedAt stays zero, so no session length is claimed). Returns how many players
// were added and removed; neither produces a connection notice.
func (t *PlayerTracker) ReconcileSnapshot(listed []*PlayerRef) (added, removed int) {
	if t == nil {
		return 0, 0
	}
	keep := make(map[string]*PlayerRef, len(listed))
	for _, p := range listed {
		if k := playerKey(p); k != "" {
			keep[k] = p
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.players {
		if _, ok := keep[k]; !ok {
			delete(t.players, k)
			removed++
		}
	}
	for k, p := range keep {
		if prev, ok := t.players[k]; ok {
			prev.Name = p.Name
			t.players[k] = prev
			continue
		}
		t.players[k] = OnlinePlayer{ID: p.ID, Name: p.Name}
		added++
	}
	return added, removed
}
