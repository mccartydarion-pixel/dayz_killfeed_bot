package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

// TraderGuildAPI is the Discord API /trader setup and the trader's shared messages use.
type TraderGuildAPI interface {
	BotID() string
	GuildChannels(guildID string) ([]*discordgo.Channel, error)
	GuildRoles(guildID string) ([]*discordgo.Role, error)
	GuildRoleCreate(guildID string, data *discordgo.RoleParams) (*discordgo.Role, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error)
	// ChannelRename returns a *discordgo.RateLimitError instead of waiting out Discord's limit of
	// two renames per channel every 10 minutes.
	ChannelRename(channelID, name string) (*discordgo.Channel, error)
	ChannelPermissionSet(channelID, targetID string, targetType discordgo.PermissionOverwriteType, allow, deny int64) error
	ChannelPermissionDelete(channelID, targetID string) error
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error)
	ChannelMessageEditComplex(m *discordgo.MessageEdit) (*discordgo.Message, error)
}

// traderSession is TraderGuildAPI over a live session.
type traderSession struct{ s *discordgo.Session }

// NewTraderSession wraps a live session; nil for none.
func NewTraderSession(s *discordgo.Session) TraderGuildAPI {
	if s == nil {
		return nil
	}
	return traderSession{s: s}
}

func (t traderSession) BotID() string {
	if t.s.State != nil && t.s.State.User != nil {
		return t.s.State.User.ID
	}
	return ""
}
func (t traderSession) GuildChannels(guildID string) ([]*discordgo.Channel, error) {
	return t.s.GuildChannels(guildID)
}
func (t traderSession) GuildRoles(guildID string) ([]*discordgo.Role, error) {
	return t.s.GuildRoles(guildID)
}
func (t traderSession) GuildRoleCreate(guildID string, data *discordgo.RoleParams) (*discordgo.Role, error) {
	return t.s.GuildRoleCreate(guildID, data)
}
func (t traderSession) GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {
	return t.s.GuildChannelCreateComplex(guildID, data)
}
func (t traderSession) ChannelRename(channelID, name string) (*discordgo.Channel, error) {
	return t.s.ChannelEditComplex(channelID, &discordgo.ChannelEdit{Name: name}, discordgo.WithRetryOnRatelimit(false))
}
func (t traderSession) ChannelPermissionSet(channelID, targetID string, targetType discordgo.PermissionOverwriteType, allow, deny int64) error {
	return t.s.ChannelPermissionSet(channelID, targetID, targetType, allow, deny)
}
func (t traderSession) ChannelPermissionDelete(channelID, targetID string) error {
	return t.s.ChannelPermissionDelete(channelID, targetID)
}
func (t traderSession) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
	return t.s.ChannelMessageSendComplex(channelID, data)
}
func (t traderSession) ChannelMessageEditComplex(m *discordgo.MessageEdit) (*discordgo.Message, error) {
	return t.s.ChannelMessageEditComplex(m)
}

// Channel names /trader setup creates.
const (
	traderCategoryName = "🛒 Trader"
	traderRoleName     = "Trader"
)

// Permission sets of the Trader channels.
const (
	traderReadPerms = discordgo.PermissionViewChannel | discordgo.PermissionReadMessageHistory
	traderBotPerms  = discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionEmbedLinks |
		discordgo.PermissionReadMessageHistory | discordgo.PermissionManageChannels
	// Players read #trader and #deals and use their buttons; they don't post in them.
	traderNoPosting = discordgo.PermissionSendMessages | discordgo.PermissionSendMessagesInThreads | discordgo.PermissionCreatePublicThreads |
		discordgo.PermissionCreatePrivateThreads | discordgo.PermissionAddReactions
)

func traderBotOverwrite(botID string, allow int64) []*discordgo.PermissionOverwrite {
	if botID == "" {
		return nil
	}
	return []*discordgo.PermissionOverwrite{{ID: botID, Type: discordgo.PermissionOverwriteTypeMember, Allow: allow}}
}

