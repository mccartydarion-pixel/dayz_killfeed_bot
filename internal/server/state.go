package server

import (
	"net/http"
	"sync"
	"time"
)

// State is the shared, sanitized runtime status of the application.
// It never contains secrets and is safe to expose via /api/v1/status.
type State struct {
	mu sync.RWMutex

	DiscordConnected   bool
	BotUsername        string
	GuildFound         bool
	ChannelFound       bool
	MissingPermissions []string

	NitradoAuthenticated bool
	ServiceVerified      bool
	ServiceGame          string
	ServiceType          string
	ServiceStatus        string

	LogSourceFound bool
	LogFilename    string
	LogPath        string
	LogSize        int64
	LogModified    time.Time

	DiscoveryState  string
	DirsVisited     int
	FilesDiscovered int

	LastPoll        time.Time
	LastLogChange   time.Time
	PollInterval    time.Duration
	BytesRead       int64
	LinesDiscovered int64
}

// NewState creates an empty runtime state container.
func NewState() *State {
	return &State{}
}

// SetDiscovery records the current discovery state and traversal counters.
func (s *State) SetDiscovery(state string, dirsVisited, filesDiscovered int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DiscoveryState = state
	s.DirsVisited = dirsVisited
	s.FilesDiscovered = filesDiscovered
}

// SetDiscord records Discord connectivity and access validation results.
func (s *State) SetDiscord(connected bool, botUsername string, guildFound, channelFound bool, missing []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.DiscordConnected = connected
	s.BotUsername = botUsername
	s.GuildFound = guildFound
	s.ChannelFound = channelFound
	s.MissingPermissions = append([]string(nil), missing...)
}

// SetNitrado records Nitrado authentication and service verification results.
func (s *State) SetNitrado(authenticated, verified bool, game, serviceType, status string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NitradoAuthenticated = authenticated
	s.ServiceVerified = verified
	s.ServiceGame = game
	s.ServiceType = serviceType
	s.ServiceStatus = status
}

// SetLogSource records the discovered gameplay/admin log source metadata.
func (s *State) SetLogSource(filename, path string, size int64, modified time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LogSourceFound = true
	s.LogFilename = filename
	s.LogPath = path
	s.LogSize = size
	s.LogModified = modified
}

// SetPollStats records the latest polling cycle counters.
func (s *State) SetPollStats(lastPoll, lastLogChange time.Time, interval time.Duration, bytesRead, lines int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastPoll = lastPoll
	s.LastLogChange = lastLogChange
	s.PollInterval = interval
	s.BytesRead = bytesRead
	s.LinesDiscovered = lines
}

// Snapshot returns a sanitized copy of the runtime state for the status endpoint.
func (s *State) Snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	formatTime := func(t time.Time) string {
		if t.IsZero() {
			return "not-yet-run"
		}
		return t.UTC().Format(time.RFC3339)
	}
	interval := "not-yet-run"
	if s.PollInterval > 0 {
		interval = s.PollInterval.String()
	}

	return map[string]any{
		"status":               "online",
		"discord":              s.DiscordConnected,
		"discord_connected":    s.DiscordConnected,
		"bot_username":         s.BotUsername,
		"guild_found":          s.GuildFound,
		"channel_found":        s.ChannelFound,
		"missing_perms":        s.MissingPermissions,
		"nitrado":              s.NitradoAuthenticated,
		"nitrado_connected":    s.NitradoAuthenticated,
		"service_verified":     s.ServiceVerified,
		"service_game":         s.ServiceGame,
		"service_type":         s.ServiceType,
		"service_status":       s.ServiceStatus,
		"discovery_state":      s.DiscoveryState,
		"directories_visited":  s.DirsVisited,
		"files_discovered":     s.FilesDiscovered,
		"log_source_found":     s.LogSourceFound,
		"selected_log":         s.LogPath,
		"log_filename":         s.LogFilename,
		"log_path":             s.LogPath,
		"log_size":             s.LogSize,
		"log_modified":         formatTime(s.LogModified),
		"last_poll":            formatTime(s.LastPoll),
		"last_log_change":      formatTime(s.LastLogChange),
		"last_successful_poll": formatTime(s.LastLogChange),
		"poll_interval":        interval,
		"bytes_read":           s.BytesRead,
		"lines_discovered":     s.LinesDiscovered,
	}
}

// StatusHandler writes the current sanitized runtime state as JSON.
func (s *State) StatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "online"})
		return
	}
	writeJSON(w, http.StatusOK, s.Snapshot())
}
