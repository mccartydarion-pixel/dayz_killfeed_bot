package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestOfflineCASEPreviewIsDeterministicAndClearlySynthetic(t *testing.T) {
	var first, second bytes.Buffer
	if err := writePreview(&first); err != nil {
		t.Fatal(err)
	}
	if err := writePreview(&second); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatal("preview must not contain live timestamps or mutable data")
	}
	var embed discordgo.MessageEmbed
	if err := json.Unmarshal(first.Bytes(), &embed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(embed.Title, "DEMO ONLY") || !strings.Contains(embed.Description, "NOT A REAL PLAYER OR DETECTION") {
		t.Fatalf("offline preview must be unmistakably synthetic: %+v", embed)
	}
	if len(embed.Fields) != 8 {
		t.Fatalf("unexpected field count %d", len(embed.Fields))
	}
	for _, f := range embed.Fields {
		if f.Name == "Enforcement" && f.Value != "DISABLED" {
			t.Fatal("enforcement was enabled")
		}
		if f.Name == "Detector status" && f.Value != "BLOCKED" {
			t.Fatal("movement was unblocked")
		}
	}
	if strings.Contains(first.String(), "@everyone") || strings.Contains(first.String(), "discord.com/api/webhooks/") {
		t.Fatal("preview must contain no mentions or webhook")
	}
}