// traderPublicOverwrites: everyone reads, nobody but the bot posts.
func traderPublicOverwrites(guildID, botID string) []*discordgo.PermissionOverwrite {
	return append([]*discordgo.PermissionOverwrite{{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole, Allow: traderReadPerms, Deny: traderNoPosting}},
		traderBotOverwrite(botID, traderBotPerms)...)
}

// traderStaffOverwrites: hidden from everyone but the Trader role and the server's Owner and
// Administrators (their mapped staff roles, and Discord administrators, who see every channel).
func traderStaffOverwrites(guildID, botID string, staff TraderStaff, allow int64) []*discordgo.PermissionOverwrite {
	out := []*discordgo.PermissionOverwrite{{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel}}
	for _, roleID := range staff.RoleIDs {
		out = append(out, &discordgo.PermissionOverwrite{ID: roleID, Type: discordgo.PermissionOverwriteTypeRole, Allow: allow})
	}
	if staff.OwnerDiscordID != "" {
		out = append(out, &discordgo.PermissionOverwrite{ID: staff.OwnerDiscordID, Type: discordgo.PermissionOverwriteTypeMember, Allow: allow})
	}
	return append(out, traderBotOverwrite(botID, traderBotPerms)...)
}

// The Trader role, the Owner and Administrators read #trader-panel and also talk in #trader-orders.
const (
	traderPanelAllow  = traderReadPerms
	traderOrdersAllow = traderReadPerms | discordgo.PermissionSendMessages
)

// traderVoiceOverwrites: everyone sees the status, nobody joins.
func traderVoiceOverwrites(guildID, botID string) []*discordgo.PermissionOverwrite {
	return append([]*discordgo.PermissionOverwrite{{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole, Allow: discordgo.PermissionViewChannel,
		Deny: discordgo.PermissionVoiceConnect | discordgo.PermissionVoiceSpeak}},
		traderBotOverwrite(botID, discordgo.PermissionViewChannel|discordgo.PermissionManageChannels)...)
}

// setup creates (or repairs) the Trader category, channels and role, and posts the trader and its
// panel. Channels and the role it made before are reused while they exist, and the private channels
// are opened to the Trader role and the server's current Owner and Administrators.
func (h *TraderHandler) setup(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, installationID int64, roleID string) {
	servers, err := h.backend.TraderServers(ctx, caller.DiscordGuildID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	if len(servers) == 0 {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: traderNoServersText})
		return
	}
	var server *TraderServer
	for idx := range servers {
		if installationID == 0 && len(servers) == 1 || servers[idx].InstallationID == installationID {
			server = &servers[idx]
		}
	}
	if server == nil {
		if installationID == 0 {
			respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "This Discord server has several game servers. Pick one in the `server` option of `/trader setup`."})
		} else {
			respondPrivate(s, i, &discordgo.InteractionResponseData{Content: traderGoneText})
		}
		return
	}
	level, err := h.backend.TraderLevel(ctx, caller.PanelCaller, server.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	if level < permissions.LevelAdministrator {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: traderSetupForbid})
		return
	}
	if roleID != "" && roleID == caller.DiscordGuildID {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "Pick a role for the people who run the trader, not @everyone."})
		return
	}
	// The plan and the shop are checked before anything is created in Discord.
	if _, err := h.backend.TraderCatalog(ctx, caller.DiscordGuildID, server.InstallationID); err != nil {
		h.reportError(s, i, err)
		return
	}
	hub, err := h.backend.TraderHub(ctx, caller.DiscordGuildID, server.InstallationID)
	if err != nil && !errors.Is(err, ErrTraderNotSetUp) {
		h.reportError(s, i, err)
		return
	}
	isNew := errors.Is(err, ErrTraderNotSetUp)
	if isNew {
		hub = TraderHub{InstallationID: server.InstallationID, DiscordGuildID: caller.DiscordGuildID, ServerName: server.Name, Open: true}
	}
	api := h.apiFor(s)
	if api == nil {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: ReplyIsUnavailable("Trader setup")})
		return
	}
	staff, err := h.backend.TraderStaff(ctx, caller.DiscordGuildID, server.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	made, err := h.ensureChannels(api, &hub, roleID, staff)
	if err != nil {
		slog.Warn("component=discord", "msg", "trader setup failed", "installation_id", hub.InstallationID, "err", err.Error())
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: traderSetupFailedText(err)})
		return
	}
	saved, err := h.backend.TraderSaveHub(ctx, caller.PanelCaller, hub)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	saved.ServerName = server.Name
	if made.voice {
		// A new voice channel already carries the status name.
		if err := h.backend.TraderVoiceApplied(ctx, saved.InstallationID, TraderVoiceName(saved.Open)); err == nil {
			saved.VoiceNameApplied = TraderVoiceName(saved.Open)
		}
	}
	saved = h.publish(ctx, api, saved)
	h.syncVoice(ctx, api, saved)
	respondPrivate(s, i, BuildTraderSetupDone(saved, made.names, isNew))
}

