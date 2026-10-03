//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// kill records a kill from killer on victim at `at` and returns its award.
func (w *rpBoostWorld) kill(killer, victim int64, at time.Time) repository.RankedAward {
	w.t.Helper()
	w.seq++
	id, err := repository.NewKillRepository(w.a.DB.Pool).InsertKillReturning(context.Background(), repository.KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "rp-bonus",
		Fingerprint: fmt.Sprintf("rp-bonus-%d-%d", time.Now().UnixNano(), w.seq), KillerPlayerID: killer, VictimPlayerID: victim, EventTime: &at})
	if err != nil {
		w.t.Fatal(err)
	}
	got, err := w.a.Ranked.AwardActiveServerKill(context.Background(), w.serverID, id)
	if err != nil {
		w.t.Fatalf("award: %v", err)
	}
	return got
}

func (w *rpBoostWorld) saveBonuses(s repository.RankedBonusSettings) {
	w.t.Helper()
	if rr := w.call(w.a.handleSaveRankedBonuses, http.MethodPut, w.path("/ranked/bonuses"), w.f.OwnerDiscordID, s, nil); rr.Code != http.StatusOK {
		w.t.Fatalf("save bonuses: %d %s", rr.Code, rr.Body.String())
	}
}

func bonusKinds(a repository.RankedAward) string {
	kinds := []string{}
	for _, b := range a.Bonuses {
		kinds = append(kinds, b.Kind)
	}
	return strings.Join(kinds, ",")
}

func TestRankedBonusesAreOffUntilTurnedOn(t *testing.T) {
	w := newRPBoostWorld(t, true)
	now := time.Now().UTC()
	if a := w.kill(w.killer, w.victim, now); a.Amount != 100 || a.BonusRP != 0 || len(a.Bonuses) != 0 {
		t.Fatalf("no bonus before any is on: %+v", a)
	}
	got := decodeBody[map[string]any](t, w.call(w.a.handleRankedBonuses, http.MethodGet, w.path("/ranked/bonuses"), w.f.OwnerDiscordID, nil, nil))
	settings := got["settings"].(map[string]any)
	if settings["bountyEnabled"] != false || settings["bountyStreak"].(float64) != 5 {
		t.Fatalf("defaults: %v", settings)
	}
	bad := repository.DefaultRankedBonusSettings()
	bad.BountyPercent = 5000
	if rr := w.call(w.a.handleSaveRankedBonuses, http.MethodPut, w.path("/ranked/bonuses"), w.f.OwnerDiscordID, bad, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("out of range is refused: %d", rr.Code)
	}
	stranger := syncUser(t, w.a, fmt.Sprintf("bonus-stranger-%d", time.Now().UnixNano()), "Stranger")
	if rr := w.call(w.a.handleSaveRankedBonuses, http.MethodPut, w.path("/ranked/bonuses"), stranger.DiscordUserID, repository.DefaultRankedBonusSettings(), nil); rr.Code != http.StatusForbidden {
		t.Fatalf("a non-staff user cannot change bonuses: %d", rr.Code)
	}
}

