package discord

import "sync"

// GuildSetup is the per-guild Champion Killfeed configuration. All fields are
// Discord resource IDs captured at setup time; names are never relied on after.
// This model is designed to be persisted in PostgreSQL in Phase 4.
type GuildSetup struct {
	GuildID        string
	WelcomeEnabled bool

	CategoryID       string
	WelcomeChannelID string

	ServerStatusChannelID  string
	KillfeedChannelID      string
	OnlinePlayersChannelID string
	LeaderboardsChannelID  string
	PlayerStatsChannelID   string

	ServerStatusMessageID  string
	OnlinePlayersMessageID string
}

// SetupStore is the guild-keyed configuration abstraction. Phase 4 will replace
// the in-memory implementation with PostgreSQL without changing callers.
type SetupStore interface {
	Get(guildID string) (*GuildSetup, error)
	Save(setup GuildSetup) error
	Delete(guildID string) error
}

// InMemorySetupStore is the Phase 3.1 SetupStore backed by process memory.
// It is NOT durable across restarts; Phase 4 adds persistence.
type InMemorySetupStore struct {
	mu    sync.RWMutex
	items map[string]GuildSetup
}

// NewInMemorySetupStore creates an empty store.
func NewInMemorySetupStore() *InMemorySetupStore {
	return &InMemorySetupStore{items: make(map[string]GuildSetup)}
}

// Get returns the stored setup for a guild, or nil if none is configured.
func (s *InMemorySetupStore) Get(guildID string) (*GuildSetup, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if setup, ok := s.items[guildID]; ok {
		cpy := setup
		return &cpy, nil
	}
	return nil, nil
}

// Save stores or replaces the setup for a guild.
func (s *InMemorySetupStore) Save(setup GuildSetup) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[setup.GuildID] = setup
	return nil
}

// Delete removes a guild's setup.
func (s *InMemorySetupStore) Delete(guildID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, guildID)
	return nil
}
