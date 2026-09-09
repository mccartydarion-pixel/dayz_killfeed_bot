package discord

import (
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// KillfeedPublisher sends authoritative PLAYER_KILL events to the configured
// killfeed channel. Publish failures are logged and never stop log processing.
// The channel resolves from the stored guild setup first, then the env fallback.
type KillfeedPublisher struct {
	client   *Client
	store    SetupStore
	guildID  string
	fallback string // legacy KILLFEED_CHANNEL_ID env fallback
}

// NewKillfeedPublisher creates a publisher. channelID is the legacy env fallback;
// the stored GuildSetup.KillfeedChannelID takes priority when present.
func NewKillfeedPublisher(client *Client, channelID string) *KillfeedPublisher {
	return &KillfeedPublisher{client: client, fallback: channelID}
}

// BindStore attaches the setup store and guild so the configured channel wins.
func (p *KillfeedPublisher) BindStore(store SetupStore, guildID string) {
	if p == nil {
		return
	}
	p.store = store
	p.guildID = guildID
}

// channelID resolves the active killfeed channel: stored setup first, then env.
func (p *KillfeedPublisher) channelID() string {
	if p.store != nil && p.guildID != "" {
		if setup, err := p.store.Get(p.guildID); err == nil && setup != nil && setup.KillfeedChannelID != "" {
			return setup.KillfeedChannelID
		}
	}
	return p.fallback
}

// PublishKill sends a compact competitive embed for an authoritative PLAYER_KILL.
// Player IDs, coordinates, and raw ADM lines are never included. Only fields that
// are actually present are shown. Returns nil even on failure (errors are logged).
func (p *KillfeedPublisher) PublishKill(ev *killfeed.Event) error {
	if p == nil || p.client == nil || p.client.Session() == nil {
		return fmt.Errorf("discord client not ready")
	}
	channelID := p.channelID()
	if channelID == "" {
		return fmt.Errorf("killfeed channel not configured")
	}
	if ev == nil || ev.Type != killfeed.EventPlayerKill {
		return nil // only authoritative kills are published in Phase 3.0
	}

	// Build the style-aware Champion embed (one kill = one embed).
	embed := BuildKillEmbed(ev)

	// Suppress all mentions: no @everyone/@here/role/user pings from player names.
	send := &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{}, // parse nothing
		},
	}

	victim, killer := "", ""
	if ev.Victim != nil {
		victim = ev.Victim.Name
	}
	if ev.Killer != nil {
		killer = ev.Killer.Name
	}

	if _, err := p.client.Session().ChannelMessageSendComplex(channelID, send); err != nil {
		slog.Error("component=discord", "msg", "killfeed publish failed", "err", err.Error())
		return nil // never propagate; log processing must continue
	}
	slog.Debug("component=discord", "msg", "killfeed published", "victim", victim, "killer", killer)
	return nil
}
