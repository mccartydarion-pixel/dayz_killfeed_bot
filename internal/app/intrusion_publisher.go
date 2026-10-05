package app

import (
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// Champion Phase 4 (docs/ZONES_UAV_RADAR.md): the two small adapters that let
// internal/killfeed's zone/intrusion engine - which has no Discord dependency of its own, matching
// this codebase's existing internal/killfeed <-> internal/discord layering - reach live Discord
// role data and actually send an alert, without internal/killfeed importing internal/discord.

// intrusionRoleChecker adapts App's cached Discord role resolution (the same 15s-TTL cache Client
// Admin actor-Level resolution already uses - saas_api_permissions.go's memberRolesCached) to
// killfeed.RoleChecker, for DISCORD_ROLE zone-ignore entries only.
type intrusionRoleChecker struct{ app *App }

func (c intrusionRoleChecker) MemberHasRole(discordGuildID, discordUserID, roleID string) (bool, error) {
	roles, err := c.app.memberRolesCached(discordGuildID, discordUserID)
	if err != nil {
		return false, err
	}
	for _, r := range roles {
		if r == roleID {
			return true, nil
		}
	}
	return false, nil
}

// intrusionPublisher adapts App to killfeed.IntrusionPublisher. Every event first gets a stable,
// structured operational log line (task section 18: "internal operational event emission...for
// future Live Ops integration" - this codebase has no existing internal event bus, so a
// consistently-shaped slog line, keyed on event/zone_id/player_id, is the concrete, honest
// implementation of that requirement this phase; a future Live Ops consumer can tail/parse it, or
// this type can grow a second sink later without any caller-visible change). A Discord embed is
// additionally sent only when the event is not alert-suppressed AND the zone has an
// alert_channel_id configured (task section 17: "never invent a fallback channel" - a zone with no
// configured channel only ever produces the log line, nothing is ever posted anywhere).
type intrusionPublisher struct{ app *App }

func (p intrusionPublisher) PublishIntrusionEvent(ev killfeed.IntrusionEvent) {
	slog.Info("component=zone_intrusion", "event", string(ev.Kind), "zone_id", ev.Zone.ID, "zone_name", ev.Zone.Name,
		"zone_type", ev.Zone.ZoneType, "installation_id", ev.Zone.InstallationID, "player_id", ev.PlayerID,
		"gamertag", ev.Gamertag, "intrusion_id", ev.IntrusionID, "suppressed", ev.Suppressed)
	// Staff copy on the installation's ADMIN_ALERTS route (skipped when that is
	// the zone's own alert channel, so nothing is posted twice).
	if alert, ok := discord.IntrusionAdminAlert(ev); ok {
		p.app.AdminAlerts.Publish(alert)
	}
	p.app.notifyZoneAlertPlayer(ev)
	if ev.Suppressed || ev.Zone.AlertChannelID == nil || *ev.Zone.AlertChannelID == "" {
		return
	}
	if p.app.Discord == nil || p.app.Discord.Session() == nil {
		return
	}
	if _, err := p.app.Discord.Session().ChannelMessageSendEmbed(*ev.Zone.AlertChannelID, buildIntrusionEmbed(ev)); err != nil {
		slog.Warn("component=zone_intrusion", "msg", "discord alert send failed", "zone_id", ev.Zone.ID, "err", err.Error())
	}
}

func buildIntrusionEmbed(ev killfeed.IntrusionEvent) *discordgo.MessageEmbed {
	// Amber for someone inside a protected zone, red for a banned player, neutral for a
	// detection that needs no action, green when they are gone again.
	title, verb, color := "Zone intrusion", "entered", presentation.Amber
	switch ev.Kind {
	case killfeed.AlertUAVIntrusion:
		title, color = "UAV detection", presentation.Neutral
	case killfeed.AlertBaseRadarIntrusion:
		title, color = "Base radar detection", presentation.Neutral
	case killfeed.AlertZoneExit:
		title, verb, color = "Zone exit", "left", presentation.Green
	case killfeed.AlertZoneBanViolation:
		title, color = "Zone ban violation", presentation.Red
	}
	// The player's and the zone's names are written by people: cleaned and markdown-escaped.
	player, zone := orUnknown(ev.Gamertag), presentation.SafeName(ev.Zone.Name, 60)
	embed := &discordgo.MessageEmbed{
		Author:      presentation.BrandAuthor("Zones"),
		Title:       title,
		Description: "**" + player + "** " + verb + " zone **" + zone + "**",
		Color:       color,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Zone type", Value: presentation.EnumLabel(presentation.CleanName(ev.Zone.ZoneType, 40)), Inline: true},
			{Name: "Player", Value: player, Inline: true},
		},
	}
	presentation.StampEmbed(embed, ev.At)
	return embed
}
