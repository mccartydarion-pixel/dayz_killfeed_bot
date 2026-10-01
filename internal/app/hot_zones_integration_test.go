//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	competitiveevents "github.com/yourname/dayz-killfeed/internal/events"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Hot zones and the feature-settings API over a real PostgreSQL (docs/HOT_ZONES.md).

var standoutSeq atomic.Int64

// newStandoutWorld is clientAdminWorld plus everything the opt-in features read.
func newStandoutWorld(t *testing.T) *clientAdminWorld {
	t.Helper()
	w := newClientAdminWorld(t)
	pool := w.a.DB.Pool
	w.a.Locations = repository.NewLocationRepository(pool)
	w.a.FeatureSettings = repository.NewFeatureSettingsRepository(pool)
	w.a.Events = repository.NewEventRepository(pool)
	w.a.EventService = competitiveevents.NewService(w.a.Events)
	w.a.hotZoneKills = repository.NewHeatmapRepository(pool)
	w.a.Players = repository.NewPlayerRepository(pool)
	w.a.Kills = repository.NewKillRepository(pool)
	w.a.ActivityRepository = repository.NewActivityRepository(pool)
	w.a.SaaSPlayer = repository.NewPlayerServerRepository(pool)
	w.a.Lives = repository.NewLifeRepository(pool)
	w.a.Retention = repository.NewRetentionRepository(pool)
	w.a.Fights = repository.NewFightRepository(pool)
	w.a.Network = repository.NewNetworkRepository(pool)
	return w
}

