package app

import (
	"context"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
)

// embedCustomizer returns the renderer publishers use, or a nil INTERFACE when the
// rollout flag is off (CHAMPION_CUSTOM_EMBEDS_ENABLED) or there is no database - so a
// disabled deployment does no template work at all and every card is the Champion
// default. (Returning the typed nil pointer would be a non-nil interface.)
func (a *App) embedCustomizer() discord.EmbedCustomizer {
	if a.EmbedRenderer == nil || !a.EmbedRenderer.Enabled() {
		return nil
	}
	return a.EmbedRenderer
}

// serverNameCache resolves a game server's display name for {{server_name}} with a
// short TTL, so publishing never adds a database read per card. A failed or empty
// lookup yields "" (the variable is then simply absent).
type serverNameCache struct {
	mu      sync.Mutex
	entries map[int64]serverNameEntry
	lookup  func(ctx context.Context, serverID int64) (string, error)
}

type serverNameEntry struct {
	name    string
	expires time.Time
}

const serverNameTTL = 5 * time.Minute

func (c *serverNameCache) name(serverID int64) string {
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.entries[serverID]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.name
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	name, err := c.lookup(ctx, serverID)
	ttl := serverNameTTL
	if err != nil {
		name, ttl = "", 10*time.Second // do not hammer a failing database
	}
	c.mu.Lock()
	if len(c.entries) > 1024 {
		c.entries = map[int64]serverNameEntry{}
	}
	c.entries[serverID] = serverNameEntry{name: name, expires: now.Add(ttl)}
	c.mu.Unlock()
	return name
}

// serverNameFunc is the ServerNameFunc publishers use (nil when there is no server
// repository).
func (a *App) serverNameFunc() discord.ServerNameFunc {
	if a.Servers == nil {
		return nil
	}
	a.serverNamesOnce.Do(func() { // workers start concurrently
		servers := a.Servers
		a.serverNames = &serverNameCache{entries: map[int64]serverNameEntry{}, lookup: func(ctx context.Context, id int64) (string, error) {
			s, err := servers.GetByID(ctx, id)
			if err != nil || s == nil {
				return "", err
			}
			return s.DisplayName, nil
		}}
	})
	return a.serverNames.name
}
