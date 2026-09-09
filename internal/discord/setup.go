package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Champion structure names. Emoji is included for display; Discord normalizes
// channel names, so we always store and reference the returned IDs.
const (
	CategoryName        = "🏆 CHAMPION KILLFEED"
	ChannelWelcome      = "👋・welcome"
	ChannelServerStatus = "📢・server-status"
	ChannelKillfeed     = "💀・killfeed"
	ChannelLeaderboards = "📊・leaderboards"
	ChannelPlayerStats  = "📈・player-stats"

	// ChannelOnlinePlayersPrefix is the voice-counter prefix; the live count is
	// appended as the channel name (e.g. "🟢・Online Players: 0").
	ChannelOnlinePlayersPrefix = "🟢・Online Players"
)

// onlineVoiceChannelName returns the initial voice counter channel name.
func onlineVoiceChannelName() string { return OnlineCounterName(0) }

// GuildAPI is the subset of Discord guild operations the setup manager needs.
// *discordgo.Session satisfies it in production; tests use fakes.
type GuildAPI interface {
	GuildChannels(guildID string) ([]*discordgo.Channel, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error)
	ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed) (*discordgo.Message, error)
	ChannelMessage(channelID, messageID string) (*discordgo.Message, error)
	Channel(channelID string) (*discordgo.Channel, error)
	ChannelEdit(channelID string, data *discordgo.ChannelEdit) (*discordgo.Channel, error)
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
	return &SetupManager{api: api, store: store, botID: botID}
}

// EnsureConfigured creates the Champion structure for a guild if absent, or
// repairs any missing pieces if partially configured. It is idempotent.
func (m *SetupManager) EnsureConfigured(guildID string) (*GuildSetup, *SetupReport, error) {
	report := &SetupReport{Failed: map[string]string{}}

	existing, err := m.store.Get(guildID)
	if err != nil {
		return nil, nil, fmt.Errorf("read setup store: %w", err)
	}

	setup := &GuildSetup{GuildID: guildID, WelcomeEnabled: true}
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

	// Channels inside the category. The online-players counter is a VOICE channel
	// whose name shows the live count; the rest are bot-output TEXT channels.
	type channelSpec struct {
		name    string
		voice   bool
		assign  func(id string)
		current func() string
		label   string
	}
	specs := []channelSpec{
		{ChannelWelcome, false, func(id string) { setup.WelcomeChannelID = id }, func() string { return setup.WelcomeChannelID }, "welcome"},
		{ChannelServerStatus, false, func(id string) { setup.ServerStatusChannelID = id }, func() string { return setup.ServerStatusChannelID }, "server-status"},
		{ChannelKillfeed, false, func(id string) { setup.KillfeedChannelID = id }, func() string { return setup.KillfeedChannelID }, "killfeed"},
		{onlineVoiceChannelName(), true, func(id string) { setup.OnlinePlayersChannelID = id }, func() string { return setup.OnlinePlayersChannelID }, "online-players"},
		{ChannelLeaderboards, false, func(id string) { setup.LeaderboardsChannelID = id }, func() string { return setup.LeaderboardsChannelID }, "leaderboards"},
		{ChannelPlayerStats, false, func(id string) { setup.PlayerStatsChannelID = id }, func() string { return setup.PlayerStatsChannelID }, "player-stats"},
	}
	for _, spec := range specs {
		currentID := spec.current()
		existing := findChannel(channels, currentID)

		// Legacy migration: an existing TEXT online-players channel must become VOICE.
		if spec.label == "online-players" && existing != nil && existing.Type != discordgo.ChannelTypeGuildVoice {
			// Create the voice counter and repoint the stored ID. The old text channel
			// is left in place (no automatic deletion) and reported for manual cleanup.
			ch, err := m.createChannel(guildID, setup.CategoryID, spec.name, true)
			if err != nil {
				report.Failed[spec.label] = err.Error()
				continue
			}
			setup.OnlinePlayersChannelID = ch.ID
			report.Repaired = append(report.Repaired, "online-players(migrated-to-voice)")
			continue
		}

		if existing != nil {
			report.Existing = append(report.Existing, spec.label)
			continue
		}
		ch, err := m.createChannel(guildID, setup.CategoryID, spec.name, spec.voice)
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

func channelExists(channels []*discordgo.Channel, id string) bool {
	return findChannel(channels, id) != nil
}

// findChannel returns the channel with the given ID, or nil.
func findChannel(channels []*discordgo.Channel, id string) *discordgo.Channel {
	if id == "" {
		return nil
	}
	for _, ch := range channels {
		if ch.ID == id {
			return ch
		}
	}
	return nil
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

func (m *SetupManager) createChannel(guildID, parentID, name string, voice bool) (*discordgo.Channel, error) {
	data := discordgo.GuildChannelCreateData{
		Name:     name,
		ParentID: parentID,
	}
	if voice {
		data.Type = discordgo.ChannelTypeGuildVoice
		data.PermissionOverwrites = m.voiceCounterOverwrites(guildID)
	} else {
		data.Type = discordgo.ChannelTypeGuildText
		data.PermissionOverwrites = m.channelPermissionOverwrites(guildID)
	}
	return m.api.GuildChannelCreateComplex(guildID, data)
}

// voiceCounterOverwrites makes the online counter display-only: everyone can see
// it but nobody can connect or speak; the bot can view and rename (Manage Channels).
func (m *SetupManager) voiceCounterOverwrites(guildID string) []*discordgo.PermissionOverwrite {
	overwrites := []*discordgo.PermissionOverwrite{
		{
			ID:    guildID, // @everyone role ID == guild ID
			Type:  discordgo.PermissionOverwriteTypeRole,
			Allow: discordgo.PermissionViewChannel,
			Deny:  discordgo.PermissionVoiceConnect | discordgo.PermissionVoiceSpeak,
		},
	}
	if m.botID != "" {
		overwrites = append(overwrites, &discordgo.PermissionOverwrite{
			ID:    m.botID,
			Type:  discordgo.PermissionOverwriteTypeMember,
			Allow: discordgo.PermissionViewChannel | discordgo.PermissionManageChannels,
		})
	}
	return overwrites
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
	return setup.CategoryID != "" && setup.WelcomeChannelID != "" && setup.KillfeedChannelID != "" && setup.OnlinePlayersChannelID != ""
}

// ChannelNameSafe returns a Discord-safe channel name (lowercase, no spaces).
// Discord normalizes emoji/unicode; we keep the display form but store IDs.
func ChannelNameSafe(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, " ", "-"))
}