// player seeds a tracked player with a collision-free DayZ id.
func (w *clientAdminWorld) player(name string) int64 {
	w.t.Helper()
	var id int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,$3) RETURNING id`,
		w.guildID, fmt.Sprintf("so-%d-%d", time.Now().UnixNano(), standoutSeq.Add(1)), name).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// killAt persists a kill and the killer's KILL location row the heatmap joins on.
func (w *clientAdminWorld) killAt(killer, victim int64, at time.Time, x, z float64) (int64, repository.KillRecord) {
	w.t.Helper()
	at = at.UTC().Truncate(time.Millisecond)
	rec := repository.KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: fmt.Sprintf("so-kill-%d", standoutSeq.Add(1)),
		KillerPlayerID: killer, VictimPlayerID: victim, WeaponRaw: "KA-M", WeaponDisplay: "KA-M", EventTime: &at}
	id, err := w.a.Kills.InsertKillReturning(context.Background(), rec)
	if err != nil {
		w.t.Fatal(err)
	}
	w.seedLocationEvent(killer, x, z, "KILL", at)
	return id, rec
}

func (w *clientAdminWorld) put(handler http.HandlerFunc, suffix, actor string, body any) (int, map[string]any) {
	w.t.Helper()
	rr := w.call(handler, http.MethodPut, w.path(suffix), actor, body, nil)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func (w *clientAdminWorld) hotZoneEvents() []repository.CompetitiveEvent {
	w.t.Helper()
	events, err := w.a.Events.HotZones(context.Background(), w.guildID, w.serverID, false, 50)
	if err != nil {
		w.t.Fatal(err)
	}
	return events
}

// tick runs the hot-zone check as the scheduler would, ignoring its per-guild throttle.
func (w *clientAdminWorld) tick(now time.Time) {
	w.a.hotZoneMu.Lock()
	w.a.hotZoneChecked = nil
	w.a.hotZoneMu.Unlock()
	w.a.hotZoneTick(context.Background(), w.guildID, now)
}

func TestHotZoneOpensScoresPaysAndCoolsDown(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	now := time.Now().UTC()
	hunter, rival, victim := w.player("Hunter"), w.player("Rival"), w.player("Victim")
	// Six kills inside grid cell (15, 25) - x 7500..8000, z 12500..13000 - and two far away.
	for i := 0; i < 6; i++ {
		w.killAt(hunter, victim, now.Add(-time.Duration(10+i)*time.Minute), 7600+float64(i*20), 12700)
	}
	w.killAt(rival, victim, now.Add(-5*time.Minute), 2100, 3300)
	w.killAt(rival, victim, now.Add(-6*time.Minute), 2150, 3300)

	// Off by default: nothing opens, however busy the map is.
	w.tick(now)
	if got := w.hotZoneEvents(); len(got) != 0 {
		t.Fatalf("a hot zone opened with the feature off: %+v", got)
	}

	body := hotZoneSettingsDTO{Enabled: true, WindowMinutes: 60, MinKills: 6, RadiusMeters: 500, DurationMinutes: 30, CooldownMinutes: 120, FirstPoints: 500, SecondPoints: 250, ThirdPoints: 100}
	if code, resp := w.put(w.a.handlePutHotZoneSettings, "/features/hot-zones", w.f.OwnerDiscordID, body); code != http.StatusOK || resp["hotZones"].(map[string]any)["enabled"] != true {
		t.Fatalf("enable hot zones: %d %v", code, resp)
	}
	w.tick(now)
	events := w.hotZoneEvents()
	if len(events) != 1 {
		t.Fatalf("expected one hot zone, got %+v", events)
	}
	zone := events[0]
	var cfg competitiveevents.HotZoneConfig
	if err := json.Unmarshal(zone.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if zone.Status != "ACTIVE" || zone.Name != "Hot Zone 7750 / 12750" || cfg.ServerID != w.serverID || cfg.CenterX != 7750 || cfg.CenterZ != 12750 ||
		cfg.RadiusM != 500 || cfg.KillsObserved != 6 || !cfg.Auto || zone.EndsAt.Sub(*zone.StartsAt) != 30*time.Minute {
		t.Fatalf("zone = %+v cfg = %+v", zone, cfg)
	}
	var first, second, third int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT winner_points, second_place_points, third_place_points FROM competitive_events WHERE id=$1`, zone.ID).Scan(&first, &second, &third); err != nil || first != 500 || second != 250 || third != 100 {
		t.Fatalf("rewards = %d/%d/%d err=%v", first, second, third, err)
	}
	// A second check while one is open opens nothing.
	w.tick(now.Add(3 * time.Minute))
	if got := w.hotZoneEvents(); len(got) != 1 {
		t.Fatalf("a second hot zone opened while one was active: %d", len(got))
	}

	// Scoring runs through the persisted-kill hook: a victim inside the circle scores, one outside does not.
	adapter := &persistenceStoreAdapter{events: w.a.Events, streaks: w.a.Streaks}
	persist := func(killer int64, x, z float64) {
		id, rec := w.killAt(killer, victim, now.Add(time.Minute), x, z)
		adapter.ProcessPersistedKill(ctx, id, rec, &killfeed.Event{Type: killfeed.EventPlayerKill, Timestamp: now.Add(time.Minute),
			Killer: &killfeed.PlayerRef{Name: "k", Position: &killfeed.Position{X: x + 150, Y: z, Z: 12}},
			Victim: &killfeed.PlayerRef{Name: "v", Position: &killfeed.Position{X: x, Y: z, Z: 9}}})
	}
	persist(hunter, 7900, 12600)
	persist(hunter, 7700, 13000)
	persist(rival, 7750, 12750)
	persist(rival, 9000, 12750) // 1250 m east of the centre: outside

	rr := w.call(w.a.handleAdminHotZones, http.MethodGet, w.path("/hot-zones"), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin hot zones: %d %s", rr.Code, rr.Body.String())
	}
	list := decodeBody[struct {
		Items []hotZoneDTO `json:"items"`
	}](t, rr)
	if len(list.Items) != 1 || len(list.Items[0].Standings) != 2 || list.Items[0].Standings[0].PlayerName != "Hunter" || list.Items[0].Standings[0].Kills != 2 ||
		list.Items[0].Standings[1].PlayerName != "Rival" || list.Items[0].Standings[1].Kills != 1 || list.Items[0].RadiusMeters != 500 {
		t.Fatalf("hot zones = %+v", list.Items)
	}

	// The player API shows the open zone to a verified player of the server.
	fan := syncUser(t, w.a, fmt.Sprintf("so-fan-%d", standoutSeq.Add(1)), "Fan")
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,'VERIFIED')`, w.guildID, hunter, fan.DiscordUserID); err != nil {
		t.Fatal(err)
	}
	playerPath := map[string]string{"installationID": strconv.FormatInt(w.f.InstallationID, 10)}
	rr = w.call(w.a.handlePlayerHotZone, http.MethodGet, "/api/saas/player/servers/x/hot-zone", fan.DiscordUserID, nil, playerPath)
	open := decodeBody[struct {
		HotZone *hotZoneDTO `json:"hotZone"`
	}](t, rr)
	if rr.Code != http.StatusOK || open.HotZone == nil || open.HotZone.ID != zone.ID || len(open.HotZone.Standings) != 2 {
		t.Fatalf("player hot zone: %d %s", rr.Code, rr.Body.String())
	}

	// The zone ends through the ordinary event scheduler and pays its podium.
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE competitive_events SET ends_at=$1 WHERE id=$2`, now.Add(2*time.Minute), zone.ID); err != nil {
		t.Fatal(err)
	}
	after := now.Add(3 * time.Minute)
	if err := w.a.EventService.SchedulerTick(ctx, after); err != nil {
		t.Fatal(err)
	}
	if err := w.a.EventService.FinalizeEvent(ctx, w.guildID, zone.ID, after); err != nil {
		t.Fatal(err)
	}
	var reward, placement int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT placement, reward_points FROM player_event_results WHERE event_id=$1 AND player_id=$2`, zone.ID, hunter).Scan(&placement, &reward); err != nil || placement != 1 || reward != 500 {
		t.Fatalf("winner placement=%d reward=%d err=%v", placement, reward, err)
	}
	rr = w.call(w.a.handlePlayerHotZone, http.MethodGet, "/api/saas/player/servers/x/hot-zone", fan.DiscordUserID, nil, playerPath)
	if closed := decodeBody[struct {
		HotZone *hotZoneDTO `json:"hotZone"`
	}](t, rr); closed.HotZone != nil {
		t.Fatalf("an ended hot zone is still reported as open: %+v", closed.HotZone)
	}

	// Cooling down: the same busy cell does not reopen until the cooldown has passed.
	w.tick(after.Add(time.Minute))
	if got := w.hotZoneEvents(); len(got) != 1 {
		t.Fatalf("a hot zone reopened inside the cooldown: %d", len(got))
	}
	body.CooldownMinutes = 0
	if code, _ := w.put(w.a.handlePutHotZoneSettings, "/features/hot-zones", w.f.OwnerDiscordID, body); code != http.StatusOK {
		t.Fatalf("set cooldown: %d", code)
	}
	w.tick(after.Add(2 * time.Minute))
	if got := w.hotZoneEvents(); len(got) != 2 || got[0].Status != "ACTIVE" {
		t.Fatalf("no new hot zone after the cooldown: %+v", got)
	}
}

func TestHotZoneNeedsEnoughKillsInOneCell(t *testing.T) {
	w := newStandoutWorld(t)
	now := time.Now().UTC()
	a, v := w.player("A"), w.player("V")
	// Five kills in one cell and five in the next: ten nearby kills, but no cell reaches six.
	for i := 0; i < 5; i++ {
		w.killAt(a, v, now.Add(-time.Duration(i+1)*time.Minute), 7900, 12700)
		w.killAt(a, v, now.Add(-time.Duration(i+1)*time.Minute-time.Second), 8100, 12700)
	}
	// A sixth kill in the first cell, but outside the 60-minute window.
	w.killAt(a, v, now.Add(-90*time.Minute), 7900, 12700)
	if _, err := w.a.FeatureSettings.SaveHotZones(context.Background(), w.f.InstallationID, 0, repository.HotZoneSettings{Enabled: true, WindowMinutes: 60, MinKills: 6,
		RadiusM: 500, DurationMinutes: 30, CooldownMinutes: 120}); err != nil {
		t.Fatal(err)
	}
	w.tick(now)
	if got := w.hotZoneEvents(); len(got) != 0 {
		t.Fatalf("a hot zone opened below the threshold: %+v", got)
	}
	// The throttle: a second scheduler pass inside the interval does not even look.
	w.a.hotZoneTick(context.Background(), w.guildID, now.Add(time.Second))
	w.a.hotZoneMu.Lock()
	checked := w.a.hotZoneChecked[w.guildID]
	w.a.hotZoneMu.Unlock()
	if !checked.Equal(now) {
		t.Fatalf("the check ran again inside its interval: %v", checked)
	}
}

func TestFeatureSettingsDefaultsValidationAndPermissions(t *testing.T) {
	w := newStandoutWorld(t)
	owner := w.f.OwnerDiscordID

	rr := w.call(w.a.handleGetFeatureSettings, http.MethodGet, w.path("/features"), owner, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get features: %d %s", rr.Code, rr.Body.String())
	}
	defaults := decodeBody[featureSettingsDTO](t, rr)
	if defaults.HotZones.Enabled || defaults.FightReplay.Public || defaults.Network.Listed || defaults.FeedIdentity.Enabled {
		t.Fatalf("a feature is on by default: %+v", defaults)
	}
	if defaults.HotZones.MinKills != 6 || defaults.HotZones.RadiusMeters != 500 || defaults.FightReplay.DelayMinutes != 60 || defaults.UpdatedAt != nil {
		t.Fatalf("defaults = %+v", defaults)
	}

	// Validation: a caller-fixable message, never a database error.
	bad := hotZoneSettingsDTO{Enabled: true, WindowMinutes: 60, MinKills: 1, RadiusMeters: 500, DurationMinutes: 30, CooldownMinutes: 120}
	if code, resp := w.put(w.a.handlePutHotZoneSettings, "/features/hot-zones", owner, bad); code != http.StatusBadRequest ||
		resp["error"].(map[string]any)["message"] != "minKills must be between 2 and 500" {
		t.Fatalf("invalid hot zone settings: %d %v", code, resp)
	}
	for name, body := range map[string]feedIdentitySettingsDTO{
		"enabled without a name": {Enabled: true},
		"discord in the name":    {Enabled: true, Name: "Discord Feed"},
		"mention in the name":    {Enabled: true, Name: "@everyone"},
		"http avatar":            {Enabled: true, Name: "Deadzone", AvatarURL: "http://cdn.example/a.png"},
	} {
		if code, _ := w.put(w.a.handlePutFeedIdentitySettings, "/features/feed-identity", owner, body); code != http.StatusBadRequest {
			t.Errorf("feed identity %q accepted: %d", name, code)
		}
	}
	if code, _ := w.put(w.a.handlePutFightReplaySettings, "/features/fight-replay", owner, fightReplaySettingsDTO{Public: true, DelayMinutes: -1}); code != http.StatusBadRequest {
		t.Fatalf("negative replay delay accepted: %d", code)
	}

	// Each section saves on its own and leaves the others alone.
	if code, resp := w.put(w.a.handlePutFeedIdentitySettings, "/features/feed-identity", owner, feedIdentitySettingsDTO{Enabled: true, Name: "  Deadzone Feed ", AvatarURL: "https://cdn.example/a.png"}); code != http.StatusOK ||
		resp["feedIdentity"].(map[string]any)["name"] != "Deadzone Feed" {
		t.Fatalf("save feed identity: %d %v", code, resp)
	}
	if code, _ := w.put(w.a.handlePutNetworkSettings, "/features/network", owner, networkSettingsDTO{Listed: true, Description: "Hardcore PvP, weekly wipes", DiscordInviteURL: "https://example.com/join"}); code != http.StatusBadRequest {
		t.Fatalf("a link that is not a Discord invite: %d, want 400", code)
	}
	if code, resp := w.put(w.a.handlePutNetworkSettings, "/features/network", owner, networkSettingsDTO{Listed: true, Description: "Hardcore PvP, weekly wipes", DiscordInviteURL: " discord.com/invite/deadzone "}); code != http.StatusOK ||
		resp["network"].(map[string]any)["discordInviteUrl"] != "https://discord.gg/deadzone" {
		t.Fatalf("save network: %d %v", code, resp)
	}
	if code, _ := w.put(w.a.handlePutFightReplaySettings, "/features/fight-replay", owner, fightReplaySettingsDTO{Public: true, DelayMinutes: 30}); code != http.StatusOK {
		t.Fatalf("save fight replay: %d", code)
	}
	saved := decodeBody[featureSettingsDTO](t, w.call(w.a.handleGetFeatureSettings, http.MethodGet, w.path("/features"), owner, nil, nil))
	if !saved.FeedIdentity.Enabled || saved.FeedIdentity.Name != "Deadzone Feed" || !saved.Network.Listed || saved.Network.Description != "Hardcore PvP, weekly wipes" || saved.Network.DiscordInviteURL != "https://discord.gg/deadzone" ||
		!saved.FightReplay.Public || saved.FightReplay.DelayMinutes != 30 || saved.HotZones.Enabled || saved.HotZones.MinKills != 6 || saved.UpdatedAt == nil {
		t.Fatalf("saved = %+v", saved)
	}
	identity, ok, err := w.a.FeatureSettings.FeedIdentityForServer(context.Background(), w.serverID)
	if err != nil || !ok || identity.Name != "Deadzone Feed" {
		t.Fatalf("FeedIdentityForServer = %+v %v %v", identity, ok, err)
	}
	var audits int
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action IN ('FEED_IDENTITY_UPDATED','NETWORK_SETTINGS_UPDATED','FIGHT_REPLAY_SETTINGS_UPDATED')`, w.f.InstallationID).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("audit rows = %d err=%v", audits, err)
	}

	// Permissions: no level sees nothing; a Moderator reads; an Administrator changes hot zones and
	// replay but not the network listing or the feed identity (Owner only).
	stranger := syncUser(t, w.a, fmt.Sprintf("so-stranger-%d", standoutSeq.Add(1)), "Stranger")
	mod := syncUser(t, w.a, fmt.Sprintf("so-mod-%d", standoutSeq.Add(1)), "Mod")
	admin := syncUser(t, w.a, fmt.Sprintf("so-admin-%d", standoutSeq.Add(1)), "Admin")
	w.mapRole(mod.DiscordUserID, "so-role-mod", "MODERATOR")
	w.mapRole(admin.DiscordUserID, "so-role-admin", "ADMINISTRATOR")
	good := hotZoneSettingsDTO{Enabled: true, WindowMinutes: 30, MinKills: 4, RadiusMeters: 300, DurationMinutes: 20, CooldownMinutes: 60}
	if rr := w.call(w.a.handleGetFeatureSettings, http.MethodGet, w.path("/features"), stranger.DiscordUserID, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("stranger read features: %d", rr.Code)
	}
	if rr := w.call(w.a.handleGetFeatureSettings, http.MethodGet, w.path("/features"), mod.DiscordUserID, nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("moderator read features: %d", rr.Code)
	}
	if code, _ := w.put(w.a.handlePutHotZoneSettings, "/features/hot-zones", mod.DiscordUserID, good); code != http.StatusForbidden {
		t.Fatalf("moderator changed hot zones: %d", code)
	}
	if code, _ := w.put(w.a.handlePutHotZoneSettings, "/features/hot-zones", admin.DiscordUserID, good); code != http.StatusOK {
		t.Fatalf("administrator could not change hot zones: %d", code)
	}
	if code, _ := w.put(w.a.handlePutNetworkSettings, "/features/network", admin.DiscordUserID, networkSettingsDTO{Listed: false}); code != http.StatusForbidden {
		t.Fatalf("administrator changed the network listing: %d", code)
	}
	if code, _ := w.put(w.a.handlePutFeedIdentitySettings, "/features/feed-identity", admin.DiscordUserID, feedIdentitySettingsDTO{}); code != http.StatusForbidden {
		t.Fatalf("administrator changed the feed identity: %d", code)
	}
}