// traderMade is what setup created this time.
type traderMade struct {
	voice bool
	names []string
}

// ensureChannels reuses the hub's channels and role that still exist, creates the rest, and opens
// the private channels that already existed to the Trader role and staff (a Trader role replaced by
// another is taken off them).
func (h *TraderHandler) ensureChannels(api TraderGuildAPI, hub *TraderHub, roleID string, staff TraderStaff) (traderMade, error) {
	var made traderMade
	guildID := hub.DiscordGuildID
	channels, err := api.GuildChannels(guildID)
	if err != nil {
		return made, fmt.Errorf("list channels: %w", err)
	}
	exists := map[string]bool{}
	for _, c := range channels {
		exists[c.ID] = true
	}
	oldRole := hub.TraderRoleID
	switch {
	case roleID != "":
		hub.TraderRoleID = roleID
	case hub.TraderRoleID != "":
		roles, err := api.GuildRoles(guildID)
		if err != nil {
			return made, fmt.Errorf("list roles: %w", err)
		}
		found := false
		for _, r := range roles {
			found = found || r.ID == hub.TraderRoleID
		}
		if !found {
			hub.TraderRoleID = ""
		}
	}
	if hub.TraderRoleID == "" {
		mentionable := false
		role, err := api.GuildRoleCreate(guildID, &discordgo.RoleParams{Name: traderRoleName, Mentionable: &mentionable})
		if err != nil {
			return made, fmt.Errorf("create role: %w", err)
		}
		hub.TraderRoleID = role.ID
		made.names = append(made.names, "the @"+traderRoleName+" role")
	}
	isStaffRole := func(id string) bool {
		for _, r := range staff.RoleIDs {
			if r == id {
				return true
			}
		}
		return false
	}
	if !isStaffRole(hub.TraderRoleID) {
		staff.RoleIDs = append([]string{hub.TraderRoleID}, staff.RoleIDs...)
	}
	for _, ch := range []struct {
		id    string
		allow int64
	}{{hub.PanelChannelID, traderPanelAllow}, {hub.OrdersChannelID, traderOrdersAllow}} {
		if ch.id == "" || !exists[ch.id] {
			continue
		}
		for _, roleID := range staff.RoleIDs {
			if err := api.ChannelPermissionSet(ch.id, roleID, discordgo.PermissionOverwriteTypeRole, ch.allow, 0); err != nil {
				return made, fmt.Errorf("open the trader channels to staff: %w", err)
			}
		}
		if staff.OwnerDiscordID != "" {
			if err := api.ChannelPermissionSet(ch.id, staff.OwnerDiscordID, discordgo.PermissionOverwriteTypeMember, ch.allow, 0); err != nil {
				return made, fmt.Errorf("open the trader channels to the owner: %w", err)
			}
		}
		if oldRole != "" && oldRole != hub.TraderRoleID && !isStaffRole(oldRole) {
			if err := api.ChannelPermissionDelete(ch.id, oldRole); err != nil && !isUnknownMessage(err) {
				slog.Warn("component=discord", "msg", "trader old role overwrite removal failed", "channel_id", ch.id, "err", err.Error())
			}
		}
	}
	botID := api.BotID()
	create := func(id *string, data discordgo.GuildChannelCreateData, label string) error {
		if *id != "" && exists[*id] {
			return nil
		}
		c, err := api.GuildChannelCreateComplex(guildID, data)
		if err != nil {
			return fmt.Errorf("create %s: %w", label, err)
		}
		*id = c.ID
		made.names = append(made.names, label)
		return nil
	}
	if err := create(&hub.CategoryID, discordgo.GuildChannelCreateData{Name: traderCategoryName, Type: discordgo.ChannelTypeGuildCategory,
		PermissionOverwrites: traderBotOverwrite(botID, traderBotPerms)}, "the Trader category"); err != nil {
		return made, err
	}
	voiceBefore := hub.VoiceChannelID
	if err := create(&hub.VoiceChannelID, discordgo.GuildChannelCreateData{Name: TraderVoiceName(hub.Open), Type: discordgo.ChannelTypeGuildVoice,
		ParentID: hub.CategoryID, PermissionOverwrites: traderVoiceOverwrites(guildID, botID)}, "the status voice channel"); err != nil {
		return made, err
	}
	made.voice = hub.VoiceChannelID != voiceBefore
	text := func(id *string, name, topic string, overwrites []*discordgo.PermissionOverwrite) error {
		return create(id, discordgo.GuildChannelCreateData{Name: name, Type: discordgo.ChannelTypeGuildText, Topic: topic, ParentID: hub.CategoryID,
			PermissionOverwrites: overwrites}, "#"+name)
	}
	if err := text(&hub.TraderChannelID, "trader", "Spend your Champion Points: press Browse.", traderPublicOverwrites(guildID, botID)); err != nil {
		return made, err
	}
	if err := text(&hub.DealsChannelID, "deals", "Deals, sales and trader news.", traderPublicOverwrites(guildID, botID)); err != nil {
		return made, err
	}
	if err := text(&hub.PanelChannelID, "trader-panel", "Trader role, Owner and Administrators: open and close the trader.",
		traderStaffOverwrites(guildID, botID, staff, traderPanelAllow)); err != nil {
		return made, err
	}
	if err := text(&hub.OrdersChannelID, "trader-orders", "Trader role, Owner and Administrators: new orders to hand over.",
		traderStaffOverwrites(guildID, botID, staff, traderOrdersAllow)); err != nil {
		return made, err
	}
	return made, nil
}

