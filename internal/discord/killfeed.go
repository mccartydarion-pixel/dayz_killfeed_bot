package discord

import (
	"fmt"
	"log/slog"
	"math"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// KillfeedPublisher sends authoritative PLAYER_KILL events to the configured
// killfeed channel. Publish failures are logged and never stop log processing.
type KillfeedPublisher struct {
	client    *Client
	channelID string
}

// NewKillfeedPublisher creates a publisher bound to the killfeed channel.
func NewKillfeedPublisher(client *Client, channelID string) *KillfeedPublisher {
	return &KillfeedPublisher{client: client, channelID: channelID}
}

// PublishKill sends a compact competitive embed for an authoritative PLAYER_KILL.
// Player IDs, coordinates, and raw ADM lines are never included. Only fields that
// are actually present are shown. Returns nil even on failure (errors are logged).
func (p *KillfeedPublisher) PublishKill(ev *killfeed.Event) error {
	if p == nil || p.client == nil || p.client.Session() == nil {
		return fmt.Errorf("discord client not ready")
	}
	if p.channelID == "" {
		return fmt.Errorf("KILLFEED_CHANNEL_ID not configured")
	}
	if ev == nil || ev.Type != killfeed.EventPlayerKill {
		return nil // only authoritative kills are published in Phase 3.0
	}

	victim := "Unknown"
	if ev.Victim != nil && ev.Victim.Name != "" {
		victim = ev.Victim.Name
	}
	killer := "Unknown"
	if ev.Killer != nil && ev.Killer.Name != "" {
		killer = ev.Killer.Name
	}

	fields := []*discordgo.MessageEmbedField{}
	if ev.Weapon != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Weapon", Value: ev.Weapon, Inline: true})
	}
	if ev.Distance != nil {
		// Round to one decimal place for display; full precision stays internal.
		rounded := math.Round(*ev.Distance*10) / 10
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Distance", Value: fmt.Sprintf("%.1fm", rounded), Inline: true})
	}

	embed := &discordgo.MessageEmbed{
		Title:       "🏆 CHAMPION KILLFEED",
		Description: fmt.Sprintf("💀 **%s** was killed by ⚔️ **%s**", victim, killer),
		Color:       0xC0392B,
		Fields:      fields,
	}

	if _, err := p.client.Session().ChannelMessageSendEmbed(p.channelID, embed); err != nil {
		slog.Error("component=discord", "msg", "killfeed publish failed", "err", err.Error())
		return nil // never propagate; log processing must continue
	}
	slog.Debug("component=discord", "msg", "killfeed published", "victim", victim, "killer", killer)
	return nil
}
