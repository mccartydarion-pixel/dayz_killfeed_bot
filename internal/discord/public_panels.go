package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/linking"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

const (
	linkPanelOpenID    = "champion:link:open:v1"
	linkPanelModalID   = "champion:link:modal:v1"
	statsPanelMeID     = "champion:stats:me:v1"
	statsPanelSearchID = "champion:stats:search:v1"
	statsSearchModalID = "champion:stats:search:modal:v1"

	economyBalanceID = "champion:economy:balance:v1"
	economyHistoryID = "champion:economy:history:v1"
)

type ProfileReader interface {
	GetPlayerProfileByPlayerID(context.Context, int64, int64) (*repository.PlayerProfile, error)
	GetPlayerProfile(context.Context, int64, string) (*repository.PlayerProfile, error)
}

func LinkUsernameInfoEmbed() *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed("Link your PlayStation account", presentation.Crimson)
	embed.Description = "Connect your PlayStation username to unlock personal stats, rankings, faction profile, Champion score, records, and competitive tracking.\n\n**Requirements**\n• Join the connected DayZ server\n• Champion must observe at least 5 minutes\n• Use your exact PlayStation username"
	return embed
}

func LinkUsernamePanelComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{CustomID: linkPanelOpenID, Label: "Link username", Style: discordgo.PrimaryButton},
	}}}
}

func PlayerStatsPanelComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{CustomID: statsPanelMeID, Label: "My stats", Style: discordgo.PrimaryButton},
		discordgo.Button{CustomID: statsPanelSearchID, Label: "Search player", Style: discordgo.SecondaryButton},
		// Private economy buttons: they only ever answer with the clicker's own
		// (linked) balance and transactions, ephemerally.
		discordgo.Button{CustomID: economyBalanceID, Label: "My balance", Style: discordgo.SecondaryButton},
		discordgo.Button{CustomID: economyHistoryID, Label: "Recent transactions", Style: discordgo.SecondaryButton},
	}}}
}

// PublicPanelHandler routes persistent panel interactions. It is registered at
// startup, so components continue working after setup messages are reused.
type PublicPanelHandler struct {
	links   *linking.LinkVerificationService
	stats   ProfileReader
	guilds  GuildStore
	economy *economy.Service
}

// SetEconomy enables the "My Balance" / "Recent Transactions" buttons. Without it
// they reply that the economy is unavailable.
func (h *PublicPanelHandler) SetEconomy(svc *economy.Service) {
	if h != nil {
		h.economy = svc
	}
}

func NewPublicPanelHandler(links *linking.LinkVerificationService, stats ProfileReader, guilds GuildStore) *PublicPanelHandler {
	return &PublicPanelHandler{links: links, stats: stats, guilds: guilds}
}

// Register routes the public panel buttons and forms. The two buttons that
// open a form answer at once (a form cannot be deferred); the rest read the
// database and answer privately.
func (h *PublicPanelHandler) Register(routes *InteractionRouter) {
	routes.Component(linkPanelOpenID, AckSelf, h.HandleComponent)
	routes.Component(statsPanelSearchID, AckSelf, h.HandleComponent)
	routes.Component(statsPanelMeID, AckPrivate, h.HandleComponent)
	routes.Component(economyBalanceID, AckPrivate, h.HandleComponent)
	routes.Component(economyHistoryID, AckPrivate, h.HandleComponent)
	routes.Modal(linkPanelModalID, AckPrivate, h.HandleModal)
	routes.Modal(statsSearchModalID, AckPrivate, h.HandleModal)
}

func (h *PublicPanelHandler) HandleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || i == nil || i.GuildID == "" || i.Member == nil || i.Member.User == nil {
		return
	}
	switch i.MessageComponentData().CustomID {
	case linkPanelOpenID:
		respondModal(s, i, linkPanelModalID, "Link username", "username", "PlayStation username", "Enter the exact username Champion observed")
	case statsPanelMeID:
		h.handleMyStats(s, i)
	case statsPanelSearchID:
		respondModal(s, i, statsSearchModalID, "Search player", "player", "DayZ display name", "Enter a player name")
	case economyBalanceID:
		h.handleMyEconomy(s, i, false)
	case economyHistoryID:
		h.handleMyEconomy(s, i, true)
	}
}

