package discord

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestWelcomePresetChampion(t *testing.T) {
	cfg := repository.WelcomeConfig{GuildID: 42}
	cfgPtr := welcomePresetConfig("champion", cfg)
	if cfgPtr == nil || cfgPtr.MessageText == "" || cfgPtr.TitleText == "" || cfgPtr.FooterText == "" {
		t.Fatal("expected preset to populate message, title, and footer text")
	}
	if cfgPtr.Color == nil || *cfgPtr.Color != ColorChampionGold {
		t.Fatal("expected champion preset to use the Champion gold accent")
	}
}

func TestWelcomeColorFromValue(t *testing.T) {
	for value, want := range map[string]int{
		"gold":   ColorChampionGold,
		"red":    ColorDangerRed,
		"green":  ColorSuccessGreen,
		"blue":   ColorInfoBlue,
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
