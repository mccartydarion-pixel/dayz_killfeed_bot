package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	competitiveevents "github.com/yourname/dayz-killfeed/internal/events"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type EventCommandHandler struct {
	service *competitiveevents.Service
	events  *repository.EventRepository
	guilds  GuildStore
}

func NewEventCommandHandler(service *competitiveevents.Service, events *repository.EventRepository, guilds GuildStore) *EventCommandHandler {
	return &EventCommandHandler{service: service, events: events, guilds: guilds}
}
func RegisterEventCommands(session *discordgo.Session, guildID string) error {
	cmd := &discordgo.ApplicationCommand{Name: "event", Description: "Champion competitive events", Options: []*discordgo.ApplicationCommandOption{{Name: "create", Description: "Create an event", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "type", Description: "Event type", Type: discordgo.ApplicationCommandOptionString, Required: true}, {Name: "name", Description: "Event name", Type: discordgo.ApplicationCommandOptionString, Required: true}}}, {Name: "start", Description: "Start an event", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "Event ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "end", Description: "End an event", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "Event ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "cancel", Description: "Cancel an event", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "Event ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "list", Description: "List events", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "status", Description: "Show an event", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "Event ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "leaderboard", Description: "Show event rankings", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "Event ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "history", Description: "Show event history", Type: discordgo.ApplicationCommandOptionSubCommand}}}
	_, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, cmd)
	return err
}
func (h *EventCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.events == nil || h.guilds == nil || i == nil {
		respondEphemeral(s, i, "Events are unavailable.")
		return
	}
	_, gid, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || gid == 0 {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	opts := i.ApplicationCommandData().Options
	if len(opts) == 0 {
		return
	}
	sub := opts[0]
	switch sub.Name {
	case "list", "history":
		rows, err := h.events.GetRecentEvents(context.Background(), gid, 10)
		if err != nil {
			respondEphemeral(s, i, "Could not load events.")
			return
		}
		var b strings.Builder
		b.WriteString("🔥 **CHAMPION EVENTS**\n\n")
		for _, e := range rows {
			fmt.Fprintf(&b, "%d. **%s** — %s\n", e.ID, e.Name, e.Status)
		}
		if len(rows) == 0 {
			b.WriteString("No events yet.")
		}
		respondEphemeral(s, i, b.String())
	case "status":
		id := optionInt(sub, "id")
		e, err := h.events.GetEvent(context.Background(), gid, id)
		if err != nil {
			respondEphemeral(s, i, "Event not found.")
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("🔥 **%s**\nType: %s\nStatus: %s", e.Name, e.Type, e.Status))
	case "leaderboard":
		id := optionInt(sub, "id")
		rows, err := h.events.Leaderboard(context.Background(), id, 10)
		if err != nil {
			respondEphemeral(s, i, "Could not load event leaderboard.")
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "🏆 **EVENT %d LEADERBOARD**\n\n", id)
		for n, row := range rows {
			who := strconv.FormatInt(row.PlayerID, 10)
			if row.FactionID > 0 {
				who = "faction " + strconv.FormatInt(row.FactionID, 10)
			}
			fmt.Fprintf(&b, "%d. %s — %.1f\n", n+1, who, row.Score)
		}
		respondEphemeral(s, i, b.String())
	case "create":
		if !isAdminInteraction(i) {
			respondEphemeral(s, i, "Administrator or Manage Server permission required.")
			return
		}
		if h.service == nil {
			respondEphemeral(s, i, "Event service unavailable.")
			return
		}
		typ := optionString(sub, "type")
		name := optionString(sub, "name")
		cfg := any(competitiveevents.MostKillsConfig{})
		switch typ {
		case competitiveevents.TypeLongestKill:
			cfg = competitiveevents.LongestKillConfig{}
		case competitiveevents.TypeKillStreak:
			cfg = competitiveevents.KillStreakConfig{}
		case competitiveevents.TypeFactionKills:
			cfg = competitiveevents.FactionKillsConfig{EnemyFactionsOnly: true}
		case competitiveevents.TypeWeaponChallenge:
			cfg = competitiveevents.WeaponChallengeConfig{WeaponNames: []string{name}}
		case competitiveevents.TypeHeadshotHunt, competitiveevents.TypeFactionWarKills, competitiveevents.TypeMostKills:
		default:
			respondEphemeral(s, i, "Unsupported or malformed event type.")
			return
		}
		payload, _ := json.Marshal(cfg)
		created, err := h.service.Create(context.Background(), repository.CompetitiveEvent{GuildID: gid, Type: typ, Name: name, Status: competitiveevents.StatusDraft, Config: payload}, cfg, i.Member.User.ID)
		if err != nil {
			respondEphemeral(s, i, "Could not create event: "+err.Error())
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("🔥 Event created: **%s** (ID %d)", created.Name, created.ID))
	default:
		if !isAdminInteraction(i) {
			respondEphemeral(s, i, "Administrator or Manage Server permission required.")
			return
		}
		id := optionInt(sub, "id")
		var err error
		switch sub.Name {
		case "start":
			err = h.service.Start(context.Background(), gid, id, time.Now().UTC())
		case "end":
			err = h.service.End(context.Background(), gid, id, time.Now().UTC())
		case "cancel":
			err = h.service.Cancel(context.Background(), gid, id)
		}
		if err != nil {
			respondEphemeral(s, i, "Event operation failed: "+err.Error())
			return
		}
		respondEphemeral(s, i, "Event updated.")
	}
}

type BountyCommandHandler struct {
	bounties *repository.BountyRepository
	players  *repository.PlayerRepository
	guilds   GuildStore
}

func NewBountyCommandHandler(b *repository.BountyRepository, p *repository.PlayerRepository, g GuildStore) *BountyCommandHandler {
	return &BountyCommandHandler{bounties: b, players: p, guilds: g}
}
func RegisterBountyCommands(session *discordgo.Session, guildID string) error {
	cmd := &discordgo.ApplicationCommand{Name: "bounty", Description: "Champion target bounties", Options: []*discordgo.ApplicationCommandOption{{Name: "create", Description: "Create a bounty", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "player", Description: "Target player", Type: discordgo.ApplicationCommandOptionString, Required: true}, {Name: "points", Description: "Champion Points", Type: discordgo.ApplicationCommandOptionInteger, Required: true}, {Name: "duration", Description: "Duration such as 2h", Type: discordgo.ApplicationCommandOptionString, Required: true}}}, {Name: "list", Description: "List active bounties", Type: discordgo.ApplicationCommandOptionSubCommand}, {Name: "status", Description: "Show a target bounty", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "player", Description: "Target player", Type: discordgo.ApplicationCommandOptionString, Required: true}}}, {Name: "cancel", Description: "Cancel a bounty", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "Bounty ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}}}
	_, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, cmd)
	return err
}
func (h *BountyCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.bounties == nil || h.players == nil || h.guilds == nil || i == nil {
		respondEphemeral(s, i, "Bounties are unavailable.")
		return
	}
	_, gid, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || gid == 0 {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	sub := i.ApplicationCommandData().Options[0]
	switch sub.Name {
	case "list":
		rows, err := h.bounties.ListActive(context.Background(), gid, 5)
		if err != nil {
			respondEphemeral(s, i, "Could not load bounties.")
			return
		}
		var b strings.Builder
		b.WriteString("🎯 **CHAMPION BOUNTIES**\n\n")
		for n, row := range rows {
			fmt.Fprintf(&b, "%d. Player %d — %d Champion Points\n", n+1, row.TargetPlayerID, row.RewardPoints)
		}
		respondEphemeral(s, i, b.String())
	case "status":
		name := optionString(sub, "player")
		pid, err := h.players.FindByDisplayName(context.Background(), gid, name)
		if err != nil {
			respondEphemeral(s, i, "Player not found.")
			return
		}
		bounty, err := h.bounties.GetActive(context.Background(), gid, pid)
		if err != nil || bounty == nil {
			respondEphemeral(s, i, "No active bounty.")
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("🎯 **ACTIVE BOUNTY**\n%s\n🏆 %d Champion Points", name, bounty.RewardPoints))
	case "create":
		if !isAdminInteraction(i) {
			respondEphemeral(s, i, "Administrator or Manage Server permission required.")
			return
		}
		pid, err := h.players.FindByDisplayName(context.Background(), gid, optionString(sub, "player"))
		if err != nil {
			respondEphemeral(s, i, "Player not found.")
			return
		}
		points := sub.Options[1].IntValue()
		duration, err := time.ParseDuration(optionString(sub, "duration"))
		if err != nil || points <= 0 {
			respondEphemeral(s, i, "Use positive points and a valid duration.")
			return
		}
		created, err := h.bounties.Create(context.Background(), repository.Bounty{GuildID: gid, TargetPlayerID: pid, RewardPoints: points, CreatedByType: repository.BountyAdmin, StartsAt: ptrTime(time.Now().UTC()), ExpiresAt: ptrTime(time.Now().UTC().Add(duration))}, i.Member.User.ID)
		if err != nil {
			respondEphemeral(s, i, "Could not create bounty: "+err.Error())
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("🎯 Bounty created for %d Champion Points (ID %d).", created.RewardPoints, created.ID))
	case "cancel":
		if !isAdminInteraction(i) {
			respondEphemeral(s, i, "Administrator or Manage Server permission required.")
			return
		}
		if err := h.bounties.Cancel(context.Background(), gid, optionInt(sub, "id")); err != nil {
			respondEphemeral(s, i, "Could not cancel bounty.")
			return
		}
		respondEphemeral(s, i, "Bounty cancelled.")
	}
}
func ptrTime(t time.Time) *time.Time { return &t }
func optionString(o *discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, v := range o.Options {
		if v.Name == name {
			return v.StringValue()
		}
	}
	return ""
}
func optionInt(o *discordgo.ApplicationCommandInteractionDataOption, name string) int64 {
	for _, v := range o.Options {
		if v.Name == name {
			return v.IntValue()
		}
	}
	return 0
}
