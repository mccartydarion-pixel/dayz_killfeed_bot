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

	"github.com/yourname/dayz-killfeed/internal/ownerops"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/servers"
)

// GET .../admin/server-status over a real PostgreSQL (docs/SERVER_STATUS.md): who may read it,
// that it only ever describes the caller's own installation, and that the stored facts show.
func TestServerStatusRoute(t *testing.T) {
	w := newClientAdminWorld(t)
	a := w.a
	ctx := context.Background()
	a.PlatformOps = repository.NewPlatformOpsRepository(a.DB.Pool)
	a.Kills = repository.NewKillRepository(a.DB.Pool)
	a.saasAdminReadLimiter = nil
	a.Config.DiscordGuildID = w.f.DiscordGuildID // this process serves the installation's Discord server
	a.discordReady = func() bool { return true }
	fresh := func() { a.serverStatusFacts = nil } // skip the 20 second cache between steps

	get := func(actor string) (int, map[string]any, string) {
		t.Helper()
		fresh()
		rr := w.call(a.handleServerStatus, http.MethodGet, w.path("/server-status"), actor, nil, nil)
		out := map[string]any{}
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out, rr.Body.String()
	}
	feed := func(body map[string]any) (string, string) {
		f, _ := body["feed"].(map[string]any)
		state, _ := f["state"].(string)
		reason, _ := f["reason"].(string)
		return state, reason
	}
	owner := w.f.OwnerDiscordID

	// Setup is not finished: DOWN, with the reason, and the server is named.
	code, body, raw := get(owner)
	if state, reason := feed(body); code != http.StatusOK || state != ownerops.StatusDown || !strings.Contains(reason, "Setup is not finished") {
		t.Fatalf("unfinished setup: %d %s", code, raw)
	}
	if body["installationId"].(float64) != float64(w.f.InstallationID) || body["serverId"].(float64) != float64(w.serverID) || body["lastKillAt"] != nil || body["mapRotation"] != nil {
		t.Fatalf("identity = %s", raw)
	}
	// Nothing that is not the customer's to see: no credential, no service secret, no Nitrado id.
	for _, secret := range []string{"test-secret", w.providerServiceID, "credential", "token", "ciphertext"} {
		if strings.Contains(strings.ToLower(raw), strings.ToLower(secret)) {
			t.Fatalf("the response contains %q: %s", secret, raw)
		}
	}

	// Who may read it: the owner and an Administrator; not a Moderator, not somebody without a
	// level, not another organization's owner.
	admin := syncUser(t, a, fmt.Sprintf("status-admin-%d", standoutSeq.Add(1)), "Admin")
	w.mapRole(admin.DiscordUserID, "role-status-admin", "ADMINISTRATOR")
	if code, _, raw := get(admin.DiscordUserID); code != http.StatusOK {
		t.Fatalf("an Administrator must be allowed: %d %s", code, raw)
	}
	mod := syncUser(t, a, fmt.Sprintf("status-mod-%d", standoutSeq.Add(1)), "Mod")
	w.mapRole(mod.DiscordUserID, "role-status-mod", "MODERATOR")
	stranger := syncUser(t, a, fmt.Sprintf("status-stranger-%d", standoutSeq.Add(1)), "Stranger")
	other := buildInstallationFixture(t, a, w.verifier)
	for name, actor := range map[string]string{"a Moderator": mod.DiscordUserID, "a user with no level": stranger.DiscordUserID, "another organization's owner": other.OwnerDiscordID} {
		if code, body, raw := get(actor); code != http.StatusForbidden || errCode(body) != codeAdminForbidden {
			t.Fatalf("%s: %d %s", name, code, raw)
		}
	}
	if rr := w.call(a.handleServerStatus, http.MethodGet, w.path("/server-status"), "", nil, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no acting user: %d", rr.Code)
	}
	// An owner cannot reach another organization's installation through their own organization.
	cross := map[string]string{"installationID": strconv.FormatInt(other.InstallationID, 10)}
	if rr := w.call(a.handleServerStatus, http.MethodGet, w.path("/server-status"), owner, nil, cross); rr.Code != http.StatusNotFound {
		t.Fatalf("another organization's installation: %d %s", rr.Code, rr.Body.String())
	}

	// Set up, bot present, the process has just started and has no worker yet: DEGRADED, not DOWN.
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE installations SET status='READY', setup_completed_at=NOW() WHERE id=$1`, w.f.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE discord_guild_connections SET bot_installed=TRUE WHERE id=$1`, w.f.ConnectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE game_servers SET display_name='Status Server' WHERE id=$1`, w.serverID); err != nil {
		t.Fatal(err)
	}
	code, body, raw = get(owner)
	if state, reason := feed(body); code != http.StatusOK || state != ownerops.StatusDegraded || !strings.Contains(reason, "just restarted") || body["serverName"] != "Status Server" {
		t.Fatalf("just started: %d %s", code, raw)
	}

	// Past start-up and still no worker: DOWN.
	a.WorkerManager = servers.NewWorkerManager(func(ctx context.Context, _ int64) error { <-ctx.Done(); return nil })
	t.Cleanup(a.WorkerManager.StopAll)
	a.ownerOpsReady = func() bool { return true }
	if state, reason := feed(func() map[string]any { _, b, _ := get(owner); return b }()); state != ownerops.StatusDown || !strings.Contains(reason, "Nothing is reading") {
		t.Fatalf("no worker: %s %s", state, reason)
	}

	// A running worker and a recorded kill.
	if err := a.WorkerManager.Start(ctx, w.serverID); err != nil {
		t.Fatal(err)
	}
	killer, victim := w.seedPlayer("Status Killer"), w.seedPlayer("Status Victim")
	at := time.Now().UTC().Add(-time.Hour)
	if _, err := a.Kills.InsertKillReturning(ctx, repository.KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s",
		Fingerprint: fmt.Sprintf("status-kill-%d", standoutSeq.Add(1)), KillerPlayerID: killer, VictimPlayerID: victim, WeaponRaw: "M4", WeaponDisplay: "M4", EventTime: &at}); err != nil {
		t.Fatal(err)
	}
	code, body, raw = get(owner)
	if state, _ := feed(body); code != http.StatusOK || state != ownerops.StatusRunning || body["lastKillAt"] == nil {
		t.Fatalf("running: %d %s", code, raw)
	}
	discord, _ := body["discord"].(map[string]any)
	if discord["reachable"] != true || discord["botInGuild"] != true {
		t.Fatalf("discord = %s", raw)
	}

	// The installation's Discord server is served by another process: this one's worker is not
	// described.
	a.Config.DiscordGuildID = "some-other-guild"
	if state, _ := feed(func() map[string]any { _, b, _ := get(owner); return b }()); state != ownerops.StatusDown {
		t.Fatalf("not served here: %s", state)
	}
	a.Config.DiscordGuildID = w.f.DiscordGuildID

	// Suspended.
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE installations SET status='SUSPENDED' WHERE id=$1`, w.f.InstallationID); err != nil {
		t.Fatal(err)
	}
	if state, reason := feed(func() map[string]any { _, b, _ := get(owner); return b }()); state != ownerops.StatusDown || !strings.Contains(reason, "suspended") {
		t.Fatalf("suspended: %s %s", state, reason)
	}
}

