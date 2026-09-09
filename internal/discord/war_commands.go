package discord

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/factions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type WarCommandHandler struct {
	wars                *repository.PostgresWarRepository
	guilds              GuildStore
	seasons             *repository.SeasonRepository
	factions            *repository.FactionRepository
	links               *repository.LinkRepository
	factionStats        *repository.FactionStatsRepository
	factionPresentation *repository.FactionPresentationRepository
}

func NewWarCommandHandler(wars *repository.PostgresWarRepository, guilds GuildStore, seasons *repository.SeasonRepository, factionRepo *repository.FactionRepository, links *repository.LinkRepository, factionStats *repository.FactionStatsRepository, factionPresentation *repository.FactionPresentationRepository) *WarCommandHandler {
	return &WarCommandHandler{wars: wars, guilds: guilds, seasons: seasons, factions: factionRepo, links: links, factionStats: factionStats, factionPresentation: factionPresentation}
}
func RegisterWarCommands(session *discordgo.Session, guildID string) error {
	warStatus := &discordgo.ApplicationCommandOption{Name: "status", Description: "Show an active or completed war", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "War ID", Type: discordgo.ApplicationCommandOptionInteger}, {Name: "opponent", Description: "Opponent faction tag", Type: discordgo.ApplicationCommandOptionString}}}
	war := &discordgo.ApplicationCommandOption{Name: "war", Description: "Manage faction wars", Type: discordgo.ApplicationCommandOptionSubCommandGroup, Options: []*discordgo.ApplicationCommandOption{{Name: "challenge", Description: "Challenge two factions", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "faction_a", Description: "First faction ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}, {Name: "faction_b", Description: "Second faction ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "accept", Description: "Accept a challenge", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "War ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "decline", Description: "Decline a challenge", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "War ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "end", Description: "End an active war", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "War ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, {Name: "cancel", Description: "Cancel a war", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "id", Description: "War ID", Type: discordgo.ApplicationCommandOptionInteger, Required: true}}}, warStatus, {Name: "history", Description: "Show war history", Type: discordgo.ApplicationCommandOptionSubCommand}}}
	rivalry := &discordgo.ApplicationCommandOption{Name: "rivalry", Description: "Show faction rivalry stats", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "faction", Description: "Faction tag", Type: discordgo.ApplicationCommandOptionString, Required: true}, {Name: "opponent", Description: "Opponent faction tag", Type: discordgo.ApplicationCommandOptionString, Required: true}}}
	leaderboard := &discordgo.ApplicationCommandOption{Name: "leaderboard", Description: "Show faction rankings", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "category", Description: "kills | kd | war-kills | longest", Type: discordgo.ApplicationCommandOptionString}, {Name: "scope", Description: "season | lifetime", Type: discordgo.ApplicationCommandOptionString}}}
	info := &discordgo.ApplicationCommandOption{Name: "info", Description: "Show faction profile", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "tag", Description: "Faction tag", Type: discordgo.ApplicationCommandOptionString, Required: true}}}
	cmd := &discordgo.ApplicationCommand{Name: "faction", Description: "Faction systems", Options: []*discordgo.ApplicationCommandOption{war, rivalry, leaderboard, info}}
	_, err := session.ApplicationCommandCreate(session.State.User.ID, guildID, cmd)
	return err
}
func (h *WarCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.wars == nil || h.guilds == nil || i == nil {
		respondEphemeral(s, i, "Faction wars are unavailable.")
		return
	}
	_, gid, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || gid == 0 {
		respondEphemeral(s, i, "Run `/setup` first.")
		return
	}
	group := i.ApplicationCommandData().Options[0]
	if len(group.Options) == 0 {
		return
	}
	sub := group.Options[0]
	ctx := context.Background()
	if group.Name == "rivalry" {
		h.handleRivalry(s, i, ctx, gid, group)
		return
	}
	if group.Name == "leaderboard" {
		h.handleLeaderboard(s, i, ctx, gid, group)
		return
	}
	if group.Name == "info" {
		h.handleInfo(s, i, ctx, gid, group)
		return
	}
	switch sub.Name {
	case "challenge":
		a, b := optionInt(sub, "faction_a"), optionInt(sub, "faction_b")
		if !h.authorized(ctx, i, gid, a, factions.CanChallengeWar) {
			respondEphemeral(s, i, "Verified OWNER or LEADER membership in the challenging faction is required.")
			return
		}
		seasonID := int64(0)
		if h.seasons != nil {
			if season, _ := h.seasons.GetActiveSeason(ctx, gid); season != nil {
				seasonID = season.ID
			}
		}
		playerID := int64(0)
		if h.links != nil {
			playerID, _ = h.links.GetVerifiedPlayerByDiscord(ctx, gid, i.Member.User.ID)
		}
		war, err := h.wars.CreateChallenge(ctx, gid, seasonID, a, b, playerID)
		if err != nil {
			respondEphemeral(s, i, "Could not create war: "+err.Error())
			return
		}
		respondEphemeral(s, i, fmt.Sprintf("⚔️ War challenge created: **%d** (%d vs %d)", war.ID, war.FactionAID, war.FactionBID))
	case "accept":
		war, lookupErr := h.wars.GetWar(ctx, gid, optionInt(sub, "id"))
		if lookupErr != nil || !h.authorized(ctx, i, gid, war.FactionBID, factions.CanAcceptWar) {
			respondEphemeral(s, i, "Verified OWNER or LEADER membership in the challenged faction is required.")
			return
		}
		err = h.wars.AcceptWar(ctx, gid, optionInt(sub, "id"))
	case "decline":
		war, lookupErr := h.wars.GetWar(ctx, gid, optionInt(sub, "id"))
		if lookupErr != nil || !h.authorized(ctx, i, gid, war.FactionBID, factions.CanDeclineWar) {
			respondEphemeral(s, i, "Verified OWNER or LEADER membership in the challenged faction is required.")
			return
		}
		err = h.wars.DeclineWar(ctx, gid, optionInt(sub, "id"))
	case "end":
		war, lookupErr := h.wars.GetWar(ctx, gid, optionInt(sub, "id"))
		if lookupErr != nil || (!h.authorized(ctx, i, gid, war.FactionAID, factions.CanEndWar) && !h.authorized(ctx, i, gid, war.FactionBID, factions.CanEndWar)) {
			respondEphemeral(s, i, "Verified OWNER or LEADER membership in either faction is required.")
			return
		}
		err = h.wars.EndWar(ctx, gid, optionInt(sub, "id"))
	case "cancel":
		if !isAdminInteraction(i) {
			respondEphemeral(s, i, "Only a server administrator may cancel a war.")
			return
		}
		err = h.wars.CancelWar(ctx, gid, optionInt(sub, "id"))
	case "status":
		warID := optionInt(sub, "id")
		if warID == 0 {
			wars, listErr := h.wars.GetActiveWars(ctx, gid)
			if listErr != nil || len(wars) == 0 {
				respondEphemeral(s, i, "No active faction wars.")
				return
			}
			var b strings.Builder
			b.WriteString("⚔️ **ACTIVE WARS**\n\n")
			for _, active := range wars {
				aScore, bScore, _ := h.wars.GetWarScore(ctx, gid, active.ID)
				fmt.Fprintf(&b, "War %d\nFaction %d %d • Faction %d %d\n\n", active.ID, active.FactionAID, aScore, active.FactionBID, bScore)
			}
			b.WriteString("Use `/faction war status id:<war>` for details.")
			respondEphemeral(s, i, b.String())
			return
		}
		war, lookupErr := h.wars.GetWar(ctx, gid, warID)
		if lookupErr != nil {
			respondEphemeral(s, i, "War not found.")
			return
		}
		aScore, bScore, scoreErr := h.wars.GetWarScore(ctx, gid, warID)
		if scoreErr != nil {
			respondEphemeral(s, i, "Could not load war score.")
			return
		}
		top, topCount, _ := h.wars.GetWarTopKiller(ctx, gid, warID)
		longestPlayer, longest, _ := h.wars.GetWarLongestKill(ctx, gid, warID)
		lead := "TIED"
		if aScore > bScore {
			lead = fmt.Sprintf("Faction %d +%d", war.FactionAID, aScore-bScore)
		} else if bScore > aScore {
			lead = fmt.Sprintf("Faction %d +%d", war.FactionBID, bScore-aScore)
		}
		respondEphemeral(s, i, fmt.Sprintf("⚔️ **FACTION WAR STATUS**\n\nFaction %d — %d\nFaction %d — %d\n\nLead: %s\nTop killer: Player %d — %d\nLongest kill: Player %d — %.1fm\nStatus: %s", war.FactionAID, aScore, war.FactionBID, bScore, lead, top, topCount, longestPlayer, longest, war.Status))
		return
	case "history":
		rows, historyErr := h.wars.GetWarHistory(ctx, gid, 10)
		if historyErr != nil {
			respondEphemeral(s, i, "Could not load war history.")
			return
		}
		var b strings.Builder
		b.WriteString("⚔️ **FACTION WAR HISTORY**\n\n")
		for _, war := range rows {
			fmt.Fprintf(&b, "%d. %d vs %d — %s\n", war.ID, war.FactionAID, war.FactionBID, war.Status)
		}
		respondEphemeral(s, i, b.String())
		return
	}
	if err != nil {
		respondEphemeral(s, i, "War operation failed: "+err.Error())
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("⚔️ War %s.", strings.ToLower(sub.Name)))
}

