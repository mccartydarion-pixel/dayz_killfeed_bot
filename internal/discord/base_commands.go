package discord

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// /mybase and /registerbase: a player checks their bases and base services,
// or asks for their base to be registered where the server log last saw them.
// Both answer only the caller, privately; registering follows exactly the
// same rules as the Security Store page.

// BaseCommandSummary is what /mybase shows.
type BaseCommandSummary struct {
	Bases         []string
	Pending       string // name of a waiting request, if any
	LastAnswer    string // plain words about the newest answered request
	PaidUntil     []BasePaidTime
	PositionFresh bool
}

// BasePaidTime is one base service the player has paid time for.
type BasePaidTime struct {
	Label string
	Until time.Time
}

// BaseCommandHandler serves /mybase and /registerbase.
type BaseCommandHandler struct {
	guilds       GuildStore
	linkedPlayer func(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool)
	server       func(ctx context.Context, guildRowID int64) (int64, bool)
	summary      func(ctx context.Context, guildRowID, serverID, playerID int64) (BaseCommandSummary, error)
	request      func(ctx context.Context, guildRowID, serverID, playerID int64, name string, radius float64, note string) (string, error)
	storeURL     string
}

func NewBaseCommandHandler(guilds GuildStore,
	linkedPlayer func(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool),
	server func(ctx context.Context, guildRowID int64) (int64, bool),
	summary func(ctx context.Context, guildRowID, serverID, playerID int64) (BaseCommandSummary, error),
	request func(ctx context.Context, guildRowID, serverID, playerID int64, name string, radius float64, note string) (string, error),
	storeURL string) *BaseCommandHandler {
	return &BaseCommandHandler{guilds: guilds, linkedPlayer: linkedPlayer, server: server, summary: summary, request: request, storeURL: storeURL}
}

// BaseCommandSizes are the sizes /registerbase offers, in metres around the player.
var BaseCommandSizes = []int{25, 50, 75, 100, 150}

// RegisterBaseCommands registers /mybase and /registerbase.
func RegisterBaseCommands(session *discordgo.Session, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
	sizes := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(BaseCommandSizes))
	for _, s := range BaseCommandSizes {
		sizes = append(sizes, &discordgo.ApplicationCommandOptionChoice{Name: fmt.Sprintf("%d m", s), Value: s})
	}
	min, max := 1, 64
	if _, err := session.ApplicationCommandCreate(applicationID, guildID, &discordgo.ApplicationCommand{
		Name: "mybase", Description: "See your registered bases, base requests and base services",
	}); err != nil {
		return err
	}
	noteMax := 300
	_, err = session.ApplicationCommandCreate(applicationID, guildID, &discordgo.ApplicationCommand{
		Name: "registerbase", Description: "Ask the server owner to register your base where you are standing in game",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "name", Description: "Your base's name", Required: true, MinLength: &min, MaxLength: max},
			{Type: discordgo.ApplicationCommandOptionInteger, Name: "size", Description: "How far around you the base reaches", Required: true, Choices: sizes},
			{Type: discordgo.ApplicationCommandOptionString, Name: "note", Description: "A note for the server owner", MaxLength: noteMax},
		},
	})
	return err
}

// resolve finds the caller's server and verified player, answering them when it can't.
func (h *BaseCommandHandler) resolve(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) (guildRowID, serverID, playerID int64, ok bool) {
	if h == nil || i == nil || i.GuildID == "" || h.guilds == nil || h.summary == nil || h.request == nil {
		respondEphemeral(s, i, "Base commands are unavailable right now.")
		return 0, 0, 0, false
	}
	_, guildRowID, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || guildRowID == 0 {
		respondEphemeral(s, i, "This server is not configured. Run `/setup` first.")
		return 0, 0, 0, false
	}
	playerID, _, linked := h.linkedPlayer(ctx, guildRowID, interactionUserID(i))
	if !linked {
		respondEphemeral(s, i, "Link your gamertag with `/link` first.")
		return 0, 0, 0, false
	}
	serverID, found := h.server(ctx, guildRowID)
	if !found {
		respondEphemeral(s, i, "No DayZ server is selected for this Discord yet.")
		return 0, 0, 0, false
	}
	return guildRowID, serverID, playerID, true
}

// HandleMyBase processes /mybase.
func (h *BaseCommandHandler) HandleMyBase(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	guildRowID, serverID, playerID, ok := h.resolve(ctx, s, i)
	if !ok {
		return
	}
	sum, err := h.summary(ctx, guildRowID, serverID, playerID)
	if err != nil {
		respondEphemeral(s, i, "Your bases couldn't be loaded right now. Try again in a minute.")
		return
	}
	respondEphemeralEmbed(s, i, MyBaseEmbed(sum, h.storeURL, time.Now()))
}

// HandleRegisterBase processes /registerbase.
func (h *BaseCommandHandler) HandleRegisterBase(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	guildRowID, serverID, playerID, ok := h.resolve(ctx, s, i)
	if !ok {
		return
	}
	var name, note string
	var size int64
	for _, o := range i.ApplicationCommandData().Options {
		switch o.Name {
		case "name":
			name = strings.TrimSpace(o.StringValue())
		case "size":
			size = o.IntValue()
		case "note":
			note = strings.TrimSpace(o.StringValue())
		}
	}
	allowed := false
	for _, s := range BaseCommandSizes {
		allowed = allowed || int64(s) == size
	}
	if name == "" || !allowed {
		respondEphemeral(s, i, "Give your base a name and pick a size.")
		return
	}
	msg, err := h.request(ctx, guildRowID, serverID, playerID, name, float64(size), note)
	if err != nil {
		respondEphemeral(s, i, "Your request couldn't be sent: "+caseSafeText(err.Error(), 200)+".")
		return
	}
	respondEphemeral(s, i, msg)
}

// MyBaseEmbed renders /mybase. Names are neutralised; the reply is private.
func MyBaseEmbed(sum BaseCommandSummary, storeURL string, now time.Time) *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed("📍 Your bases", presentation.InfoSteel)
	bases := "None yet. Stand at your base in game and use `/registerbase`."
	if len(sum.Bases) > 0 {
		names := make([]string, 0, len(sum.Bases))
		for _, b := range sum.Bases {
			names = append(names, presentation.SafeName(b, 64))
		}
		bases = strings.Join(names, "\n")
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Registered", Value: bases})
	if sum.Pending != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Waiting for the server owner", Value: presentation.SafeName(sum.Pending, 64)})
	} else if sum.LastAnswer != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Last request", Value: caseSafeText(sum.LastAnswer, 300)})
	}
	paid := "None. See what's on sale in the Security Store."
	if len(sum.PaidUntil) > 0 {
		lines := make([]string, 0, len(sum.PaidUntil))
		for _, p := range sum.PaidUntil {
			lines = append(lines, fmt.Sprintf("%s: until <t:%d:f>", caseSafeText(p.Label, 40), p.Until.Unix()))
		}
		paid = strings.Join(lines, "\n")
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Base services", Value: paid})
	if storeURL != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Security Store", Value: storeURL})
	}
	presentation.StampEmbed(embed, now)
	return embed
}

func respondEphemeralEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Embeds: []*discordgo.MessageEmbed{embed},
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}},
	})
}
