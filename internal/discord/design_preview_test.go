package discord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/heatmap"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The design preview: a representative set of what the bot posts, rendered from the real
// builders with obviously fictional sample data. It serves two purposes:
//
//   - `DESIGN_PREVIEW_OUT=/abs/path/file.json go test -p 1 -run TestDesignPreview ./internal/discord/ ./internal/app/`
//     writes the set to a JSON file a non-technical owner can be shown before a release;
//   - TestDesignRules walks the same set and asserts the design rules of docs/DISCORD_DESIGN.md.
//
// A card added here is therefore both previewable and rule-checked.

// designPreviewCard is one entry of the preview file.
type designPreviewCard struct {
	ID      string                    `json:"id"`
	Title   string                    `json:"title"` // for humans
	Group   string                    `json:"group"`
	Source  string                    `json:"source"`            // the Go package that built it
	Content string                    `json:"content,omitempty"` // message text, when there is any
	Embed   *discordgo.MessageEmbed   `json:"embed,omitempty"`   // the (first) embed as Discord API JSON
	Embeds  []*discordgo.MessageEmbed `json:"embeds,omitempty"`  // every embed, when one message carries several
}

const designPreviewSource = "internal/discord"

var (
	previewAt     = time.Date(2026, 10, 5, 18, 30, 0, 0, time.UTC)
	previewServer = "Example Server Alpha"
)

func previewFloat(f float64) *float64 { return &f }
func previewInt(n int) *int           { return &n }
func previewInt64(n int64) *int64     { return &n }

func previewKill() *killfeed.Event {
	return &killfeed.Event{
		Type: killfeed.EventPlayerKill, Timestamp: previewAt,
		Killer: &killfeed.PlayerRef{Name: "Sample_Raven"}, Victim: &killfeed.PlayerRef{Name: "Fictional_Fox"},
		Weapon: "M4-A1", Ammo: "5.56x45", Distance: previewFloat(42.3),
		KillerStats:  &killfeed.CombatRecord{Kills: 1284, Deaths: 310, PvPDeaths: 262, DeathSplitKnown: true}, // also died 48 times to the world: a PvP line
		VictimStats:  &killfeed.CombatRecord{Kills: 96, Deaths: 142, PvPDeaths: 142, DeathSplitKnown: true},   // only ever killed by players: no second line
		KillerStreak: previewInt(3), Encounters: &killfeed.HeadToHead{KillerWins: 4, VictimWins: 1},
		SeasonName: "Season 3",
	}
}

