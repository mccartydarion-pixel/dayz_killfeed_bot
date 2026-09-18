package nitrado

import "strings"

// ConsolePlatform is a stable, persistence-safe identifier for a supported
// DayZ console platform - never a display label (see ClassifyDayZPlatform).
type ConsolePlatform string

const (
	PlatformPlayStation ConsolePlatform = "PLAYSTATION"
	PlatformXbox        ConsolePlatform = "XBOX"
	// PlatformUnsupported covers DayZ PC, non-DayZ services, and any
	// service whose game string doesn't match a known console marker.
	// Champion does not support these in this stage.
	PlatformUnsupported ConsolePlatform = "UNSUPPORTED"
)

// DisplayName returns the customer-facing label for a platform. Backend
// persistence always uses the stable ConsolePlatform value, never this
// string - see docs/SAAS_API.md's platform contract note.
func (p ConsolePlatform) DisplayName() string {
	switch p {
	case PlatformPlayStation:
		return "PlayStation"
	case PlatformXbox:
		return "Xbox"
	default:
		return "Unsupported"
	}
}

// ClassifyDayZPlatform determines which supported DayZ console platform a
// Nitrado service represents, from the authoritative game-identifier field
// Nitrado's own API returns (Service.Game, which UnmarshalJSON already
// resolves from details.game when the top-level game field is absent) -
// never from a user-editable display name like details.name/server_name or
// the details.address/hostname fields (section 6: "do not guess from
// server display name").
//
// Confirmed real payload shape (live Nitrado account, 2026-09-18):
// details.game = "DayZ (PS4)" for a PlayStation service, with a
// corroborating details.folder_short = "dayzps" (also matched below as a
// secondary signal, since it's a stable backend identifier, not a display
// string). No live Xbox DayZ service was available in this account to
// confirm Nitrado's exact Xbox game string; Xbox detection covers Nitrado's
// documented console naming ("xbox", "xbox one", "xbox series", "xb1",
// "xsx") as a best-effort match. If a real Xbox payload is ever observed
// with a different exact string, update xboxGameMarkers/xboxFolderMarkers
// below - this is the single, centralized place platform detection happens.
func ClassifyDayZPlatform(service Service) ConsolePlatform {
	game := strings.ToLower(service.Game)
	if game == "" {
		game = strings.ToLower(service.Details.Game)
	}
	if !strings.Contains(game, "dayz") {
		return PlatformUnsupported
	}

	folder := strings.ToLower(service.Details.FolderShort)

	if containsAny(game, playStationGameMarkers) || containsAny(folder, playStationFolderMarkers) {
		return PlatformPlayStation
	}
	if containsAny(game, xboxGameMarkers) || containsAny(folder, xboxFolderMarkers) {
		return PlatformXbox
	}
	return PlatformUnsupported
}

func containsAny(haystack string, markers []string) bool {
	if haystack == "" {
		return false
	}
	for _, marker := range markers {
		if strings.Contains(haystack, marker) {
			return true
		}
	}
	return false
}

// Game-string markers match Nitrado's full-word console naming (e.g.
// "DayZ (PS4)"). Folder-short markers match Nitrado's short internal
// directory naming (confirmed live: "dayzps" for PlayStation) - a
// different vocabulary, so kept as a separate list rather than reusing the
// game markers.
var playStationGameMarkers = []string{"ps4", "ps5", "playstation"}
var playStationFolderMarkers = []string{"dayzps", "ps4", "ps5"}

var xboxGameMarkers = []string{"xbox", "xb1", "xsx"}
var xboxFolderMarkers = []string{"dayzxb", "xbox", "xb1", "xsx"}