func traderSetupFailedText(err error) string {
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusForbidden {
		return "Champion couldn't create the Trader channels or role: it needs the **Manage Channels** and **Manage Roles** permissions. Give its role those permissions, then run `/trader setup` again. Anything already created is reused."
	}
	return "Champion couldn't finish setting up the Trader. Run `/trader setup` again; anything already created is reused."
}

// publish edits the trader post and the panel to the hub's current state, posting either again when
// its message is gone, and returns the hub with the message ids it now has.
func (h *TraderHandler) publish(ctx context.Context, api TraderGuildAPI, hub TraderHub) TraderHub {
	cat, err := h.backend.TraderCatalog(ctx, hub.DiscordGuildID, hub.InstallationID)
	if err != nil {
		slog.Warn("component=discord", "msg", "trader catalog failed", "installation_id", hub.InstallationID, "err", err.Error())
		return hub
	}
	postID := upsertTraderMessage(api, hub.TraderChannelID, hub.PostMessageID, BuildTraderPost(hub, cat))
	panelID := upsertTraderMessage(api, hub.PanelChannelID, hub.PanelMessageID, BuildTraderPanel(hub, cat, h.now()))
	if postID != hub.PostMessageID || panelID != hub.PanelMessageID {
		if err := h.backend.TraderSetMessages(ctx, hub.InstallationID, postID, panelID); err != nil {
			slog.Warn("component=discord", "msg", "trader messages save failed", "installation_id", hub.InstallationID, "err", err.Error())
			return hub
		}
		hub.PostMessageID, hub.PanelMessageID = postID, panelID
	}
	return hub
}

