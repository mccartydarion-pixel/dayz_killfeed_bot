package discord

import (
	"context"
	"log/slog"

	"github.com/bwmarrin/discordgo"
)

// Faction Security: when a Base Raid Alarm or Perimeter Watch alert fires for a
// base, the base owner's faction mates (everyone, or only leaders, as the owner
// chose) get the same message, worded for the faction. Off until the server
// owner turns it on; while it is on sale only owners with paid time share.

// FactionShareStore is the repository surface Faction Security needs.
type FactionShareStore interface {
	Recipients(ctx context.Context, installationID, guildID, serverID, ownerPlayerID int64) ([]string, error)
	LogShare(ctx context.Context, installationID, guildID, serverID, baseID int64, source string, sent, failed int) error
}

type factionSharer struct {
	store FactionShareStore
	dm    DMSender
}

// factionShare identifies one alert to pass on.
type factionShare struct {
	route, source                           string
	installationID, guildID, serverID, base int64
	ownerPlayerID                           int64
}

// share DMs each recipient and logs how many were reached. It returns the
// numbers sent and failed.
func (f *factionSharer) share(ctx context.Context, s factionShare, msg *discordgo.MessageSend) (int, int) {
	if f == nil || f.store == nil || f.dm == nil || msg == nil || s.ownerPlayerID <= 0 {
		return 0, 0
	}
	ids, err := f.store.Recipients(ctx, s.installationID, s.guildID, s.serverID, s.ownerPlayerID)
	if err != nil {
		slog.Warn("component=faction_security", "msg", "recipients failed", "base_id", s.base, "err", err.Error())
		return 0, 0
	}
	if len(ids) == 0 {
		return 0, 0
	}
	sent, failed := 0, 0
	for _, id := range ids {
		err := deliver(s.route, "", func() error {
			ch, err := f.dm.UserChannelCreate(id)
			if err != nil {
				return err
			}
			_, err = f.dm.ChannelMessageSendComplex(ch.ID, msg)
			return err
		})
		if err != nil {
			failed++
			continue
		}
		sent++
	}
	if err := f.store.LogShare(ctx, s.installationID, s.guildID, s.serverID, s.base, s.source, sent, failed); err != nil {
		slog.Warn("component=faction_security", "msg", "log share failed", "base_id", s.base, "err", err.Error())
	}
	slog.Info("component=faction_security", "event", "shared", "source", s.source, "base_id", s.base, "sent", sent, "failed", failed)
	return sent, failed
}

// factionVersion copies an owner alert and rewords it for a faction mate.
func factionVersion(msg *discordgo.MessageSend, title, description string) *discordgo.MessageSend {
	if msg == nil || len(msg.Embeds) == 0 || msg.Embeds[0] == nil {
		return nil
	}
	e := *msg.Embeds[0]
	e.Title, e.Description = title, description
	e.Fields = append([]*discordgo.MessageEmbedField(nil), msg.Embeds[0].Fields...)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Shared with you by Faction Security"}
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{&e},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

// FactionRaidMessage is the raid alarm as a faction mate sees it.
func FactionRaidMessage(owner *discordgo.MessageSend, baseName string) *discordgo.MessageSend {
	return factionVersion(owner, "🚨 Someone is breaking into your faction's base "+caseFallback(caseSafeText(baseName, 80), ""),
		"A player who isn't in your faction or on the base's friend list is taking apart a base a faction mate registered.")
}

// FactionPerimeterMessage is the perimeter alert as a faction mate sees it.
func FactionPerimeterMessage(owner *discordgo.MessageSend, baseName string) *discordgo.MessageSend {
	return factionVersion(owner, "👀 Someone is near your faction's base "+caseFallback(caseSafeText(baseName, 80), ""),
		"A player who isn't in your faction or on the base's friend list was seen near a base a faction mate registered.")
}
