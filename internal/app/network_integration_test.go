//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Cross-server network over a real PostgreSQL (docs/NETWORK.md). The database is shared with every
// other integration test, so assertions look for this test's own uniquely named rows rather than
// assuming it owns the whole directory or board.

// account seeds a player with an explicit DayZ id (the cross-server identity).
func (w *clientAdminWorld) account(dayzID, name string, lastSeen time.Time) int64 {
	w.t.Helper()
	var id int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id, dayz_player_id, display_name, last_seen_at) VALUES($1,$2,$3,$4) RETURNING id`,
		w.guildID, dayzID, name, lastSeen).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *clientAdminWorld) plainKill(killer, victim int64, at time.Time, distance float64) {
	w.t.Helper()
	if _, err := w.a.Kills.InsertKillReturning(context.Background(), repository.KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s",
		Fingerprint: fmt.Sprintf("net-kill-%d", standoutSeq.Add(1)), KillerPlayerID: killer, VictimPlayerID: victim, WeaponRaw: "Tundra", WeaponDisplay: "Tundra",
		Distance: &distance, EventTime: &at}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *clientAdminWorld) list(listed bool, description string) {
	w.t.Helper()
	if _, err := w.a.FeatureSettings.SaveNetwork(context.Background(), w.f.InstallationID, 0, repository.NetworkSettings{Listed: listed, Description: description}); err != nil {
		w.t.Fatal(err)
	}
}

func findRank(items []networkRankDTO, name string) *networkRankDTO {
	for i := range items {
		if items[i].PlayerName == name {
			return &items[i]
		}
	}
	return nil
}

func findServer(items []networkServerDTO, installationID int64) *networkServerDTO {
	for i := range items {
		if items[i].InstallationID == installationID {
			return &items[i]
		}
	}
	return nil
}

func TestNetworkDirectoryAndCrossServerLeaderboards(t *testing.T) {
	a, b, hidden := newStandoutWorld(t), newStandoutWorld(t), newStandoutWorld(t)
	now := time.Now().UTC()
	tag := fmt.Sprint(standoutSeq.Add(1), "-", now.UnixNano())
	ghostID := "acct-ghost-" + tag
	ghostOld, ghostNew, solo, hiddenName := "GhostOld-"+tag, "GhostNew-"+tag, "Solo-"+tag, "Hidden-"+tag

	// One account on two listed servers, under an older name on A and a newer one on B.
	ghostA := a.account(ghostID, ghostOld, now.Add(-48*time.Hour))
	ghostB := b.account(ghostID, ghostNew, now.Add(-time.Hour))
	soloA := a.account("acct-solo-"+tag, solo, now)
	victimA, victimB := a.player("VictimA"), b.player("VictimB")
	for i := 0; i < 3; i++ {
		a.plainKill(ghostA, victimA, now.Add(-time.Duration(i+1)*time.Hour), 100000+float64(i))
	}
	for i := 0; i < 2; i++ {
		b.plainKill(ghostB, victimB, now.Add(-time.Duration(i+1)*time.Hour), 50)
	}
	for i := 0; i < 4; i++ {
		a.plainKill(soloA, victimA, now.Add(-time.Duration(i+1)*time.Hour), 60)
	}
	a.plainKill(soloA, soloA, now, 1) // a self-kill is not a kill on any board
	// A server that never opted in, with a player who would top every board.
	hiddenKiller := hidden.account("acct-hidden-"+tag, hiddenName, now)
	for i := 0; i < 50; i++ {
		hidden.plainKill(hiddenKiller, hidden.player("V"), now.Add(-time.Hour), 999999)
	}
	// A recorded life and some presence on A.
	if _, err := a.a.DB.Pool.Exec(context.Background(), `INSERT INTO player_lives(guild_id, server_id, player_id, started_at, ended_at, playtime_seconds, cause, death_fingerprint)
