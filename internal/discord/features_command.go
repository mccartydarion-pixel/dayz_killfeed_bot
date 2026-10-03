package discord

import (
	"context"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// FeaturesChannelName is the read-only channel /features creates (docs/FEATURES_CHANNEL.md).
const FeaturesChannelName = "📢・champion-features"

const featuresChannelTopic = "What's new in Champion and how each feature works. Staff turn this channel on or off with /features."

// featuresMessageLimit keeps each posted message under Discord's 6000-character embed total.
const featuresMessageLimit = 5500

// FeaturesStore is what /features needs from the guild table: the guild's category, and the
// channel it created last time.
type FeaturesStore interface {
	GetGuild(ctx context.Context, discordGuildID string) (*repository.GuildRecord, int64, error)
	FeaturesChannel(ctx context.Context, discordGuildID string) (string, error)
	SetFeaturesChannel(ctx context.Context, discordGuildID, channelID string) error
}

// featuresDiscord is the slice of *discordgo.Session /features uses, so tests can fake it.
type featuresDiscord interface {
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelDelete(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// FeaturesCommandHandler serves /features: the first run creates the features channel and posts
// the feature guide in it; the next run deletes that channel. It only ever deletes the channel it
// recorded creating.
type FeaturesCommandHandler struct {
	store FeaturesStore
}

func NewFeaturesCommandHandler(store FeaturesStore) *FeaturesCommandHandler {
	return &FeaturesCommandHandler{store: store}
}

// RegisterFeaturesCommand registers /features for server managers.
func RegisterFeaturesCommand(s CommandRegistrar, guildID string) error {
	appID, err := ApplicationID(s)
	if err != nil {
		return err
	}
	perms := int64(discordgo.PermissionAdministrator | discordgo.PermissionManageServer)
	_, err = s.ApplicationCommandCreate(appID, guildID, &discordgo.ApplicationCommand{
		Name: "features", Description: "Turn the Champion features channel on or off", DefaultMemberPermissions: &perms,
	})
	return err
}

// Handle processes /features.
func (h *FeaturesCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.store == nil || i == nil || i.GuildID == "" {
		respondEphemeral(s, i, "The features channel is unavailable until the database is connected.")
		return
	}
	if !isAdminInteraction(i) {
		respondEphemeral(s, i, "Administrator or Manage Server permission required.")
		return
	}
	deferEphemeral(s, i) // creating the channel and posting the guide takes several requests
	botID := ""
	if s.State != nil && s.State.User != nil {
		botID = s.State.User.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	respondEphemeral(s, i, h.toggle(ctx, s, i.GuildID, botID))
}

// toggle removes the recorded features channel when it still exists, and otherwise creates a new
// one. It returns the reply for the staff member.
func (h *FeaturesCommandHandler) toggle(ctx context.Context, api featuresDiscord, guildID, botID string) string {
	guild, guildRowID, err := h.store.GetGuild(ctx, guildID)
	if err != nil || guildRowID == 0 {
		return "This server is not configured. Run `/setup` first."
	}
	current, err := h.store.FeaturesChannel(ctx, guildID)
	if err != nil {
		return "Could not read the features channel setting. Try again in a moment."
	}
	if current != "" {
		ch, err := api.Channel(current)
		switch {
		case err == nil && ch != nil && ch.GuildID == guildID:
			if _, err := api.ChannelDelete(current); err != nil {
				return "Could not remove the features channel: " + featuresErrorText(err)
			}
			if err := h.store.SetFeaturesChannel(ctx, guildID, ""); err != nil {
				slog.Warn("component=discord", "event", "features_channel_forget_failed", "err", err.Error())
			}
			return "Features channel removed. Run `/features` again to bring it back."
		case err != nil && welcomeErrorClass(err) != "CHANNEL_MISSING":
			return "Could not check the features channel: " + featuresErrorText(err)
		}
		// The recorded channel was deleted by hand (or is not in this server): make a new one.
	}

	parent := ""
	if guild != nil {
		parent = guild.CategoryID
	}
	data := discordgo.GuildChannelCreateData{
		Name: FeaturesChannelName, Type: discordgo.ChannelTypeGuildText, Topic: featuresChannelTopic,
		ParentID: parent, PermissionOverwrites: featuresChannelOverwrites(guildID, botID),
	}
	ch, err := api.GuildChannelCreateComplex(guildID, data)
	if err != nil && parent != "" { // the Champion category may have been deleted
		data.ParentID = ""
		ch, err = api.GuildChannelCreateComplex(guildID, data)
	}
	if err != nil || ch == nil {
		return "Could not create the features channel: " + featuresErrorText(err)
	}
	if err := h.store.SetFeaturesChannel(ctx, guildID, ch.ID); err != nil {
		_, _ = api.ChannelDelete(ch.ID) // never leave a channel the next /features cannot find
		return "Could not save the features channel. Nothing was changed; try again in a moment."
	}
	for _, msg := range featuresGuideMessages() {
		if _, err := api.ChannelMessageSendComplex(ch.ID, msg); err != nil {
			_, _ = api.ChannelDelete(ch.ID)
			_ = h.store.SetFeaturesChannel(ctx, guildID, "")
			return "Could not post the feature guide: " + featuresErrorText(err)
		}
	}
	return "Features channel created: <#" + ch.ID + ">. Run `/features` again to remove it."
}

// featuresChannelOverwrites makes the channel read-only for everyone and lets the bot post in it
// and delete it.
func featuresChannelOverwrites(guildID, botID string) []*discordgo.PermissionOverwrite {
	overwrites := []*discordgo.PermissionOverwrite{{
		ID: guildID, Type: discordgo.PermissionOverwriteTypeRole,
		Allow: discordgo.PermissionViewChannel | discordgo.PermissionReadMessageHistory,
		Deny: discordgo.PermissionSendMessages | discordgo.PermissionAddReactions | discordgo.PermissionCreatePublicThreads |
			discordgo.PermissionCreatePrivateThreads | discordgo.PermissionSendMessagesInThreads,
	}}
	if botID != "" {
		overwrites = append(overwrites, &discordgo.PermissionOverwrite{
			ID: botID, Type: discordgo.PermissionOverwriteTypeMember,
			Allow: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionEmbedLinks |
				discordgo.PermissionReadMessageHistory | discordgo.PermissionManageChannels,
		})
	}
	return overwrites
}

func featuresErrorText(err error) string {
	switch welcomeErrorClass(err) {
	case "PERMISSION_BLOCKED":
		return "Champion needs the Manage Channels permission."
	case "RATE_LIMITED":
		return "Discord is rate limiting the bot. Try again in a minute."
	default:
		return "Discord did not respond. Try again in a moment."
	}
}

// featuresGuideMessages splits the guide into as few messages as Discord's limits allow: at most
// 10 embeds and featuresMessageLimit characters each.
func featuresGuideMessages() []*discordgo.MessageSend {
	var out []*discordgo.MessageSend
	var cur []*discordgo.MessageEmbed
	size := 0
	for _, e := range featuresGuideEmbeds() {
		n := embedTextLength(e)
		if len(cur) > 0 && (len(cur) == 10 || size+n > featuresMessageLimit) {
			out = append(out, featuresMessage(cur))
			cur, size = nil, 0
		}
		cur = append(cur, e)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, featuresMessage(cur))
	}
	return out
}

func featuresMessage(embeds []*discordgo.MessageEmbed) *discordgo.MessageSend {
	return &discordgo.MessageSend{Embeds: embeds, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}}
}

func embedTextLength(e *discordgo.MessageEmbed) int {
	n := len([]rune(e.Title)) + len([]rune(e.Description))
	if e.Footer != nil {
		n += len([]rune(e.Footer.Text))
	}
	for _, f := range e.Fields {
		n += len([]rune(f.Name)) + len([]rune(f.Value))
	}
	return n
}

// featuresGuideEmbeds is the guide itself: one card per feature, newest first after the intro.
// Only features that are live for everyone belong here; nothing still in testing.
func featuresGuideEmbeds() []*discordgo.MessageEmbed {
	card := func(color int, title, what, how, where string) *discordgo.MessageEmbed {
		return &discordgo.MessageEmbed{Color: color, Title: title, Description: what, Fields: []*discordgo.MessageEmbedField{
			{Name: "How it works", Value: how},
			{Name: "Where", Value: where},
		}}
	}
	return []*discordgo.MessageEmbed{
		{
			Color: ColorChampionGold, Title: "🏆 What's new on Champion",
			Description: "This channel lists Champion's newest features and how to use them.\n\n" +
				"Some features are switched on by this server's staff, so not every one may be active here yet.",
			Footer: &discordgo.MessageEmbedFooter{Text: "CHAMPION • Staff can remove this channel with /features"},
		},
		card(ColorInfoBlue, "🔗 Start here: link your account",
			"Link your PlayStation name to Discord once to unlock your stats, card and Player Hub.",
			"• Run `/link` and follow the quick in-game check\n• You get the server's Verified role when it is done\n• Then try `/stats` and `/card`",
			"Discord `/link`, then the website Player Hub"),
		card(ColorChampionGold, "⚡ Double RP",
			"Staff can open a double RP window: every ranked kill inside it earns twice the usual RP.",
			"• A window lasts 1 to 72 hours and is announced with a \"2× RP IS LIVE\" card\n• What counts is when the kill happened, so a late-logged kill is still doubled\n• The repeat-kill cooldown on the same player still applies\n• When it ends, the top three RP earners are posted",
			"Events channel, and beside your rank in the Player Hub"),
		card(ColorChampionGold, "🪪 Champion Card",
			"A picture of your stats you can show off in any channel.",
			"• Run `/card` in a channel to post your own card\n• It is always your card, nobody can post someone else's\n• One card every 30 seconds",
			"Discord `/card`, website"),
		card(ColorInfoBlue, "🧬 Lives",
			"Every life from spawn to death: how long it lasted, what happened and how it ended.",
			"• Each life is tracked automatically once your account is linked\n• `/life me` shows your current and recent lives, `/life top` the boards\n• `/life recap on` sends you a DM recap when you die",
			"Discord `/life`, website Player Hub"),
		card(ColorDangerRed, "🔥 Hot zones",
			"When a fight breaks out, a short event opens on that spot by itself.",
			"• The opening card shows where it is, how big, when it ends and what it pays\n• Get kills inside the circle while it is open\n• The top three when it closes earn Champion Points",
			"Server status channel"),
		card(ColorDangerRed, "💀 Bounties",
			"Put Champion Points on another player's head.",
			"• `/bounty create` with the player, the points and how long it lasts\n• Kill the target before it runs out to claim it\n• `/bounty list` shows every open bounty",
			"Discord `/bounty`, bounties channel"),
		card(ColorSuccessGreen, "🛒 Shop orders and tickets",
			"Spend Champion Points in the server's Shop and confirm what you received.",
			"• Buy on the website and pick where you want the item dropped\n• After delivery you get a DM with **Received order** and **Issue with order**\n• **Issue with order** opens a private ticket channel with staff\n• No answer in 48 hours completes the order",
			"Website Shop, Discord DM"),
		card(ColorChampionGold, "💎 Supporter tiers and perks",
			"Support the server and get recognised for it.",
			"• When staff give you a supporter tier you get a DM, and a tier card shows in your Player Hub\n• The Donate tab sells non-gameplay perks for Champion Points",
			"Website Player Hub and Donate tab"),
		card(ColorWarningOrange, "🛡️ Base protection",
			"Know when someone is at your base, even when you are offline.",
			"• Register your base with `/registerbase` and check it with `/mybase`\n• Base Raid Alarm: a DM when someone starts taking your base apart\n• Perimeter Watch: a DM when another player comes near\n• Faction Security sends the alerts to your whole faction",
			"Discord, Security Store"),
		card(ColorInfoBlue, "⚔️ Factions",
			"Team up under a name, tag and flag, then take on other factions.",
			"• Create or join a faction on the website\n• Recruitment cards in Discord have Join and Apply buttons\n• `/faction` shows wars, rivalries and the faction leaderboard",
			"Website, faction recruitment channel, Discord `/faction`"),
		card(ColorInfoBlue, "🗺️ Fight replay and heatmaps",
			"See where the action is and how fights played out.",
			"• Fight replay groups kills into fights and plays them back on the map\n• Heatmaps show where kills and deaths happen",
			"Website, heatmaps channel"),
	}
}
