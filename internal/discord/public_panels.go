package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	embed := presentation.NewChampionEmbed("LINK YOUR PLAYSTATION ACCOUNT", presentation.InfoSteel)
	embed.Description = "Connect your PlayStation username to unlock personal stats, rankings, faction profile, Champion score, records, and competitive tracking.\n\n**REQUIREMENTS**\n• Join the connected DayZ server\n• Champion must observe at least 5 minutes\n• Use your exact PlayStation username"
	return embed
}

func LinkUsernamePanelComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{CustomID: linkPanelOpenID, Label: "Link Username", Style: discordgo.PrimaryButton},
	}}}
}

func PlayerStatsPanelComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{CustomID: statsPanelMeID, Label: "My Stats", Style: discordgo.PrimaryButton},
		discordgo.Button{CustomID: statsPanelSearchID, Label: "Search Player", Style: discordgo.SecondaryButton},
		// Private economy buttons: they only ever answer with the clicker's own
		// (linked) balance and transactions, ephemerally.
		discordgo.Button{CustomID: economyBalanceID, Label: "My Balance", Style: discordgo.SecondaryButton},
		discordgo.Button{CustomID: economyHistoryID, Label: "Recent Transactions", Style: discordgo.SecondaryButton},
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

func (h *PublicPanelHandler) HandleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || i == nil || i.GuildID == "" || i.Member == nil || i.Member.User == nil {
		return
	}
	switch i.MessageComponentData().CustomID {
	case linkPanelOpenID:
		respondModal(s, i, linkPanelModalID, "Link Username", "username", "PlayStation username", "Enter the exact username Champion observed")
	case statsPanelMeID:
		h.handleMyStats(s, i)
	case statsPanelSearchID:
		respondModal(s, i, statsSearchModalID, "Search Player", "player", "DayZ display name", "Enter a player name")
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
		respondEphemeral(s, i, "The economy is unavailable until the database is connected.")
		return
	}
	guildID, err := h.guildRowID(i.GuildID)
	if err != nil {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	link, err := h.links.Status(context.Background(), guildID, i.Member.User.ID)
	playerID, verified := VerifiedPlayerID(link)
	if err != nil || !verified {
		respondEphemeral(s, i, "🔗 **ACCOUNT NOT LINKED**\nLink and verify your PlayStation username first in #link-username.")
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
		respondEphemeral(s, i, "Account linking is unavailable until the database is connected.")
		return
	}
	guildID, err := h.guildRowID(i.GuildID)
	if err != nil {
		respondEphemeral(s, i, "Run `/setup` before linking your account.")
		return
	}
	link, err := h.links.Request(context.Background(), guildID, i.Member.User.ID, username)
	if err != nil {
		respondEphemeral(s, i, linkErrorMessage(err))
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("🟡 **PENDING VERIFICATION**\n\nPlayStation\n%s\n\nTo prove you're this account, **disconnect from the server and reconnect** before your request expires <t:%d:R>. Champion will verify it automatically within seconds of you reconnecting.\n\nCan't reconnect in time? Ask an admin to run `/admin verify-link`.", link.RequestedName, link.ExpiresAt.Unix()))
}

func (h *PublicPanelHandler) handleMyStats(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h.links == nil || h.stats == nil {
		respondEphemeral(s, i, "Player stats are unavailable until the database is connected.")
		return
	}
	guildID, err := h.guildRowID(i.GuildID)
	if err != nil {
		respondEphemeral(s, i, "Run `/setup` before viewing stats.")
		return
	}
	link, err := h.links.Status(context.Background(), guildID, i.Member.User.ID)
	if err != nil || link == nil || link.PlayerID == 0 {
		respondEphemeral(s, i, "🔗 **ACCOUNT NOT LINKED**\nLink your PlayStation username first in #link-username.")
		return
	}
	prof, err := h.stats.GetPlayerProfileByPlayerID(context.Background(), guildID, link.PlayerID)
	if err != nil || prof == nil {
		respondEphemeral(s, i, "Could not load stats right now.")
		return
	}
	respondEphemeral(s, i, formatPlayerProfile(prof))
}

func (h *PublicPanelHandler) handleSearch(s *discordgo.Session, i *discordgo.InteractionCreate, name string) {
	if h.stats == nil {
		respondEphemeral(s, i, "Player stats are unavailable until the database is connected.")
		return
	}
	guildID, err := h.guildRowID(i.GuildID)
	if err != nil {
		respondEphemeral(s, i, "Run `/setup` before searching stats.")
		return
	}
	prof, err := h.stats.GetPlayerProfile(context.Background(), guildID, name)
	if err != nil {
		respondEphemeral(s, i, "Could not load stats right now.")
		return
	}
	if prof == nil {
		respondEphemeral(s, i, fmt.Sprintf("No record for player `%s`.", name))
		return
	}
	respondEphemeral(s, i, formatPlayerProfile(prof))
}

func respondModal(s *discordgo.Session, i *discordgo.InteractionCreate, customID, title, inputID, label, placeholder string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseModal, Data: &discordgo.InteractionResponseData{
		CustomID: customID, Title: title, Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.TextInput{CustomID: inputID, Label: label, Style: discordgo.TextInputShort, Placeholder: placeholder, Required: true, MaxLength: 64},
		}}},
	}})
}

func linkErrorMessage(err error) string {
	switch {
	case errors.Is(err, linking.ErrLinkCheckUnavailable):
		return "⚠️ **LINK CHECK UNAVAILABLE**\nChampion cannot verify server activity right now. Please try again shortly."
	case errors.Is(err, linking.ErrNoConnectedServer):
		return "⚙️ **SERVER NOT CONNECTED**\nNo DayZ server is connected to this Discord yet, so Champion has no server activity to check. Ask an admin to connect the server in the Champion dashboard."
	case errors.Is(err, linking.ErrPlayerNotFound):
		return "❌ **PLAYER NOT FOUND**\nChampion has not seen that PlayStation username on the DayZ server yet."
	case errors.Is(err, linking.ErrPlaytimeRequired):
		return "⏱️ **MORE PLAYTIME REQUIRED**\nStay connected for at least 5 minutes, then try again."
	case errors.Is(err, linking.ErrAlreadyLinked):
		return "⚠️ **ACCOUNT ALREADY LINKED**\nUse `/unlink` before linking another account."
	case errors.Is(err, linking.ErrPlayerClaimed):
		return "❌ **ALREADY LINKED**\nThat PlayStation account is already linked to another Discord member."
	default:
		return "❌ Could not create a pending link right now."
	}
}
