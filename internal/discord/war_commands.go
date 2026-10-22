package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/factionstats"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// hubFactionLookup is the /faction command's view of the Faction Hub, so tests can substitute fakes.
type hubFactionLookup interface {
	FactionsByTag(ctx context.Context, guildID int64, tag string) ([]repository.HubFactionRef, error)
	MemberFactions(ctx context.Context, guildID, playerID int64) ([]repository.HubFactionRef, error)
	Rivalry(ctx context.Context, guildID, a, b int64) (repository.HubRivalry, error)
	GuildInstallations(ctx context.Context, guildID int64) ([]repository.HubInstallationRef, error)
}

// hubFactionFigures are the Faction Hub stats the profile and leaderboard show.
type hubFactionFigures interface {
	GetFactionStats(ctx context.Context, organizationID, installationID, factionID int64) (*factionstats.Stats, error)
	GetFactionLeaderboard(ctx context.Context, organizationID, installationID int64, q factionstats.LeaderboardQuery) (*factionstats.Leaderboard, error)
}

// WarCommandHandler serves /faction: wars, rivalries, the leaderboard and faction profiles, all
// for Faction Hub factions. Factions are named by tag; a faction's leader declares, accepts,
// declines and ends its wars, and server staff may act for any faction.
type WarCommandHandler struct {
	wars    *repository.PostgresWarRepository
	guilds  GuildStore
	seasons *repository.SeasonRepository
	links   *repository.LinkRepository
	hub     hubFactionLookup
	figures hubFactionFigures
	players completionPlayerNames
}

func NewWarCommandHandler(wars *repository.PostgresWarRepository, guilds GuildStore, seasons *repository.SeasonRepository, links *repository.LinkRepository, hub *repository.FactionHubMembershipRepository, figures *factionstats.Service, players *repository.PlayerRepository) *WarCommandHandler {
	h := &WarCommandHandler{wars: wars, guilds: guilds, seasons: seasons, links: links}
	// Assign only non-nil repositories so the interface nil checks hold.
	if hub != nil {
		h.hub = hub
	}
	if figures != nil {
		h.figures = figures
	}
	if players != nil {
		h.players = players
	}
	return h
}

// leaderboardCategories maps the command's categories to Faction Hub leaderboard metrics.
var leaderboardCategories = []struct {
	name   string
	metric factionstats.LeaderboardMetric
}{
	{"kills", factionstats.LeaderboardKills},
	{"kd", factionstats.LeaderboardKD},
	{"headshots", factionstats.LeaderboardHeadshots},
	{"longshots", factionstats.LeaderboardLongshots},
	{"streak", factionstats.LeaderboardBestStreak},
	{"bounties", factionstats.LeaderboardBountiesClaimed},
}

func RegisterWarCommands(session CommandRegistrar, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
	warID := func(required bool) []*discordgo.ApplicationCommandOption {
		return []*discordgo.ApplicationCommandOption{{Name: "id", Description: "War number", Type: discordgo.ApplicationCommandOptionInteger, Required: required}}
	}
	categories := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(leaderboardCategories))
	for _, c := range leaderboardCategories {
		categories = append(categories, &discordgo.ApplicationCommandOptionChoice{Name: c.name, Value: c.name})
	}
	war := &discordgo.ApplicationCommandOption{Name: "war", Description: "Faction wars", Type: discordgo.ApplicationCommandOptionSubCommandGroup, Options: []*discordgo.ApplicationCommandOption{
		{Name: "challenge", Description: "Challenge another faction to a war (faction leaders)", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{
			{Name: "opponent", Description: "Tag of the faction you challenge", Type: discordgo.ApplicationCommandOptionString, Required: true},
			{Name: "faction", Description: "Your faction's tag (needed only by staff, or if you lead factions on several servers)", Type: discordgo.ApplicationCommandOptionString},
		}},
		{Name: "accept", Description: "Accept a war your faction was challenged to", Type: discordgo.ApplicationCommandOptionSubCommand, Options: warID(true)},
		{Name: "decline", Description: "Decline a war your faction was challenged to", Type: discordgo.ApplicationCommandOptionSubCommand, Options: warID(true)},
		{Name: "end", Description: "End an active war", Type: discordgo.ApplicationCommandOptionSubCommand, Options: warID(true)},
		{Name: "cancel", Description: "Cancel a war (staff)", Type: discordgo.ApplicationCommandOptionSubCommand, Options: warID(true)},
		{Name: "status", Description: "Show the active wars, or one war", Type: discordgo.ApplicationCommandOptionSubCommand, Options: warID(false)},
		{Name: "history", Description: "Show recent wars", Type: discordgo.ApplicationCommandOptionSubCommand},
	}}
	rivalry := &discordgo.ApplicationCommandOption{Name: "rivalry", Description: "Show the kill exchange between two factions", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "faction", Description: "Faction tag", Type: discordgo.ApplicationCommandOptionString, Required: true}, {Name: "opponent", Description: "Opponent faction tag", Type: discordgo.ApplicationCommandOptionString, Required: true}}}
	leaderboard := &discordgo.ApplicationCommandOption{Name: "leaderboard", Description: "Show faction rankings", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "category", Description: "What to rank by (default kills)", Type: discordgo.ApplicationCommandOptionString, Choices: categories}}}
	info := &discordgo.ApplicationCommandOption{Name: "info", Description: "Show a faction's profile", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "tag", Description: "Faction tag", Type: discordgo.ApplicationCommandOptionString, Required: true}}}
	cmd := &discordgo.ApplicationCommand{Name: "faction", Description: "Faction wars, rivalries, rankings and profiles", Options: []*discordgo.ApplicationCommandOption{war, rivalry, leaderboard, info}}
	_, err = session.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}