VALUES($1,$2,$3,$4,$5,900000000,'OTHER',$6)`, a.guildID, a.serverID, soloA, now.Add(-72*time.Hour), now.Add(-2*time.Hour), "net-life-"+tag); err != nil {
		t.Fatal(err)
	}
	a.activeOn(soloA, now, 600, 1)
	a.activeOn(ghostA, now.AddDate(0, 0, -3), 600, 1)
	if err := a.a.ActivityRepository.Connect(context.Background(), a.guildID, a.serverID, soloA, now); err != nil {
		t.Fatal(err)
	}

	a.list(true, "Hardcore PvP "+tag)
	b.list(true, "")
	// Leave nothing listed behind: the directory and boards are shared with later runs.
	t.Cleanup(func() {
		a.list(false, "")
		b.list(false, "")
	})

	get := func(handler http.HandlerFunc, path string, pv map[string]string) *httptest.ResponseRecorder {
		return a.call(handler, http.MethodGet, path, "", nil, pv)
	}
	type serverList struct {
		Items []networkServerDTO `json:"items"`
	}
	type board struct {
		Board string           `json:"board"`
		Items []networkRankDTO `json:"items"`
	}

	rr := get(a.a.handleNetworkServers, "/api/saas/network/servers", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("directory: %d %s", rr.Code, rr.Body.String())
	}
	servers := decodeBody[serverList](t, rr).Items
	sa, sb := findServer(servers, a.f.InstallationID), findServer(servers, b.f.InstallationID)
	if sa == nil || sb == nil || findServer(servers, hidden.f.InstallationID) != nil {
		t.Fatalf("directory listed=%v/%v hidden=%v", sa != nil, sb != nil, findServer(servers, hidden.f.InstallationID) != nil)
	}
	if sa.Description != "Hardcore PvP "+tag || sa.Platform != "PLAYSTATION" || sa.TotalKills != 8 || sa.Kills7d != 8 || sa.ActivePlayers7d != 2 ||
		sa.PlayersOnline != 1 || sa.TrackedPlayers != 1 || sa.LastActivityAt == nil || sb.TotalKills != 2 {
		t.Fatalf("server A = %+v, server B total kills = %d", *sa, sb.TotalKills)
	}

	// Kills: the account's kills on both listed servers add up under its most recent name.
	rr = get(a.a.handleNetworkLeaderboard, "/api/saas/network/leaderboard?limit=100", nil)
	kills := decodeBody[board](t, rr)
	ghost, soloRank := findRank(kills.Items, ghostNew), findRank(kills.Items, solo)
	if rr.Code != http.StatusOK || kills.Board != "KILLS" || ghost == nil || soloRank == nil {
		t.Fatalf("kills board: %d ghost=%v solo=%v", rr.Code, ghost, soloRank)
	}
	if ghost.Value != 5 || ghost.Servers == nil || *ghost.Servers != 2 || soloRank.Value != 4 || ghost.Rank >= soloRank.Rank {
		t.Fatalf("ghost = %+v solo = %+v", *ghost, *soloRank)
	}
	if findRank(kills.Items, ghostOld) != nil || findRank(kills.Items, hiddenName) != nil {
		t.Fatal("the board shows a stale name or an unlisted server's player")
	}

	longest := decodeBody[board](t, get(a.a.handleNetworkLeaderboard, "/api/saas/network/leaderboard?board=longest_kill&limit=100", nil))
	top := findRank(longest.Items, ghostOld)
	if top == nil || top.Value != 100002 || top.ServerName == nil || top.Weapon == nil || *top.Weapon != "Tundra" || findRank(longest.Items, hiddenName) != nil {
		t.Fatalf("longest kill board = %+v", top)
	}
	lives := decodeBody[board](t, get(a.a.handleNetworkLeaderboard, "/api/saas/network/leaderboard?board=LONGEST_LIFE&limit=100", nil))
	if life := findRank(lives.Items, solo); life == nil || life.Value != 900000000 || life.Rank != 1 {
		t.Fatalf("longest life board = %+v", lives.Items)
	}

	// One listed server's page; an unlisted installation does not exist as far as the network goes.
	pv := func(id int64) map[string]string {
		return map[string]string{"installationID": strconv.FormatInt(id, 10)}
	}
	rr = get(a.a.handleNetworkServer, "/x", pv(a.f.InstallationID))
	detail := decodeBody[networkServerDetailDTO](t, rr)
	if rr.Code != http.StatusOK || len(detail.TopKillers) != 2 || detail.TopKillers[0].PlayerName != solo || detail.TopKillers[0].Value != 4 ||
		detail.TopKillers[1].PlayerName != ghostOld || len(detail.LongestLives) != 1 || len(detail.LongestKills) == 0 {
		t.Fatalf("server page: %d %+v", rr.Code, detail)
	}
	if rr := get(a.a.handleNetworkServer, "/x", pv(hidden.f.InstallationID)); rr.Code != http.StatusNotFound {
		t.Fatalf("an unlisted server has a network page: %d", rr.Code)
	}

	for _, q := range []string{"?board=nope", "?platform=bad%20value", "?days=9999", "?limit=0"} {
		if rr := get(a.a.handleNetworkLeaderboard, "/api/saas/network/leaderboard"+q, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", q, rr.Code)
		}
	}
	if none := decodeBody[board](t, get(a.a.handleNetworkLeaderboard, "/api/saas/network/leaderboard?platform=XBOX&limit=100", nil)); findRank(none.Items, ghostNew) != nil {
		t.Fatal("a PlayStation account appeared on the Xbox board")
	}

	// Unlisting through the API takes effect at once (the cache is dropped): A leaves the directory
	// and its kills leave the account's total.
	if code, _ := a.put(a.a.handlePutNetworkSettings, "/features/network", a.f.OwnerDiscordID, networkSettingsDTO{Listed: false}); code != http.StatusOK {
		t.Fatalf("unlist: %d", code)
	}
	servers = decodeBody[serverList](t, get(a.a.handleNetworkServers, "/api/saas/network/servers", nil)).Items
	if findServer(servers, a.f.InstallationID) != nil || findServer(servers, b.f.InstallationID) == nil {
		t.Fatal("unlisting did not remove exactly server A from the directory")
	}
	kills = decodeBody[board](t, get(a.a.handleNetworkLeaderboard, "/api/saas/network/leaderboard?limit=100", nil))
	if g := findRank(kills.Items, ghostNew); g == nil || g.Value != 2 || *g.Servers != 1 || findRank(kills.Items, solo) != nil {
		t.Fatalf("after unlisting: ghost = %+v, solo present = %v", g, findRank(kills.Items, solo) != nil)
	}
	if rr := get(a.a.handleNetworkServer, "/x", pv(a.f.InstallationID)); rr.Code != http.StatusNotFound {
		t.Fatalf("an unlisted server still has a page: %d", rr.Code)
	}

	// The routes need the service secret.
	req := httptest.NewRequest(http.MethodGet, "/api/saas/network/servers", nil)
	bare := httptest.NewRecorder()
	a.a.handleNetworkServers(bare, req)
	if bare.Code != http.StatusUnauthorized {
		t.Fatalf("directory without service auth: %d", bare.Code)
	}
}