// designPreviewCards is the preview set of this package (internal/app adds its own).
func designPreviewCards() []designPreviewCard {
	var cards []designPreviewCard
	add := func(id, title, group string, embeds ...*discordgo.MessageEmbed) {
		c := designPreviewCard{ID: id, Title: title, Group: group, Source: designPreviewSource}
		if len(embeds) > 0 {
			c.Embed = embeds[0]
		}
		if len(embeds) > 1 {
			c.Embeds = embeds
		}
		cards = append(cards, c)
	}

	// --- Feeds -------------------------------------------------------------------------------------
	add("kill-normal", "Kill card: a normal kill", "Feeds", BuildKillEmbed(previewKill()))

	headshot := previewKill()
	headshot.HitZone, headshot.Damage, headshot.Distance = "Head", previewFloat(98.4), previewFloat(61.0)
	add("kill-headshot", "Kill card: a headshot", "Feeds", BuildKillEmbed(headshot))

	long := previewKill()
	long.Weapon, long.Ammo, long.Distance = "Mosin 91/30", "7.62x54", previewFloat(164.2)
	add("kill-longshot", "Kill card: a long shot", "Feeds", BuildKillEmbed(long))

	extreme := previewKill()
	extreme.Weapon, extreme.Ammo, extreme.Distance = "M70 Tundra", ".308", previewFloat(1287.6)
	add("kill-extreme-range", "Kill card: an extreme-range shot", "Feeds", BuildKillEmbed(extreme))

	claimed := previewKill()
	claimed.BountyClaimed, claimed.BountyPoints, claimed.WarBadge = true, 25000, "⚔️ FACTION WAR"
	add("kill-bounty-claimed", "Kill card: a bounty claimed during a faction war", "Feeds", BuildKillEmbed(claimed))

	spree := previewKill()
	spree.KillingSpree, spree.KillerStreak = true, previewInt(10)
	spree.BountyTarget, spree.RankedTag = true, "+250 RP ⚡"
	add("kill-spree", "Kill card: a killing spree on a wanted player", "Feeds", BuildKillEmbed(spree))

	add("death", "Death card", "Feeds", BuildDeathEmbed(&killfeed.Event{
		Type: killfeed.EventPlayerDeath, Timestamp: previewAt, Player: &killfeed.PlayerRef{Name: "Fictional_Fox"},
		Cause: killfeed.DeathCauseInfected, PlayerStats: &killfeed.CombatRecord{Kills: 96, Deaths: 143, PvPDeaths: 120, DeathSplitKnown: true}, SeasonName: "Season 3",
	}))
	add("death-suicide", "Death card: a suicide", "Feeds", BuildDeathEmbed(&killfeed.Event{
		Type: killfeed.EventSuicideAction, Timestamp: previewAt, Player: &killfeed.PlayerRef{Name: "Made_Up_Moose"},
		PlayerStats: &killfeed.CombatRecord{Kills: 12, Deaths: 40},
	}))
	add("hit", "Hit card", "Feeds", buildHitEmbed(&hitEncounter{
		attacker: "Sample_Raven", victim: "Fictional_Fox", weapon: "M4-A1", ammo: "Bullet_556x45", zone: "Torso",
		distance: previewFloat(42), hits: 3, damage: 84, damageHits: 3,
	}))
	add("pve-suicide", "PvE feed: a suicide", "Feeds", buildPveEmbed(killfeed.PveDeathNotice{Name: "Made_Up_Moose", Cause: killfeed.DeathCauseSuicide}))
	add("pve-death", "PvE feed: killed by an infected", "Feeds", buildPveEmbed(killfeed.PveDeathNotice{Name: "Made_Up_Moose", Cause: killfeed.DeathCauseInfected}))
	add("connection-joined", "Connection card: a player joined", "Feeds", buildConnectionsEmbed([]killfeed.ConnectionNotice{{Kind: killfeed.ConnectionConnected, Name: "Sample_Raven"}}, 0))
	add("connection-left", "Connection card: a player left", "Feeds", buildConnectionsEmbed([]killfeed.ConnectionNotice{{Kind: killfeed.ConnectionDisconnected, Name: "Fictional_Fox", Session: 72 * time.Minute}}, 0))
	add("connection-batch", "Connection card: several at once", "Feeds", buildConnectionsEmbed([]killfeed.ConnectionNotice{
		{Kind: killfeed.ConnectionConnected, Name: "Sample_Raven"},
		{Kind: killfeed.ConnectionDisconnected, Name: "Fictional_Fox", Session: 72 * time.Minute},
		{Kind: killfeed.ConnectionConnected, Name: "Made_Up_Moose"},
	}, 2))
	add("build-activity", "Build feed: one action", "Feeds", BuildActivityEmbed(buildItem{
		Player: "Sample_Raven", Action: killfeed.BuildAction{Action: "Built", Object: "Wall", Target: "Fence", Tool: "Hammer"}, HasPos: true, MapX: 7750, MapZ: 12750,
	}, previewServer))

	add("build-activity-burst", "Build feed: a burst of actions", "Feeds", BuildActivitySummaryEmbed([]buildItem{
		{Player: "Sample_Raven", Action: killfeed.BuildAction{Action: "Built", Object: "Wall", Target: "Fence"}},
		{Player: "Fictional_Fox", Action: killfeed.BuildAction{Action: "Dismantled", Object: "Gate", Target: "Fence"}, HasPos: true, MapX: 7750, MapZ: 12750},
	}, 4, previewServer))

	// --- Bounties and economy -----------------------------------------------------------------------
	add("bounty-placed", "Bounty card: a bounty was placed", "Bounties and economy", buildBountyEventEmbed(bounties.Event{Kind: bounties.EventPlaced, Target: "Fictional_Fox", Amount: 25000, Automatic: true}))
	add("bounty-claimed", "Bounty card: a bounty was claimed", "Bounties and economy", buildBountyEventEmbed(bounties.Event{
		Kind: bounties.EventClaimed, Target: "Fictional_Fox", Hunter: "Sample_Raven", Amount: 25000, Count: 2, Weapon: "M4-A1", Distance: previewFloat(86),
	}))
	add("bounty-expired", "Bounty card: a bounty expired", "Bounties and economy", buildBountyEventEmbed(bounties.Event{Kind: bounties.EventExpired, Target: "Fictional_Fox", Amount: 25000}))
	add("bounty-board", "Bounty board", "Bounties and economy", buildBountyBoardEmbed([]repository.BoardEntry{
		{TargetName: "Fictional_Fox", Total: 25000, Count: 2}, {TargetName: "Made_Up_Moose", Total: 12500, Count: 1}, {TargetName: "Pretend_Panda", Total: 1000, Count: 1},
	}))
	add("economy-reward", "Economy card: a reward", "Bounties and economy", buildEconomyEmbed(economy.Event{
		Type: economy.TypeSystemReward, PlayerName: "Sample_Raven", Amount: 1500, Credit: true, BalanceAfter: 341500, Reason: "Daily play reward (3 days in a row)",
	}))
	add("economy-bounty-reward", "Economy card: a bounty payout", "Bounties and economy", buildEconomyEmbed(economy.Event{
		Type: economy.TypeBountyClaim, PlayerName: "Sample_Raven", Amount: 25000, Credit: true, BalanceAfter: 366500,
	}))
	add("economy-admin-debit", "Economy card: staff removed points", "Bounties and economy", buildEconomyEmbed(economy.Event{
		Type: economy.TypeAdminDebit, PlayerName: "Fictional_Fox", Amount: 5000, BalanceAfter: 1200,
	}))

	add("economy-shop-purchase", "Economy card: a shop purchase", "Bounties and economy", buildEconomyEmbed(economy.Event{
		Type: economy.TypeShopPurchase, PlayerName: "Fictional_Fox", Amount: 750, Item: "Field Rations",
	}))

	// --- Boards and panels --------------------------------------------------------------------------
	entries := func(values ...string) []repository.LeaderboardEntry {
		names := []string{"Sample_Raven", "Fictional_Fox", "Made_Up_Moose", "Pretend_Panda", "Imaginary_Ibis"}
		out := make([]repository.LeaderboardEntry, 0, len(values))
		for i, v := range values {
			out = append(out, repository.LeaderboardEntry{DisplayName: names[i%len(names)], Value: v})
		}
		return out
	}
	add("leaderboard-board", "Auto leaderboard board", "Boards and panels", BuildAutoLeaderboardEmbeds(LeaderboardSnapshot{
		ServerName: previewServer, GeneratedAt: previewAt,
		TopKills: entries("6053", "4120", "1284", "960", "512"), TopStreaks: entries("27", "19", "14", "9", "7"),
		TopDeaths: entries("5012", "3999", "1500", "720", "310"), TopLongest: entries("1104.2", "866.0", "512.4", "401.9", "215.0"),
		RanksEnabled: true, CurrentRanks: []RankEntry{{DisplayName: "Sample_Raven", Rank: "Diamond III"}, {DisplayName: "Fictional_Fox", Rank: "Gold I"}},
		KillMoves: map[string]string{"Fictional_Fox": "▲2"},
	}, DefaultLeaderboardConfig())...)
	add("leaderboard-command", "Leaderboard command answer", "Boards and panels", presentation.BuildPlayerLeaderboardEmbed("Kills", []presentation.RankedEntry{
		{Name: "Sample_Raven", Value: "6053"}, {Name: "Fictional_Fox", Value: "4120"}, {Name: "Made_Up_Moose", Value: "1284"},
	}, "Lifetime"))
	add("server-status-board", "Server status board", "Boards and panels", BuildServerStatusEmbed([]ServerStatusSection{
		{ServerName: previewServer, Seen: true, Snapshot: killfeed.AdmSnapshot{State: killfeed.StatePolling, LastPoll: previewAt.Add(-20 * time.Second), LastLogChange: previewAt.Add(-45 * time.Second), OnlineCount: 34}},
		{ServerName: "Example Server Bravo", Seen: true, Snapshot: killfeed.AdmSnapshot{State: killfeed.StatePolling, LastPoll: previewAt.Add(-25 * time.Second), LastLogChange: previewAt.Add(-3 * time.Minute), OnlineCount: 12}},
	}, previewAt))
	add("server-ranks-board", "Server ranks board", "Boards and panels", BuildServerRanksEmbed(ServerRanksSnapshot{
		ServerName: previewServer, SeasonName: "Season 3", UpdatedAt: previewAt,
		Standings: []repository.ServerStanding{{Name: "Sample_Raven", RP: 12850, Tier: "Diamond III"}, {Name: "Fictional_Fox", RP: 8400, Tier: "Gold I"}},
	}))
	add("server-ranks-inactive", "Server ranks board: no ranked season yet", "Boards and panels", ServerRanksInactiveEmbed(previewServer))
	add("heatmap-board", "PvP heatmap board", "Boards and panels", BuildHeatmapSummaryEmbed([]heatmapSection{{ServerName: previewServer, Result: &heatmap.Result{TotalEvents: 247, Cells: []heatmap.Cell{
		{CellX: 29, CellZ: 38, CenterX: 7375, CenterZ: 9625, Count: 32}, {CellX: 18, CellZ: 13, CenterX: 4625, CenterZ: 3375, Count: 21},
	}}}}, 24*time.Hour, 250, previewAt))
	if store := SecurityStorePanel([]SecurityPanelItem{{ServiceID: "BASE_RAID_ALARM", PricePoints: 12500, DurationDays: 30}, {ServiceID: "PERIMETER_MONITORING", PricePoints: 7500, DurationDays: 30}},
		previewServer, "https://example.invalid/store", previewAt); store != nil && len(store.Embeds) > 0 {
		add("security-store-panel", "Security Store panel", "Boards and panels", store.Embeds[0])
	}
	add("features-guide", "What's new guide", "Boards and panels", featuresGuideEmbeds()...)
	add("online-players-panel", "Online players panel", "Boards and panels", OnlinePlayersEmbed([]string{"Sample_Raven", "Fictional_Fox", "Made_Up_Moose"}, true))
	add("link-panel", "Link your account panel", "Boards and panels", LinkUsernameInfoEmbed())
	add("player-stats-panel", "Player stats panel", "Boards and panels", PlayerStatsInfoEmbed())

	// --- Announcements ------------------------------------------------------------------------------
	ends := previewAt.Add(45 * time.Minute)
	add("hot-zone-open", "Hot zone announcement", "Announcements", BuildHotZoneOpenedEmbed(HotZoneAnnouncement{
		Name: "North West Airfield", ServerName: previewServer, CenterX: 4500, CenterZ: 10200, RadiusM: 300,
		KillsObserved: 7, WindowMinutes: 15, EndsAt: &ends, FirstPoints: 5000, SecondPoints: 2500, ThirdPoints: 1000,
	}))
	starts := previewAt.Add(26 * time.Hour)
	eventEnds := previewAt.Add(50 * time.Hour)
	add("event-upcoming", "Event announcement: upcoming", "Announcements", BuildEventAnnouncementEmbed(EventAnnouncementCard{
		Kind: "UPCOMING", Name: "Sniper Sunday", Description: "Longest shot of the day takes the pot.", Type: "LONGEST_KILL",
		Config: json.RawMessage(`{"minimum_distance":300}`), StartsAt: &starts, EndsAt: &eventEnds, Prizes: [3]int{10000, 5000, 2500},
	}))
	add("event-started", "Event announcement: started", "Announcements", BuildEventAnnouncementEmbed(EventAnnouncementCard{
		Kind: "STARTED", Name: "Sniper Sunday", Type: "LONGEST_KILL", EndsAt: &eventEnds, Prizes: [3]int{10000, 5000, 2500},
	}))
	add("event-complete", "Event result", "Announcements", BuildEventCompletionEmbed("Sniper Sunday", "Season 3", []EventPlacement{
		{Rank: 1, Name: "Sample_Raven", Score: 1287.6}, {Rank: 2, Name: "Fictional_Fox", Score: 866}, {Rank: 3, Name: "Made_Up_Moose", Score: 512.4},
	}))
	add("season-complete", "Season result", "Announcements", BuildSeasonCompletionEmbed("Season 3", "Sample_Raven", 1284, "Example Wolves", 5120, "Fictional_Fox", 1287.6, "Made_Up_Moose", 27))
	add("war-complete", "Faction war result", "Announcements", BuildWarCompletionEmbed(WarCompletionCard{
		FactionA: "Example Wolves", FactionB: "Sample Ravens", ScoreA: 48, ScoreB: 41, Winner: "Example Wolves",
		TopKiller: "Sample_Raven", TopKills: 19, LongestKiller: "Fictional_Fox", Longest: 612.5, Season: "Season 3",
	}))
	add("season-plan", "Season change scheduled", "Announcements", BuildSeasonPlanEmbed(SeasonPlanCard{
		Stage: "SCHEDULED", Kind: "STATS_SEASON", RunAt: previewAt.Add(72 * time.Hour), NewSeasonName: "Season 4", ServerName: previewServer,
	}))
	add("season-plan-ranked-done", "Ranked season reset done", "Announcements", BuildSeasonPlanEmbed(SeasonPlanCard{Stage: "DONE", Kind: "RANKED_RESET", RunAt: previewAt, ServerName: previewServer}))
	add("welcome", "Welcome card", "Announcements", WelcomeEmbed(&discordgo.Member{User: &discordgo.User{ID: "100000000000000001", Username: "sample_member"}}))

	// --- Direct messages ----------------------------------------------------------------------------
	played, longest := previewInt64(5400), previewFloat(412.7)
	add("dm-life-recap", "DM: death recap", "Direct messages", BuildLifeRecapEmbed(repository.Life{
		PlayerName: "Sample_Raven", EndedAt: previewAt, PlaytimeSeconds: played, Kills: 4, Headshots: 1, LongestKillM: longest,
		Cause: repository.LifeCauseSuicide,
	}, previewServer))
	if raid := BaseRaidAlarmMessage("Example Hideout", previewServer, repository.BaseRaidEvent{RaiderName: "Fictional_Fox", Part: "Wall", Target: "Fence", Tool: "Hacksaw", TimeOfDay: "18:29:41"}, previewAt); raid != nil && len(raid.Embeds) > 0 {
		add("dm-base-raid-alarm", "DM: base raid alarm", "Direct messages", raid.Embeds[0])
	}
	delivered, _ := BuildShopOrderDeliveredMessage(repository.ShopOrderNotice{
		PurchaseID: 1042, GuildName: "Example Community", TotalPoints: 12500, DeadlineAt: previewAt.Add(48 * time.Hour),
		Items: []repository.ShopNoticeItem{{Name: "Field Rations", Quantity: 2}},
	}, "https://example.invalid")
	add("dm-shop-order-delivered", "DM: shop order delivered", "Direct messages", delivered)

	first := func(id, title string, msg *discordgo.MessageSend) {
		if msg != nil && len(msg.Embeds) > 0 {
			add(id, title, "Direct messages", msg.Embeds[0])
		}
	}
	first("dm-base-approved", "DM: base request approved", BaseRequestDecisionMessage(true, "Example Hideout", previewServer, ""))
	first("dm-base-declined", "DM: base request declined", BaseRequestDecisionMessage(false, "Example Hideout", previewServer, "Too close to a trader."))
	first("dm-rent-due", "DM: base rent is due", BaseRentNoticeMessage(true, "Example Hideout", previewServer, previewAt.Add(48*time.Hour), 12500, 30, "https://example.invalid/store"))
	first("dm-rent-paused", "DM: base paused for rent", BaseRentNoticeMessage(false, "Example Hideout", previewServer, previewAt.Add(-24*time.Hour), 12500, 30, "https://example.invalid/store"))
	first("dm-security-gift", "DM: a gift of base security", SecurityGiftMessage("Raid Alarm", previewServer, 30, previewAt.Add(30*24*time.Hour), "Thanks for playing.", "https://example.invalid/store"))
	first("dm-perimeter-watch", "DM: someone is near your base", PerimeterWatchMessage("Example Hideout", previewServer, "Fictional_Fox", 85, previewAt))
	add("dm-life-profile", "Command answer: /life me", "Direct messages", BuildLifeProfileEmbed("Sample_Raven", &repository.CurrentLife{StartedAt: previewAt.Add(-3 * time.Hour), PlaytimeSeconds: played, Kills: 4},
		repository.LifeSummary{Lives: 37, LongestPlaytime: previewInt64(40200), MostKills: 11}, nil, true))
	orderState, _ := BuildShopOrderStateMessage(repository.ShopOrderConfirmation{PurchaseID: 1042, State: repository.ConfirmationReceived, DeadlineAt: previewAt.Add(48 * time.Hour)}, 0, "https://example.invalid")
	add("dm-shop-order-received", "DM: shop order confirmed", "Direct messages", orderState)
	add("mybase", "Command answer: /mybase", "Direct messages", MyBaseEmbed(BaseCommandSummary{
		Bases: []string{"Example Hideout"}, Rent: []BaseRentLine{{BaseName: "Example Hideout", DueAt: previewAt.Add(9 * 24 * time.Hour)}},
		PaidUntil: []BasePaidTime{{Label: "Raid Alarm", Until: previewAt.Add(20 * 24 * time.Hour)}},
	}, "https://example.invalid/store", previewAt))

	// --- Staff ---------------------------------------------------------------------------------------
	add("staff-alert", "Staff alert: the server log stopped updating", "Staff", BuildAdminAlertEmbed(previewStaffAlert(), previewServer))
	add("staff-alert-resolved", "Staff alert: resolved", "Staff", BuildAdminAlertEmbed(previewStaffAlertResolved(), previewServer))
	add("staff-adm-download-failed", "Staff log: a log download failed", "Staff", BuildADMDownloadEmbed(killfeed.DownloadReport{Result: "failure"}))

	add("staff-adm-monitor", "Staff log: the live log monitor", "Staff", BuildADMMonitorEmbed(killfeed.AdmSnapshot{
		CurrentFile: "DayZServer_PS4_x64.ADM", FileSize: 2048000, ProcessedOffset: 2047000, LastPoll: previewAt.Add(-20 * time.Second),
		LastLogChange: previewAt.Add(-45 * time.Second), LastDownload: previewAt.Add(-45 * time.Second), OnlineCount: 34,
	}, previewAt))
	if first := NewBaseRequestMessage("Sample_Raven", "Example Hideout", previewServer, 25, "https://example.invalid/bases"); first != nil && len(first.Embeds) > 0 {
		add("staff-base-request", "Owner DM: a new base request", "Staff", first.Embeds[0])
	}
	var setup SetupLayoutResult
	setup.Installations = 1
	setup.AddChannel(SetupChannel{ID: "1", Name: "combat-feed", System: "Combat Feed", Outcome: SetupChannelCreated})
	setup.AddChannel(SetupChannel{ID: "2", Name: "economy", System: "Economy", Outcome: SetupChannelReused})
	setup.AddVerifiedSystem("Combat Feed")
	setup.AddVerifiedSystem("Economy")
	add("setup-report", "Command answer: /setup run", "Staff", SetupLayoutEmbed(setup, false))

	cards = append(cards, designPreviewReplies()...)
	return cards
}

