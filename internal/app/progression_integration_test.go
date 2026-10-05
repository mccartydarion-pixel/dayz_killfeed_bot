//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/progression"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Progression over a real PostgreSQL (docs/PROGRESSION.md): territory captures and income,
// challenges completed and paid, and the battle pass from XP to rewards, premium and cosmetics.

type progressionWorld struct {
	*liveMapWorld
	cards []*discordgo.MessageEmbed
}

func newProgressionWorld(t *testing.T) *progressionWorld {
	t.Helper()
	pw := &progressionWorld{liveMapWorld: newLiveMapWorld(t)}
	pool := pw.a.DB.Pool
	pw.a.Challenges = repository.NewChallengeRepository(pool)
	pw.a.BattlePass = repository.NewBattlePassRepository(pool)
	pw.a.Territory = repository.NewTerritoryRepository(pool)
	pw.a.EconomyService = economy.NewService(repository.NewEconomyRepository(pool), nil)
	pw.a.rpBoostAnnouncer = func(_ int64, e *discordgo.MessageEmbed) { pw.cards = append(pw.cards, e) }
	return pw
}

func (pw *progressionWorld) playerPath() map[string]string {
	return map[string]string{"installationID": strconv.FormatInt(pw.f.InstallationID, 10)}
}

func (pw *progressionWorld) playerGet(t *testing.T, handler http.HandlerFunc, suffix, actor string) map[string]any {
	t.Helper()
	rr := pw.call(handler, http.MethodGet, "/api/saas/player/servers/x/"+suffix, actor, nil, pw.playerPath())
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", suffix, rr.Code, rr.Body.String())
	}
	return decodeBody[map[string]any](t, rr)
}

