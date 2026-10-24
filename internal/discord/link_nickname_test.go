package discord

import (
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/linking"
)

func TestLinkedNicknameFitsDiscord(t *testing.T) {
	if got := linkedNickname("  RustyAxe_77 "); got != "RustyAxe_77" {
		t.Fatalf("nickname: %q", got)
	}
	long := strings.Repeat("ä", 40)
	if got := linkedNickname(long); len([]rune(got)) != 32 {
		t.Fatalf("a nickname is cut to Discord's 32 characters: %d", len([]rune(got)))
	}
	if linkedNickname("   ") != "" {
		t.Fatal("no name, no nickname")
	}
}

func TestLinkedMessageSaysLinkedNotPending(t *testing.T) {
	msg := linkedMessage(&linking.LinkRecord{RequestedName: "Rusty_Axe", Status: linking.StatusVerified})
	if !strings.Contains(msg, "✅ **Linked**") || !strings.Contains(msg, `Rusty\_Axe`) || strings.Contains(strings.ToLower(msg), "reconnect") {
		t.Fatalf("message: %q", msg)
	}
}
