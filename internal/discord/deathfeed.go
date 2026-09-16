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
		fields = append(fields, sectionDividerField())
		fields = append(fields, &discordgo.MessageEmbedField{Name: "DEATH DETAILS", Value: safeTrunc(strings.Join(details, "\n"), 1000), Inline: false})
	}
	if ev.PlayerStats != nil {
		value := fmt.Sprintf("Kills: %d\nDeaths: %d\nK/D: %.2f", ev.PlayerStats.Kills, ev.PlayerStats.Deaths, ev.PlayerStats.KD())
		fields = append(fields, sectionDividerField())
		fields = append(fields, &discordgo.MessageEmbedField{Name: "PLAYER STATS", Value: value, Inline: false})
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
		return fmt.Sprintf("🏆 CHAMPION • %s\nEVERY KILL TELLS A STORY", sanitizeName(ev.SeasonName))
	}
	return "CHAMPION KILLFEED • EVERY KILL TELLS A STORY"
}

// DeathfeedPublisher sends PLAYER_DEATH/SUICIDE_ACTION events to the configured
// death-feed channel. Publish failures are logged and never stop log processing.
type DeathfeedPublisher struct {
	client  *Client
	store   SetupStore
	guildID string
	feed    *RotatingFeed
}

// NewDeathfeedPublisher creates a publisher bound to the guild setup store.
func NewDeathfeedPublisher(client *Client, store SetupStore, guildID string) *DeathfeedPublisher {
	return &DeathfeedPublisher{client: client, store: store, guildID: guildID}
}

// SetFeed attaches the rotating batch/cycle feed. When set, PublishDeath
// enqueues into it instead of sending immediately. Optional: unset falls
// back to sending immediately, same as before the rotating feed existed.
func (p *DeathfeedPublisher) SetFeed(feed *RotatingFeed) {
	if p == nil {
		return
	}
	p.feed = feed
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

	// The rotating feed batches embeds and posts them on its own cycle; see
	// RotatingFeed. Without one configured, fall back to an immediate send.
	if p.feed != nil {
		p.feed.Enqueue(embed)
		slog.Debug("component=discord", "msg", "death feed queued", "type", string(ev.Type))
		return nil
	}

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
