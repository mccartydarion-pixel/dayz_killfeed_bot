package killfeed

import "time"

// EventType describes the normalized event kind.
type EventType string

const (
	EventPlayerConnecting  EventType = "PLAYER_CONNECTING"
	EventPlayerConnect     EventType = "PLAYER_CONNECT"
	EventPlayerDisconnect  EventType = "PLAYER_DISCONNECT"
	EventPlayerHit         EventType = "PLAYER_HIT"
	EventPlayerKill        EventType = "PLAYER_KILL"
	EventPlayerDeath       EventType = "PLAYER_DEATH"
	EventSuicideAction     EventType = "SUICIDE_ACTION"
	EventPlayerUnconscious EventType = "PLAYER_UNCONSCIOUS"
	EventPlayerConscious   EventType = "PLAYER_CONSCIOUS"
	EventPlayerRespawn     EventType = "PLAYER_RESPAWN"
)

// Position is a 3D coordinate from the ADM log. Z can be negative.
type Position struct {
	X float64
	Y float64
	Z float64
}

// PlayerRef is normalized player information. ID is the stable ADM identity and
// is never shown in Discord. Position is optional.
type PlayerRef struct {
	Name     string
	ID       string
	Position *Position
}

// Event is the normalized DayZ killfeed event model. Fields are pointers or
// zero-omitted so events never carry fabricated values.
type Event struct {
	GuildID   int64
	ServerID  int64
	SessionID string
	Type      EventType
	Timestamp time.Time
	// TimeOfDay preserves the raw HH:MM:SS clock from the ADM line when a full
	// session date is not available (the date comes from the log filename).
	TimeOfDay string

	// Player is the subject for connect/disconnect/death/suicide/unconscious/etc.
	Player *PlayerRef

	// Combat fields (hit / explicit kill).
	Victim   *PlayerRef
	Killer   *PlayerRef
	Attacker *PlayerRef

	Weapon   string
	Ammo     string
	Distance *float64

	Damage    *float64
	HP        *float64
	HitZone   string
	HitZoneID string

	Dead bool // the (DEAD) marker was present

	Raw string
	// Competitive context is populated only after durable persistence and is
	// rendered into the same kill embed; it never creates another message.
	BountyTarget      bool
	BountyClaimed     bool
	BountyPoints      int64
	ActiveEventBadges []string
	WarBadge          string
	SeasonName        string
}
