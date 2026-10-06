package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/progression"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The design preview, part two: the cards built in this package (map rotation, challenges, battle
// pass, ranked, territory). See internal/discord/design_preview_test.go for how to write the
// file and what the rules are; both packages merge into the same DESIGN_PREVIEW_OUT file.

type designPreviewCard struct {
	ID      string                    `json:"id"`
	Title   string                    `json:"title"`
	Group   string                    `json:"group"`
	Source  string                    `json:"source"`
	Content string                    `json:"content,omitempty"`
	Embed   *discordgo.MessageEmbed   `json:"embed,omitempty"`
	Embeds  []*discordgo.MessageEmbed `json:"embeds,omitempty"`
}

const designPreviewSource = "internal/app"

var (
	previewAt     = time.Date(2026, 10, 5, 18, 30, 0, 0, time.UTC)
	previewServer = "Example Server Alpha"
)

func designPreviewCards() []designPreviewCard {
	var cards []designPreviewCard
	add := func(id, title, group, content string, embeds ...*discordgo.MessageEmbed) {
		c := designPreviewCard{ID: id, Title: title, Group: group, Source: designPreviewSource, Content: content}
		if len(embeds) > 0 {
			c.Embed = embeds[0]
		}
		if len(embeds) > 1 {
			c.Embeds = embeds
		}
		cards = append(cards, c)
	}
	message := func(id, title, group string, msg *discordgo.MessageSend) {
		add(id, title, group, msg.Content, msg.Embeds...)
	}

	// --- Map rotation -------------------------------------------------------------------------------
	closed := previewAt.Add(2 * time.Hour)
	vote := repository.MapRotationVote{
		ID: 7, OpensAt: previewAt, ClosesAt: previewAt.Add(2 * time.Hour),
		Options: []repository.MapRotationVoteOption{{MapID: 1, Name: "Chernarus", Votes: 14}, {MapID: 2, Name: "Livonia", Votes: 9}, {MapID: 3, Name: "Sakhal", Votes: 21}},
	}
	message("map-vote-open", "Map vote: voting is open", "Announcements", buildMapVoteOpenMessage(previewServer, vote, "https://example.invalid/vote/1", true))
	vote.ClosedAt, vote.WinnerName, vote.TotalVotes = &closed, "Sakhal", 44
	message("map-vote-result", "Map vote: the result", "Announcements", buildMapVoteResultMessage(previewServer, vote))
	message("map-changed", "Map vote: the map changed", "Announcements", buildMapChangedMessage(previewServer, "Sakhal", previewAt.Add(3*time.Hour)))

	// The failure alert is built by the real worker code and rendered by the staff alert builder.
	var failure discord.AdminAlert
	a := &App{mapRotationAlert: func(alert discord.AdminAlert) { failure = alert }}
	a.mapRotationStaffAlert(repository.MapRotationTarget{GuildID: 1, ServerID: 1, ServerName: previewServer},
		repository.MapRotationSwitch{MapName: "Sakhal", Message: "The server settings could not be saved at Nitrado, so the map was left as it is."}, true)
	failure.At = previewAt
	add("map-rotation-failed", "Staff alert: the map could not be changed", "Staff", "", discord.BuildAdminAlertEmbed(failure, previewServer))

	// --- Progression --------------------------------------------------------------------------------
	add("challenges-daily", "Daily challenges", "Progression", "", buildChallengesCard(repository.ChallengeSet{
		Period: progression.PeriodDay, StartsAt: previewAt, EndsAt: previewAt.Add(24 * time.Hour),
		Challenges: []progression.Challenge{{Title: "Get 2 kills"}, {Title: "Get 1 headshot kill"}, {Title: "Get a kill with a sniper rifle"}},
	}, 1500, previewServer))
	season := repository.BattlePassSeason{Name: "Example Season One", StartsAt: previewAt, EndsAt: previewAt.Add(30 * 24 * time.Hour), Levels: 30,
		PremiumPrice: 25000, XPKill: 50, XPHour: 100, XPDailyChallenge: 200, XPWeeklyChallenge: 600}
	add("battle-pass-start", "Battle pass: a season starts", "Progression", "", buildBattlePassStartCard(season, previewServer))
	add("battle-pass-end", "Battle pass: a season ends", "Progression", "", buildBattlePassEndCard(season, []repository.BattlePassProgress{
		{Name: "Sample_Raven", XP: 48250, Level: 30}, {Name: "Fictional_Fox", XP: 31900, Level: 24}, {Name: "Made_Up_Moose", XP: 12040, Level: 11},
	}, previewServer))
	add("rank-up", "Ranked: a player ranks up", "Progression", "", buildRankUpCard(repository.RankUp{Name: "Sample_Raven", From: ranked.Gold, To: ranked.Platinum, RP: 12850, Position: 3}, previewServer, "https://example.invalid"))
	add("ranked-wanted", "Ranked: a bounty on the top player", "Progression", "", buildWantedCard(repository.WantedPlayer{Name: "Sample_Raven", Reason: repository.WantedStreak, Streak: 12, RP: 12850, Bounty: 500}, previewServer))
	add("ranked-week-recap", "Ranked: week in review", "Progression", "", buildWeeklyRecapCard(repository.RankedRecap{
		WeekStart: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), Kills: 1482,
		Climbers:   []repository.RPBoostLeader{{PlayerName: "Sample_Raven", RP: 2150, Kills: 43}, {PlayerName: "Fictional_Fox", RP: 1400, Kills: 28}},
		BestStreak: 14, StreakName: "Made_Up_Moose", Revenges: 37, RevengeName: "Fictional_Fox", RevengeMost: 6,
	}, previewServer))
	boost := repository.RPBoost{ID: 1, ServerID: 1, Multiplier: 2, StartsAt: previewAt, EndsAt: previewAt.Add(3 * time.Hour)}
	add("double-rp-live", "Ranked: double RP is live", "Progression", "", buildRPBoostCard("STARTED", boost, 25, nil, previewServer, "https://example.invalid/dashboard/player"))
	add("territory-captured", "Territory: a zone was captured", "Progression", "", buildTerritoryCard(progression.Zone{Key: "nwaf", Name: "North West Airfield"},
		repository.TerritoryFaction{ID: 2, Name: "Sample Ravens", Tag: "RVN"}, repository.TerritoryFaction{ID: 1, Name: "Example Wolves", Tag: "WLF"},
		23, repository.TerritorySettings{WindowDays: 7, IncomePoints: 1500}, previewServer))
	add("season-champions", "Ranked: season champions", "Progression", "", buildSeasonChampionsCard([]repository.SeasonFinisher{
		{Name: "Sample_Raven", RP: 12850, Kills: 512}, {Name: "Fictional_Fox", RP: 8400, Kills: 377}, {Name: "Made_Up_Moose", RP: 6020, Kills: 290},
	}, [3]int64{10000, 5000, 2500}, previewServer, repository.EndedSeason{StartsAt: previewAt.Add(-60 * 24 * time.Hour), EndsAt: previewAt}))

	add("ranked-bounty-claimed", "Ranked: a bounty was claimed", "Progression", "", buildBountyClaimCard(repository.BountyClaim{
		KillerName: "Fictional_Fox", VictimName: "Sample_Raven", Detail: "was on a 12-kill streak", BountyRP: 500, TotalRP: 525, At: previewAt,
	}))
	add("double-rp-scheduled", "Ranked: double RP is coming", "Progression", "", buildRPBoostCard("SCHEDULED", boost, 25, nil, previewServer, "https://example.invalid/dashboard/player"))
	add("double-rp-ended", "Ranked: double RP has ended", "Progression", "", buildRPBoostCard("ENDED", boost, 25,
		[]repository.RPBoostLeader{{PlayerName: "Sample_Raven", RP: 1250, Kills: 25}, {PlayerName: "Fictional_Fox", RP: 800, Kills: 16}}, previewServer, ""))
	add("territory-unclaimed", "Territory: a zone is unclaimed", "Progression", "", buildTerritoryCard(progression.Zone{Key: "nwaf", Name: "North West Airfield"},
		repository.TerritoryFaction{ID: 2, Name: "Sample Ravens", Tag: "RVN"}, repository.TerritoryFaction{}, 0, repository.TerritorySettings{WindowDays: 7}, previewServer))
	eventEnds := previewAt.Add(5 * time.Hour)
	add("event-scoreboard-live", "Event scoreboard: live", "Announcements", "", buildEventScoreboard(repository.CompetitiveEvent{Name: "Sniper Sunday", Type: "LONGEST_KILL", EndsAt: &eventEnds},
		[]eventScoreLine{{Name: "Sample_Raven", Score: 1288}, {Name: "Fictional_Fox", Score: 866}}, false, previewAt))
	add("perk-shoutout", "Supporters: a perk was bought", "Announcements", "", buildPerkShoutout(repository.PerkPurchase{BuyerName: "Sample_Raven", OfferName: "Supporter Pack", PriorityQueue: true, DurationDays: 30, CreatedAt: previewAt},
		[]repository.PerkSupporter{{PlayerName: "Sample_Raven", Points: 25000}, {PlayerName: "Fictional_Fox", Points: 12500}}, "https://example.invalid/dashboard/player"))
	add("automation-news", "New features switched on", "Announcements", "", buildAutomationNews([][2]string{{"Daily play reward", "Play every day to earn Champion Points."}}, previewServer))
	add("starter-card", "A new feed channel's first card", "Announcements", "", starterEmbed(*destinationByKey("COMBAT_FEED").Starter))
	add("zone-intrusion", "Zone alert: someone entered a protected zone", "Staff", "", buildIntrusionEmbed(killfeed.IntrusionEvent{
		Kind: killfeed.AlertZoneIntrusion, Gamertag: "Fictional_Fox", At: previewAt, Zone: repository.Zone{Name: "Example Trader", ZoneType: "RESTRICTED"},
	}))

	// --- Direct messages ----------------------------------------------------------------------------
	expires := previewAt.Add(30 * 24 * time.Hour)
	message("dm-vip-tier", "DM: you received a supporter tier", "Direct messages", buildVIPNotice(repository.PlayerTier{Name: "Gold Supporter", Badge: "⭐", DiscordRole: true, RewardMultiplier: 1.5, ExpiresAt: &expires},
		previewServer, "https://example.invalid/dashboard/player"))
	message("dm-winback", "DM: we miss you", "Direct messages", buildWinbackDM("Sample_Raven", previewServer, 21, []string{"• A new battle pass season started."}, "https://example.invalid/dashboard/player"))
	bountyTitle, bountyDesc := buildBountyPlacerDM("Fictional_Fox", 25000, "CLAIMED", "Sample_Raven")
	add("dm-bounty-claimed", "DM: the bounty you placed was claimed", "Direct messages", "", upgradeEmbed("Bounties", bountyTitle, bountyDesc, presentation.Crimson))
	message("dm-rank-up", "DM: you ranked up", "Direct messages", buildRankUpDM(repository.RankUp{Name: "Sample_Raven", To: ranked.Platinum, RP: 12850, Position: 3}, previewServer, "https://example.invalid/dashboard/player", "https://example.invalid"))
	return cards
}

