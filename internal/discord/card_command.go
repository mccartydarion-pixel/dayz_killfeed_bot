package discord

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/playercard"
)

// CardCommandHandler serves /card (docs/CHAMPION_CARD.md): it posts the caller's own Champion Card
// in the channel, animated (the card filling in, as a GIF), or as the still when the animation
// cannot be made. The card is always the caller's - there is no player argument, so nobody can
// post someone else's stats on their behalf.
type CardCommandHandler struct {
	guilds       GuildStore
	linkedPlayer func(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool)
	server       func(ctx context.Context, guildRowID int64) (int64, bool)
	// card assembles the caller's card; (nil, nil) means the player is not known on the server.
	card func(ctx context.Context, guildRowID, serverID, playerID int64) (*playercard.Card, error)
	// animate and still render the card: playercard.RenderAnimation and playercard.Render.
	animate func(playercard.Card) ([]byte, error)
	still   func(playercard.Card) ([]byte, error)

	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

// cardCommandCooldown is the per-user gap between cards: rendering is CPU work and the result is a
// public channel message.
const cardCommandCooldown = 30 * time.Second

// The attachment names of the two forms of the card.
const (
	cardAnimationFile = "champion-card.gif"
	cardStillFile     = "champion-card.png"
)

func NewCardCommandHandler(guilds GuildStore,
	linkedPlayer func(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool),
	server func(ctx context.Context, guildRowID int64) (int64, bool),
	card func(ctx context.Context, guildRowID, serverID, playerID int64) (*playercard.Card, error)) *CardCommandHandler {
	return &CardCommandHandler{guilds: guilds, linkedPlayer: linkedPlayer, server: server, card: card,
		animate: playercard.RenderAnimation, still: playercard.Render, last: map[string]time.Time{}, now: time.Now}
}

// RegisterCardCommand registers /card.
func RegisterCardCommand(session CommandRegistrar, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
	_, err = session.ApplicationCommandCreate(applicationID, guildID, &discordgo.ApplicationCommand{
		Name: "card", Description: "Post your Champion Card in this channel",
	})
	return err
}

// allow reports whether userID may render now, recording the attempt if so.
func (h *CardCommandHandler) allow(userID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if at, ok := h.last[userID]; ok && now.Sub(at) < cardCommandCooldown {
		return false
	}
	for id, at := range h.last { // keep the map from growing without bound
		if now.Sub(at) >= cardCommandCooldown {
			delete(h.last, id)
		}
	}
	h.last[userID] = now
	return true
}

// Handle processes /card.
func (h *CardCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || i == nil || i.GuildID == "" || h.guilds == nil || h.card == nil {
		respondEphemeral(s, i, ReplyAreUnavailable("Cards"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, guildRowID, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || guildRowID == 0 {
		respondEphemeral(s, i, ReplyNotSetUp)
		return
	}
	userID := interactionUserID(i)
	playerID, _, linked := h.linkedPlayer(ctx, guildRowID, userID)
	if !linked {
		respondEphemeral(s, i, ReplyNotLinked("Link your gamertag with `/link` to get a Champion Card."))
		return
	}
	serverID, ok := h.server(ctx, guildRowID)
	if !ok {
		respondEphemeral(s, i, ReplyNoServerSelected)
		return
	}
	if !h.allow(userID) {
		respondEphemeral(s, i, "You just posted your card. Try again in a moment.")
		return
	}
	// Building the card, rendering the animation (a few seconds) and the upload exceed Discord's
	// three-second window, so acknowledge first (a no-op when the router already has).
	if !deferPublic(s, i) {
		return
	}
	card, err := h.card(ctx, guildRowID, serverID, playerID)
	if err == nil && card == nil {
		err = errPlayerNotFound
	}
	if err != nil {
		slog.Warn("component=cards", "event", "card_command_failed", "err", err.Error())
		msg := ReplyCouldNot("build your card")
		_ = editDeferred(s, i, &discordgo.WebhookEdit{Content: &msg})
		return
	}
	name, contentType := cardAnimationFile, "image/gif"
	data, err := h.animate(*card)
	if err != nil {
		// The still is the same card without the motion: better than no card.
		slog.Warn("component=cards", "event", "card_gif_failed", "err", err.Error())
		name, contentType = cardStillFile, "image/png"
		if data, err = h.still(*card); err != nil {
			slog.Warn("component=cards", "event", "card_command_failed", "err", err.Error())
			msg := ReplyCouldNot("build your card")
			_ = editDeferred(s, i, &discordgo.WebhookEdit{Content: &msg})
			return
		}
	}
	_ = editDeferred(s, i, &discordgo.WebhookEdit{
		Files:           []*discordgo.File{{Name: name, ContentType: contentType, Reader: bytes.NewReader(data)}},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
}

// errPlayerNotFound is the card builder reporting a player it does not know.
var errPlayerNotFound = errors.New("player not found")