// The silent-feed incident through the real monitor (docs/OWNER_OPS.md "Silent feeds"): it opens
// once, tells the admins once, closes by itself when the feed recovers, and never opens for a
// suspended installation.
func TestOwnerOpsSilentFeedIncident(t *testing.T) {
	w := newOwnerOpsWorld(t)
	a := w.a
	ctx := context.Background()
	now := time.Now().UTC()
	if err := a.WorkerManager.Start(ctx, w.serverID); err != nil {
		t.Fatal(err)
	}
	if err := a.PlatformOps.SaveSettings(ctx, repository.OwnerOpsSettings{AlertsEnabled: true, BriefingHourUTC: 13}, "test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = a.PlatformOps.SaveSettings(context.Background(), repository.DefaultOwnerOpsSettings(), "test")
	})

	// watch sets what the feed watch has timed for the server, as its sampler would have.
	watch := func(players int, onlineFor, watchingFor time.Duration, playerList bool) {
		e := &feedWatchEntry{watchingSince: now.Add(-watchingFor), lastAt: now,
			last: feedSample{WorkerRunning: true, PlayersKnown: true, PlayersOnline: players, PlayersSource: counterSourceNitrado}}
		if players > 0 {
			e.playersSince = now.Add(-onlineFor)
		}
		if playerList {
			e.lastPlayerList = now.Add(-watchingFor)
			e.last.LastPlayerList = e.lastPlayerList
		}
		a.feedWatch.mu.Lock()
		a.feedWatch.entries = map[int64]*feedWatchEntry{w.serverID: e}
		a.feedWatch.mu.Unlock()
	}
	tick := func(at time.Time) {
		t.Helper()
		if err := a.ownerOpsTick(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	stalled := func() []repository.PlatformIncident {
		out := []repository.PlatformIncident{}
		for _, inc := range w.incidents("") {
			if inc.Kind == repository.IncidentFeedStalled {
				out = append(out, inc)
			}
		}
		return out
	}

	// An empty server that has been silent for hours, and a busy server just after a restart:
	// neither is an incident, however often the monitor looks.
	watch(0, 0, 5*time.Hour, true)
	tick(now)
	tick(now.Add(ownerops.DetectGrace + time.Minute))
	watch(6, 10*time.Minute, 10*time.Minute, false)
	tick(now.Add(ownerops.DetectGrace + 2*time.Minute))
	tick(now.Add(2*ownerops.DetectGrace + 3*time.Minute))
	if got := stalled(); len(got) != 0 {
		t.Fatalf("an empty or just-restarted server opened an incident: %+v", got)
	}

	// Suspended, with players online and a silent log: nothing.
	watch(4, 3*time.Hour, 3*time.Hour, true)
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE installations SET status='SUSPENDED' WHERE id=$1`, w.a1.InstallationID); err != nil {
		t.Fatal(err)
	}
	base := now.Add(time.Hour)
	watchAt := func() { // the watch's clocks are relative to "now"; keep the same evidence at the later ticks
		a.feedWatch.mu.Lock()
		e := a.feedWatch.entries[w.serverID]
		e.watchingSince, e.playersSince = base.Add(-3*time.Hour), base.Add(-3*time.Hour)
		a.feedWatch.mu.Unlock()
	}
	watchAt()
	tick(base)
	tick(base.Add(ownerops.DetectGrace + time.Minute))
	if got := stalled(); len(got) != 0 {
		t.Fatalf("a suspended installation opened an incident: %+v", got)
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE installations SET status='READY' WHERE id=$1`, w.a1.InstallationID); err != nil {
		t.Fatal(err)
	}

	// Ready again, players online for hours and not one log line: one observation is not an
	// incident; after the grace period it is - once.
	first := base.Add(10 * time.Minute)
	a.ownerOpsForget(w.a1.InstallationID, ownerops.KindFeedStalled)
	tick(first)
	if got := stalled(); len(got) != 0 {
		t.Fatalf("opened on the first observation: %+v", got)
	}
	tick(first.Add(ownerops.DetectGrace + time.Minute))
	tick(first.Add(ownerops.DetectGrace + 3*time.Minute))
	tick(first.Add(ownerops.DetectGrace + 5*time.Minute))
	got := stalled()
	if len(got) != 1 || got[0].Status != repository.IncidentOpen || !strings.Contains(got[0].Detail, "4 player(s) have been online") || !strings.Contains(got[0].Detail, "produced nothing") {
		t.Fatalf("silent feed incident = %+v", got)
	}
	if n := w.sent("dm", "Incident opened: Feed stalled"); n != 1 {
		t.Fatalf("the admins were told %d times, want once", n)
	}
	if !a.WorkerManager.Running(w.serverID) {
		t.Fatal("with self-healing off the worker must be left alone")
	}

	// A log line arrives: the incident closes by itself and the admins are told.
	a.feedWatch.observe(w.serverID, feedSample{WorkerRunning: true, PlayersKnown: true, PlayersOnline: 4, LastLogLineAt: first.Add(ownerops.DetectGrace + 6*time.Minute)},
		first.Add(ownerops.DetectGrace+6*time.Minute))
	tick(first.Add(ownerops.DetectGrace + 7*time.Minute))
	got = stalled()
	if len(got) != 1 || got[0].Status != repository.IncidentResolved || got[0].Resolution != "recovered on its own" {
		t.Fatalf("after recovery = %+v", got)
	}
	if w.sent("dm", "Incident resolved: Feed stalled") != 1 {
		t.Fatal("the admins were not told it resolved")
	}
	tick(first.Add(ownerops.DetectGrace + 9*time.Minute))
	if again := stalled(); len(again) != 1 {
		t.Fatalf("a healthy feed opened another incident: %+v", again)
	}
}

