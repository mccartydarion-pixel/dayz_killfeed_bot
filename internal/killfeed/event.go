package killfeed

import "time"

// EventType describes the normalized event kind.
type EventType string

const (
	EventPlayerKill       EventType = "PLAYER_KILL"
	EventSuicide          EventType = "SUICIDE"
	EventEnvironmentDeath EventType = "ENVIRONMENT_DEATH"
	EventInfectedDeath    EventType = "INFECTED_DEATH"
	EventPlayerConnect    EventType = "PLAYER_CONNECT"
	EventPlayerDisconnect EventType = "PLAYER_DISCONNECT"
)

// Event is the normalized DayZ killfeed event model.
type Event struct {
	Type      EventType
	Killer    string
	Victim    string
	Weapon    string
	Distance  float64
	Timestamp time.Time
}
