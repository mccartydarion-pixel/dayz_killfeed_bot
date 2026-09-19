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

// DeathCause is a non-player cause of death that the ADM parser PROVED from the
// log line itself. The empty value means "not proven" and is the default: a
// cause is never inferred from a weapon string, a name or the surrounding lines.
//
// Only DeathCauseSuicide is populated today (the "performed EmoteSuicide" line).
// The ADM sample lines this project has contain no infected, animal or
// environment source at all - the parser does not even read "killed by
// <non-player>" lines - so DeathCauseInfected/Animal/Environment are never set
// yet. They exist so that a parser extension backed by real log evidence can set
// them without touching the classification or the feed. Fall, drowning,
// bleeding, starvation, cold, fire, explosion and gas are deliberately absent:
// nothing here can tell them apart.
type DeathCause string

const (
	DeathCauseSuicide     DeathCause = "SUICIDE"
	DeathCauseInfected    DeathCause = "INFECTED"
	DeathCauseAnimal      DeathCause = "ANIMAL"
	DeathCauseEnvironment DeathCause = "ENVIRONMENT"
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

	// Cause is the non-player cause of a death/suicide when the parser proved
	// one (see DeathCause); empty otherwise. Never inferred.
	Cause DeathCause

	Raw string
	// Competitive context is populated only after durable persistence and is
	// rendered into the same kill embed; it never creates another message.
	BountyTarget      bool
	BountyClaimed     bool
	BountyPoints      int64
	ActiveEventBadges []string
	WarBadge          string
	SeasonName        string

	// Stat fields below are populated post-persistence (see
	// persistenceStoreAdapter.ProcessPersistedKill/ProcessPersistedDeath in
	// app.go), same as the competitive context above - best-effort, never
	// blocking publish, and rendered into the same embed rather than a
	// separate message. Pointers distinguish "not available" from a real
	// zero value; a guild without the stats/analytics repositories wired
	// still gets a working embed, just without these sections.
	KillerStats  *CombatRecord // kills/deaths for the killer, all-time
	VictimStats  *CombatRecord // kills/deaths for the victim, all-time
	PlayerStats  *CombatRecord // kills/deaths for ev.Player, all-time (death/suicide only)
	KillerStreak *int          // killer's current kill streak after this kill
	Encounters   *HeadToHead   // killer vs. victim all-time record

	// Streak event context, copied from the durably persisted KillRecord (see
	// persistenceStoreAdapter.ProcessPersistedKill) - never recomputed from
	// current player_combat_stats, so it stays accurate even after the streak
	// has since moved on.
	KillingSpree     bool
	StreakEnded      bool
	EndedStreakCount *int
}

// CombatRecord is a lightweight kills/deaths snapshot for the stat-rich kill
// and death embeds.
type CombatRecord struct {
	Kills  int64
	Deaths int64
}

// KD returns Kills/Deaths, or Kills if Deaths is zero (matches
// repository.PlayerProfile.KD's convention).
func (c CombatRecord) KD() float64 {
	if c.Deaths == 0 {
		return float64(c.Kills)
	}
	return float64(c.Kills) / float64(c.Deaths)
}

// HeadToHead is two players' all-time kill record against each other.
type HeadToHead struct {
	KillerWins int64
	VictimWins int64
}