// writeDesignPreview merges cards into the JSON file at path: entries this package wrote before
// are replaced, entries of other packages are kept exactly as they were written, and the result
// is ordered by group so two runs can be shown side by side.
func writeDesignPreview(t *testing.T, path, source string, cards []designPreviewCard) {
	t.Helper()
	if !filepath.IsAbs(path) {
		t.Fatalf("DESIGN_PREVIEW_OUT must be an absolute path, got %q", path)
	}
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

// TestDesignPreview writes the preview file when DESIGN_PREVIEW_OUT is set; otherwise it only
// proves the whole set still renders.
func TestDesignPreview(t *testing.T) {
	cards := designPreviewCards()
	seen := map[string]bool{}
	for _, c := range cards {
		if c.ID == "" || c.Title == "" || c.Group == "" {
			t.Fatalf("preview card needs an id, a title and a group: %+v", c)
		}
		if seen[c.ID] {
			t.Fatalf("duplicate preview id %q", c.ID)
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

// previewStaffAlert and previewStaffAlertResolved come out of the real alert publisher, so the
// headline and detail are exactly what staff would be sent.
func previewStaffAlerts() (raised, resolved AdminAlert) {
	p := NewAdminAlertPublisher(nil, nil)
	now := previewAt
	p.now = func() time.Time { return now }
	p.ObserveSnapshot(1, 1, killfeed.AdmSnapshot{OnlineCount: 34, LastLogChange: previewAt.Add(-9 * time.Minute)})
	raised = <-p.queue
	now = previewAt.Add(4 * time.Minute)
	p.ObserveSnapshot(1, 1, killfeed.AdmSnapshot{OnlineCount: 34, LastLogChange: now.Add(-10 * time.Second)})
	resolved = <-p.queue
	return raised, resolved
}

func previewStaffAlert() AdminAlert         { a, _ := previewStaffAlerts(); return a }
func previewStaffAlertResolved() AdminAlert { _, a := previewStaffAlerts(); return a }