func writeDesignPreview(t *testing.T, path, source string, cards []designPreviewCard) {
	t.Helper()
	if !filepath.IsAbs(path) {
		t.Fatalf("DESIGN_PREVIEW_OUT must be an absolute path, got %q", path)
	}
	// Other packages' entries are kept exactly as they were written (raw JSON).
	type sourced struct {
		Source string `json:"source"`
		Group  string `json:"group"`
	}
	var all []json.RawMessage
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		var existing []json.RawMessage
		if err := json.Unmarshal(raw, &existing); err != nil {
			t.Fatalf("existing preview file is not valid: %v", err)
		}
		for _, e := range existing {
			var s sourced
			_ = json.Unmarshal(e, &s)
			if s.Source != source {
				all = append(all, e)
			}
		}
	}
	for _, c := range cards {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b)
	}
	order := map[string]int{"Feeds": 0, "Bounties and economy": 1, "Boards and panels": 2, "Announcements": 3, "Progression": 4, "Direct messages": 5, "Staff": 6, "Command replies": 7}
	group := func(e json.RawMessage) int {
		var s sourced
		_ = json.Unmarshal(e, &s)
		return order[s.Group]
	}
	sort.SliceStable(all, func(i, j int) bool { return group(all[i]) < group(all[j]) })
	out, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d cards from %s (%d in total) to %s", len(cards), source, len(all), path)
}

func TestDesignPreview(t *testing.T) {
	cards := designPreviewCards()
	seen := map[string]bool{}
	for _, c := range cards {
		if c.ID == "" || c.Title == "" || c.Group == "" || seen[c.ID] {
			t.Fatalf("preview card needs a unique id, a title and a group: %+v", c)
		}
		seen[c.ID] = true
		if c.Embed == nil && c.Content == "" {
			t.Fatalf("%s renders nothing", c.ID)
		}
	}
	if path := os.Getenv("DESIGN_PREVIEW_OUT"); path != "" {
		writeDesignPreview(t, path, designPreviewSource, cards)
	}
}
