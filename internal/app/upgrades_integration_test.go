//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Feature upgrades (docs/FEATURE_UPGRADES.md) over a real PostgreSQL.

type upgradeWorld struct {
	*clientAdminWorld
	dms *fakeDMs
}

func newUpgradeWorld(t *testing.T) *upgradeWorld {
	t.Helper()
	w := &upgradeWorld{clientAdminWorld: newStandoutWorld(t)}
	pool := w.a.DB.Pool
	w.a.Upgrades = repository.NewUpgradeRepository(pool)
	w.a.Ranked = repository.NewRankedRepository(pool)
	w.a.EconomyService = economy.NewService(repository.NewEconomyRepository(pool), nil)
	w.dms = &fakeDMs{closed: map[string]bool{}, sent: map[string][]*discordgo.MessageSend{}}
	w.a.VIPNotices = w.dms
	return w
}

func (w *upgradeWorld) save(s repository.UpgradeSettings) repository.UpgradeSettings {
	w.t.Helper()
	rr := w.call(w.a.handleSaveUpgradeSettings, http.MethodPut, w.path("/upgrades"), w.f.OwnerDiscordID, s, nil)
	if rr.Code != http.StatusOK {
		w.t.Fatalf("save automations: %d %s", rr.Code, rr.Body.String())
	}
	return decodeBody[repository.UpgradeSettings](w.t, rr)
}

// linked makes a player with a verified Discord account and returns both.
func (w *upgradeWorld) linked(name string) (int64, string) {
	w.t.Helper()
	id := w.player(name)
	user := syncUser(w.t, w.a, fmt.Sprintf("up-%s-%d", strings.ToLower(name), standoutSeq.Add(1)), name)
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,$3,'VERIFIED',NOW())`,
		w.guildID, id, user.DiscordUserID); err != nil {
		w.t.Fatal(err)
	}
	return id, user.DiscordUserID
}

func (w *upgradeWorld) played(playerID int64, day time.Time) {
	w.t.Helper()
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO player_daily_activity(guild_id,server_id,player_id,day,observed_seconds,sessions,first_seen_at,last_seen_at)
VALUES($1,$2,$3,$4::DATE,600,1,$5,$5) ON CONFLICT DO NOTHING`, w.guildID, w.serverID, playerID, day.Format("2006-01-02"), day.Add(time.Hour)); err != nil {
		w.t.Fatal(err)
	}
}

