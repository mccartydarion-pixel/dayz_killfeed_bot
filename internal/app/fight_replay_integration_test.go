//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

// Fight replay over a real PostgreSQL (docs/FIGHT_REPLAY.md).

// fightKill persists a kill with the position rows ADM logs for both sides of it.
func (w *clientAdminWorld) fightKill(killer, victim int64, at time.Time, kx, kz, vx, vz float64) int64 {
	w.t.Helper()
	at = at.UTC().Truncate(time.Millisecond)
	dist := 42.5
	id, err := w.a.Kills.InsertKillReturning(context.Background(), repository.KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s",
		Fingerprint: fmt.Sprintf("fight-kill-%d", standoutSeq.Add(1)), KillerPlayerID: killer, VictimPlayerID: victim, WeaponRaw: "SVAL", WeaponDisplay: "SVAL",
		Distance: &dist, Headshot: true, EventTime: &at})
	if err != nil {
		w.t.Fatal(err)
	}
	w.seedLocationEvent(killer, kx, kz, "KILL", at)
	w.seedLocationEvent(victim, vx, vz, "KILL", at)
	return id
}

func TestFightReplayForStaffAndDelayedPublicAccess(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	owner := w.f.OwnerDiscordID
	now := time.Now().UTC()
	ace, bob, cat, dan := w.player("Ace"), w.player("Bob"), w.player("Cat"), w.player("Dan")
	w.a.Shop = shop.NewService(repository.NewShopRepository(w.a.DB.Pool), w.a.EconomyAccounts, nil)

	// Fight A, two hours ago: three kills in 80 seconds around (5000, 5000).
	a0 := now.Add(-2 * time.Hour)
	a1 := w.fightKill(ace, bob, a0, 5000, 5000, 5040, 5000)
	a2 := w.fightKill(cat, ace, a0.Add(50*time.Second), 5100, 5100, 5000, 5000)
	w.fightKill(cat, dan, a0.Add(80*time.Second), 5100, 5100, 5300, 5200)
	w.seedLocationEvent(ace, 4800, 4900, "OTHER_ADM", a0.Add(-90*time.Second)) // inside both lead-ins
	w.seedLocationEvent(ace, 4000, 4000, "OTHER_ADM", a0.Add(-4*time.Minute))  // staff lead-in only
	w.seedLocationEvent(ace, 1000, 1000, "OTHER_ADM", a0.Add(-6*time.Minute))  // before any lead-in
	w.seedLocationEvent(bob, 5040, 5010, "HIT", a0.Add(-5*time.Second))
	w.seedLocationEvent(w.player("Bystander"), 5050, 5050, "OTHER_ADM", a0) // not a participant: never in the replay
	// A lone kill an hour ago, far away.
	solo := w.fightKill(dan, bob, now.Add(-time.Hour), 12000, 3000, 12010, 3000)
	// Fight C, ten minutes ago: two kills.
	c0 := now.Add(-10 * time.Minute)
	c1 := w.fightKill(bob, cat, c0, 7000, 7000, 7010, 7000)
	w.fightKill(cat, bob, c0.Add(40*time.Second), 7000, 7010, 7000, 7000)

	type list struct {
		Enabled      bool              `json:"enabled"`
		DelayMinutes int               `json:"delayMinutes"`
		Items        []fightSummaryDTO `json:"items"`
	}
	rr := w.call(w.a.handleAdminFights, http.MethodGet, w.path("/fights"), owner, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin fights: %d %s", rr.Code, rr.Body.String())
	}
	fights := decodeBody[list](t, rr).Items
	if len(fights) != 2 || fights[0].ID != c1 || fights[1].ID != a1 || fights[1].Kills != 3 || len(fights[1].Participants) != 4 || fights[1].Participants[0] != "Ace" {
		t.Fatalf("fights = %+v", fights)
	}
	if fights[1].CenterX == nil || *fights[1].CenterX < 5100 || *fights[1].CenterX > 5120 {
		t.Fatalf("fight centre = %v", fights[1].CenterX)
	}
	if all := decodeBody[list](t, w.call(w.a.handleAdminFights, http.MethodGet, w.path("/fights")+"?minKills=1", owner, nil, nil)).Items; len(all) != 3 || all[1].ID != solo {
		t.Fatalf("fights with minKills=1 = %+v", all)
	}
	if rr := w.call(w.a.handleAdminFights, http.MethodGet, w.path("/fights")+"?hours=0", owner, nil, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("hours=0 accepted: %d", rr.Code)
	}

	// The staff replay: asked for by the fight's second kill, it is the whole fight.
	rr = w.call(w.a.handleAdminFightReplay, http.MethodGet, w.path("/fights/x"), owner, nil, map[string]string{"killID": strconv.FormatInt(a2, 10)})
	if rr.Code != http.StatusOK {
		t.Fatalf("staff replay: %d %s", rr.Code, rr.Body.String())
	}
	replay := decodeBody[fightReplayDTO](t, rr)
	if replay.ID != a1 || replay.Kills != 3 || len(replay.KillEvents) != 3 || replay.DurationSeconds != 300+80+30 || replay.Truncated {
		t.Fatalf("replay = id %d kills %d events %d duration %v", replay.ID, replay.Kills, len(replay.KillEvents), replay.DurationSeconds)
	}
	if replay.MapKey != "" {
		t.Fatalf("no map is configured, yet the replay names %q", replay.MapKey)
	}
	first := replay.KillEvents[0]
	if first.T != 300 || first.KillerID != ace || first.VictimID != bob || first.Weapon != "SVAL" || !first.Headshot ||
		first.KillerX == nil || *first.KillerX != 5000 || first.VictimX == nil || *first.VictimX != 5040 || *first.DistanceMeters != 42.5 {
		t.Fatalf("first kill event = %+v", first)
	}
	stats := map[string]fightPlayerDTO{}
	for _, p := range replay.Players {
		stats[p.Name] = p
	}
	if len(replay.Players) != 4 || stats["Ace"].Kills != 1 || stats["Ace"].Deaths != 1 || stats["Cat"].Kills != 2 || stats["Dan"].Deaths != 1 {
		t.Fatalf("players = %+v", replay.Players)
	}
	tracks := map[int64][]fightPointDTO{}
	for _, tr := range replay.Tracks {
		tracks[tr.PlayerID] = tr.Points
	}
	// Ace: the -4 min and -90 s samples plus two KILL rows (as killer, then as victim); never the -6 min one.
	if len(replay.Tracks) != 4 || len(tracks[ace]) != 4 || tracks[ace][0].T != 60 || tracks[ace][0].X != 4000 || tracks[ace][1].T != 210 || tracks[ace][3].Type != "KILL" {
		t.Fatalf("ace track = %+v", tracks[ace])
	}
	if len(tracks[bob]) != 2 || tracks[bob][0].Type != "HIT" || tracks[bob][0].T != 295 {
		t.Fatalf("bob track = %+v", tracks[bob])
	}
	if replay.Bounds == nil || replay.Bounds.MinX != 4000 || replay.Bounds.MaxX != 5300 || replay.Bounds.MinZ != 4000 || replay.Bounds.MaxZ != 5200 {
		t.Fatalf("bounds = %+v", replay.Bounds)
	}
	var audited int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='FIGHT_REPLAY_VIEWED'`, w.f.InstallationID).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("replay views audited = %d err=%v", audited, err)
	}

	// Unknown kills and another tenant's kills do not resolve.
	other := newStandoutWorld(t)
	foreign := other.fightKill(other.player("X"), other.player("Y"), now.Add(-time.Hour), 1, 1, 2, 2)
	for _, id := range []int64{999999999, foreign} {
		if rr := w.call(w.a.handleAdminFightReplay, http.MethodGet, w.path("/fights/x"), owner, nil, map[string]string{"killID": strconv.FormatInt(id, 10)}); rr.Code != http.StatusNotFound {
			t.Fatalf("kill %d resolved: %d", id, rr.Code)
		}
	}
	// Positions are an Administrator capability: a Moderator gets neither the list nor a replay.
	mod := syncUser(t, w.a, fmt.Sprintf("fight-mod-%d", standoutSeq.Add(1)), "Mod")
	w.mapRole(mod.DiscordUserID, "fight-role-mod", "MODERATOR")
	if rr := w.call(w.a.handleAdminFights, http.MethodGet, w.path("/fights"), mod.DiscordUserID, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("moderator listed fights: %d", rr.Code)
	}
	if rr := w.call(w.a.handleAdminFightReplay, http.MethodGet, w.path("/fights/x"), mod.DiscordUserID, nil, map[string]string{"killID": strconv.FormatInt(a1, 10)}); rr.Code != http.StatusForbidden {
		t.Fatalf("moderator watched a replay: %d", rr.Code)
	}

	// Players: nothing until the installation opts in.
	fan := syncUser(t, w.a, fmt.Sprintf("fight-fan-%d", standoutSeq.Add(1)), "Fan")
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,'VERIFIED')`, w.guildID, ace, fan.DiscordUserID); err != nil {
		t.Fatal(err)
	}
	pv := func(kill int64) map[string]string {
		return map[string]string{"installationID": strconv.FormatInt(w.f.InstallationID, 10), "killID": strconv.FormatInt(kill, 10)}
	}
	closed := decodeBody[list](t, w.call(w.a.handlePlayerFights, http.MethodGet, "/x", fan.DiscordUserID, nil, pv(0)))
	if closed.Enabled || len(closed.Items) != 0 {
		t.Fatalf("fights listed to players before opt-in: %+v", closed)
	}
	if rr := w.call(w.a.handlePlayerFightReplay, http.MethodGet, "/x", fan.DiscordUserID, nil, pv(a1)); rr.Code != http.StatusNotFound {
		t.Fatalf("replay served to players before opt-in: %d", rr.Code)
	}

	if _, err := w.a.FeatureSettings.SaveFightReplay(ctx, w.f.InstallationID, 0, repository.FightReplaySettings{Public: true, DelayMinutes: 60}); err != nil {
		t.Fatal(err)
	}
	// With a 60-minute delay only fight A (two hours old) is public; fight C is ten minutes old.
	open := decodeBody[list](t, w.call(w.a.handlePlayerFights, http.MethodGet, "/x", fan.DiscordUserID, nil, pv(0)))
	if !open.Enabled || open.DelayMinutes != 60 || len(open.Items) != 1 || open.Items[0].ID != a1 {
		t.Fatalf("public fights = %+v", open)
	}
	if rr := w.call(w.a.handlePlayerFightReplay, http.MethodGet, "/x", fan.DiscordUserID, nil, pv(c1)); rr.Code != http.StatusNotFound {
		t.Fatalf("a fight inside the delay was served to a player: %d", rr.Code)
	}
	// Once the server's map is set, replays carry it so the website can draw the satellite map.
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO shop_delivery_settings (installation_id, organization_id, map_key) VALUES ($1, $2, 'chernarusplus')
ON CONFLICT (installation_id) DO UPDATE SET map_key = EXCLUDED.map_key`, w.f.InstallationID, w.f.OrgID); err != nil {
		t.Fatal(err)
	}
	rr = w.call(w.a.handlePlayerFightReplay, http.MethodGet, "/x", fan.DiscordUserID, nil, pv(a2))
	if rr.Code != http.StatusOK {
		t.Fatalf("public replay: %d %s", rr.Code, rr.Body.String())
	}
	public := decodeBody[fightReplayDTO](t, rr)
	if public.MapKey != "chernarusplus" {
		t.Fatalf("public replay map = %q, want the configured chernarusplus", public.MapKey)
	}
	// The public lead-in is two minutes: the first kill is at t=120 and the -4 min sample is gone.
	if public.ID != a1 || public.KillEvents[0].T != 120 || public.DurationSeconds != 120+80+30 {
		t.Fatalf("public replay = id %d first kill t=%v duration %v", public.ID, public.KillEvents[0].T, public.DurationSeconds)
	}
	for _, tr := range public.Tracks {
		if tr.PlayerID == ace && (len(tr.Points) != 3 || tr.Points[0].X != 4800) {
			t.Fatalf("public ace track = %+v", tr.Points)
		}
	}
	// A user with no verified link on this server gets nothing.
	stranger := syncUser(t, w.a, fmt.Sprintf("fight-stranger-%d", standoutSeq.Add(1)), "Stranger")
	if rr := w.call(w.a.handlePlayerFights, http.MethodGet, "/x", stranger.DiscordUserID, nil, pv(0)); rr.Code == http.StatusOK {
		t.Fatal("an unlinked user listed fights")
	}
}