func (h *WarCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.wars == nil || h.guilds == nil || h.hub == nil || i == nil {
		respondEphemeral(s, i, ReplyAreUnavailable("Faction wars"))
		return
	}
	_, gid, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || gid == 0 {
		respondEphemeral(s, i, ReplyNotSetUp)
		return
	}
	data := i.ApplicationCommandData()
	if len(data.Options) == 0 {
		return
	}
	group := data.Options[0]
	ctx := context.Background()
	switch group.Name {
	case "rivalry":
		h.handleRivalry(s, i, ctx, gid, group)
		return
	case "leaderboard":
		h.handleLeaderboard(s, i, ctx, gid, group)
		return
	case "info":
		h.handleInfo(s, i, ctx, gid, group)
		return
	}
	if len(group.Options) == 0 {
		return
	}
	sub := group.Options[0]
	switch sub.Name {
	case "challenge":
		h.handleChallenge(s, i, ctx, gid, sub)
	case "accept", "decline", "end":
		h.handleWarChange(s, i, ctx, gid, sub)
	case "cancel":
		if !isAdminInteraction(i) {
			respondEphemeral(s, i, ReplyNeedsPermission("cancel a war"))
			return
		}
		h.respondWarChange(s, i, sub.Name, h.wars.CancelWar(ctx, gid, optionInt(sub, "id")))
	case "status":
		h.handleStatus(s, i, ctx, gid, sub)
	case "history":
		rows, err := h.wars.GetWarHistory(ctx, gid, 10)
		if err != nil {
			respondEphemeral(s, i, ReplyCouldNot("load the war history"))
			return
		}
		respondEphemeral(s, i, warHistoryText(rows))
	}
}

// callerPlayer is the caller's verified player on this Discord server, or 0.
func (h *WarCommandHandler) callerPlayer(ctx context.Context, i *discordgo.InteractionCreate, guildID int64) int64 {
	if h.links == nil || i.Member == nil || i.Member.User == nil {
		return 0
	}
	playerID, err := h.links.GetVerifiedPlayerByDiscord(ctx, guildID, i.Member.User.ID)
	if err != nil {
		return 0
	}
	return playerID
}

// ledFactions are the factions the caller leads on this Discord server.
func (h *WarCommandHandler) ledFactions(ctx context.Context, i *discordgo.InteractionCreate, guildID int64) []repository.HubFactionRef {
	playerID := h.callerPlayer(ctx, i, guildID)
	if playerID == 0 {
		return nil
	}
	memberships, err := h.hub.MemberFactions(ctx, guildID, playerID)
	if err != nil {
		return nil
	}
	var out []repository.HubFactionRef
	for _, m := range memberships {
		if m.Role == factionhub.RoleLeader {
			out = append(out, m)
		}
	}
	return out
}

// leads reports whether the caller is server staff or the leader of factionID.
func (h *WarCommandHandler) leads(ctx context.Context, i *discordgo.InteractionCreate, guildID, factionID int64) bool {
	if isAdminInteraction(i) {
		return true
	}
	if factionID <= 0 {
		return false
	}
	for _, f := range h.ledFactions(ctx, i, guildID) {
		if f.ID == factionID {
			return true
		}
	}
	return false
}