// upsertTraderMessage edits messageID in channelID, or posts the message when there is none or it was
// deleted. It returns the message's id ("" when the channel itself can't be posted in).
func upsertTraderMessage(api TraderGuildAPI, channelID, messageID string, data *discordgo.InteractionResponseData) string {
	if channelID == "" {
		return messageID
	}
	if messageID != "" {
		edit := discordgo.NewMessageEdit(channelID, messageID)
		edit.Embeds = &data.Embeds
		edit.Components = &data.Components
		edit.AllowedMentions = data.AllowedMentions
		_, err := api.ChannelMessageEditComplex(edit)
		if err == nil {
			return messageID
		}
		if !isUnknownMessage(err) || isUnknownChannel(err) {
			slog.Warn("component=discord", "msg", "trader message edit failed", "channel_id", channelID, "err", err.Error())
			return messageID
		}
	}
	m, err := api.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Embeds: data.Embeds, Components: data.Components, AllowedMentions: data.AllowedMentions})
	if err != nil {
		slog.Warn("component=discord", "msg", "trader message post failed", "channel_id", channelID, "err", err.Error())
		return messageID
	}
	return m.ID
}

func isUnknownChannel(err error) bool {
	var rest *discordgo.RESTError
	return errors.As(err, &rest) && rest.Message != nil && rest.Message.Code == discordgo.ErrCodeUnknownChannel
}

// syncVoice renames the voice channel to the trader's state. A rename Discord rate-limits is left
// for the refresher, which retries until the name matches.
func (h *TraderHandler) syncVoice(ctx context.Context, api TraderGuildAPI, hub TraderHub) {
	want := TraderVoiceName(hub.Open)
	if hub.VoiceChannelID == "" || hub.VoiceNameApplied == want {
		return
	}
	if _, err := api.ChannelRename(hub.VoiceChannelID, want); err != nil {
		var limited *discordgo.RateLimitError
		if !errors.As(err, &limited) {
			slog.Warn("component=discord", "msg", "trader voice rename failed", "installation_id", hub.InstallationID, "err", err.Error())
		}
		return
	}
	if err := h.backend.TraderVoiceApplied(ctx, hub.InstallationID, want); err != nil {
		slog.Warn("component=discord", "msg", "trader voice name save failed", "installation_id", hub.InstallationID, "err", err.Error())
	}
}

// traderRefreshEvery is how often every trader post, panel and voice channel is brought up to date
// (shop edits on the website, a rename Discord delayed).
const traderRefreshEvery = 2 * time.Minute

// RunRefresh keeps every Trader current. Run it on one process (the singleton leader).
func (h *TraderHandler) RunRefresh(s *discordgo.Session) func(ctx context.Context) {
	return func(ctx context.Context) {
		ticker := time.NewTicker(traderRefreshEvery)
		defer ticker.Stop()
		for {
			h.refreshAll(ctx, h.apiFor(s))
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
}

func (h *TraderHandler) refreshAll(ctx context.Context, api TraderGuildAPI) {
	if h == nil || h.backend == nil || api == nil {
		return
	}
	hubs, err := h.backend.TraderHubs(ctx)
	if err != nil {
		slog.Warn("component=discord", "msg", "trader list failed", "err", err.Error())
		return
	}
	for _, hub := range hubs {
		if ctx.Err() != nil {
			return
		}
		hub = h.publish(ctx, api, hub)
		h.syncVoice(ctx, api, hub)
	}
}

// traderMention is a channel mention, or "" for none.
func traderMention(channelID string) string {
	if strings.TrimSpace(channelID) == "" {
		return ""
	}
	return "<#" + channelID + ">"
}

// traderRoleMention is a role mention, or "Trader" when there is no role.
func traderRoleMention(roleID string) string {
	if strings.TrimSpace(roleID) == "" {
		return traderRoleName
	}
	return "<@&" + roleID + ">"
}
