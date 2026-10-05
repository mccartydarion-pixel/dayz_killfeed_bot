package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

func TestPublicPanelEmbedsUseChampionBranding(t *testing.T) {
	for name, embed := range map[string]*discordgo.MessageEmbed{
		"link":  LinkUsernameInfoEmbed(),
		"stats": PlayerStatsInfoEmbed(),
	} {
		// The brand lives once, in the author line; the title is the section.
		if embed.Author == nil || embed.Author.Name != presentation.AuthorName {
			t.Errorf("%s is not branded: %#v", name, embed.Author)
		}
		if strings.Contains(strings.ToUpper(embed.Title), "CHAMPION") || embed.Title == "" {
			t.Errorf("%s title should be the section only: %q", name, embed.Title)
		}
		// A static panel has no server and no context to name, so it carries no footer.
		if embed.Footer != nil {
			t.Errorf("%s has nothing to say in a footer: %#v", name, embed.Footer)
		}
		if problems := presentation.CheckEmbed(embed); len(problems) != 0 {
			t.Errorf("%s breaks the design rules: %v", name, problems)
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
