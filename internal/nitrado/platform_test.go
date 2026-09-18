package nitrado

import "testing"

// TestClassifyDayZPlatformRealPlayStationPayload uses the exact confirmed
// real Nitrado payload shape (live account, 2026-09-18): details.game =
// "DayZ (PS4)", details.folder_short = "dayzps".
func TestClassifyDayZPlatformRealPlayStationPayload(t *testing.T) {
	service := Service{
		Game: "DayZ (PS4)",
		Details: ServiceDetails{
			Name:        "! Champions® | MAP: The Lost City Deathmatch",
			Game:        "DayZ (PS4)",
			FolderShort: "dayzps",
		},
	}
	if got := ClassifyDayZPlatform(service); got != PlatformPlayStation {
		t.Fatalf("expected PLAYSTATION, got %s", got)
	}
}

// TestClassifyDayZPlatformXboxNaming covers the documented/expected Nitrado
// Xbox naming patterns (no live Xbox payload was available to confirm the
// exact string - see platform.go's doc comment).
func TestClassifyDayZPlatformXboxNaming(t *testing.T) {
	cases := []string{"DayZ (XBOX)", "DayZ (Xbox One)", "DayZ (Xbox Series X)", "DayZ (XB1)", "dayz (xsx)"}
	for _, game := range cases {
		t.Run(game, func(t *testing.T) {
			service := Service{Game: game}
			if got := ClassifyDayZPlatform(service); got != PlatformXbox {
				t.Fatalf("expected XBOX for game=%q, got %s", game, got)
			}
		})
	}
}

// TestClassifyDayZPlatformFolderShortFallback proves the corroborating
// folder_short signal works even when the game string itself is ambiguous
// (defensive: real payloads always have both, but the classifier should
// not depend solely on one field being present).
func TestClassifyDayZPlatformFolderShortFallback(t *testing.T) {
	service := Service{
		Game:    "DayZ",
		Details: ServiceDetails{FolderShort: "dayzps"},
	}
	if got := ClassifyDayZPlatform(service); got != PlatformPlayStation {
		t.Fatalf("expected PLAYSTATION via folder_short fallback, got %s", got)
	}
}

// TestClassifyDayZPlatformRejectsPC is section 1/6: DayZ PC (no console
// marker in either the game string or folder_short) must classify
// UNSUPPORTED, never guessed as a console platform.
func TestClassifyDayZPlatformRejectsPC(t *testing.T) {
	cases := []Service{
		{Game: "DayZ"},
		{Game: "DayZ Standalone"},
		{Details: ServiceDetails{Game: "DayZ", FolderShort: "dayz"}},
	}
	for _, service := range cases {
		if got := ClassifyDayZPlatform(service); got != PlatformUnsupported {
			t.Fatalf("expected UNSUPPORTED for PC-shaped service %+v, got %s", service, got)
		}
	}
}

// TestClassifyDayZPlatformRejectsUnrelatedGame is section 1: non-DayZ
// services (e.g. Minecraft) must never classify as a supported platform,
// even if they happen to mention a console name.
func TestClassifyDayZPlatformRejectsUnrelatedGame(t *testing.T) {
	cases := []Service{
		{Game: "Minecraft (PS4)"},
		{Game: "ARK: Survival Evolved (Xbox)"},
		{Game: ""},
	}
	for _, service := range cases {
		if got := ClassifyDayZPlatform(service); got != PlatformUnsupported {
			t.Fatalf("expected UNSUPPORTED for non-DayZ service %+v, got %s", service, got)
		}
	}
}

func TestConsolePlatformDisplayName(t *testing.T) {
	if PlatformPlayStation.DisplayName() != "PlayStation" {
		t.Fatalf("expected 'PlayStation', got %q", PlatformPlayStation.DisplayName())
	}
	if PlatformXbox.DisplayName() != "Xbox" {
		t.Fatalf("expected 'Xbox', got %q", PlatformXbox.DisplayName())
	}
	if PlatformUnsupported.DisplayName() != "Unsupported" {
		t.Fatalf("expected 'Unsupported', got %q", PlatformUnsupported.DisplayName())
	}
}
