package killfeed

import (
	"sort"
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
}

// NewPlayerTracker creates an empty tracker.
func NewPlayerTracker() *PlayerTracker {
	return &PlayerTracker{players: make(map[string]OnlinePlayer)}
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
	return "name:" + p.Name
}

// PlayerConnected records an authoritative "is connected" event. Duplicate
// connections for the same player refresh the record rather than double-counting.
func (t *PlayerTracker) PlayerConnected(p *PlayerRef) {
	if t == nil || p == nil {
		return
	}
	key := playerKey(p)
	if key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.players[key] = OnlinePlayer{ID: p.ID, Name: p.Name, ConnectedAt: time.Now()}
}

// PlayerDisconnected removes a player on "has been disconnected". Unknown
// disconnects are ignored safely.
func (t *PlayerTracker) PlayerDisconnected(p *PlayerRef) {
	if t == nil || p == nil {
		return
	}
	key := playerKey(p)
	if key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.players, key)
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
