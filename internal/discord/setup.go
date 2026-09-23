package discord

import (
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ChannelOnlinePlayersPrefix is the voice-counter prefix; the live count is
// appended as the channel name (e.g. "🟢・Online Players: 0").
const ChannelOnlinePlayersPrefix = "🟢・Online Players"

// GuildAPI is the subset of Discord guild operations the setup manager needs.
// *discordgo.Session satisfies it in production; tests use fakes.
type GuildAPI interface {
	GuildChannels(guildID string) ([]*discordgo.Channel, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error)
	ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed) (*discordgo.Message, error)
	ChannelMessageSendComplex(channelID string, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error)
	ChannelMessageEditComplex(channelID, messageID string, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error)
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

// SetupManager maintains the legacy per-guild panels (GuildSetup) for guilds
// that still run on the legacy channels. It never creates a channel: every
// Champion channel is created by the one Channel System V2 layout engine
// (internal/app saas_channel_layout.go), which Discord /setup also uses.
type SetupManager struct {
	api   GuildAPI
	store SetupStore
	botID string

	// routed reports whether a route key is served by installation routes for
	// this guild. When true the matching legacy panel message is not created,
	// so a routed feature is never also posted in its legacy channel.
	routed func(routeKey string) bool
}

// SetRouteGate attaches the "is this feature routed?" check (RouteSyncer.HasRoute).
// Optional: unset behaves exactly as before routes existed.
func (m *SetupManager) SetRouteGate(fn func(routeKey string) bool) {
	if m != nil {
		m.routed = fn
	}
}

func (m *SetupManager) hasRoute(routeKey string) bool {
	return m.routed != nil && m.routed(routeKey)
}

// NewSetupManager creates a manager bound to a guild API and store.
func NewSetupManager(api GuildAPI, store SetupStore, botID string) *SetupManager {
	return &SetupManager{api: api, store: store, botID: botID}
}

// RestoreLegacyPanels re-posts a missing legacy panel (leaderboard, player
// stats, link) into its existing legacy channel, for a guild whose feature is
// not routed. A guild with no legacy setup, or a legacy channel that no longer
// exists, is left alone - nothing is ever created. Idempotent.
func (m *SetupManager) RestoreLegacyPanels(guildID string) (*GuildSetup, *SetupReport, error) {
	report := &SetupReport{Failed: map[string]string{}}
	existing, err := m.store.Get(guildID)
	if err != nil {
		return nil, nil, fmt.Errorf("read setup store: %w", err)
	}
	if existing == nil {
		return nil, report, nil
	}
	setup := &GuildSetup{}
	*setup = *existing

	channels, err := m.api.GuildChannels(guildID)
	if err != nil {
		return nil, nil, fmt.Errorf("list guild channels: %w", err)
	}
	// A deleted legacy channel is dropped rather than recreated.
	for _, field := range []*string{&setup.LeaderboardsChannelID, &setup.PlayerStatsChannelID, &setup.LinkPanelChannelID} {
		if *field != "" && !channelExists(channels, *field) {
			*field = ""
		}
	}
	if setup.LeaderboardsChannelID == "" {
		setup.LeaderboardMessageID = ""
	}
	if setup.PlayerStatsChannelID == "" {
		setup.PlayerStatsInfoMessageID = ""
	}
	if setup.LinkPanelChannelID == "" {
		setup.LinkPanelMessageID = ""
	}

	// Create each persistent panel exactly once. Stored IDs are reused on restart;
	// missing messages are recreated by setup/repair.
	if setup.LeaderboardsChannelID != "" && setup.LeaderboardMessageID != "" {
		if _, err := m.api.ChannelMessage(setup.LeaderboardsChannelID, setup.LeaderboardMessageID); err != nil {
			setup.LeaderboardMessageID = ""
			report.Repaired = append(report.Repaired, "leaderboard-message")
		}
	}
	if setup.PlayerStatsChannelID != "" && setup.PlayerStatsInfoMessageID != "" {
		if _, err := m.api.ChannelMessage(setup.PlayerStatsChannelID, setup.PlayerStatsInfoMessageID); err != nil {
			setup.PlayerStatsInfoMessageID = ""
			report.Repaired = append(report.Repaired, "player-stats-message")
		}
	}
	if setup.LinkPanelChannelID != "" && setup.LinkPanelMessageID != "" {
		if _, err := m.api.ChannelMessage(setup.LinkPanelChannelID, setup.LinkPanelMessageID); err != nil {
			setup.LinkPanelMessageID = ""
			report.Repaired = append(report.Repaired, "link-username-message")
		}
	}
	if setup.LeaderboardsChannelID != "" && setup.LeaderboardMessageID == "" && !m.hasRoute(routeKeyAutoLeaderboard) {
		msg, err := m.api.ChannelMessageSendEmbed(setup.LeaderboardsChannelID, BuildLeaderboardEmbed(LeaderboardSnapshot{GeneratedAt: time.Now()}, DefaultLeaderboardConfig()))
		if err != nil {
			report.Failed["leaderboard-message"] = err.Error()
		} else {
			setup.LeaderboardMessageID = msg.ID
			report.Created = append(report.Created, "leaderboard-message")
		}
	}
	if setup.PlayerStatsChannelID != "" && setup.PlayerStatsInfoMessageID == "" && !m.hasRoute(routeKeyStatsLeaderboards) {
		msg, err := m.api.ChannelMessageSendComplex(setup.PlayerStatsChannelID, PlayerStatsInfoEmbed(), PlayerStatsPanelComponents())
		if err != nil {
			report.Failed["player-stats-message"] = err.Error()
		} else {
			setup.PlayerStatsInfoMessageID = msg.ID
			report.Created = append(report.Created, "player-stats-message")
		}
	}
	if setup.LinkPanelChannelID != "" && setup.LinkPanelMessageID == "" && !m.hasRoute(routeKeyLinkGamertag) {
		msg, err := m.api.ChannelMessageSendComplex(setup.LinkPanelChannelID, LinkUsernameInfoEmbed(), LinkUsernamePanelComponents())
		if err != nil {
			report.Failed["link-username-message"] = err.Error()
		} else {
			setup.LinkPanelMessageID = msg.ID
			report.Created = append(report.Created, "link-username-message")
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

// IsConfigured reports whether a guild already has a stored, complete setup.
func (m *SetupManager) IsConfigured(guildID string) bool {
	setup, err := m.store.Get(guildID)
	if err != nil || setup == nil {
		return false
	}
	return setup.CategoryID != "" && setup.WelcomeChannelID != "" && setup.KillfeedChannelID != "" && setup.OnlinePlayersChannelID != ""
}