// A deploy that is not healthy at the deadline reaches the platform admins through the Owner
// Hub's alert switch, and only through it.
func TestDeploySelfCheckAlert(t *testing.T) {
	w := newOwnerOpsWorld(t)
	a := w.a
	ctx := context.Background()
	t.Cleanup(func() {
		_ = a.PlatformOps.SaveSettings(context.Background(), repository.DefaultOwnerOpsSettings(), "test")
	})
	a.discordReady = func() bool { return false }
	a.Config.DatabaseURL = "postgres://configured"
	var applied int
	if err := a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil || applied == 0 {
		t.Fatalf("migrations: %d %v", applied, err)
	}
	a.deploy.setMigrations(applied)
	a.deploy.setExpectedWorkers(1)

	facts := a.deployFactsNow(time.Now())
	problems := deployProblems(facts)
	if facts.MigrationsApplied != applied || !facts.DatabaseConnected || len(problems) != 2 {
		t.Fatalf("facts = %+v, problems = %q", facts, problems)
	}

	if err := a.PlatformOps.SaveSettings(ctx, repository.DefaultOwnerOpsSettings(), "test"); err != nil {
		t.Fatal(err)
	}
	a.deployUnhealthy(ctx, time.Now(), facts, problems)
	if n := w.sent("dm", "Deploy self-check failed"); n != 0 {
		t.Fatalf("alerts are switched off, yet %d message(s) went out", n)
	}
	if got := a.runtimeDeploy(time.Now()); got.State != DeployUnhealthy || len(got.Problems) != 2 || got.MigrationsApplied == nil || *got.MigrationsApplied != applied || got.WorkersExpected != 1 {
		t.Fatalf("runtime status deploy block = %+v", got)
	}

	if err := a.PlatformOps.SaveSettings(ctx, repository.OwnerOpsSettings{AlertsEnabled: true, BriefingHourUTC: 13}, "test"); err != nil {
		t.Fatal(err)
	}
	a.deployUnhealthy(ctx, time.Now(), facts, problems)
	if n := w.sent("dm", adminFounderID+"|**Deploy self-check failed**"); n != 1 {
		t.Fatalf("the platform admin was told %d times, want once", n)
	}
	if w.sent("dm", "the Discord gateway is not connected") != 1 || w.sent("dm", "1 of 1 server workers are not running") != 1 {
		t.Fatalf("the message must say what is wrong: %v", w.dms)
	}
	if !a.deploy.alerted {
		t.Fatal("a delivered alert must be remembered, so the recovery is announced")
	}
}
