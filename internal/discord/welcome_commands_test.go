package discord

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestWelcomePresetChampion(t *testing.T) {
	cfg := repository.WelcomeConfig{GuildID: 42}
	cfgPtr := welcomePresetConfig("champion", cfg)
	if cfgPtr == nil || cfgPtr.MessageText == "" || cfgPtr.TitleText == "" || cfgPtr.FooterText == "" {
		t.Fatal("expected preset to populate message, title, and footer text")
	}
	// The Champion preset carries the brand accent, which is crimson (presentation.Crimson).
	if cfgPtr.Color == nil || *cfgPtr.Color != presentation.Crimson {
		t.Fatal("expected champion preset to use the Champion crimson accent")
	}
}

func TestWelcomeColorFromValue(t *testing.T) {
	for value, want := range map[string]int{
		"gold":   ColorChampionGold,
		"red":    ColorDangerRed,
		"green":  ColorSuccessGreen,
		"blue":   welcomeBlue, // the owner's own blue, not a built-in card colour
		"orange": ColorWarningOrange,
	} {
		got := colorFromValue(value)
		if got == nil || *got != want {
			t.Fatalf("colorFromValue(%q) = %v, want %d", value, got, want)
		}
	}
	if colorFromValue("unknown") != nil {
		t.Fatal("unknown color preset should be rejected")
	}
}
