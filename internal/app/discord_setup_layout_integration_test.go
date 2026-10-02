//go:build integration

package app

import (
	"context"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// /setup run and repair write every installation's routes, sync the panels once, then verify. Run
// twice, they must build the full layout once and never duplicate a channel.
func TestDiscordSetupLayoutIsIdempotent(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	ctx := context.Background()

	for run := 1; run <= 2; run++ {
		out, err := a.DiscordSetupLayout(ctx, fixture.DiscordGuildID)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if out.Installations != 1 {
			t.Fatalf("run %d: %d installations, want 1", run, out.Installations)
		}
	}
	total := 0
	for _, ch := range verifier.channels[fixture.DiscordGuildID] {
		if ch.Type == discordgo.ChannelTypeGuildText || ch.Type == discordgo.ChannelTypeGuildVoice {
			total++
		}
	}
	if total != championActiveDestinationCount {
		t.Fatalf("expected exactly %d text/voice channels after two /setup runs, got %d", championActiveDestinationCount, total)
	}
}
