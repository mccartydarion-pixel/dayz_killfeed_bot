package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// The "your link is verified" DM is a success card, not plain text.
func TestVerifiedMessageIsACard(t *testing.T) {
	at := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	msg := VerifiedMessage(true, at)
	if msg.Content != "" || len(msg.Embeds) != 1 || msg.AllowedMentions == nil || len(msg.AllowedMentions.Parse) != 0 {
		t.Fatalf("message: %+v", msg)
	}
	e := msg.Embeds[0]
	if e.Title != "✅ Verification complete" || e.Color != presentation.Green || e.Timestamp != "2026-10-15T08:00:00.000Z" {
		t.Fatalf("card: %+v", e)
	}
	if !strings.HasPrefix(e.Description, "Your PlayStation account is now verified with Champion.") || !strings.HasSuffix(e.Description, "Your Verified role has been assigned.") {
		t.Fatalf("description: %q", e.Description)
	}
	if strings.Contains(VerifiedMessage(false, at).Embeds[0].Description, "role") {
		t.Fatal("no role assigned: the card does not mention one")
	}
	if problems := presentation.CheckEmbed(e); len(problems) > 0 {
		t.Fatalf("design rules: %v", problems)
	}
}