func (pw *progressionWorld) balance(t *testing.T, player int64) int64 {
	t.Helper()
	b, err := pw.a.EconomyService.Balance(context.Background(), pw.guildID, player)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fresh forgets when each pass last ran, so the next call runs every pass again.
func (pw *progressionWorld) fresh() { pw.a.upgradeRuns = upgradeThrottle{} }

func cardTitles(cards []*discordgo.MessageEmbed) string {
	out := []string{}
	for _, c := range cards {
		out = append(out, c.Title)
	}
	return strings.Join(out, " | ")
}

func TestTerritoryCaptureIncomeAndMap(t *testing.T) {
	pw := newProgressionWorld(t)
	ctx := context.Background()
	if code, body := pw.put(pw.a.handleSaveTerritory, "/territory", pw.f.OwnerDiscordID, repository.TerritorySettings{WindowDays: 0, MinKills: 2, IncomePoints: 100}); code != http.StatusBadRequest {
		t.Fatalf("a zero window is refused: %d %v", code, body)
	}
	if code, body := pw.put(pw.a.handleSaveTerritory, "/territory", pw.f.OwnerDiscordID, repository.TerritorySettings{Enabled: true, WindowDays: 7, MinKills: 2, IncomePoints: 100, Announce: true}); code != http.StatusOK {
		t.Fatalf("save: %d %v", code, body)
	}
	if code, _ := pw.put(pw.a.handleSaveTerritory, "/territory", pw.mateID, repository.DefaultTerritorySettings()); code != http.StatusForbidden {
		t.Fatalf("a player cannot change territory: %d", code)
	}

	// The world has one Red Dawn kill at NWAF (Deelo, 50 minutes ago): below the two needed.
	now := pw.now
	pw.a.runTerritory(ctx, pw.guildID, now)
	if holds, _ := pw.a.Territory.Holds(ctx, pw.serverID); len(holds) != 0 {
		t.Fatalf("one kill takes nothing: %+v", holds)
	}
	// A second kill by the same pair in the same hour does not count; Mate's kill does.
	pw.killAt(pw.deelo, pw.stranger, now.Add(-49*time.Minute), 4500, 10450)
	pw.fresh()
	pw.a.runTerritory(ctx, pw.guildID, now)
	if holds, _ := pw.a.Territory.Holds(ctx, pw.serverID); len(holds) != 0 {
		t.Fatalf("the same pair counts once an hour: %+v", holds)
	}
	pw.killAt(pw.mate, pw.stranger, now.Add(-5*time.Minute), 4700, 10200)
	pw.fresh()
	pw.a.runTerritory(ctx, pw.guildID, now)
	holds, _ := pw.a.Territory.Holds(ctx, pw.serverID)
	if holds["nwaf"].FactionID != pw.faction || len(holds) != 1 {
		t.Fatalf("Red Dawn takes NWAF: %+v", holds)
	}
	if len(pw.cards) != 1 || !strings.Contains(pw.cards[0].Title, "NWAF captured") || !strings.Contains(pw.cards[0].Description, `\[RD\]`) {
		t.Fatalf("capture card: %s", cardTitles(pw.cards))
	}
	// Income: 100 a day split between Deelo and Mate, who both played this week. Paid once a day.
	if pw.balance(t, pw.deelo) != 50 || pw.balance(t, pw.mate) != 50 || pw.balance(t, pw.stranger) != 0 {
		t.Fatalf("income: %d %d %d", pw.balance(t, pw.deelo), pw.balance(t, pw.mate), pw.balance(t, pw.stranger))
	}
	pw.fresh()
	pw.a.runTerritory(ctx, pw.guildID, now.Add(time.Minute))
	if pw.balance(t, pw.deelo) != 50 {
		t.Fatalf("income is paid once a day: %d", pw.balance(t, pw.deelo))
	}

	// Player view: the holder, the race and the player's faction's points.
	got := pw.playerGet(t, pw.a.handlePlayerTerritory, "territory", pw.deeloID)
	if got["enabled"] != true || got["held"].(float64) != 1 {
		t.Fatalf("player territory: %v", got)
	}
	var nwaf map[string]any
	for _, z := range got["zones"].([]any) {
		if z.(map[string]any)["key"] == "nwaf" {
			nwaf = z.(map[string]any)
		}
	}
	if nwaf == nil || nwaf["holder"].(map[string]any)["tag"] != "RD" || nwaf["myPoints"].(float64) != 2 || len(nwaf["standings"].([]any)) != 1 {
		t.Fatalf("NWAF for the player: %v", nwaf)
	}

	// The public live map draws the zones and holders, never the scores.
	if _, err := pw.a.DB.Pool.Exec(ctx, `INSERT INTO installation_feature_settings(installation_id,live_map_visibility) VALUES($1,'PUBLIC')
ON CONFLICT (installation_id) DO UPDATE SET live_map_visibility='PUBLIC'`, pw.f.InstallationID); err != nil {
		t.Fatal(err)
	}
	pw.a.invalidateLiveMapCache()
	rr, pub := pw.publicMap(t, "", pw.f.InstallationID)
	if rr.Code != http.StatusOK || len(pub.Territory) != len(progression.Zones("chernarusplus")) {
		t.Fatalf("public territory: %d %d", rr.Code, len(pub.Territory))
	}
	if strings.Contains(rr.Body.String(), "standings") || strings.Contains(rr.Body.String(), "myPoints") {
		t.Fatal("the public map never carries scores")
	}

	// A week later with no Red Dawn kills there, NWAF goes back to nobody.
	pw.fresh()
	pw.a.runTerritory(ctx, pw.guildID, now.AddDate(0, 0, 8))
	if holds, _ := pw.a.Territory.Holds(ctx, pw.serverID); len(holds) != 0 {
		t.Fatalf("an idle holder loses the zone: %+v", holds)
	}
	if last := pw.cards[len(pw.cards)-1]; !strings.Contains(last.Title, "unclaimed") {
		t.Fatalf("neutral card: %s", cardTitles(pw.cards))
	}
	admin := decodeBody[map[string]any](t, pw.call(pw.a.handleAdminTerritory, http.MethodGet, pw.path("/territory"), pw.f.OwnerDiscordID, nil, nil))
	if len(admin["captures"].([]any)) != 1 || admin["settings"].(map[string]any)["minKills"].(float64) != 2 {
		t.Fatalf("admin territory: %v", admin["captures"])
	}
}

func TestChallengesCompletePayOnceAndGiveXP(t *testing.T) {
	pw := newProgressionWorld(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if now.Sub(now.Truncate(24*time.Hour)) < 10*time.Minute {
		t.Skip("too close to midnight UTC for a day's challenges")
	}
	if code, _ := pw.put(pw.a.handleSaveChallenges, "/challenges", pw.f.OwnerDiscordID, repository.ChallengeSettings{Enabled: true, DailyCount: 9}); code != http.StatusBadRequest {
		t.Fatalf("nine a day is refused: %d", code)
	}
	if code, body := pw.put(pw.a.handleSaveChallenges, "/challenges", pw.f.OwnerDiscordID,
		repository.ChallengeSettings{Enabled: true, DailyCount: 4, WeeklyCount: 0, DailyPoints: 50, Announce: true}); code != http.StatusOK {
		t.Fatalf("save: %d %v", code, body)
	}
	// A battle pass season, so challenges also earn XP.
	season := repository.DefaultBattlePassSeason(now)
	season.XPKill, season.XPHour = 0, 0
	if rr := pw.call(pw.a.handleCreateBattlePass, http.MethodPost, pw.path("/battle-pass"), pw.f.OwnerDiscordID, season, nil); rr.Code != http.StatusOK {
		t.Fatalf("season: %d %s", rr.Code, rr.Body.String())
	}
	// Today's challenges, fixed for the test (the scheduler keeps a set once it exists).
	day := progression.PeriodStart(progression.PeriodDay, now)
	set := []progression.Challenge{
		{Key: "kills", Kind: progression.KindKills, Target: 2, Title: "Get 2 kills"},
		{Key: "headshots", Kind: progression.KindHeadshots, Target: 1, Title: "Get 1 headshot kill"},
		{Key: "rifle", Kind: progression.KindWeapon, Class: progression.ClassRifle, Target: 1, Title: "Get a kill with an assault rifle"},
		{Key: "sniper", Kind: progression.KindWeapon, Class: progression.ClassSniper, Target: 1, Title: "Get a kill with a sniper rifle"},
	}
	raw, _ := json.Marshal(set)
	if _, err := pw.a.DB.Pool.Exec(ctx, `INSERT INTO challenge_sets(server_id,period,starts_at,ends_at,challenges) VALUES($1,'DAY',$2,$3,$4)`,
		pw.serverID, day, day.AddDate(0, 0, 1), raw); err != nil {
		t.Fatal(err)
	}
	// Deelo: two KA-M kills on different players today, one a headshot. Two kills of the same
	// player in the same hour count once.
	pw.killAt(pw.deelo, pw.mate, now.Add(-3*time.Minute), 100, 100)
	pw.killAt(pw.deelo, pw.mate, now.Add(-2*time.Minute), 100, 100)
	head := now.Add(-time.Minute).Truncate(time.Millisecond)
	if _, err := pw.a.Kills.InsertKillReturning(ctx, repository.KillRecord{GuildID: pw.guildID, ServerID: pw.serverID, SessionID: "s", Fingerprint: fmt.Sprintf("ch-%d", standoutSeq.Add(1)),
		KillerPlayerID: pw.deelo, VictimPlayerID: pw.ghost, WeaponRaw: "M4A1", WeaponDisplay: "M4-A1", Headshot: true, EventTime: &head}); err != nil {
		t.Fatal(err)
	}

	// Completions are stamped when the pass runs, after the season started.
	now = time.Now().UTC()
	pw.a.runChallenges(ctx, pw.guildID, now)
	if pw.balance(t, pw.deelo) != 150 {
		t.Fatalf("three challenges at 50: %d", pw.balance(t, pw.deelo))
	}
	if len(pw.cards) != 1 || !strings.Contains(pw.cards[0].Title, "Today's challenges") || !strings.Contains(pw.cards[0].Description, "Get 2 kills") {
		t.Fatalf("announcement: %s", cardTitles(pw.cards))
	}
	var xp int64
	if err := pw.a.DB.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(xp),0) FROM battle_pass_xp WHERE player_id=$1 AND source='DAILY'`, pw.deelo).Scan(&xp); err != nil || xp != 900 {
		t.Fatalf("each challenge adds the season's 300 XP: %d %v", xp, err)
	}
	pw.fresh()
	pw.a.runChallenges(ctx, pw.guildID, now.Add(time.Minute))
	if pw.balance(t, pw.deelo) != 150 || len(pw.cards) != 1 {
		t.Fatalf("paid and announced once: %d %d", pw.balance(t, pw.deelo), len(pw.cards))
	}

	got := pw.playerGet(t, pw.a.handlePlayerChallenges, "challenges", pw.deeloID)
	sets := got["sets"].([]any)
	if got["enabled"] != true || len(sets) != 1 {
		t.Fatalf("player challenges: %v", got)
	}
	list := sets[0].(map[string]any)["challenges"].([]any)
	if len(list) != 4 || list[0].(map[string]any)["done"] != true || list[3].(map[string]any)["done"] != false || list[3].(map[string]any)["progress"].(float64) != 0 {
		t.Fatalf("progress: %v", list)
	}
	if sets[0].(map[string]any)["xp"].(float64) != 300 || sets[0].(map[string]any)["points"].(float64) != 50 {
		t.Fatalf("rewards shown: %v", sets[0])
	}
	admin := decodeBody[map[string]any](t, pw.call(pw.a.handleAdminChallenges, http.MethodGet, pw.path("/challenges"), pw.f.OwnerDiscordID, nil, nil))
	first := admin["sets"].([]any)[0].(map[string]any)["challenges"].([]any)[0].(map[string]any)
	if first["completedBy"].(float64) != 1 {
		t.Fatalf("admin sees who finished: %v", first)
	}
}

func TestBattlePassLevelsPremiumAndCosmetics(t *testing.T) {
	pw := newProgressionWorld(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second)
	bad := repository.BattlePassSeason{Name: "S", Levels: 5, XPPerLevel: 100, EndsAt: start.Add(time.Hour),
		Rewards: []progression.Reward{{Level: 6, Track: progression.TrackFree, Kind: progression.RewardPoints, Amount: 10}}}
	if rr := pw.call(pw.a.handleCreateBattlePass, http.MethodPost, pw.path("/battle-pass"), pw.f.OwnerDiscordID, bad, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("a reward past the last level is refused: %d", rr.Code)
	}
	season := repository.BattlePassSeason{Name: "Season of Blood", Levels: 5, XPPerLevel: 100, PremiumPrice: 300, EndsAt: start.AddDate(0, 0, 30),
		XPKill: 100, XPKillDailyCap: 20, Rewards: []progression.Reward{
			{Level: 1, Track: progression.TrackFree, Kind: progression.RewardPoints, Amount: 10},
			{Level: 1, Track: progression.TrackPremium, Kind: progression.RewardTitle, Text: "Hunter"},
			{Level: 2, Track: progression.TrackFree, Kind: progression.RewardBadge, Text: "🔥", Label: "On Fire"},
			{Level: 5, Track: progression.TrackFree, Kind: progression.RewardTitle, Text: "Legend"},
		}}
	rr := pw.call(pw.a.handleCreateBattlePass, http.MethodPost, pw.path("/battle-pass"), pw.f.OwnerDiscordID, season, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	created := decodeBody[repository.BattlePassSeason](t, rr)
	if rr := pw.call(pw.a.handleCreateBattlePass, http.MethodPost, pw.path("/battle-pass"), pw.f.OwnerDiscordID, season, nil); rr.Code != http.StatusConflict {
		t.Fatalf("one season at a time: %d", rr.Code)
	}
	// Kills after the start: Deelo two different victims (200 XP, level 2); the world's older
	// kills came before the season and earn nothing.
	pw.killAt(pw.deelo, pw.mate, start.Add(10*time.Second), 100, 100)
	pw.killAt(pw.deelo, pw.ghost, start.Add(20*time.Second), 100, 100)
	pw.killAt(pw.deelo, pw.ghost, start.Add(30*time.Second), 100, 100) // same pair, same hour
	now := start.Add(time.Minute)
	pw.a.runBattlePass(ctx, pw.guildID, now)
	if !strings.Contains(cardTitles(pw.cards), "New battle pass season") {
		t.Fatalf("start card: %s", cardTitles(pw.cards))
	}
	if pw.balance(t, pw.deelo) != 10 {
		t.Fatalf("level 1 free reward: %d", pw.balance(t, pw.deelo))
	}
	got := pw.playerGet(t, pw.a.handlePlayerBattlePass, "battle-pass", pw.deeloID)
	me := got["me"].(map[string]any)
	if me["xp"].(float64) != 200 || me["level"].(float64) != 2 || me["premium"] != false {
		t.Fatalf("progress: %v", me)
	}
	if cos := got["cosmetics"].([]any); len(cos) != 1 || cos[0].(map[string]any)["value"] != "🔥" {
		t.Fatalf("the level 2 badge: %v", got["cosmetics"])
	}

	// Premium: not enough points, then enough. The premium rewards already reached arrive at once.
	if rr := pw.call(pw.a.handleBuyBattlePassPremium, http.MethodPost, "/api/saas/player/servers/x/battle-pass/premium", pw.deeloID, map[string]any{}, pw.playerPath()); rr.Code != http.StatusPaymentRequired && rr.Code != http.StatusConflict && rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("10 points cannot buy a 300 premium: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := pw.a.EconomyService.Credit(ctx, economy.Request{GuildID: pw.guildID, PlayerID: pw.deelo, Amount: 500, Type: economy.TypeSystemReward, ReferenceID: "bp-test"}); err != nil {
		t.Fatal(err)
	}
	rr = pw.call(pw.a.handleBuyBattlePassPremium, http.MethodPost, "/api/saas/player/servers/x/battle-pass/premium", pw.deeloID, map[string]any{}, pw.playerPath())
	if rr.Code != http.StatusOK || decodeBody[map[string]any](t, rr)["balance"].(float64) != 210 {
		t.Fatalf("buy: %d %s", rr.Code, rr.Body.String())
	}
	if rr := pw.call(pw.a.handleBuyBattlePassPremium, http.MethodPost, "/api/saas/player/servers/x/battle-pass/premium", pw.deeloID, map[string]any{}, pw.playerPath()); rr.Code != http.StatusConflict {
		t.Fatalf("premium is bought once: %d", rr.Code)
	}
	cos, _, _ := pw.a.BattlePass.Cosmetics(ctx, pw.serverID, pw.deelo)
	if len(cos) != 2 {
		t.Fatalf("the premium level 1 title arrives with the purchase: %+v", cos)
	}

	// Showing a title and badge: only ones the player owns.
	if rr := pw.call(pw.a.handleSetCosmetics, http.MethodPut, "/api/saas/player/servers/x/cosmetics", pw.deeloID, repository.CosmeticChoice{Title: "Legend"}, pw.playerPath()); rr.Code != http.StatusBadRequest {
		t.Fatalf("a title not reached yet: %d", rr.Code)
	}
	rr = pw.call(pw.a.handleSetCosmetics, http.MethodPut, "/api/saas/player/servers/x/cosmetics", pw.deeloID, repository.CosmeticChoice{Title: "Hunter", Badge: "🔥"}, pw.playerPath())
	if rr.Code != http.StatusOK || decodeBody[repository.CosmeticChoice](t, rr).Title != "Hunter" {
		t.Fatalf("choose: %d %s", rr.Code, rr.Body.String())
	}
	got = pw.playerGet(t, pw.a.handlePlayerBattlePass, "battle-pass", pw.deeloID)
	board := got["leaderboard"].([]any)
	if len(board) != 1 || board[0].(map[string]any)["title"] != "Hunter" || board[0].(map[string]any)["premium"] != true {
		t.Fatalf("leaderboard: %v", board)
	}
	rewards := got["rewards"].([]any)
	if rewards[0].(map[string]any)["granted"] != true || rewards[3].(map[string]any)["reached"] != false {
		t.Fatalf("reward states: %v", rewards)
	}

	// Staff stats, an edit and the end of the season with its card.
	admin := decodeBody[map[string]any](t, pw.call(pw.a.handleAdminBattlePass, http.MethodGet, pw.path("/battle-pass"), pw.f.OwnerDiscordID, nil, nil))
	stats := admin["stats"].(map[string]any)
	if stats["players"].(float64) != 1 || stats["premium"].(float64) != 1 || stats["premiumPaid"].(float64) != 300 {
		t.Fatalf("stats: %v", stats)
	}
	edit := created
	edit.Name = "Season of Bones"
	edit.Rewards = season.Rewards
	idPath := map[string]string{"seasonID": strconv.FormatInt(created.ID, 10)}
	if rr := pw.call(pw.a.handleUpdateBattlePass, http.MethodPut, pw.path("/battle-pass/x"), pw.f.OwnerDiscordID, edit, idPath); rr.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rr.Code, rr.Body.String())
	}
	if rr := pw.call(pw.a.handleEndBattlePass, http.MethodPost, pw.path("/battle-pass/x/end"), pw.f.OwnerDiscordID, nil, idPath); rr.Code != http.StatusOK {
		t.Fatalf("end: %d %s", rr.Code, rr.Body.String())
	}
	pw.fresh()
	pw.a.runBattlePass(ctx, pw.guildID, now.Add(time.Minute))
	if last := pw.cards[len(pw.cards)-1]; !strings.Contains(last.Title, "Season of Bones has ended") || !strings.Contains(last.Description, "Deelo") {
		t.Fatalf("end card: %s", cardTitles(pw.cards))
	}
	if rr := pw.call(pw.a.handleEndBattlePass, http.MethodPost, pw.path("/battle-pass/x/end"), pw.f.OwnerDiscordID, nil, idPath); rr.Code != http.StatusNotFound {
		t.Fatalf("an ended season cannot end again: %d", rr.Code)
	}
}
