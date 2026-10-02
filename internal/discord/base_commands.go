package discord

import (
	"context"
	"fmt"
	"strconv"
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
	Rent          []BaseRentLine
	RentPrice     int64
	RentDays      int
}

// BaseRentLine is one rented base's rent state for /mybase.
type BaseRentLine struct {
	BaseID   int64
	BaseName string
	DueAt    time.Time
	Paused   bool
	// OwnerName is set for a faction mate's base the player may pay for.
	OwnerName string
}

// RentQuote is what one period of rent on a base costs, for the confirm step.
type RentQuote struct {
	BaseName    string
	PricePoints int64
	PeriodDays  int
}

// RentPaid is the outcome of paying rent from Discord.
type RentPaid struct {
	BaseName  string
	PaidUntil time.Time
	Balance   int64
	Duplicate bool
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
	rentQuote    func(ctx context.Context, guildRowID, serverID, playerID, baseID int64) (RentQuote, error)
	rentPay      func(ctx context.Context, guildRowID, serverID, playerID, baseID int64, key string) (RentPaid, error)
}

// SetRentPayment lets /mybase offer a Pay rent button. quote prices one period
// for a base the player may pay for; pay charges it (key makes it idempotent).
func (h *BaseCommandHandler) SetRentPayment(
	quote func(ctx context.Context, guildRowID, serverID, playerID, baseID int64) (RentQuote, error),
	pay func(ctx context.Context, guildRowID, serverID, playerID, baseID int64, key string) (RentPaid, error)) {
	if h != nil {
		h.rentQuote, h.rentPay = quote, pay
	}
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
func RegisterBaseCommands(session CommandRegistrar, guildID string) error {
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
	deferEphemeral(s, i)
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
	var components []discordgo.MessageComponent
	if h.rentPay != nil {
		components = MyBaseComponents(sum)
	}
	respondPrivate(s, i, &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{MyBaseEmbed(sum, h.storeURL, time.Now())},
		Components: components, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}})
}

// Pay rent buttons. "ask" shows the price and a confirm button; "pay" charges;
// "cancel" closes the prompt. The base ID in a button is only a request: the
// payment checks again that the clicker may pay rent on that base.
const (
	rentAskPrefix  = "baserent:ask:"
	rentPayPrefix  = "baserent:pay:"
	rentCancelID   = "baserent:cancel"
	maxRentButtons = 5
)

// IsBaseRentInteraction reports whether a button belongs to Pay rent.
func IsBaseRentInteraction(customID string) bool {
	return strings.HasPrefix(customID, "baserent:")
}

// MyBaseComponents is one Pay rent button per rented base (at most 5).
func MyBaseComponents(sum BaseCommandSummary) []discordgo.MessageComponent {
	if sum.RentPrice <= 0 || len(sum.Rent) == 0 {
		return nil
	}
	buttons := make([]discordgo.MessageComponent, 0, maxRentButtons)
	for _, r := range sum.Rent {
		if r.BaseID <= 0 || len(buttons) == maxRentButtons {
			continue
		}
		label := "Pay rent · " + plainLabel(r.BaseName, 60)
		buttons = append(buttons, discordgo.Button{Label: label, Style: discordgo.SecondaryButton, CustomID: fmt.Sprintf("%s%d", rentAskPrefix, r.BaseID), Emoji: &discordgo.ComponentEmoji{Name: "🏠"}})
	}
	if len(buttons) == 0 {
		return nil
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: buttons}}
}

// RentConfirmMessage asks the player to confirm one period of rent.
func RentConfirmMessage(baseID int64, q RentQuote) (string, []discordgo.MessageComponent) {
	text := fmt.Sprintf("Pay **%s** Champion Points for **%d more days** of rent on **%s**? Nothing is charged until you confirm.",
		presentation.FormatThousands(q.PricePoints), q.PeriodDays, presentation.SafeName(q.BaseName, 64))
	return text, []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Confirm and pay", Style: discordgo.SuccessButton, CustomID: fmt.Sprintf("%s%d", rentPayPrefix, baseID)},
		discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: rentCancelID},
	}}}
}

