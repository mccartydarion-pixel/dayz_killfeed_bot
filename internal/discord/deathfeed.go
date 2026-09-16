package discord

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// BuildDeathEmbed renders one embed for a PLAYER_DEATH or SUICIDE_ACTION event.
// Unlike a PLAYER_KILL there is no killer to credit, so this only ever reports
// facts about the deceased player - never a fabricated cause.
func BuildDeathEmbed(ev *killfeed.Event) *discordgo.MessageEmbed {
	if ev == nil {
		return nil
	}

	player := "Unknown"
	if ev.Player != nil && ev.Player.Name != "" {
		player = sanitizeName(ev.Player.Name)
	}

	title := "☠️ PLAYER DEATH"
	color := ColorNeutralGraphite
	if ev.Type == killfeed.EventSuicideAction {
		title = "💀 SUICIDE"
		color = ColorWarningOrange
	}

	details := make([]string, 0, 3)
	if ev.Weapon != "" {
		details = append(details, "**Cause**  "+safeTrunc(ev.Weapon, maxWeaponLen))
	}
	if ev.HitZone != "" {
		details = append(details, "**Hit Zone**  "+safeTrunc(strings.ToUpper(ev.HitZone), 40))
	}

	fields := []*discordgo.MessageEmbedField{}
	if len(details) > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "DETAILS", Value: safeTrunc(strings.Join(details, "\n"), 1000), Inline: false})
	}

	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: player,
		Color:       color,
		Fields:      fields,
		Author: &discordgo.MessageEmbedAuthor{
			Name: "CHAMPION KILLFEED",
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: safeTrunc(presentationFooter(ev), maxFooterLen),
		},
	}
	if !ev.Timestamp.IsZero() {
		embed.Timestamp = ev.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return embed
}

func presentationFooter(ev *killfeed.Event) string {
	if ev != nil && ev.SeasonName != "" {
		return fmt.Sprintf("CHAMPION KILLFEED • %s", sanitizeName(ev.SeasonName))
	}
	return "CHAMPION KILLFEED • EVERY KILL TELLS A STORY"
}

// DeathfeedPublisher sends PLAYER_DEATH/SUICIDE_ACTION events to the configured
// death-feed channel. Publish failures are logged and never stop log processing.
type DeathfeedPublisher struct {
	client  *Client
	store   SetupStore
	guildID string
}

// NewDeathfeedPublisher creates a publisher bound to the guild setup store.
func NewDeathfeedPublisher(client *Client, store SetupStore, guildID string) *DeathfeedPublisher {
	return &DeathfeedPublisher{client: client, store: store, guildID: guildID}
}

func (p *DeathfeedPublisher) channelID() string {
	if p.store != nil && p.guildID != "" {
		if setup, err := p.store.Get(p.guildID); err == nil && setup != nil {
			return setup.DeathChannelID
		}
	}
	return ""
}

// PublishDeath sends a death/suicide embed. Only PLAYER_DEATH and
// SUICIDE_ACTION are published; anything else is a no-op.
func (p *DeathfeedPublisher) PublishDeath(ev *killfeed.Event) error {
	if p == nil || p.client == nil || p.client.Session() == nil {
		return fmt.Errorf("discord client not ready")
	}
	if ev == nil || (ev.Type != killfeed.EventPlayerDeath && ev.Type != killfeed.EventSuicideAction) {
		return nil
	}
	channelID := p.channelID()
	if channelID == "" {
		return fmt.Errorf("death feed channel not configured")
	}

	embed := BuildDeathEmbed(ev)
	send := &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{},
		},
	}
	if _, err := p.client.Session().ChannelMessageSendComplex(channelID, send); err != nil {
		slog.Error("component=discord", "msg", "death feed publish failed", "err", err.Error())
		return nil // never propagate; log processing must continue
	}
	slog.Debug("component=discord", "msg", "death feed published", "type", string(ev.Type))
	return nil
}
