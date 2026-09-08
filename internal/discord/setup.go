package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Champion structure names. Emoji is included for display; Discord normalizes
// channel names, so we always store and reference the returned IDs.
const (
	CategoryName         = "🏆 CHAMPION KILLFEED"
	ChannelServerStatus  = "📢・server-status"
	ChannelKillfeed      = "💀・killfeed"
	ChannelOnlinePlayers = "🟢・online-players"
	ChannelLeaderboards  = "📊・leaderboards"
	ChannelPlayerStats   = "📈・player-stats"
)

// GuildAPI is the subset of Discord guild operations the setup manager needs.
// *discordgo.Session satisfies it in production; tests use fakes.
type GuildAPI interface {
	GuildChannels(guildID string) ([]*discordgo.Channel, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error)
	ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed) (*discordgo.Message, error)
	ChannelMessage(channelID, messageID string) (*discordgo.Message, error)
	Channel(channelID string) (*discordgo.Channel, error)
}

// SetupReport records which resources were created or already existed.
type SetupReport struct {
	CategoryCreated bool
	Created         []string
	Existing        []string
	Repaired        []string
	Failed          map[string]string
}

// SetupManager creates and maintains the Champion Discord structure for a guild.
type SetupManager struct {
	api   GuildAPI
	store SetupStore
	botID string
}

// NewSetupManager creates a manager bound to a guild API and store.
func NewSetupManager(api GuildAPI, store SetupStore, botID string) *SetupManager {
	return &SetupManager{api: api, store: store}
}

// EnsureConfigured creates the Champion structure for a guild if absent, or
// repairs any missing pieces if partially configured. It is idempotent.
func (m *SetupManager) EnsureConfigured(guildID string) (*GuildSetup, *SetupReport, error) {
	report := &SetupReport{Failed: map[string]string{}}

	existing, err := m.store.Get(guildID)
	if err != nil {
		return nil, nil, fmt.Errorf("read setup store: %w", err)
	}

	setup := &GuildSetup{GuildID: guildID}
	if existing != nil {
		*setup = *existing
	}

	channels, err := m.api.GuildChannels(guildID)
	if err != nil {
		return nil, nil, fmt.Errorf("list guild channels: %w", err)
	}

	// Category
	if setup.CategoryID == "" || !channelExists(channels, setup.CategoryID) {
		cat, err := m.createCategory(guildID, channels)
		if err != nil {
			report.Failed["category"] = err.Error()
			return setup, report, fmt.Errorf("create category: %w", err)
		}
		setup.CategoryID = cat.ID
		report.CategoryCreated = true
	} else {
		report.Existing = append(report.Existing, "category")
	}

	// Re-read channels after category creation so children attach correctly.
	channels, _ = m.api.GuildChannels(guildID)

	// Channels inside the category.
	type channelSpec struct {
		name   string
		assign func(id string)
		label  string
	}
	specs := []channelSpec{
		{ChannelServerStatus, func(id string) { setup.ServerStatusChannelID = id }, "server-status"},
		{ChannelKillfeed, func(id string) { setup.KillfeedChannelID = id }, "killfeed"},
		{ChannelOnlinePlayers, func(id string) { setup.OnlinePlayersChannelID = id }, "online-players"},
		{ChannelLeaderboards, func(id string) { setup.LeaderboardsChannelID = id }, "leaderboards"},
		{ChannelPlayerStats, func(id string) { setup.PlayerStatsChannelID = id }, "player-stats"},
	}
	for _, spec := range specs {
		currentID := channelIDFor(spec, setup)
		if currentID != "" && channelExists(channels, currentID) {
			report.Existing = append(report.Existing, spec.label)
			continue
		}
		ch, err := m.createChannel(guildID, setup.CategoryID, spec.name)
		if err != nil {
			report.Failed[spec.label] = err.Error()
			continue
		}
		spec.assign(ch.ID)
		if currentID == "" {
			report.Created = append(report.Created, spec.label)
		} else {
			report.Repaired = append(report.Repaired, spec.label)
		}
	}

	if err := m.store.Save(*setup); err != nil {
		return setup, report, fmt.Errorf("save setup: %w", err)
	}
	return setup, report, nil
}

func channelIDFor(spec struct {
	name   string
	assign func(id string)
	label  string
}, setup *GuildSetup) string {
	switch spec.label {
	case "server-status":
		return setup.ServerStatusChannelID
	case "killfeed":
		return setup.KillfeedChannelID
	case "online-players":
		return setup.OnlinePlayersChannelID
	case "leaderboards":
		return setup.LeaderboardsChannelID
	case "player-stats":
		return setup.PlayerStatsChannelID
	}
	return ""
}

func channelExists(channels []*discordgo.Channel, id string) bool {
	for _, ch := range channels {
		if ch.ID == id {
			return true
		}
	}
	return false
}

func (m *SetupManager) createCategory(guildID string, channels []*discordgo.Channel) (*discordgo.Channel, error) {
	// Reuse an existing category with the Champion name rather than duplicating.
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildCategory && ch.Name == CategoryName {
			return ch, nil
		}
	}
	cat, err := m.api.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name: CategoryName,
		Type: discordgo.ChannelTypeGuildCategory,
	})
	if err != nil {
		return nil, err
	}
	return cat, nil
}

func (m *SetupManager) createChannel(guildID, parentID, name string) (*discordgo.Channel, error) {
	overwrites := m.channelPermissionOverwrites(guildID)
	data := discordgo.GuildChannelCreateData{
		Name:                 name,
		Type:                 discordgo.ChannelTypeGuildText,
		ParentID:             parentID,
		PermissionOverwrites: overwrites,
	}
	return m.api.GuildChannelCreateComplex(guildID, data)
}

// channelPermissionOverwrites makes these bot-output channels: @everyone (whose
// role ID equals the guild ID) can read but not send; the bot retains full
// send/embed/manage access; administrators are unaffected.
func (m *SetupManager) channelPermissionOverwrites(guildID string) []*discordgo.PermissionOverwrite {
	overwrites := []*discordgo.PermissionOverwrite{
		{
			ID:    guildID, // @everyone role ID == guild ID
			Type:  discordgo.PermissionOverwriteTypeRole,
			Allow: discordgo.PermissionViewChannel | discordgo.PermissionReadMessageHistory,
			Deny:  discordgo.PermissionSendMessages,
		},
	}
	if m.botID != "" {
		overwrites = append(overwrites, &discordgo.PermissionOverwrite{
			ID:   m.botID,
			Type: discordgo.PermissionOverwriteTypeMember,
			Allow: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages |
				discordgo.PermissionEmbedLinks | discordgo.PermissionReadMessageHistory |
				discordgo.PermissionManageMessages,
		})
	}
	return overwrites
}

// IsConfigured reports whether a guild already has a stored, complete setup.
func (m *SetupManager) IsConfigured(guildID string) bool {
	setup, err := m.store.Get(guildID)
	if err != nil || setup == nil {
		return false
	}
	return setup.CategoryID != "" && setup.KillfeedChannelID != "" && setup.OnlinePlayersChannelID != ""
}

// ChannelNameSafe returns a Discord-safe channel name (lowercase, no spaces).
// Discord normalizes emoji/unicode; we keep the display form but store IDs.
func ChannelNameSafe(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, " ", "-"))
}
