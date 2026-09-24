// Package livesync is Champion Live Sync (C.L.S.) phase 1 (docs/CHAMPION_LIVE_SYNC.md): the
// canonical event envelope, DayZ/Nitrado source classification, and parsers for the non-ADM log
// families a console Nitrado service exposes - RPT, script_*.log, crash_*.log and restart.log -
// plus correlation of their boot evidence into one boot observation.
//
// Everything here is pure (no network, no database). It never fabricates a field: a value is set
// only when the source line states it, unknown lines are kept as UNKNOWN records, and a server
// restart is counted once however many sources report it.
package livesync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Source families.
const (
	FamilyADM       = "ADM"
	FamilyRPT       = "RPT"
	FamilyScript    = "SCRIPT_LOG"
	FamilyCrash     = "CRASH_LOG"
	FamilyRestart   = "RESTART_LOG"
	FamilyBanList   = "BAN_LIST"
	FamilyWhitelist = "WHITELIST"
	FamilyUnknown   = "UNKNOWN"
)

// Event categories (Envelope.Category). UNKNOWN is a preserved, unparsed record - never a success.
const (
	CategoryBootStarted        = "SERVER_BOOT_STARTED"
	CategoryShutdownCountdown  = "SERVER_SHUTDOWN_COUNTDOWN"
	CategoryShutdownComplete   = "SERVER_SHUTDOWN_COMPLETE"
	CategoryEngineDestroy      = "ENGINE_DESTROY"
	CategoryScriptException    = "SCRIPT_EXCEPTION"
	CategoryObjectSpawnerError = "OBJECT_SPAWNER_ERROR"
	CategoryMissingModel       = "MISSING_MODEL"
	CategoryConfigWarning      = "CONFIG_WARNING"
	CategoryLocalization       = "LOCALIZATION_NOTICE"
	CategoryCentralEconomy     = "CENTRAL_ECONOMY"
	CategoryNetworkPlayerLeft  = "NETWORK_PLAYER_REMOVED"
	CategoryResourceLeak       = "RESOURCE_LEAK"
	CategoryQuery              = "SERVER_QUERY"
	CategoryScriptModule       = "SCRIPT_MODULE_LOADED"
	CategoryRestartRequested   = "RESTART_REQUESTED"
	CategoryHostReboot         = "HOST_REBOOT"
	CategoryPreStartCheck      = "PRE_START_CHECK"
	CategoryClientAdminRequest = "CLIENT_ADMIN_REQUEST"
	CategoryLogHeader          = "LOG_HEADER"
	CategoryUnknown            = "UNKNOWN"
)

// Validation status of an envelope.
const (
	StatusParsed  = "PARSED"  // a supported category; every payload field was read from the source
	StatusPartial = "PARTIAL" // a known category with a field the source did not provide
	StatusUnknown = "UNKNOWN" // preserved for parser development; not a successful parse
)

// ParserVersion is recorded on every envelope so reprocessing can tell parser generations apart.
const ParserVersion = "cls-1.0"

// Scope is the tenant scope of an event. Envelopes from different scopes are never merged.
type Scope struct {
	OrganizationID, InstallationID, ServerID int64
}

// Envelope is the canonical Champion Live Sync event (task section 8). It carries no secret and no
// raw line beyond a bounded, redacted Evidence excerpt.
type Envelope struct {
	EventID  string
	Scope    Scope
	Family   string
	SourceID string // canonical source identity (see CanonicalSourceID)
	BootID   string // server boot/session identity, when known
	// PlayerSessionID is set only on player-scoped events that identify a session.
	PlayerSessionID string
	// SourceLocalTime is the server-local wall time stated in the line (DayZ writes no zone), or nil.
	SourceLocalTime *time.Time
	// SourceUTC is set only when the source states its own UTC offset (restart.log does).
	SourceUTC  *time.Time
	ObservedAt time.Time // when Champion read the bytes
	IngestedAt time.Time // when Champion accepted the event
	Offset     int64     // byte offset of the end of the record in the source (stable record identity)
	Category   string
	Payload    map[string]string
	Parser     string
	Evidence   string // redacted excerpt, at most 240 characters
	Status     string
}

// NewEventID is deterministic: the same record (scope + source + offset + category) always gets the
// same id, which makes replays idempotent.
func NewEventID(s Scope, sourceID string, offset int64, category string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%d|%s|%d|%s", s.OrganizationID, s.InstallationID, s.ServerID, sourceID, offset, category)))
	return hex.EncodeToString(h[:16])
}

// Record is one parsed source record before it is scoped into an Envelope.
type Record struct {
	Offset          int64
	Category        string
	Status          string
	SourceLocalTime *time.Time
	SourceUTC       *time.Time
	Payload         map[string]string
	Evidence        string
}

// Envelope scopes a record. ingested is the acceptance time; observed the read time.
func (r Record) Envelope(s Scope, family, sourceID, bootID string, observed, ingested time.Time) Envelope {
	return Envelope{
		EventID: NewEventID(s, sourceID, r.Offset, r.Category), Scope: s, Family: family, SourceID: sourceID, BootID: bootID,
		SourceLocalTime: r.SourceLocalTime, SourceUTC: r.SourceUTC, ObservedAt: observed, IngestedAt: ingested,
		Offset: r.Offset, Category: r.Category, Payload: r.Payload, Parser: ParserVersion, Evidence: r.Evidence, Status: r.Status,
	}
}
