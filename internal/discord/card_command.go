package discord

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// CardCommandHandler serves /card (docs/CHAMPION_CARD.md): it posts the caller's own Champion Card
// in the channel as an image. The card is always the caller's - there is no player argument, so
// nobody can post someone else's stats on their behalf.
type CardCommandHandler struct {
	guilds       GuildStore
	linkedPlayer func(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool)
	server       func(ctx context.Context, guildRowID int64) (int64, bool)
	render       func(ctx context.Context, guildRowID, serverID, playerID int64) ([]byte, error)

	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

// cardCommandCooldown is the per-user gap between cards: rendering is CPU work and the result is a
// public channel message.
const cardCommandCooldown = 30 * time.Second

func NewCardCommandHandler(guilds GuildStore,
	linkedPlayer func(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool),
	server func(ctx context.Context, guildRowID int64) (int64, bool),
	render func(ctx context.Context, guildRowID, serverID, playerID int64) ([]byte, error)) *CardCommandHandler {
	return &CardCommandHandler{guilds: guilds, linkedPlayer: linkedPlayer, server: server, render: render, last: map[string]time.Time{}, now: time.Now}
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
	if h == nil || i == nil || i.GuildID == "" || h.guilds == nil || h.render == nil {
		respondEphemeral(s, i, "Cards are unavailable until the database is connected.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, guildRowID, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || guildRowID == 0 {
		respondEphemeral(s, i, "This server is not configured. Run `/setup` first.")
		return
	}
	userID := interactionUserID(i)
	playerID, _, linked := h.linkedPlayer(ctx, guildRowID, userID)
	if !linked {
		respondEphemeral(s, i, "Link your gamertag with `/link` to get a Champion Card.")
		return
	}
	serverID, ok := h.server(ctx, guildRowID)
	if !ok {
		respondEphemeral(s, i, "No DayZ server is selected for this Discord yet.")
		return
	}
	if !h.allow(userID) {
		respondEphemeral(s, i, "You just posted your card. Try again in a moment.")
		return
	}
	// Rendering and the upload can exceed Discord's three-second window, so acknowledge first.
	if !deferPublic(s, i) {
		return
	}
	png, err := h.render(ctx, guildRowID, serverID, playerID)
	if err != nil {
		slog.Warn("component=cards", "event", "card_command_failed", "err", err.Error())
		msg := "Could not build your card right now."
		_ = editDeferred(s, i, &discordgo.WebhookEdit{Content: &msg})
		return
	}
	_ = editDeferred(s, i, &discordgo.WebhookEdit{
		Files:           []*discordgo.File{{Name: "champion-card.png", ContentType: "image/png", Reader: bytes.NewReader(png)}},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
}