// handleMyEconomy answers the clicker's OWN balance or history, resolved through
// the existing account link - never another player's, and only ephemerally.
func (h *PublicPanelHandler) handleMyEconomy(s *discordgo.Session, i *discordgo.InteractionCreate, history bool) {
	if h.links == nil || h.economy == nil {
		respondEphemeral(s, i, ReplyIsUnavailable("The economy"))
		return
	}
	guildID, err := h.guildRowID(i.GuildID)
	if err != nil {
		respondEphemeral(s, i, ReplyNotSetUp)
		return
	}
	link, err := h.links.Status(context.Background(), guildID, i.Member.User.ID)
	playerID, verified := VerifiedPlayerID(link)
	if err != nil || !verified {
		respondEphemeral(s, i, ReplyNotLinked("Link and verify your PlayStation username first in #link-username."))
		return
	}
	if history {
		respondEphemeral(s, i, economyHistoryMessage(context.Background(), h.economy, guildID, playerID))
		return
	}
	respondEphemeral(s, i, economyBalanceMessage(context.Background(), h.economy, guildID, playerID))
}

func (h *PublicPanelHandler) HandleModal(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || i == nil || i.GuildID == "" || i.Member == nil || i.Member.User == nil {
		return
	}
	data := i.ModalSubmitData()
	value := ""
	if len(data.Components) > 0 {
		if row, ok := data.Components[0].(*discordgo.ActionsRow); ok && len(row.Components) > 0 {
			if input, ok := row.Components[0].(*discordgo.TextInput); ok {
				value = strings.TrimSpace(input.Value)
			}
		}
	}
	switch data.CustomID {
	case linkPanelModalID:
		h.handleLink(s, i, value)
	case statsSearchModalID:
		h.handleSearch(s, i, value)
	}
}

func (h *PublicPanelHandler) guildRowID(guildID string) (int64, error) {
	if h.guilds == nil {
		return 0, errors.New("guild store unavailable")
	}
	_, id, err := h.guilds.GetGuild(context.Background(), guildID)
	if err != nil || id == 0 {
		return 0, errors.New("guild is not configured")
	}
	return id, nil
}

func (h *PublicPanelHandler) handleLink(s *discordgo.Session, i *discordgo.InteractionCreate, username string) {
	if h.links == nil {
		respondEphemeral(s, i, ReplyIsUnavailable("Account linking"))
		return
	}
	deferEphemeral(s, i) // matches the name against every server's players
	guildID, err := h.guildRowID(i.GuildID)
	if err != nil {
		respondEphemeral(s, i, ReplyNotSetUp)
		return
	}
	link, err := h.links.Request(context.Background(), guildID, i.Member.User.ID, username)
	if err != nil {
		respondEphemeral(s, i, linkErrorMessage(err))
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("🟡 **Pending verification**\n\nPlayStation\n%s\n\nTo prove you're this account, **disconnect from the server and reconnect** before your request expires <t:%d:R>. Champion will verify it automatically within seconds of you reconnecting.\n\nCan't reconnect in time? Ask an admin to run `/admin verify-link`.", link.RequestedName, link.ExpiresAt.Unix()))
}

func (h *PublicPanelHandler) handleMyStats(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h.links == nil || h.stats == nil {
		respondEphemeral(s, i, ReplyAreUnavailable("Player stats"))
		return
	}
	guildID, err := h.guildRowID(i.GuildID)
	if err != nil {
		respondEphemeral(s, i, ReplyNotSetUp)
		return
	}
	link, err := h.links.Status(context.Background(), guildID, i.Member.User.ID)
	if err != nil || link == nil || link.PlayerID == 0 {
		respondEphemeral(s, i, ReplyNotLinked("Link your PlayStation username first in #link-username."))
		return
	}
	prof, err := h.stats.GetPlayerProfileByPlayerID(context.Background(), guildID, link.PlayerID)
	if err != nil || prof == nil {
		respondEphemeral(s, i, ReplyCouldNot("load stats"))
		return
	}
	respondEphemeral(s, i, formatPlayerProfile(prof))
}

