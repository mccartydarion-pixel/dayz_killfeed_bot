package app

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// previewSampleNames are the made-up names in the preview data: data, not labels.
var previewSampleNames = []string{
	"Example Server Alpha", "Example Season One", "Example Wolves", "Sample Ravens", "Example Trader", "Supporter Pack", "Gold Supporter",
	"Sample_Raven", "Fictional_Fox", "Made_Up_Moose", "Sniper Sunday", "North West Airfield", "Sakhal", "Chernarus", "Livonia", "Platinum",
}

// TestDesignRules walks the cards this package builds and asserts the rules of
// docs/DISCORD_DESIGN.md (see internal/discord/design_rules_test.go for the other half).
func TestDesignRules(t *testing.T) {
	cards := designPreviewCards()
	if len(cards) < 20 {
		t.Fatalf("the preview set should cover this package's cards, it has only %d", len(cards))
	}
	for _, c := range cards {
		embeds := c.Embeds
		if len(embeds) == 0 && c.Embed != nil {
			embeds = []*discordgo.MessageEmbed{c.Embed}
		}
		for i, e := range embeds {
			for _, problem := range presentation.CheckEmbed(e) {
				t.Errorf("%s (embed %d): %s", c.ID, i, problem)
			}
			if e.Author == nil {
				t.Errorf("%s (embed %d): a card built here carries the brand author line", c.ID, i)
			}
			labels := []string{e.Title}
			for _, f := range e.Fields {
				labels = append(labels, f.Name)
			}
			for _, label := range labels {
				if words := presentation.TitleCaseWords(label, previewSampleNames...); len(words) > 0 {
					t.Errorf("%s (embed %d): label %q is in Title Case (%s); use sentence case", c.ID, i, label, strings.Join(words, ", "))
				}
			}
			// Hand-formatted clock times are not used where the builder has an instant.
			for _, banned := range []string{" UTC on ", "AM UTC", "PM UTC"} {
				if strings.Contains(e.Description, banned) {
					t.Errorf("%s (embed %d): description has a hand-formatted time %q; use presentation.Timestamp", c.ID, i, banned)
				}
			}
		}
	}
}