func sameTag(f repository.HubFactionRef, tag string) bool {
	return strings.EqualFold(f.Tag, strings.TrimSpace(strings.Trim(strings.TrimSpace(tag), "[]")))
}

// pickWarSides chooses the challenger among candidates and the opponent among opponents: two
// different factions of the same installation. ok is false when there is no such pair, and
// ambiguous when there is more than one.
func pickWarSides(candidates, opponents []repository.HubFactionRef) (challenger, opponent repository.HubFactionRef, ok, ambiguous bool) {
	for _, c := range candidates {
		for _, o := range opponents {
			if c.InstallationID != o.InstallationID || c.ID == o.ID {
				continue
			}
			if ok {
				return challenger, opponent, false, true
			}
			challenger, opponent, ok = c, o, true
		}
	}
	return challenger, opponent, ok, false
}

func (h *WarCommandHandler) handleChallenge(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, guildID int64, sub *discordgo.ApplicationCommandInteractionDataOption) {
	opponentTag, factionTag := optionString(sub, "opponent"), optionString(sub, "faction")
	opponents, err := h.hub.FactionsByTag(ctx, guildID, opponentTag)
	if err != nil {
		respondEphemeral(s, i, ReplyCouldNot("look up the factions"))
		return
	}
	if len(opponents) == 0 {
		respondEphemeral(s, i, fmt.Sprintf("No faction with the tag **%s** on this server.", safePanelText(opponentTag)))
		return
	}
	var candidates []repository.HubFactionRef
	for _, f := range h.ledFactions(ctx, i, guildID) {
		if factionTag == "" || sameTag(f, factionTag) {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 && isAdminInteraction(i) {
		if factionTag == "" {
			respondEphemeral(s, i, "Name the challenging faction with the `faction` option.")
			return
		}
		if candidates, err = h.hub.FactionsByTag(ctx, guildID, factionTag); err != nil {
			respondEphemeral(s, i, ReplyCouldNot("look up the factions"))
			return
		}
	}
	if len(candidates) == 0 {
		respondEphemeral(s, i, "Only a faction's leader can declare its wars. Link your gamertag and lead a faction in the Faction Hub first.")
		return
	}
	challenger, opponent, ok, ambiguous := pickWarSides(candidates, opponents)
	if ambiguous {
		respondEphemeral(s, i, "Those tags match factions on more than one server here. Add the `faction` option to say which faction is challenging.")
		return
	}
	if !ok {
		respondEphemeral(s, i, "A war needs two different factions of the same game server.")
		return
	}
	seasonID := int64(0)
	if h.seasons != nil {
		if season, _ := h.seasons.GetActiveSeason(ctx, guildID); season != nil {
			seasonID = season.ID
		}
	}
	war, err := h.wars.Challenge(ctx, challenger.InstallationID, seasonID, challenger.ID, opponent.ID, h.callerPlayer(ctx, i, guildID))
	switch {
	case errors.Is(err, repository.ErrWarAlreadyOpen):
		respondEphemeral(s, i, fmt.Sprintf("**%s** and **%s** already have a pending or active war.", safePanelText(challenger.Label()), safePanelText(opponent.Label())))
		return
	case err != nil:
		respondEphemeral(s, i, ReplyCouldNot("create the war"))
		return
	}
	respondEphemeral(s, i, fmt.Sprintf("⚔️ War **#%d** declared: **%s** vs **%s**.\nA leader of %s accepts with `/faction war accept id:%d`.", war.ID, safePanelText(war.FactionALabel), safePanelText(war.FactionBLabel), safePanelText(war.FactionBLabel), war.ID))
}

// handleWarChange accepts or declines a challenge (the challenged faction's leader) or ends a
// war (a leader of either faction).
func (h *WarCommandHandler) handleWarChange(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, guildID int64, sub *discordgo.ApplicationCommandInteractionDataOption) {
	id := optionInt(sub, "id")
	war, err := h.wars.GetWar(ctx, guildID, id)
	if err != nil {
		respondEphemeral(s, i, "War not found.")
		return
	}
	var allowed bool
	var need string
	switch sub.Name {
	case "accept", "decline":
		allowed, need = h.leads(ctx, i, guildID, war.FactionBID), "the leader of the challenged faction"
	default:
		allowed = h.leads(ctx, i, guildID, war.FactionAID) || h.leads(ctx, i, guildID, war.FactionBID)
		need = "a leader of either faction"
	}
	if !allowed {
		respondEphemeral(s, i, fmt.Sprintf("Only %s can do that.", need))
		return
	}
	switch sub.Name {
	case "accept":
		err = h.wars.AcceptWar(ctx, guildID, id)
	case "decline":
		err = h.wars.DeclineWar(ctx, guildID, id)
	default:
		err = h.wars.EndWar(ctx, guildID, id)
	}
	h.respondWarChange(s, i, sub.Name, err)
}

var warChangeReplies = map[string]string{"accept": "⚔️ War accepted. Kills between the two factions now count.", "decline": "War declined.", "end": "🏁 War ended.", "cancel": "War cancelled."}

func (h *WarCommandHandler) respondWarChange(s *discordgo.Session, i *discordgo.InteractionCreate, action string, err error) {
	switch {
	case errors.Is(err, repository.ErrWarNotChanged):
		respondEphemeral(s, i, "That war can't be changed that way now (check its status with `/faction war status`).")
	case err != nil:
		respondEphemeral(s, i, ReplyCouldNot("update the war"))
	default:
		respondEphemeral(s, i, warChangeReplies[action])
	}
}

func (h *WarCommandHandler) handleStatus(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, guildID int64, sub *discordgo.ApplicationCommandInteractionDataOption) {
	warID := optionInt(sub, "id")
	if warID == 0 {
		wars, err := h.wars.GetActiveWars(ctx, guildID)
		if err != nil || len(wars) == 0 {
			respondEphemeral(s, i, "No active faction wars.")
			return
		}
		var b strings.Builder
		b.WriteString("⚔️ **Active wars**\n\n")
		for _, w := range wars {
			aScore, bScore, _ := h.wars.GetWarScore(ctx, guildID, w.ID)
			fmt.Fprintf(&b, "**#%d** %s **%d** — **%d** %s\n", w.ID, safePanelText(w.FactionALabel), aScore, bScore, safePanelText(w.FactionBLabel))
		}
		b.WriteString("\nUse `/faction war status id:<war>` for details.")
		respondEphemeral(s, i, b.String())
		return
	}
	war, err := h.wars.GetWar(ctx, guildID, warID)
	if err != nil {
		respondEphemeral(s, i, "War not found.")
		return
	}
	aScore, bScore, err := h.wars.GetWarScore(ctx, guildID, warID)
	if err != nil {
		respondEphemeral(s, i, ReplyCouldNot("load the war score"))
		return
	}
	topID, topCount, _ := h.wars.GetWarTopKiller(ctx, guildID, warID)
	longID, longest, _ := h.wars.GetWarLongestKill(ctx, guildID, warID)
	names := h.playerNames(ctx, guildID, topID, longID)
	respondEphemeral(s, i, warStatusText(*war, aScore, bScore, names[topID], topCount, names[longID], longest))
}

func (h *WarCommandHandler) playerNames(ctx context.Context, guildID int64, ids ...int64) map[int64]string {
	names := map[int64]string{}
	if h.players != nil {
		// A failed lookup only drops the name lines.
		if resolved, err := h.players.DisplayNamesByID(ctx, guildID, ids); err == nil {
			names = resolved
		}
	}
	// ID 0 means no holder; never render a name for it.
	delete(names, 0)
	return names
}

// warStatusText formats one war. An empty holder name omits that line.
func warStatusText(w repository.War, aScore, bScore int64, topName string, topKills int64, longestName string, longest float64) string {
	lead := "Tied"
	if aScore > bScore {
		lead = fmt.Sprintf("%s +%d", w.FactionALabel, aScore-bScore)
	} else if bScore > aScore {
		lead = fmt.Sprintf("%s +%d", w.FactionBLabel, bScore-aScore)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "⚔️ **Faction war #%d** — %s\n\n%s — **%d**\n%s — **%d**\n\nLead: %s", w.ID, strings.ToLower(w.Status), safePanelText(w.FactionALabel), aScore, safePanelText(w.FactionBLabel), bScore, safePanelText(lead))
	if winner := w.WinnerLabel(); winner != "" {
		fmt.Fprintf(&b, "\nWinner: %s", safePanelText(winner))
	}
	if topName != "" {
		fmt.Fprintf(&b, "\n🔥 Top killer: %s — %d kills", presentation.SafeName(topName, presentation.MaxRankNameRunes), topKills)
	}
	if longestName != "" {
		fmt.Fprintf(&b, "\n🎯 Longest kill: %s — %s", presentation.SafeName(longestName, presentation.MaxRankNameRunes), presentation.FormatDistance(longest))
	}
	return b.String()
}

func warHistoryText(rows []repository.War) string {
	if len(rows) == 0 {
		return "No faction wars yet."
	}
	var b strings.Builder
	b.WriteString("⚔️ **Faction war history**\n\n")
	for _, w := range rows {
		fmt.Fprintf(&b, "**#%d** %s vs %s — %s", w.ID, safePanelText(w.FactionALabel), safePanelText(w.FactionBLabel), strings.ToLower(w.Status))
		if w.Status == repository.WarEnded {
			fmt.Fprintf(&b, " (%d–%d)", w.ScoreA, w.ScoreB)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// factionByTag resolves one faction by tag on this Discord server. found is false when there is
// none; ambiguous when the tag is used on several of its game servers.
func (h *WarCommandHandler) factionByTag(ctx context.Context, guildID int64, tag string) (f repository.HubFactionRef, found, ambiguous bool, err error) {
	refs, err := h.hub.FactionsByTag(ctx, guildID, tag)
	if err != nil || len(refs) == 0 {
		return f, false, false, err
	}
	if len(refs) > 1 {
		return f, false, true, nil
	}
	return refs[0], true, false, nil
}

func (h *WarCommandHandler) handleInfo(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, guildID int64, group *discordgo.ApplicationCommandInteractionDataOption) {
	if h.figures == nil {
		respondEphemeral(s, i, ReplyAreUnavailable("Faction profiles"))
		return
	}
	f, found, ambiguous, err := h.factionByTag(ctx, guildID, optionString(group, "tag"))
	switch {
	case err != nil:
		respondEphemeral(s, i, ReplyCouldNot("load the faction"))
		return
	case ambiguous:
		respondEphemeral(s, i, "That tag is used by factions on more than one server here. Open the Faction Hub to see each one.")
		return
	case !found:
		respondEphemeral(s, i, "Faction not found.")
		return
	}
	stats, err := h.figures.GetFactionStats(ctx, f.OrganizationID, f.InstallationID, f.ID)
	if err != nil || stats == nil {
		respondEphemeral(s, i, ReplyCouldNot("load the faction profile"))
		return
	}
	respondEphemeral(s, i, factionInfoText(f, *stats))
}

// factionInfoText formats a Faction Hub faction's profile.
func factionInfoText(f repository.HubFactionRef, stats factionstats.Stats) string {
	sum := stats.Summary
	var b strings.Builder
	fmt.Fprintf(&b, "🏆 **%s**\n\n", safePanelText(f.Label()))
	for _, m := range stats.MemberContributions {
		if m.Role != nil && *m.Role == factionhub.RoleLeader && m.Status == factionstats.StatusActive {
			fmt.Fprintf(&b, "👑 **Leader**\n%s\n\n", presentation.SafeName(m.DisplayName, presentation.MaxRankNameRunes))
			break
		}
	}
	fmt.Fprintf(&b, "👥 **Members**\n%d\n\n⚔️ **Combat**\n%d kills\n%d deaths\n%.2f K/D\n%d headshots\n%d longshots\nBest streak %d\n\n💰 **Bounties claimed**\n%d",
		sum.MemberCount, sum.Kills, sum.Deaths, sum.KDRatio, sum.Headshots, sum.Longshots, sum.BestKillStreak, sum.BountiesClaimed)
	return b.String()
}

func (h *WarCommandHandler) handleRivalry(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, guildID int64, group *discordgo.ApplicationCommandInteractionDataOption) {
	a, aFound, aAmbiguous, err := h.factionByTag(ctx, guildID, optionString(group, "faction"))
	if err != nil {
		respondEphemeral(s, i, ReplyCouldNot("load the rivalry"))
		return
	}
	b, bFound, bAmbiguous, err := h.factionByTag(ctx, guildID, optionString(group, "opponent"))
	switch {
	case err != nil:
		respondEphemeral(s, i, ReplyCouldNot("load the rivalry"))
		return
	case aAmbiguous || bAmbiguous:
		respondEphemeral(s, i, "A tag is used by factions on more than one server here. Open the Faction Hub to compare them.")
		return
	case !aFound:
		respondEphemeral(s, i, "Faction not found.")
		return
	case !bFound:
		respondEphemeral(s, i, "Opponent faction not found.")
		return
	case a.ID == b.ID:
		respondEphemeral(s, i, "Pick two different factions.")
		return
	}
	text, err := h.rivalryText(ctx, guildID, a, b)
	if err != nil {
		respondEphemeral(s, i, ReplyCouldNot("load the rivalry"))
		return
	}
	respondEphemeral(s, i, text)
}

// rivalryText loads the rivalry and resolves the record holders to their display names. A
// holder that cannot be resolved is passed as "" and its line is omitted.
func (h *WarCommandHandler) rivalryText(ctx context.Context, guildID int64, a, b repository.HubFactionRef) (string, error) {
	r, err := h.hub.Rivalry(ctx, guildID, a.ID, b.ID)
	if err != nil {
		return "", err
	}
	names := h.playerNames(ctx, guildID, r.LongestPlayerID, r.TopPlayerID)
	return BuildRivalryText(a, b, r, names[r.LongestPlayerID], names[r.TopPlayerID]), nil
}

// BuildRivalryText formats the rivalry response. It is pure formatting: the caller resolves
// names, and an empty holder name omits that line.
func BuildRivalryText(a, b repository.HubFactionRef, r repository.HubRivalry, longestName, topName string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "🔥 **Champion rivalry**\n\n%s vs %s\n\n⚔️ Kill exchange\n%s — %d\n%s — %d\n\n📊 Total encounters\n%d\n🏆 Wars\n%d\n📈 Differential\n%s %+d",
		safePanelText(a.Label()), safePanelText(b.Label()), safePanelText(a.Tag), r.AKills, safePanelText(b.Tag), r.BKills, r.AKills+r.BKills, r.Wars, safePanelText(a.Tag), r.AKills-r.BKills)
	if longestName != "" {
		fmt.Fprintf(&sb, "\n🎯 Longest rivalry kill\n%s — %s", presentation.SafeName(longestName, presentation.MaxRankNameRunes), presentation.FormatDistance(r.Longest))
	}
	if topName != "" {
		fmt.Fprintf(&sb, "\n🔥 Most active killer\n%s — %d rivalry kills", presentation.SafeName(topName, presentation.MaxRankNameRunes), r.TopKills)
	}
	return sb.String()
}

func (h *WarCommandHandler) handleLeaderboard(s *discordgo.Session, i *discordgo.InteractionCreate, ctx context.Context, guildID int64, group *discordgo.ApplicationCommandInteractionDataOption) {
	if h.figures == nil {
		respondEphemeral(s, i, ReplyAreUnavailable("Faction leaderboards"))
		return
	}
	category := strings.ToLower(strings.TrimSpace(optionString(group, "category")))
	if category == "" {
		category = "kills"
	}
	metric := factionstats.LeaderboardMetric("")
	for _, c := range leaderboardCategories {
		if c.name == category {
			metric = c.metric
		}
	}
	if metric == "" {
		respondEphemeral(s, i, "Unknown category.")
		return
	}
	installs, err := h.hub.GuildInstallations(ctx, guildID)
	if err != nil {
		respondEphemeral(s, i, ReplyCouldNot("load the faction leaderboard"))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🏆 **Faction leaderboard — %s**\n", category)
	for _, in := range installs {
		board, err := h.figures.GetFactionLeaderboard(ctx, in.OrganizationID, in.ID, factionstats.LeaderboardQuery{Metric: metric, Limit: 10})
		if err != nil {
			respondEphemeral(s, i, ReplyCouldNot("load the faction leaderboard"))
			return
		}
		if len(installs) > 1 {
			fmt.Fprintf(&b, "\n**%s**\n", safePanelText(in.ServerName))
		} else {
			b.WriteString("\n")
		}
		b.WriteString(leaderboardLines(*board))
	}
	if len(installs) == 0 {
		b.WriteString("\nNo faction data yet.")
	}
	respondEphemeral(s, i, b.String())
}

func leaderboardLines(board factionstats.Leaderboard) string {
	if len(board.Items) == 0 {
		return "No factions yet.\n"
	}
	var b strings.Builder
	for _, e := range board.Items {
		value := fmt.Sprintf("%d", int64(e.Value))
		if board.Metric.IsDecimal() {
			value = fmt.Sprintf("%.2f", e.Value)
		}
		fmt.Fprintf(&b, "%d. [%s] %s — %s\n", e.Rank, safePanelText(e.Faction.Tag), safePanelText(e.Faction.Name), value)
	}
	return b.String()
}