func TestDailyFirstKillAndRevenge(t *testing.T) {
	w := newRPBoostWorld(t, true)
	s := repository.DefaultRankedBonusSettings()
	s.DailyFirstEnabled, s.RevengeEnabled = true, true
	w.saveBonuses(s)
	a, b, c := w.killer, w.victim, w.newPlayer("Third")
	// 01:00 UTC tomorrow, so the kills below never straddle midnight.
	t0 := time.Now().UTC().Truncate(24 * time.Hour).Add(25 * time.Hour)

	if got := w.kill(a, b, t0); got.Amount != 150 || bonusKinds(got) != "DAILY_FIRST" {
		t.Fatalf("first kill of the day earns +50: %+v", got)
	}
	if got := w.kill(a, c, t0.Add(time.Minute)); got.Amount != 100 {
		t.Fatalf("only the first kill of the day: %+v", got)
	}
	if got := w.kill(b, a, t0.Add(2*time.Minute)); got.Amount != 175 || bonusKinds(got) != "REVENGE,DAILY_FIRST" {
		t.Fatalf("B's revenge on A plus B's first kill: %+v", got)
	}
	if got := w.kill(a, b, t0.Add(10*time.Minute)); got.Amount != 125 || bonusKinds(got) != "REVENGE" {
		t.Fatalf("A pays B back: %+v", got)
	}
	if got := w.kill(b, a, t0.Add(20*time.Minute)); got.Amount != 100 {
		t.Fatalf("revenge on the same player pays once an hour: %+v", got)
	}
	if got := w.kill(a, b, t0.Add(30*time.Minute)); got.Amount != 100 {
		t.Fatalf("revenge on the same player pays once an hour: %+v", got)
	}
	// Too long after being killed is not revenge.
	if got := w.kill(c, a, t0.Add(3*time.Hour)); bonusKinds(got) != "DAILY_FIRST" {
		t.Fatalf("C's first kill: %+v", got)
	}
	if got := w.kill(a, c, t0.Add(4*time.Hour)); got.Amount != 100 {
		t.Fatalf("an hour later it is not revenge: %+v", got)
	}
	// A kill that earns nothing (cooldown) earns no bonus either.
	if got := w.kill(a, c, t0.Add(4*time.Hour+time.Minute)); got.Outcome != "COOLDOWN" || got.Amount != 0 || len(got.Bonuses) != 0 {
		t.Fatalf("cooldown: %+v", got)
	}
	// A replay returns the stored decision with its bonuses.
	var stored int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT bonus_rp FROM ranked_awards WHERE season_id=$1 AND attacker_key=$2 ORDER BY event_time LIMIT 1`, w.seasonID(), fmt.Sprint(a)).Scan(&stored); err != nil || stored != 50 {
		t.Fatalf("stored bonus: %d %v", stored, err)
	}
}

func (w *rpBoostWorld) seasonID() int64 {
	w.t.Helper()
	s, err := w.a.Ranked.ActiveServerSeason(context.Background(), w.guildID, w.serverID)
	if err != nil || s == nil {
		w.t.Fatalf("season: %v", err)
	}
	return s.ID
}

func (w *rpBoostWorld) cardTitles() string {
	titles := []string{}
	for _, c := range w.cards {
		titles = append(titles, c.Title)
	}
	return strings.Join(titles, " | ")
}

func TestBountyAndUnderdog(t *testing.T) {
	w := newRPBoostWorld(t, true)
	s := repository.DefaultRankedBonusSettings()
	s.BountyEnabled, s.BountyStreak, s.UnderdogEnabled = true, 3, true
	w.saveBonuses(s)
	ctx := context.Background()
	t0 := time.Now().UTC()
	king, hunter := w.killer, w.newPlayer("Hunter2")
	for i := 0; i < 3; i++ {
		w.kill(king, w.newPlayer(fmt.Sprintf("Victim%d", i)), t0.Add(time.Duration(i)*time.Minute))
	}

	// King is #1 and on a 3-kill streak: wanted once, for the streak.
	w.a.runRankedBonusAnnouncements(ctx, w.guildID, t0.Add(4*time.Minute))
	if len(w.cards) != 1 || !strings.Contains(w.cards[0].Title, "BOUNTY ON HUNTER") || !strings.Contains(w.cards[0].Description, "3-kill streak") {
		t.Fatalf("wanted card: %s", w.cardTitles())
	}
	w.a.runRankedBonusAnnouncements(ctx, w.guildID, t0.Add(5*time.Minute))
	if len(w.cards) != 1 {
		t.Fatalf("a bounty is announced once: %s", w.cardTitles())
	}
	_, wanted := w.a.playerRankedBonuses(ctx, w.guildID, w.serverID)
	if len(wanted) != 1 || wanted[0].PlayerID != king || wanted[0].Bounty != 100 {
		t.Fatalf("player hub wanted list: %+v", wanted)
	}

	// The unranked hunter takes King (Bronze) down: bounty and underdog.
	got := w.kill(hunter, king, t0.Add(6*time.Minute))
	if got.Amount != 250 || bonusKinds(got) != "BOUNTY,UNDERDOG" {
		t.Fatalf("bounty + underdog: %+v", got)
	}
	w.a.runRankedBonusAnnouncements(ctx, w.guildID, t0.Add(7*time.Minute))
	if len(w.cards) != 2 || !strings.Contains(w.cards[1].Title, "Bounty claimed") || !strings.Contains(w.cards[1].Description, "3-kill streak") {
		t.Fatalf("claim card: %s", w.cardTitles())
	}
	// King is still #1 but their bounty was just claimed: no new bounty for an hour.
	if _, wanted := w.a.playerRankedBonuses(ctx, w.guildID, w.serverID); len(wanted) != 0 {
		t.Fatalf("no bounty right after a claim: %+v", wanted)
	}
	if got := w.kill(hunter, king, t0.Add(20*time.Minute)); got.Amount != 100 {
		t.Fatalf("no second bounty or underdog bonus on the same player within the hour: %+v", got)
	}
	// Hunter2 now has the most RP: the bounty moves to them.
	w.a.runRankedBonusAnnouncements(ctx, w.guildID, t0.Add(21*time.Minute))
	if len(w.cards) != 3 || !strings.Contains(w.cards[2].Title, "BOUNTY ON HUNTER2") || !strings.Contains(w.cards[2].Description, "is #1") {
		t.Fatalf("the new #1 is wanted: %s", w.cardTitles())
	}
}

func TestRankUpsAndWeeklyRecap(t *testing.T) {
	w := newRPBoostWorld(t, true)
	ctx := context.Background()
	dms := &fakeDMs{closed: map[string]bool{}, sent: map[string][]*discordgo.MessageSend{}}
	w.a.VIPNotices = dms
	fan := syncUser(t, w.a, fmt.Sprintf("rank-fan-%d", time.Now().UnixNano()), "Fan")
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,$3,'VERIFIED',NOW())`, w.guildID, w.killer, fan.DiscordUserID); err != nil {
		t.Fatal(err)
	}
	// 01:00 UTC tomorrow, so the week's recap below never straddles Monday.
	t0 := time.Now().UTC().Truncate(24 * time.Hour).Add(25 * time.Hour)
	// Reached before rank-ups were on: not announced later.
	w.kill(w.killer, w.newPlayer("Early"), t0)
	s := repository.DefaultRankedBonusSettings()
	s.RankUpCards, s.RankUpDMs, s.WeeklyRecap = true, true, true
	w.saveBonuses(s)
	w.a.runRankedBonusAnnouncements(ctx, w.guildID, t0.Add(time.Minute))
	if len(w.cards) != 0 {
		t.Fatalf("turning rank-ups on announces nothing old: %s", w.cardTitles())
	}
	// Two more kills: 300 RP is Bronze.
	w.kill(w.killer, w.newPlayer("V2"), t0.Add(2*time.Minute))
	w.kill(w.killer, w.newPlayer("V3"), t0.Add(3*time.Minute))
	w.a.runRankedBonusAnnouncements(ctx, w.guildID, t0.Add(4*time.Minute))
	if len(w.cards) != 1 || !strings.Contains(w.cards[0].Title, "Hunter reached Bronze") || !strings.Contains(w.cards[0].Description, "300 RP · #1") {
		t.Fatalf("rank-up card: %s", w.cardTitles())
	}
	if len(dms.sent[fan.DiscordUserID]) != 1 || !strings.Contains(dms.sent[fan.DiscordUserID][0].Embeds[0].Title, "You reached Bronze") {
		t.Fatalf("rank-up DM: %+v", dms.sent)
	}
	// The victim's first kill makes them Rookie: not news.
	w.kill(w.victim, w.newPlayer("V4"), t0.Add(5*time.Minute))
	w.a.runRankedBonusAnnouncements(ctx, w.guildID, t0.Add(6*time.Minute))
	if len(w.cards) != 1 {
		t.Fatalf("Rookie is not announced and Bronze only once: %s", w.cardTitles())
	}

	// The recap of this week (the season started in it), and it is claimed once.
	season, _ := w.a.Ranked.ActiveServerSeason(ctx, w.guildID, w.serverID)
	week := repository.RecapWeekStart(t0)
	rc, err := w.a.Ranked.WeeklyRecap(ctx, *season, week)
	if err != nil || rc.Kills != 4 || len(rc.Climbers) != 2 || rc.Climbers[0].PlayerName != "Hunter" || rc.BestStreak != 3 || rc.StreakName != "Hunter" {
		t.Fatalf("recap: %+v %v", rc, err)
	}
	card := buildWeeklyRecapCard(rc, "Champions")
	if !strings.Contains(card.Fields[0].Value, "Hunter · +300 RP from 3 kills") {
		t.Fatalf("recap card: %+v", card.Fields)
	}
	if ok, err := w.a.Ranked.ClaimWeeklyRecap(ctx, w.serverID, week, t0); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if ok, _ := w.a.Ranked.ClaimWeeklyRecap(ctx, w.serverID, week, t0); ok {
		t.Fatal("a week's recap is claimed once")
	}
}
