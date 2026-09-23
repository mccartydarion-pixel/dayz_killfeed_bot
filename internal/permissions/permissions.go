// Package permissions implements Champion's tenant administration permission model (Client Admin
// Control Plane Phase 1, docs/CLIENT_ADMIN.md). This is a capability-based system with four fixed
// levels (OWNER > ADMINISTRATOR > MODERATOR > GATEKEEPER), each level automatically holding every
// capability the levels below it hold. A Client maps their own Discord roles to these levels
// (internal/repository.PermissionsRepository); the level -> capability mapping itself is fixed by
// Champion, not customer-configurable.
//
// This is deliberately separate from two other, unrelated authorization concepts already in this
// codebase:
//   - Champion's own platform-owner access (internal/app admin_api.go's requirePlatformAdmin) -
//     that gates Champion's own founder/staff access to every tenant, never a Client's own admins.
//   - An organization's flat OWNER/ADMIN/MEMBER membership role (internal/repository.RoleOwner
//     etc.) - that gates the website onboarding/billing/settings flows built in earlier phases.
//     A Client's organization owner is always bootstrapped to Level OWNER here (see
//     internal/app's actor-resolution helper), but every other Level requires an explicit Discord
//     role mapping - organization ADMIN/MEMBER roles grant nothing here on their own.
package permissions

import (
	"sort"
	"strings"
)

// Level is a Champion tenant-administration permission level. Levels are ordered: a higher Level
// numerically is a strict superset of every capability a lower Level has (Allows below implements
// this as a single threshold comparison, not a set union, since the task's own hierarchy is
// simple and total - every capability has exactly one minimum required Level).
type Level int

const (
	LevelNone          Level = 0
	LevelGatekeeper    Level = 1
	LevelModerator     Level = 2
	LevelAdministrator Level = 3
	LevelOwner         Level = 4
)

var levelNames = map[Level]string{
	LevelGatekeeper:    "GATEKEEPER",
	LevelModerator:     "MODERATOR",
	LevelAdministrator: "ADMINISTRATOR",
	LevelOwner:         "OWNER",
}

var namesToLevel = map[string]Level{
	"GATEKEEPER":    LevelGatekeeper,
	"MODERATOR":     LevelModerator,
	"ADMINISTRATOR": LevelAdministrator,
	"OWNER":         LevelOwner,
}

// String returns the level's stable wire name, or "" for LevelNone.
func (l Level) String() string { return levelNames[l] }

// ParseLevel validates a level name exactly as stored/transmitted (case-sensitive, matching the
// migration's CHECK constraint values). An unrecognized value fails closed to (LevelNone, false) -
// never guessed at or defaulted to some other level.
func ParseLevel(raw string) (Level, bool) {
	l, ok := namesToLevel[strings.TrimSpace(raw)]
	return l, ok
}

// Capability is a stable permission key, checked by handlers at the call site - never inferred
// from a UI label (task: "individual capabilities should ultimately be checked by permission key,
// not just UI labels").
type Capability string

const (
	CapPermissionsView    Capability = "PERMISSIONS_VIEW"
	CapPermissionsManage  Capability = "PERMISSIONS_MANAGE"
	CapEconomyView        Capability = "ECONOMY_VIEW"
	CapWarningsView       Capability = "WARNINGS_VIEW"
	CapWarningsClear      Capability = "WARNINGS_CLEAR"
	CapFactionModerate    Capability = "FACTION_MODERATE"
	CapFactionDissolve    Capability = "FACTION_DISSOLVE"
	CapBountyManage       Capability = "BOUNTY_MANAGE"
	CapPlayerStatsReset   Capability = "PLAYER_STATS_RESET"
	CapServerStatsReset   Capability = "SERVER_STATS_RESET"
	CapServerRestart      Capability = "SERVER_RESTART"
	CapServerStop         Capability = "SERVER_STOP"
	CapServerAutostart    Capability = "SERVER_AUTOSTART"
	CapServerNameEdit     Capability = "SERVER_NAME_EDIT"
	CapWhitelistManage    Capability = "WHITELIST_MANAGE"
	CapBanlistManage      Capability = "BANLIST_MANAGE"
	CapPlayerLastOnline   Capability = "PLAYER_LAST_ONLINE_VIEW"
	CapFeedLocationManage Capability = "FEED_LOCATION_MANAGE"
	CapMaintenanceMode    Capability = "MAINTENANCE_MODE"
	// Champion Phase 3 (docs/PLAYER_INTELLIGENCE.md): the authoritative player directory and
	// location-history foundation. Location access is privileged (task section 10) - both
	// location-viewing capabilities sit at Administrator, one level above the plain directory
	// listing/last-online view.
	CapPlayerDirectoryView    Capability = "PLAYER_DIRECTORY_VIEW"
	CapPlayerLastLocationView Capability = "PLAYER_LAST_LOCATION_VIEW"
	CapPlayerLocationView     Capability = "PLAYER_LOCATION_VIEW"
	// Champion Phase 4 (docs/ZONES_UAV_RADAR.md): zones plus the UAV/Base Radar intrusion engine.
	// UAV_MANAGE is deliberately its own, higher-than-ZONE_MANAGE capability: creating/editing a
	// zone whose type is UAV or BASE_RADAR requires it IN ADDITION to ZONE_MANAGE (an Administrator
	// can manage ordinary zones but not UAV/Base Radar ones without also holding Owner).
	CapZoneView         Capability = "ZONE_VIEW"
	CapZoneManage       Capability = "ZONE_MANAGE"
	CapZoneIgnoreManage Capability = "ZONE_IGNORE_MANAGE"
	CapUAVManage        Capability = "UAV_MANAGE"
	CapIntrusionAck     Capability = "INTRUSION_ACK"
)

