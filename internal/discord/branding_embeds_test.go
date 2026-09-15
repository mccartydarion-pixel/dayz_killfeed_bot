package discord

import (
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/presentation"
)

func TestPublicPanelEmbedsUseChampionBranding(t *testing.T) {
	for name, embed := range map[string]struct {
		Title      string
		Color      int
		FooterText string
	}{
		"link":  {LinkUsernameInfoEmbed().Title, LinkUsernameInfoEmbed().Color, LinkUsernameInfoEmbed().Footer.Text},
		"stats": {PlayerStatsInfoEmbed().Title, PlayerStatsInfoEmbed().Color, PlayerStatsInfoEmbed().Footer.Text},
	} {
		if !strings.HasPrefix(embed.Title, "CHAMPION KILLFEED\n") {
			t.Errorf("%s title is not branded: %q", name, embed.Title)
		}
		if embed.FooterText != presentation.ChampionSlogan {
			t.Errorf("%s footer is not standardized: %q", name, embed.FooterText)
		}
	}
}

func TestServerStatusDoesNotExposeServiceID(t *testing.T) {
	embed := ServerStatusEmbed("internal-service-id", "DayZ", "ONLINE")
	for _, field := range embed.Fields {
		if strings.Contains(field.Value, "internal-service-id") || strings.Contains(field.Name, "ID") {
			t.Fatalf("server status exposed an internal identifier: %#v", field)
		}
	}
}
