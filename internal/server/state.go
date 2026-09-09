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

	ADMLinesProcessed      int64
	EventsParsed           int64
	EventsIgnored          int64
	HitsParsed             int64
	ExplicitKillsParsed    int64
	DeathsParsed           int64
	ConnectsParsed         int64
	DisconnectsParsed      int64
	DuplicateEventsDropped int64
	DiscordKillsPublished  int64
	DiscordPublishErrors   int64
	LastKillTime           time.Time

	OnlinePlayers int

	SelectedLogActive              bool
	SetupComplete                  bool
	KillfeedChannelReady           bool
	OnlinePlayersChannelReady      bool
	ServerStatusChannelReady       bool
	OnlineCounterLastPublished     int
	OnlineCounterUpdateErrors      int
	OnlineCounterPermissionBlocked bool
}

// NewState creates an empty runtime state container.
func NewState() *State {
	return &State{}
}

// SetOnlinePlayers records the current online player count (no player IDs).
func (s *State) SetOnlinePlayers(count int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.OnlinePlayers = count
}

// SetSelectedLogActive records whether the selected ADM log is currently growing.
func (s *State) SetSelectedLogActive(active bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SelectedLogActive = active
}

// SetSetupReadiness records which Champion Discord resources are configured.
func (s *State) SetSetupReadiness(complete, killfeed, onlinePlayers, serverStatus bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SetupComplete = complete
	s.KillfeedChannelReady = killfeed
	s.OnlinePlayersChannelReady = onlinePlayers
	s.ServerStatusChannelReady = serverStatus
}

// SetOnlineCounter records the voice counter publish state.
func (s *State) SetOnlineCounter(lastPublished, updateErrors int, permissionBlocked bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.OnlineCounterLastPublished = lastPublished
	s.OnlineCounterUpdateErrors = updateErrors
	s.OnlineCounterPermissionBlocked = permissionBlocked
}

// SetMetrics records parser and publisher counters from the killfeed engine.
func (s *State) SetMetrics(m map[string]int64, lastKill time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ADMLinesProcessed = m["adm_lines_processed"]
	s.EventsParsed = m["events_parsed"]
	s.EventsIgnored = m["events_ignored"]
	s.HitsParsed = m["hits_parsed"]
	s.ExplicitKillsParsed = m["explicit_kills_parsed"]
	s.DeathsParsed = m["deaths_parsed"]
	s.ConnectsParsed = m["connects_parsed"]
	s.DisconnectsParsed = m["disconnects_parsed"]
	s.DuplicateEventsDropped = m["duplicate_events_dropped"]
	s.DiscordKillsPublished = m["discord_kills_published"]
	s.DiscordPublishErrors = m["discord_publish_errors"]
	s.LastKillTime = lastKill
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

		"adm_lines_processed":      s.ADMLinesProcessed,
		"events_parsed":            s.EventsParsed,
		"events_ignored":           s.EventsIgnored,
		"hits_parsed":              s.HitsParsed,
		"explicit_kills_parsed":    s.ExplicitKillsParsed,
		"deaths_parsed":            s.DeathsParsed,
		"connects_parsed":          s.ConnectsParsed,
		"disconnects_parsed":       s.DisconnectsParsed,
		"duplicate_events_dropped": s.DuplicateEventsDropped,
		"discord_kills_published":  s.DiscordKillsPublished,
		"discord_publish_errors":   s.DiscordPublishErrors,
		"last_kill_time":           formatTime(s.LastKillTime),
		"online_players":           s.OnlinePlayers,

		"selected_log_active":               s.SelectedLogActive,
		"setup_complete":                    s.SetupComplete,
		"killfeed_channel_ready":            s.KillfeedChannelReady,
		"online_players_channel_ready":      s.OnlinePlayersChannelReady,
		"server_status_channel_ready":       s.ServerStatusChannelReady,
		"online_counter_last_published":     s.OnlineCounterLastPublished,
		"online_counter_update_errors":      s.OnlineCounterUpdateErrors,
		"online_counter_permission_blocked": s.OnlineCounterPermissionBlocked,
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