func (w *upgradeWorld) balance(playerID int64) int64 {
	var b int64
	_ = w.a.DB.Pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2),0)`, w.guildID, playerID).Scan(&b)
	return b
}

func (w *upgradeWorld) servers() []repository.UpgradeServer {
	w.t.Helper()
	s, err := w.a.Upgrades.GuildServers(context.Background(), w.guildID)
	if err != nil || len(s) == 0 {
		w.t.Fatalf("guild servers: %v %v", s, err)
	}
	return s
}

func TestAutomationSettingsAPI(t *testing.T) {
	w := newUpgradeWorld(t)
	got := decodeBody[repository.UpgradeSettings](t, w.call(w.a.handleUpgradeSettings, http.MethodGet, w.path("/upgrades"), w.f.OwnerDiscordID, nil, nil))
	if got.WinbackDays != 7 || got.PriorityRankedTop != 0 || got.ShopProgressDMs {
		t.Fatalf("defaults: %+v", got)
	}
	s := repository.DefaultUpgradeSettings()
	s.ShopProgressDMs, s.DailyLoginCredits = true, 10
	saved := w.save(s)
	if !saved.ShopProgressDMs || saved.OnSince("shopProgressDms").IsZero() || saved.OnSince("dailyLoginCredits").IsZero() {
		t.Fatalf("switch-on times are recorded: %+v", saved.SwitchedOn)
	}
	first := saved.OnSince("shopProgressDms")
	// Saving again (the website never sends switchedOn) keeps the first time.
	s.WinbackDays = 10
	if again := w.save(s); !again.OnSince("shopProgressDms").Equal(first) {
		t.Fatal("a switch that stays on keeps its time")
	}
	bad := s
	bad.PriorityRankedTop = 99
	if rr := w.call(w.a.handleSaveUpgradeSettings, http.MethodPut, w.path("/upgrades"), w.f.OwnerDiscordID, bad, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("out of range: %d", rr.Code)
	}
	stranger := syncUser(t, w.a, fmt.Sprintf("up-stranger-%d", standoutSeq.Add(1)), "Stranger")
	if rr := w.call(w.a.handleSaveUpgradeSettings, http.MethodPut, w.path("/upgrades"), stranger.DiscordUserID, s, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("a non-staff user cannot change automations: %d", rr.Code)
	}
}

func TestDailyPlayRewardPaysOnceADayWithStreak(t *testing.T) {
	w := newUpgradeWorld(t)
	s := repository.DefaultUpgradeSettings()
	s.DailyLoginCredits, s.DailyLoginStreakBonus = 10, 5
	w.save(s)
	ctx := context.Background()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	p := w.player("Regular")
	w.played(p, today.AddDate(0, 0, -2))
	w.played(p, today.AddDate(0, 0, -1))
	w.played(p, today)
	lazy := w.player("Lazy")
	w.played(lazy, today.AddDate(0, 0, -1))

	now := time.Now().UTC()
	w.a.runDailyPlayReward(ctx, w.guildID, w.servers()[0], now)
	if b := w.balance(p); b != 20 {
		t.Fatalf("three days in a row pays 10 + 2×5: %d", b)
	}
	if w.balance(lazy) != 0 {
		t.Fatal("not seen today: nothing")
	}
	w.a.upgradeRuns = upgradeThrottle{}
	w.a.runDailyPlayReward(ctx, w.guildID, w.servers()[0], now.Add(time.Minute))
	if b := w.balance(p); b != 20 {
		t.Fatalf("once a day: %d", b)
	}
}

func TestSeasonEndRewardsPayTheTopThreeOnce(t *testing.T) {
	w := newUpgradeWorld(t)
	ctx := context.Background()
	req := serverRankedSeasonRequest{RPPerKill: 100, Thresholds: ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}}
	if rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, w.path("/ranked/server-season"), w.f.OwnerDiscordID, req, nil); rr.Code != http.StatusOK {
		t.Fatalf("season: %d %s", rr.Code, rr.Body.String())
	}
	champ, champUser := w.linked("Champ")
	second := w.player("Second")
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		id, _ := w.killAt(champ, w.player(fmt.Sprintf("V%d", i)), now.Add(time.Duration(i)*time.Second), 100, 100)
		if _, err := w.a.Ranked.AwardActiveServerKill(ctx, w.serverID, id); err != nil {
			t.Fatal(err)
		}
	}
	id, _ := w.killAt(second, w.player("V9"), now.Add(5*time.Second), 100, 100)
	if _, err := w.a.Ranked.AwardActiveServerKill(ctx, w.serverID, id); err != nil {
		t.Fatal(err)
	}
	s := repository.DefaultUpgradeSettings()
	s.SeasonRewards = [3]int64{1000, 500, 0}
	w.save(s)
	var cards []*discordgo.MessageEmbed
	w.a.rpBoostAnnouncer = func(_ int64, e *discordgo.MessageEmbed) { cards = append(cards, e) }

	// Reset the season: the old one ends.
	time.Sleep(10 * time.Millisecond)
	reset := serverRankedSeasonRequest{RPPerKill: 100, Thresholds: req.Thresholds, Confirm: "RESET SERVER RANKED"}
	if rr := w.call(w.a.handleResetServerRankedSeason, http.MethodPost, w.path("/ranked/server-season"), w.f.OwnerDiscordID, reset, nil); rr.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rr.Code, rr.Body.String())
	}
	w.a.runSeasonRewards(ctx, w.guildID, w.servers()[0], time.Now().UTC())
	w.a.runSeasonRewards(ctx, w.guildID, w.servers()[0], time.Now().UTC())
	if w.balance(champ) != 1000 || w.balance(second) != 500 {
		t.Fatalf("payouts: %d %d", w.balance(champ), w.balance(second))
	}
	if len(cards) != 1 || cards[0].Title != "🏆 Season champions" || !strings.Contains(cards[0].Fields[0].Value, "Champ") {
		t.Fatalf("one champions card: %d", len(cards))
	}
	if len(w.dms.sent[champUser]) != 1 || !strings.Contains(w.dms.sent[champUser][0].Embeds[0].Title, "#1") {
		t.Fatalf("the winner hears about it: %+v", w.dms.sent)
	}
}

func TestWinbackMessagesLapsedLinkedPlayersOnce(t *testing.T) {
	w := newUpgradeWorld(t)
	ctx := context.Background()
	gone, goneUser := w.linked("Gone")
	w.played(gone, time.Now().UTC().AddDate(0, 0, -10))
	recent, recentUser := w.linked("Recent")
	w.played(recent, time.Now().UTC().AddDate(0, 0, -1))
	s := repository.DefaultUpgradeSettings()
	s.WinbackEnabled = true
	w.save(s)
	w.a.runWinback(ctx, w.guildID, w.servers()[0], time.Now().UTC())
	if len(w.dms.sent[goneUser]) != 1 || !strings.Contains(w.dms.sent[goneUser][0].Embeds[0].Title, "We miss you") {
		t.Fatalf("win-back: %+v", w.dms.sent)
	}
	if len(w.dms.sent[recentUser]) != 0 {
		t.Fatal("a player seen yesterday is not messaged")
	}
	w.a.upgradeRuns = upgradeThrottle{}
	w.a.runWinback(ctx, w.guildID, w.servers()[0], time.Now().UTC().Add(2*time.Hour))
	if len(w.dms.sent[goneUser]) != 1 {
		t.Fatal("once per time away")
	}
}

func TestAppealsFromPlayerToStaff(t *testing.T) {
	w := newUpgradeWorld(t)
	p, user := w.linked("Banned")
	w.played(p, time.Now().UTC())
	w.killAt(p, w.player("Someone"), time.Now().UTC(), 1, 1) // seen on the server
	playerPath := map[string]string{"installationID": strconv.FormatInt(w.f.InstallationID, 10)}
	body := createAppealRequest{Topic: "ban", Message: "I was banned by mistake, please look again."}
	if rr := w.call(w.a.handleCreateAppeal, http.MethodPost, "/api/saas/player/servers/x/appeals", user, body, playerPath); rr.Code != http.StatusConflict {
		t.Fatalf("appeals are off by default: %d %s", rr.Code, rr.Body.String())
	}
	s := repository.DefaultUpgradeSettings()
	s.CaseAppeals = true
	w.save(s)
	rr := w.call(w.a.handleCreateAppeal, http.MethodPost, "/api/saas/player/servers/x/appeals", user, body, playerPath)
	if rr.Code != http.StatusCreated {
		t.Fatalf("appeal: %d %s", rr.Code, rr.Body.String())
	}
	appeal := decodeBody[repository.Appeal](t, rr)
	if rr := w.call(w.a.handleCreateAppeal, http.MethodPost, "/api/saas/player/servers/x/appeals", user, body, playerPath); rr.Code != http.StatusConflict {
		t.Fatalf("one open appeal at a time: %d", rr.Code)
	}
	list := decodeBody[map[string]any](t, w.call(w.a.handleStaffAppeals, http.MethodGet, w.path("/appeals"), w.f.OwnerDiscordID, nil, nil))
	if items := list["items"].([]any); len(items) != 1 {
		t.Fatalf("staff see it: %v", list)
	}
	decide := map[string]string{"appealID": strconv.FormatInt(appeal.ID, 10)}
	rr = w.call(w.a.handleDecideAppeal, http.MethodPost, w.path("/appeals/x/decide"), w.f.OwnerDiscordID, decideAppealRequest{Accept: true, Note: "Unbanned, sorry!"}, decide)
	if rr.Code != http.StatusOK || decodeBody[repository.Appeal](t, rr).Status != "ACCEPTED" {
		t.Fatalf("decide: %d %s", rr.Code, rr.Body.String())
	}
	if len(w.dms.sent[user]) != 1 || !strings.Contains(w.dms.sent[user][0].Embeds[0].Description, "Unbanned, sorry!") {
		t.Fatalf("the player hears the answer: %+v", w.dms.sent)
	}
	if rr := w.call(w.a.handleDecideAppeal, http.MethodPost, w.path("/appeals/x/decide"), w.f.OwnerDiscordID, decideAppealRequest{}, decide); rr.Code != http.StatusNotFound {
		t.Fatalf("answered once: %d", rr.Code)
	}
	mine := decodeBody[map[string]any](t, w.call(w.a.handlePlayerAppeals, http.MethodGet, "/api/saas/player/servers/x/appeals", user, nil, playerPath))
	if mine["enabled"] != true || len(mine["items"].([]any)) != 1 {
		t.Fatalf("player's appeals: %v", mine)
	}
}

func TestRivalsAndWeaponMastery(t *testing.T) {
	w := newUpgradeWorld(t)
	me, user := w.linked("Me")
	foe, prey := w.player("Foe"), w.player("Prey")
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		w.killAt(foe, me, now.Add(time.Duration(i)*time.Second), 1, 1)
	}
	for i := 0; i < 26; i++ {
		w.killAt(me, prey, now.Add(time.Duration(10+i)*time.Second), 1, 1)
	}
	w.played(me, now)
	playerPath := map[string]string{"installationID": strconv.FormatInt(w.f.InstallationID, 10)}
	rr := w.call(w.a.handlePlayerRivals, http.MethodGet, "/api/saas/player/servers/x/rivals", user, nil, playerPath)
	if rr.Code != http.StatusOK {
		t.Fatalf("rivals: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeBody[struct {
		Nemesis         *repository.Rival  `json:"nemesis"`
		FavouriteVictim *repository.Rival  `json:"favouriteVictim"`
		Weapons         []weaponMasteryDTO `json:"weapons"`
	}](t, rr)
	if got.Nemesis == nil || got.Nemesis.Name != "Foe" || got.Nemesis.Kills != 3 || got.FavouriteVictim == nil || got.FavouriteVictim.Name != "Prey" {
		t.Fatalf("rivals: %+v %+v", got.Nemesis, got.FavouriteVictim)
	}
	if len(got.Weapons) != 1 || got.Weapons[0].Weapon != "KA-M" || got.Weapons[0].Kills != 26 || got.Weapons[0].Rank != "Bronze" {
		t.Fatalf("weapons: %+v", got.Weapons)
	}
}

func TestShopOrderUpdatesOnlyAfterSwitchOn(t *testing.T) {
	w := newUpgradeWorld(t)
	ctx := context.Background()
	buyer, user := w.linked("Buyer")
	order := func() int64 {
		var id int64
		if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO shop_purchases(organization_id,installation_id,game_server_id,player_id,status,total_points,delivery_type,idempotency_key)
VALUES($1,$2,$3,$4,'PENDING_FULFILLMENT',250,'MANUAL',$5) RETURNING id`, w.f.OrgID, w.f.InstallationID, w.serverID, buyer, fmt.Sprintf("order-key-%06d", standoutSeq.Add(1))).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	order() // before the switch: never messaged
	time.Sleep(10 * time.Millisecond)
	s := repository.DefaultUpgradeSettings()
	s.ShopProgressDMs = true
	w.save(s)
	time.Sleep(10 * time.Millisecond)
	id := order()
	w.a.runShopOrderUpdates(ctx, w.guildID, w.servers()[0], time.Now().UTC())
	if len(w.dms.sent[user]) != 1 || !strings.Contains(w.dms.sent[user][0].Embeds[0].Title, "Order received") {
		t.Fatalf("received: %+v", w.dms.sent[user])
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE shop_purchases SET status='REFUNDED',refunded_at=NOW() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	w.a.upgradeRuns = upgradeThrottle{}
	w.a.runShopOrderUpdates(ctx, w.guildID, w.servers()[0], time.Now().UTC())
	if len(w.dms.sent[user]) != 2 || !strings.Contains(w.dms.sent[user][1].Embeds[0].Title, "refunded") {
		t.Fatalf("refunded: %d", len(w.dms.sent[user]))
	}
}
