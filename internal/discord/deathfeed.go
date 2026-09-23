package discord

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// BuildDeathEmbed renders one embed for a PLAYER_DEATH or SUICIDE_ACTION event.
// Unlike a PLAYER_KILL there is no killer to credit, so this only ever reports
// facts about the deceased player - never a fabricated cause:
//
//	author  CHAMPIONS® KILLFEED
//	title   ☠️ PLAYER DEATH  |  💀 SUICIDE
//	desc    **Player**
//	fields  CAUSE | WEAPON | FINAL HIT      inline, each only when proven
//	        PLAYER STATS                    **1 K** • **50 D** • **0.02 K/D**
//	footer  EVERY KILL TELLS A STORY
func BuildDeathEmbed(ev *killfeed.Event) *discordgo.MessageEmbed {
	if ev == nil {
		return nil
	}

	title := "☠️ PLAYER DEATH"
	color := presentation.NeutralGraphite
	suicide := ev.Type == killfeed.EventSuicideAction
	if suicide {
		title = "💀 SUICIDE"
		color = presentation.WarningAmber
	}

	embed := presentation.NewFeedEmbed(title, color)
	embed.Description = "**" + cardName(ev.Player) + "**"
	embed.Footer.Text = safeTrunc(presentation.SeasonFooterText(ev.SeasonName), maxFooterLen)

	// A proven non-player cause (infected/animal/environment) is the cause. A
	// suicide's weapon is the item used, so it is labelled as such; on any
	// other death a weapon string is the cause the log reported.
	if cause := deathCauseLabel(ev.Cause); cause != "" {
		presentation.AppendFields(embed, presentation.MetricField("CAUSE", cause, true))
	} else if w := strings.TrimSpace(ev.Weapon); w != "" && !suicide {
		presentation.AppendFields(embed, presentation.MetricField("CAUSE", presentation.EscapeMarkdown(presentation.CleanName(w, maxWeaponLen)), true))
	}
	if w := strings.TrimSpace(ev.Weapon); w != "" && suicide {
		presentation.AppendFields(embed, presentation.MetricField("WEAPON", "`"+strings.ReplaceAll(safeTrunc(w, maxWeaponLen), "`", "'")+"`", true))
	}
	if ev.HitZone != "" {
		presentation.AppendFields(embed, presentation.MetricField("FINAL HIT", presentation.EscapeMarkdown(presentation.TitleCase(presentation.CleanName(ev.HitZone, 40))), true))
	}
	if ev.PlayerStats != nil {
		presentation.AppendFields(embed, presentation.MetricField("PLAYER STATS", presentation.CompactStats(ev.PlayerStats.Kills, ev.PlayerStats.Deaths, ev.PlayerStats.KD()), false))
	}
	presentation.StampEmbed(embed, ev.Timestamp)
	return presentation.FitEmbed(embed)
}

// deathCauseLabel names a parser-proven non-player cause. Suicide is already
// the card's title, so it has no separate cause field.
func deathCauseLabel(c killfeed.DeathCause) string {
	switch c {
	case killfeed.DeathCauseInfected:
		return "Infected"
	case killfeed.DeathCauseAnimal:
		return "Animal"
	case killfeed.DeathCauseEnvironment:
		return "Environment"
	default:
		return ""
	}
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