func (h *PublicPanelHandler) handleSearch(s *discordgo.Session, i *discordgo.InteractionCreate, name string) {
	if h.stats == nil {
		respondEphemeral(s, i, ReplyAreUnavailable("Player stats"))
		return
	}
	guildID, err := h.guildRowID(i.GuildID)
	if err != nil {
		respondEphemeral(s, i, ReplyNotSetUp)
		return
	}
	prof, err := h.stats.GetPlayerProfile(context.Background(), guildID, name)
	if err != nil {
		respondEphemeral(s, i, ReplyCouldNot("load stats"))
		return
	}
	if prof == nil {
		respondEphemeral(s, i, fmt.Sprintf("No record for player `%s`.", name))
		return
	}
	respondEphemeral(s, i, formatPlayerProfile(prof))
}

func respondModal(s *discordgo.Session, i *discordgo.InteractionCreate, customID, title, inputID, label, placeholder string) {
	_ = respondModalData(s, i, &discordgo.InteractionResponseData{
		CustomID: customID, Title: title, Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.TextInput{CustomID: inputID, Label: label, Style: discordgo.TextInputShort, Placeholder: placeholder, Required: true, MaxLength: 64},
		}}},
	})
}

func linkErrorMessage(err error) string {
	var shortfall *linking.PlaytimeShortfallError
	switch {
	case errors.Is(err, linking.ErrInvalidUsername):
		return "**Invalid PlayStation username**\nEnter your PlayStation Online ID exactly as it appears in-game - not your Discord name or an @mention."
	case errors.Is(err, linking.ErrNoConnectedServer):
		return "⚙️ **Server not connected**\nNo DayZ server is connected to this Discord yet, so Champion has no server activity to check. Ask an admin to connect the server in the Champion dashboard."
	case errors.Is(err, linking.ErrActivityUnavailable), errors.Is(err, linking.ErrLinkCheckUnavailable):
		return "**Link check unavailable**\nChampion cannot verify server activity right now. Try again in a moment."
	case errors.Is(err, linking.ErrPlayerNotFound):
		return "**Player not found**\nChampion has not seen that PlayStation username on the DayZ server. Check the spelling, or join the server and try again after 5 minutes."
	case errors.Is(err, linking.ErrPlayerNotObserved):
		return "⏱️ **Not observed on server**\nChampion knows that username but has not recorded you online on a connected server yet. Join the server, stay connected for 5 minutes, then try again."
	case errors.As(err, &shortfall):
		return fmt.Sprintf("⏱️ **More playtime required**\nChampion has observed that account online for %s of the required %s. Stay connected and try again.", formatLinkPlaytime(shortfall.Observed), formatLinkPlaytime(shortfall.Required))
	case errors.Is(err, linking.ErrPlaytimeRequired):
		return "⏱️ **More playtime required**\nStay connected for at least 5 minutes, then try again."
	case errors.Is(err, linking.ErrAlreadyLinked):
		return "**Account already linked**\nUse `/unlink` before linking another account."
	case errors.Is(err, linking.ErrPlayerClaimed):
		return "**Already linked**\nThat PlayStation account is already linked to another Discord member."
	default:
		return ReplyCouldNot("create a pending link")
	}
}

// formatLinkPlaytime renders an observed duration as whole minutes and
// seconds ("3m 20s"), never rounding a shortfall up to the requirement.
func formatLinkPlaytime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	m, sec := int(d/time.Minute), int((d%time.Minute)/time.Second)
	if sec == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm %ds", m, sec)
}