// requiredLevel is the default minimum Level each capability needs (task's "DEFAULT ROLE
// PERMISSIONS" and each capability's own "Default permission" line). Not customer-configurable
// this phase - only the Discord-role -> Level mapping is (task: "Client should be allowed to
// customize the mapping later" refers to that mapping, not this table).
var requiredLevel = map[Capability]Level{
	CapPermissionsView:        LevelModerator,
	CapPermissionsManage:      LevelModerator, // escalation ceiling enforced separately by CanGrant
	CapEconomyView:            LevelModerator,
	CapWarningsView:           LevelModerator,
	CapWarningsClear:          LevelAdministrator,
	CapFactionModerate:        LevelModerator,
	CapFactionDissolve:        LevelAdministrator,
	CapBountyManage:           LevelModerator,
	CapPlayerStatsReset:       LevelAdministrator,
	CapServerStatsReset:       LevelOwner,
	CapServerRestart:          LevelModerator,
	CapServerStop:             LevelAdministrator,
	CapServerAutostart:        LevelAdministrator,
	CapServerNameEdit:         LevelAdministrator,
	CapWhitelistManage:        LevelGatekeeper,
	CapBanlistManage:          LevelModerator,
	CapPlayerLastOnline:       LevelModerator,
	CapFeedLocationManage:     LevelModerator,
	CapMaintenanceMode:        LevelAdministrator,
	CapPlayerDirectoryView:    LevelModerator,
	CapPlayerLastLocationView: LevelAdministrator,
	CapPlayerLocationView:     LevelAdministrator,
	CapZoneView:               LevelModerator,
	CapZoneManage:             LevelAdministrator,
	CapZoneIgnoreManage:       LevelAdministrator,
	CapUAVManage:              LevelOwner,
	CapIntrusionAck:           LevelModerator,
}

// Allows reports whether actorLevel satisfies capability's required minimum Level. An unknown
// capability key always returns false (fail closed) rather than silently granting access to a
// typo'd or future key nothing here recognizes yet.
func Allows(actorLevel Level, capability Capability) bool {
	need, ok := requiredLevel[capability]
	if !ok || actorLevel == LevelNone {
		return false
	}
	return actorLevel >= need
}

// RequiredLevel returns the minimum Level capability needs and whether it is a known key.
func RequiredLevel(capability Capability) (Level, bool) {
	l, ok := requiredLevel[capability]
	return l, ok
}

// CanGrant reports whether an actor at actorLevel may create or modify a Discord-role mapping
// that grants targetLevel (task's "PERMISSION ESCALATION SAFETY": an actor can only grant a level
// at or below their own ceiling - a Moderator can never grant Owner, and nobody without a level at
// all can grant anything). This must gate every permission-mapping write; never trust the caller's
// own claimed level from the frontend.
func CanGrant(actorLevel, targetLevel Level) bool {
	return actorLevel != LevelNone && targetLevel != LevelNone && targetLevel <= actorLevel
}

// CapabilitiesForLevel returns every capability actorLevel satisfies (task's "CAPABILITY LIST":
// derived from the single fixed requiredLevel table, never a second hand-maintained list), sorted
// alphabetically for a stable, diff-friendly wire response. LevelNone always returns an empty,
// non-nil slice.
func CapabilitiesForLevel(level Level) []Capability {
	out := make([]Capability, 0, len(requiredLevel))
	if level == LevelNone {
		return out
	}
	for capability, need := range requiredLevel {
		if level >= need {
			out = append(out, capability)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