func (h *WarCommandHandler) handleInfo(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, guildID int64, group *discordgo.ApplicationCommandInteractionDataOption) {
	if h.factionPresentation == nil || h.factions == nil {
		respondEphemeral(s, i, "Faction profiles are unavailable.")
		return
	}
	f, err := h.factions.GetByTag(ctx, guildID, optionString(group, "tag"))
	if err != nil {
		respondEphemeral(s, i, "Faction not found.")
		return
	}
	seasonID := int64(0)
	seasonName := ""
	if h.seasons != nil {
		if season, _ := h.seasons.GetActiveSeason(ctx, guildID); season != nil {
			seasonID = season.ID
			seasonName = season.Name
		}
	}
	p, err := h.factionPresentation.Load(ctx, guildID, seasonID, f.ID)
	if err != nil {
		respondEphemeral(s, i, "Could not load faction profile.")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🏆 **[%s] %s**\n\n👑 **OWNER**\n%s\n\n👥 **MEMBERS**\n%d / %d\n\n", safePanelText(p.Tag), safePanelText(p.Name), safePanelText(p.OwnerName), p.MemberCount, p.MaxMembers)
	if seasonName != "" {
		fmt.Fprintf(&b, "🏆 **SEASON**\n%s\n\n", safePanelText(seasonName))
	}
	fmt.Fprintf(&b, "⚔️ **SEASON COMBAT**\n%d Kills\n%d Deaths\n%.2f K/D\n\n🛡️ **FACTION COMBAT**\n%d Enemy Faction Kills\n%d Team Kills\n%d War Kills\n\n🏆 **CHAMPION POINTS**\n%d", p.SeasonKills, p.SeasonDeaths, p.SeasonKD, p.EnemyFactionKills, p.TeamKills, p.WarKills, p.ChampionPoints)
	respondEphemeral(s, i, b.String())
}

func (h *WarCommandHandler) authorized(ctx context.Context, i *discordgo.InteractionCreate, guildID, factionID int64, capability factions.Capability) bool {
	if isAdminInteraction(i) {
		return true
	}
	if h.links == nil || h.factions == nil || i.Member == nil || i.Member.User == nil {
		return false
	}
	playerID, err := h.links.GetVerifiedPlayerByDiscord(ctx, guildID, i.Member.User.ID)
	if err != nil {
		return false
	}
	membership, err := h.factions.GetMembership(ctx, guildID, playerID)
	return err == nil && membership.FactionID == factionID && factions.Can(membership.Role, capability)
}

func (h *WarCommandHandler) handleRivalry(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, guildID int64, group *discordgo.ApplicationCommandInteractionDataOption) {
	if h.factions == nil || h.factionStats == nil {
		respondEphemeral(s, i, "Faction statistics are unavailable.")
		return
	}
	a, err := h.factions.GetByTag(ctx, guildID, optionString(group, "faction"))
	if err != nil {
		respondEphemeral(s, i, "Faction not found.")
		return
	}
	b, err := h.factions.GetByTag(ctx, guildID, optionString(group, "opponent"))
	if err != nil {
		respondEphemeral(s, i, "Opponent faction not found.")
		return
	}
	r, err := h.factionStats.GetRivalry(ctx, guildID, a.ID, b.ID)
	if err != nil {
		respondEphemeral(s, i, "Could not load rivalry.")
		return
	}
	player, distance, _ := h.factionStats.GetRivalryLongestKill(ctx, guildID, a.ID, b.ID)
	top, count, _ := h.factionStats.GetRivalryTopKiller(ctx, guildID, a.ID, b.ID)
	diff := r.AKills - r.BKills
	if a.ID > b.ID {
		diff = -diff
	}
	respondEphemeral(s, i, fmt.Sprintf("🔥 **CHAMPION RIVALRY**\n\n[%s] %s vs [%s] %s\n\n⚔️ Lifetime Kill Exchange\n%s — %d\n%s — %d\n\n📊 Total Encounters\n%d\n🏆 Wars\n%d\n📈 Differential\n%s %+d\n🎯 Longest Rivalry Kill\nPlayer %d — %.1fm\n🔥 Most Active Killer\nPlayer %d — %d rivalry kills", a.Tag, a.Name, b.Tag, b.Name, a.Tag, r.AKills, b.Tag, r.BKills, r.TotalKills, r.WarCount, a.Tag, diff, player, distance, top, count))
}

func (h *WarCommandHandler) handleLeaderboard(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, guildID int64, group *discordgo.ApplicationCommandInteractionDataOption) {
	if h.factionStats == nil {
		respondEphemeral(s, i, "Faction leaderboards are unavailable.")
		return
	}
	category := strings.ToLower(optionString(group, "category"))
	if category == "" {
		category = "kills"
	}
	scope := strings.ToLower(optionString(group, "scope"))
	if scope == "" {
		scope = "season"
	}
	seasonID := int64(0)
	if scope == "season" && h.seasons != nil {
		if season, _ := h.seasons.GetActiveSeason(ctx, guildID); season != nil {
			seasonID = season.ID
		}
	}
	rows, err := h.factionStats.Leaderboard(ctx, guildID, seasonID, scope, category, 10)
	if err != nil {
		respondEphemeral(s, i, "Could not load faction leaderboard.")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🏆 **FACTION LEADERBOARD — %s**\n\n", strings.ToUpper(category))
	for n, row := range rows {
		fmt.Fprintf(&b, "%d. [%s] %s — %d kills\n", n+1, row.Tag, row.Name, row.Kills)
	}
	if len(rows) == 0 {
		b.WriteString("No faction data yet.")
	}
	respondEphemeral(s, i, b.String())
}