// RentPaidMessage reports a payment.
func RentPaidMessage(p RentPaid) string {
	return fmt.Sprintf("✅ Rent paid for **%s**. It's now paid until <t:%d:f>. Your balance: %s Champion Points.",
		presentation.SafeName(p.BaseName, 64), p.PaidUntil.Unix(), presentation.FormatThousands(p.Balance))
}

func plainLabel(s string, max int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	if s == "" {
		return "base"
	}
	return s
}

func parseRentBaseID(customID, prefix string) (int64, bool) {
	if !strings.HasPrefix(customID, prefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(customID, prefix), 10, 64)
	return id, err == nil && id > 0
}

func updateRentPrompt(s *discordgo.Session, i *discordgo.InteractionCreate, content string, components []discordgo.MessageComponent) {
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: content, Components: components, Embeds: []*discordgo.MessageEmbed{},
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}},
	})
}

// HandleRentComponent processes the Pay rent buttons.
func (h *BaseCommandHandler) HandleRentComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	customID := i.MessageComponentData().CustomID
	if customID == rentCancelID {
		updateRentPrompt(s, i, "Cancelled. Nothing was charged.", nil)
		return
	}
	if h == nil || h.rentQuote == nil || h.rentPay == nil {
		respondEphemeral(s, i, "Paying rent from Discord is unavailable right now. Use the Security Store page.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if baseID, ok := parseRentBaseID(customID, rentAskPrefix); ok {
		deferEphemeral(s, i)
		guildRowID, serverID, playerID, ok := h.resolve(ctx, s, i)
		if !ok {
			return
		}
		q, err := h.rentQuote(ctx, guildRowID, serverID, playerID, baseID)
		if err != nil {
			respondEphemeral(s, i, caseSafeText(err.Error(), 200))
			return
		}
		text, components := RentConfirmMessage(baseID, q)
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: text, Components: components,
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}})
		return
	}
	baseID, ok := parseRentBaseID(customID, rentPayPrefix)
	if !ok || i.Message == nil || i.Message.ID == "" {
		respondEphemeral(s, i, "That button is no longer valid. Run `/mybase` again.")
		return
	}
	guildRowID, serverID, playerID, ok := h.resolve(ctx, s, i)
	if !ok {
		return
	}
	// The confirm prompt's message ID is the idempotency key, so clicking
	// Confirm twice on the same prompt charges once.
	paid, err := h.rentPay(ctx, guildRowID, serverID, playerID, baseID, "discord-"+i.Message.ID)
	if err != nil {
		updateRentPrompt(s, i, "Rent wasn't paid: "+caseSafeText(err.Error(), 200)+". Nothing was charged.", nil)
		return
	}
	updateRentPrompt(s, i, RentPaidMessage(paid), nil)
}

// HandleRegisterBase processes /registerbase.
func (h *BaseCommandHandler) HandleRegisterBase(s *discordgo.Session, i *discordgo.InteractionCreate) {
	deferEphemeral(s, i)
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
	if len(sum.Rent) > 0 {
		lines := make([]string, 0, len(sum.Rent))
		for _, r := range sum.Rent {
			name := presentation.SafeName(r.BaseName, 64)
			if r.OwnerName != "" {
				name += " (" + presentation.SafeName(r.OwnerName, 40) + "'s, your faction)"
			}
			if r.Paused {
				lines = append(lines, fmt.Sprintf("⏸️ %s: paused, rent overdue", name))
			} else if r.DueAt.Before(now) {
				lines = append(lines, fmt.Sprintf("⚠️ %s: rent was due <t:%d:R>; pay soon to avoid a pause", name, r.DueAt.Unix()))
			} else {
				lines = append(lines, fmt.Sprintf("🏠 %s: rent paid until <t:%d:f>", name, r.DueAt.Unix()))
			}
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Rent", Value: strings.Join(lines, "\n")})
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
	respondPrivate(s, i, &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}})
}
