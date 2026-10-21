//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Live map over a real PostgreSQL (docs/LIVE_MAP.md): the public kills-and-pressure map behind
// its delay, the faction layer that shows only a faction's own members, and the audited staff
// views.

// liveMapWorld is a standout world with the live map, zones and a recorded boot session.
type liveMapWorld struct {
	*clientAdminWorld
	now      time.Time
	admFile  string
	deelo    int64 // the acting player (faction leader)
	mate     int64 // a faction member
	stranger int64 // a player outside the faction
	ghost    int64 // an offline player outside the faction, with the newest kill
	deeloID  string
	mateID   string
	faction  int64
	base     int64
}

func newLiveMapWorld(t *testing.T) *liveMapWorld {
	t.Helper()
	w := newStandoutWorld(t)
	pool := w.a.DB.Pool
	w.a.LiveMap = repository.NewLiveMapRepository(pool)
	w.a.Zones = repository.NewZoneRepository(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	lw := &liveMapWorld{clientAdminWorld: w, now: now, admFile: fmt.Sprintf("DayZServer_PS4_x64_%d.ADM", standoutSeq.Add(1))}
	lw.deelo, lw.mate, lw.stranger, lw.ghost = w.player("Deelo"), w.player("Mate"), w.player("Stranger"), w.player("Ghost")

	// The server booted an hour ago (the recorded ADM session), its clock runs at UTC+2.
	if _, err := pool.Exec(ctx, `INSERT INTO server_adm_sessions(server_id, guild_id, adm_file, session_local_start, selected_at) VALUES($1,$2,$3,$4::timestamp,$5)
ON CONFLICT (server_id) DO UPDATE SET adm_file=EXCLUDED.adm_file, session_local_start=EXCLUDED.session_local_start, selected_at=EXCLUDED.selected_at, ended_at=NULL`,
		w.serverID, w.guildID, lw.admFile, now.Add(-time.Hour).Add(2*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO live_sync_server_clock(server_id, guild_id, utc_offset_minutes, learned_from) VALUES($1,$2,120,'test') ON CONFLICT (server_id) DO UPDATE SET utc_offset_minutes=120`,
		w.serverID, w.guildID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO shop_delivery_settings(installation_id, organization_id, map_key) VALUES($1,$2,'chernarusplus') ON CONFLICT (installation_id) DO UPDATE SET map_key='chernarusplus'`,
		w.f.InstallationID, w.f.OrgID); err != nil {
		t.Fatal(err)
	}

	// Two verified web users: Deelo leads a hub faction Mate belongs to; Stranger is in none.
	deelo := syncUser(t, w.a, fmt.Sprintf("lm-deelo-%d", standoutSeq.Add(1)), "Deelo")
	mate := syncUser(t, w.a, fmt.Sprintf("lm-mate-%d", standoutSeq.Add(1)), "Mate")
	lw.deeloID, lw.mateID = deelo.DiscordUserID, mate.DiscordUserID
	for _, l := range []struct {
		player int64
		user   UserSummary
	}{{lw.deelo, deelo}, {lw.mate, mate}} {
		if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,'VERIFIED')`, w.guildID, l.player, l.user.DiscordUserID); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO hub_factions(organization_id, installation_id, game_server_id, name, tag, slug, primary_color, secondary_color, created_by_user_id)
VALUES($1,$2,$3,$4,'RD',$5,'#ff1726','#f0c65e',$6) RETURNING id`, w.f.OrgID, w.f.InstallationID, w.serverID, fmt.Sprintf("Red Dawn %d", standoutSeq.Add(1)),
		fmt.Sprintf("red-dawn-%d", standoutSeq.Add(1)), deelo.ID).Scan(&lw.faction); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		user   UserSummary
		player int64
		role   string
	}{{deelo, lw.deelo, "LEADER"}, {mate, lw.mate, "MEMBER"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO hub_faction_members(faction_id, installation_id, user_id, player_id, role_key) VALUES($1,$2,$3,$4,$5)`,
			lw.faction, w.f.InstallationID, m.user.ID, m.player, m.role); err != nil {
			t.Fatal(err)
		}
	}
	// Deelo and Stranger are connected; Mate logged off a while ago.
	w.connectPlayer(lw.deelo, now.Add(-50*time.Minute))
	w.connectPlayer(lw.stranger, now.Add(-40*time.Minute))
	w.connectPlayer(lw.mate, now.Add(-45*time.Minute))
	w.disconnectPlayer(lw.mate, now.Add(-20*time.Minute))

	// Positions: player-list rows for everyone, a trail for Deelo, and a stamped line that sets the
	// server clock.
	w.seedLocationEvent(lw.deelo, 4600, 10250, "PLAYER_LIST", now.Add(-12*time.Minute))
	w.seedLocationEvent(lw.deelo, 4620, 10270, "PLAYER_LIST", now.Add(-7*time.Minute))
	w.seedLocationEvent(lw.deelo, 4650, 10300, "PLAYER_LIST", now.Add(-2*time.Minute))
	w.seedLocationEvent(lw.mate, 5300, 9700, "PLAYER_LIST", now.Add(-25*time.Minute))
	w.seedLocationEvent(lw.stranger, 9000, 9000, "PLAYER_LIST", now.Add(-time.Minute))
	local := now.Add(-90 * time.Second).Add(2 * time.Hour) // zone-less server-local stamp
	if _, err := w.a.Locations.InsertLocationEvents(ctx, []repository.LocationEventInput{{GuildID: w.guildID, ServerID: w.serverID, PlayerID: lw.stranger, Gamertag: "Stranger",
		X: 9010, Z: 9010, EventType: "HIT", ObservedAt: now.Add(-90 * time.Second), SourceFile: lw.admFile, SourceOffset: 4096, SourceLocalTime: &local}}); err != nil {
		t.Fatal(err)
	}
	// Kills in time order (so ids follow time): an old one, one far away, one ten minutes ago and
	// one thirty seconds ago - each with the killer's position. Plus two hits in the busy cell.
	w.killAt(lw.deelo, lw.stranger, now.Add(-50*time.Minute), 4612, 10390)
	w.killAt(lw.mate, lw.stranger, now.Add(-20*time.Minute), 1200, 2200)
	w.killAt(lw.stranger, lw.mate, now.Add(-10*time.Minute), 4810, 10330)
	w.killAt(lw.ghost, lw.stranger, now.Add(-30*time.Second), 4700, 10350)
	w.seedLocationEvent(lw.deelo, 4700, 10400, "HIT", now.Add(-5*time.Minute))
	w.seedLocationEvent(lw.stranger, 4720, 10410, "HIT", now.Add(-5*time.Minute))

	// Mate's base, with a raid on it a few minutes ago.
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases(installation_id, guild_id, server_id, owner_player_id, map_key, name, center_x, center_z, radius)
VALUES($1,$2,$3,$4,'chernarusplus','Kabanino ridge',5300,9700,80) RETURNING id`, w.f.InstallationID, w.guildID, w.serverID, lw.mate).Scan(&lw.base); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO base_raid_alerts(installation_id, guild_id, server_id, base_id, raider_player_id, raider_name, part, target, tool, created_at)
VALUES($1,$2,$3,$4,$5,'Stranger','Wall','Gate','Hatchet',$6)`, w.f.InstallationID, w.guildID, w.serverID, lw.base, lw.stranger, now.Add(-4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return lw
}

func (lw *liveMapWorld) publicMap(t *testing.T, query string, installationID int64) (*httptest.ResponseRecorder, liveMapPublicDTO) {
	t.Helper()
	rr := lw.call(lw.a.handlePublicLiveMap, http.MethodGet, "/api/saas/network/servers/x/map"+query, "", nil,
		map[string]string{"installationID": strconv.FormatInt(installationID, 10)})
	var out liveMapPublicDTO
	if rr.Code == http.StatusOK {
		out = decodeBody[liveMapPublicDTO](t, rr)
	}
	return rr, out
}

func (lw *liveMapWorld) factionMap(t *testing.T, actor string) (*httptest.ResponseRecorder, liveMapFactionLayerDTO) {
	t.Helper()
	rr := lw.call(lw.a.handlePlayerFactionMap, http.MethodGet, "/api/saas/player/servers/x/map/faction", actor, nil,
		map[string]string{"installationID": strconv.FormatInt(lw.f.InstallationID, 10)})
	var out liveMapFactionLayerDTO
	if rr.Code == http.StatusOK {
		out = decodeBody[liveMapFactionLayerDTO](t, rr)
	}
	return rr, out
}

// withLiveMapNitrado serves the Nitrado reads the live map makes: the services listing the
// connect step validates, the gameserver facts with the in-game clock settings, and the
// scheduled tasks with a restart due in two hours.
func (lw *liveMapWorld) withLiveMapNitrado(t *testing.T) {
	t.Helper()
	next := lw.now.Add(2 * time.Hour).Format("2006-01-02 15:04:05")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/services":
			_, _ = w.Write([]byte(psServiceJSON))
		case strings.HasSuffix(r.URL.Path, "/gameservers"):
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"gameserver":{"service_id":111111,"status":"started","slots":32,"game":"dayzps",
				"settings":{"config":{"serverTime":"2024/6/1/6/00","serverTimeAcceleration":"12","serverNightTimeAcceleration":"1"}}}}}`)
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"tasks":[{"id":1,"action_method":"backup","next_run":%q},{"id":2,"action_method":"restart","next_run":%q}]}}`,
				lw.now.Add(time.Hour).Format("2006-01-02 15:04:05"), next)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	lw.a.saasNitradoClientFactory = func(token string) *nitrado.Client { return nitrado.NewClient(srv.URL, token, nil) }
	connectNitrado(t, lw.a, lw.f.OrgID, lw.f.OwnerDiscordID)
	if _, err := lw.a.DB.Pool.Exec(context.Background(), `UPDATE game_servers SET provider_service_id='111111' WHERE id=$1`, lw.serverID); err != nil {
		t.Fatal(err)
	}
}

func TestPublicLiveMapShowsKillsAndPressureBehindTheDelay(t *testing.T) {
	lw := newLiveMapWorld(t)
	lw.withLiveMapNitrado(t)
	ctx := context.Background()

	// By default the map follows the network listing, and this server is not listed.
	if rr, _ := lw.publicMap(t, "", lw.f.InstallationID); rr.Code != http.StatusNotFound {
		t.Fatalf("an unlisted server's map is public by default: %d %s", rr.Code, rr.Body.String())
	}
	if code, _ := lw.put(lw.a.handlePutLiveMapSettings, "/features/live-map", lw.f.OwnerDiscordID, liveMapSettingsDTO{Visibility: "PUBLIC", DelaySeconds: 0, FactionLayer: true}); code != http.StatusOK {
		t.Fatalf("make public: %d", code)
	}
	rr, m := lw.publicMap(t, "?window=60", lw.f.InstallationID)
	if rr.Code != http.StatusOK {
		t.Fatalf("public map: %d %s", rr.Code, rr.Body.String())
	}
	if m.InstallationID != lw.f.InstallationID || m.Platform != "PLAYSTATION" || !m.Public || m.Listed || m.DelaySeconds != 0 || m.Map.Key != "chernarusplus" || m.Map.Size != 15360 || m.Map.Guessed {
		t.Fatalf("header = %+v", m)
	}
	if m.PlayersOnline != 2 || m.LastActivityAt == nil {
		t.Fatalf("occupancy = %d / %v", m.PlayersOnline, m.LastActivityAt)
	}
	// Kills: newest first, within the hour, with where the victim died - never the living killer's
	// position - and the cursor is the highest id.
	if len(m.Kills) != 4 || m.Kills[0].KillerName != "Ghost" || m.Kills[0].VictimName != "Stranger" || m.Kills[1].KillerName != "Stranger" || m.Kills[2].KillerName != "Mate" || m.Kills[3].KillerName != "Deelo" ||
		m.Kills[0].KillerX != nil || m.Kills[0].KillerZ != nil || m.Kills[0].Weapon != "KA-M" ||
		m.Kills[0].Headshot || m.Kills[0].Longshot || m.LastKillID != maxKillID(m.Kills) {
		t.Fatalf("kills = %+v lastKillId=%d", m.Kills, m.LastKillID)
	}
	// Pressure: the busy cell around (4750, 10250) holds the two recent kills plus two hits. The
	// far kill and the stranger's hit are one event each, too few to show (one event would place a
	// single player); the kill fifty minutes ago is outside the thirty-minute window.
	if m.Pressure.Resolution != 500 || m.Pressure.WindowMinutes != 30 || len(m.Pressure.Cells) != 1 || m.Pressure.Cells[0].CenterX != 4750 || m.Pressure.Cells[0].CenterZ != 10250 ||
		m.Pressure.Cells[0].Count != 4 || m.Pressure.Cells[0].Intensity != 1 {
		t.Fatalf("pressure = %+v", m.Pressure)
	}
	// Clock: the stamped line (server-local 90 s ago) advanced by the real seconds since, the
	// learned offset, the boot, the Nitrado restart and the in-game estimate (06:00 at boot, 12x,
	// one real hour later = 18:00).
	if m.Clock.ServerLocalTime == nil || m.Clock.UTCOffsetMinutes == nil || *m.Clock.UTCOffsetMinutes != 120 || m.Clock.BootedAt == nil || m.Clock.NextRestartAt == nil || m.Clock.InGame == nil {
		t.Fatalf("clock = %+v", m.Clock)
	}
	if got, err := time.Parse("15:04:05", *m.Clock.ServerLocalTime); err != nil || secondsOfDayApart(got, time.Now().UTC().Add(2*time.Hour)) > 120 {
		t.Fatalf("serverLocalTime = %s, want about %s (%v)", *m.Clock.ServerLocalTime, time.Now().UTC().Add(2*time.Hour).Format("15:04:05"), err)
	}
	if !m.Clock.InGame.Estimated || m.Clock.InGame.Time != "18:00" && m.Clock.InGame.Time != "18:01" {
		t.Fatalf("inGame = %+v", m.Clock.InGame)
	}
	if want := lw.now.Add(2 * time.Hour).Format(time.RFC3339); *m.Clock.NextRestartAt != want {
		t.Fatalf("nextRestartAt = %s, want %s", *m.Clock.NextRestartAt, want)
	}
	if m.HotZone != nil {
		t.Fatalf("a hot zone appeared from nowhere: %+v", m.HotZone)
	}

	// sinceKillId sends what the client does not have, plus the most recent few again (ids follow
	// ingest order, so a late kill can have a lower id; the client dedupes).
	if _, since := lw.publicMap(t, "?sinceKillId="+strconv.FormatInt(m.LastKillID, 10), lw.f.InstallationID); len(since.Kills) != len(m.Kills) || since.Kills[0].ID != m.Kills[0].ID {
		t.Fatalf("since = %+v", since.Kills)
	}
	for _, q := range []string{"?window=4", "?window=181", "?sinceKillId=-1", "?sinceKillId=abc"} {
		if rr, _ := lw.publicMap(t, q, lw.f.InstallationID); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", q, rr.Code)
		}
	}
	if rr, _ := lw.publicMap(t, "", 999999999); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown installation: %d", rr.Code)
	}

	// A two-minute delay hides the kill thirty seconds ago (the cache is dropped by the save).
	if code, resp := lw.put(lw.a.handlePutLiveMapSettings, "/features/live-map", lw.f.OwnerDiscordID, liveMapSettingsDTO{Visibility: "PUBLIC", DelaySeconds: 120, FactionLayer: true}); code != http.StatusOK ||
		resp["liveMap"].(map[string]any)["delaySeconds"] != float64(120) {
		t.Fatalf("save live map settings: %d %v", code, resp)
	}
	_, delayed := lw.publicMap(t, "", lw.f.InstallationID)
	if delayed.DelaySeconds != 120 || len(delayed.Kills) != 3 || delayed.Kills[0].KillerName != "Stranger" || delayed.LastKillID != maxKillID(delayed.Kills) {
		t.Fatalf("delayed kills = %+v", delayed.Kills)
	}
	for _, k := range delayed.Kills {
		if at, _ := time.Parse(time.RFC3339, k.At); at.After(lw.now.Add(-120 * time.Second)) {
			t.Fatalf("a kill newer than the delay leaked: %+v", k)
		}
	}
	if code, _ := lw.put(lw.a.handlePutLiveMapSettings, "/features/live-map", lw.f.OwnerDiscordID, liveMapSettingsDTO{Visibility: "PUBLIC", DelaySeconds: 3601}); code != http.StatusBadRequest {
		t.Fatalf("delay above an hour accepted: %d", code)
	}
	features := decodeBody[featureSettingsDTO](t, lw.call(lw.a.handleGetFeatureSettings, http.MethodGet, lw.path("/features"), lw.f.OwnerDiscordID, nil, nil))
	if !features.LiveMap.Public || features.LiveMap.Visibility != "PUBLIC" || features.LiveMap.DelaySeconds != 120 || !features.LiveMap.FactionLayer {
		t.Fatalf("features.liveMap = %+v", features.LiveMap)
	}
	var audits int
	if err := lw.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='LIVE_MAP_SETTINGS_UPDATED'`, lw.f.InstallationID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("settings audit rows = %d err=%v", audits, err)
	}

	// Switched off, the installation has no public map at all.
	if code, _ := lw.put(lw.a.handlePutLiveMapSettings, "/features/live-map", lw.f.OwnerDiscordID, liveMapSettingsDTO{Visibility: "OFF", FactionLayer: true}); code != http.StatusOK {
		t.Fatalf("switch off: %d", code)
	}
	if rr, _ := lw.publicMap(t, "", lw.f.InstallationID); rr.Code != http.StatusNotFound {
		t.Fatalf("a private map answered: %d %s", rr.Code, rr.Body.String())
	}
	if code, _ := lw.put(lw.a.handlePutLiveMapSettings, "/features/live-map", lw.f.OwnerDiscordID, liveMapSettingsDTO{Visibility: "BOGUS", FactionLayer: true}); code != http.StatusBadRequest {
		t.Fatalf("unknown visibility accepted: %d", code)
	}

	// Following the listing: once the server is listed, the map opens and says so.
	if code, _ := lw.put(lw.a.handlePutLiveMapSettings, "/features/live-map", lw.f.OwnerDiscordID, liveMapSettingsDTO{Visibility: "LISTED", DelaySeconds: 120, FactionLayer: true}); code != http.StatusOK {
		t.Fatalf("follow listing: %d", code)
	}
	if _, err := lw.a.DB.Pool.Exec(ctx, `UPDATE installation_feature_settings SET network_listed=TRUE WHERE installation_id=$1`, lw.f.InstallationID); err != nil {
		t.Fatal(err)
	}
	lw.a.invalidateLiveMapCache()
	if rr, listed := lw.publicMap(t, "", lw.f.InstallationID); rr.Code != http.StatusOK || !listed.Listed {
		t.Fatalf("listed server's map: %d %s", rr.Code, rr.Body.String())
	}

	// No open game-log session (the boot ended, or none was recorded): the map still answers,
	// without a boot time.
	if _, err := lw.a.DB.Pool.Exec(ctx, `UPDATE server_adm_sessions SET ended_at=NOW() WHERE server_id=$1`, lw.serverID); err != nil {
		t.Fatal(err)
	}
	lw.a.invalidateLiveMapCache()
	if rr, noSession := lw.publicMap(t, "", lw.f.InstallationID); rr.Code != http.StatusOK || noSession.Clock.BootedAt != nil {
		t.Fatalf("map without a session: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := lw.a.DB.Pool.Exec(ctx, `DELETE FROM server_adm_sessions WHERE server_id=$1`, lw.serverID); err != nil {
		t.Fatal(err)
	}
	lw.a.invalidateLiveMapCache()
	if rr, _ := lw.publicMap(t, "", lw.f.InstallationID); rr.Code != http.StatusOK {
		t.Fatalf("map with no session ever recorded: %d %s", rr.Code, rr.Body.String())
	}
	// Bearer only: no service secret, no map.
	req := withPathValues(saasRequest(http.MethodGet, "/x", nil), map[string]string{"installationID": strconv.FormatInt(lw.f.InstallationID, 10)})
	req.Header.Del("Authorization")
	rec := httptest.NewRecorder()
	lw.a.handlePublicLiveMap(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without service auth: %d", rec.Code)
	}
}

func TestFactionMapShowsOnlyOwnMembers(t *testing.T) {
	lw := newLiveMapWorld(t)
	rr, m := lw.factionMap(t, lw.deeloID)
	if rr.Code != http.StatusOK {
		t.Fatalf("faction map: %d %s", rr.Code, rr.Body.String())
	}
	if !m.Enabled || m.Faction == nil || m.Faction.ID != lw.faction || m.Faction.Tag != "RD" || m.Faction.PrimaryColor == nil || *m.Faction.PrimaryColor != "#ff1726" ||
		m.Self.PlayerID != lw.deelo || m.Self.Gamertag != "Deelo" {
		t.Fatalf("header = %+v", m)
	}
	if len(m.Members) != 2 {
		t.Fatalf("members = %+v", m.Members)
	}
	byName := map[string]liveMapMemberDTO{}
	for _, mem := range m.Members {
		byName[mem.Gamertag] = mem
		if mem.PlayerID == lw.stranger {
			t.Fatalf("a non-member's position left the faction route: %+v", mem)
		}
	}
	deelo, mate := byName["Deelo"], byName["Mate"]
	if !deelo.IsSelf || !deelo.Online || deelo.LastKnown == nil || deelo.LastKnown.X != 4650 || deelo.LastKnown.Z != 10300 || deelo.LastKnown.EventType != "PLAYER_LIST" ||
		deelo.LastKnown.AgeSeconds < 110 || deelo.LastKnown.AgeSeconds > 180 {
		t.Fatalf("deelo = %+v lastKnown=%+v", deelo, deelo.LastKnown)
	}
	// The trail is the positions before the latest one, oldest first: the kill fifty minutes ago,
	// two player-list rows and the hit five minutes ago.
	if len(deelo.Trail) != 4 || deelo.Trail[0].X != 4612 || deelo.Trail[1].X != 4600 || deelo.Trail[2].X != 4620 || deelo.Trail[3].X != 4700 {
		t.Fatalf("deelo trail = %+v", deelo.Trail)
	}
	// Mate's newest sample is the kill twenty minutes ago; the player-list row before it is the trail.
	if mate.IsSelf || mate.Online || mate.LastKnown == nil || mate.LastKnown.X != 1200 || mate.LastKnown.EventType != "KILL" || len(mate.Trail) != 1 || mate.Trail[0].X != 5300 {
		t.Fatalf("mate = %+v lastKnown=%+v", mate, mate.LastKnown)
	}
	if len(m.Bases) != 1 || m.Bases[0].ID != lw.base || m.Bases[0].Name != "Kabanino ridge" || m.Bases[0].OwnerPlayerID != lw.mate || m.Bases[0].Radius != 80 {
		t.Fatalf("bases = %+v", m.Bases)
	}
	if len(m.Alerts) != 1 || m.Alerts[0].BaseID != lw.base || m.Alerts[0].Kind != "RAID" || m.Alerts[0].Part != "Wall" || m.Alerts[0].Target != "Gate" || m.Alerts[0].RaiderName != "Stranger" || m.Alerts[0].BaseName != "Kabanino ridge" {
		t.Fatalf("alerts = %+v", m.Alerts)
	}
	// The raw body never carries the stranger's coordinates or identity as a member.
	if body := rr.Body.String(); strings.Contains(body, `"x":9000`) || strings.Contains(body, `"x":9010`) || strings.Contains(body, `"gamertag":"Stranger"`) || strings.Contains(body, `"gamertag":"Ghost"`) {
		t.Fatalf("stranger data in the faction response: %s", body)
	}

	// A member whose DayZ link was revoked no longer shows: the membership row keeps their old
	// player id, but only a verified link counts.
	if _, err := lw.a.DB.Pool.Exec(context.Background(), `UPDATE player_links SET status='REVOKED' WHERE guild_id=$1 AND player_id=$2`, lw.guildID, lw.mate); err != nil {
		t.Fatal(err)
	}
	if rr, revoked := lw.factionMap(t, lw.deeloID); rr.Code != http.StatusOK || len(revoked.Members) != 1 || revoked.Members[0].PlayerID != lw.deelo {
		t.Fatalf("revoked member still shown: %d %+v", rr.Code, revoked.Members)
	}
	if _, err := lw.a.DB.Pool.Exec(context.Background(), `UPDATE player_links SET status='VERIFIED' WHERE guild_id=$1 AND player_id=$2`, lw.guildID, lw.mate); err != nil {
		t.Fatal(err)
	}

	// A verified player in no faction sees only themselves.
	loner := syncUser(t, lw.a, fmt.Sprintf("lm-loner-%d", standoutSeq.Add(1)), "Stranger")
	if _, err := lw.a.DB.Pool.Exec(context.Background(), `INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,'VERIFIED')`, lw.guildID, lw.stranger, loner.DiscordUserID); err != nil {
		t.Fatal(err)
	}
	rr, solo := lw.factionMap(t, loner.DiscordUserID)
	if rr.Code != http.StatusOK || !solo.Enabled || solo.Faction != nil || len(solo.Members) != 1 || solo.Members[0].PlayerID != lw.stranger || !solo.Members[0].IsSelf ||
		solo.Members[0].LastKnown == nil || solo.Members[0].LastKnown.X != 9000 || len(solo.Bases) != 0 || len(solo.Alerts) != 0 {
		t.Fatalf("loner = %d %+v", rr.Code, solo)
	}
	if body := rr.Body.String(); strings.Contains(body, "Deelo") || strings.Contains(body, "4650") {
		t.Fatalf("faction data leaked to a non-member: %s", body)
	}

	// Not linked at all: the same 404/identity errors as every player route.
	outsider := syncUser(t, lw.a, fmt.Sprintf("lm-outsider-%d", standoutSeq.Add(1)), "Outsider")
	if rr, _ := lw.factionMap(t, outsider.DiscordUserID); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), codePlayerIdentityRequired) {
		t.Fatalf("unlinked user: %d %s", rr.Code, rr.Body.String())
	}

	// The layer switched off: enabled=false and nothing else.
	if code, _ := lw.put(lw.a.handlePutLiveMapSettings, "/features/live-map", lw.f.OwnerDiscordID, liveMapSettingsDTO{Visibility: "LISTED", DelaySeconds: 120, FactionLayer: false}); code != http.StatusOK {
		t.Fatalf("switch the layer off: %d", code)
	}
	rr, off := lw.factionMap(t, lw.deeloID)
	if rr.Code != http.StatusOK || off.Enabled || off.Faction != nil || len(off.Members) != 0 || len(off.Bases) != 0 || len(off.Alerts) != 0 || off.Self.PlayerID != lw.deelo {
		t.Fatalf("layer off = %d %+v", rr.Code, off)
	}
}

func TestStaffLiveMapAndHistory(t *testing.T) {
	lw := newLiveMapWorld(t)
	ctx := context.Background()
	owner := lw.f.OwnerDiscordID

	// A zone with an active intrusion by the stranger.
	ownerUser, err := lw.a.SaaSUsers.GetByDiscordID(ctx, owner)
	if err != nil || ownerUser == nil {
		t.Fatalf("owner user: %v %v", ownerUser, err)
	}
	zone, err := lw.a.Zones.CreateZone(ctx, lw.f.InstallationID, lw.guildID, lw.serverID, "Airfield", "UAV", 9000, 9000, 300, nil, 60, ownerUser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lw.a.DB.Pool.Exec(ctx, `INSERT INTO zone_intrusions(zone_id, installation_id, guild_id, server_id, player_id, gamertag, status, entered_at)
VALUES($1,$2,$3,$4,$5,'Stranger','ACTIVE',$6)`, zone.ID, lw.f.InstallationID, lw.guildID, lw.serverID, lw.stranger, lw.now.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rr := lw.call(lw.a.handleAdminLiveMap, http.MethodGet, lw.path("/map/live"), owner, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("staff live: %d %s", rr.Code, rr.Body.String())
	}
	live := decodeBody[liveMapStaffLiveDTO](t, rr)
	if len(live.Players) != 2 || live.Players[0].Gamertag != "Deelo" || live.Players[1].Gamertag != "Stranger" {
		t.Fatalf("players = %+v", live.Players)
	}
	deelo, stranger := live.Players[0], live.Players[1]
	if !deelo.Online || deelo.FactionTag == nil || *deelo.FactionTag != "RD" || deelo.LastKnown == nil || deelo.LastKnown.X != 4650 ||
		stranger.FactionTag != nil || stranger.LastKnown == nil || stranger.LastKnown.X != 9000 || stranger.LastKnown.EventType != "PLAYER_LIST" {
		t.Fatalf("deelo = %+v stranger = %+v", deelo, stranger)
	}
	if len(live.Zones) != 1 || live.Zones[0].ID != zone.ID || len(live.Intrusions) != 1 || live.Intrusions[0].ZoneID != zone.ID || live.Intrusions[0].PlayerID != lw.stranger ||
		live.Intrusions[0].CenterX != 9000 || live.Intrusions[0].CenterZ != 9000 || live.Intrusions[0].ZoneName != "Airfield" {
		t.Fatalf("zones = %+v intrusions = %+v", live.Zones, live.Intrusions)
	}
	// Polling again within ten minutes writes no second audit row.
	for i := 0; i < 3; i++ {
		if rr := lw.call(lw.a.handleAdminLiveMap, http.MethodGet, lw.path("/map/live"), owner, nil, nil); rr.Code != http.StatusOK {
			t.Fatalf("poll %d: %d", i, rr.Code)
		}
	}
	var audits int
	if err := lw.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='LIVE_MAP_VIEWED'`, lw.f.InstallationID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("LIVE_MAP_VIEWED audit rows = %d err=%v", audits, err)
	}
	// A moderator may not see positions.
	mod := syncUser(t, lw.a, fmt.Sprintf("lm-mod-%d", standoutSeq.Add(1)), "Mod")
	lw.mapRole(mod.DiscordUserID, "lm-role-mod", "MODERATOR")
	if rr := lw.call(lw.a.handleAdminLiveMap, http.MethodGet, lw.path("/map/live"), mod.DiscordUserID, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("moderator saw the live map: %d", rr.Code)
	}

	// History: the last hour by default, with every player's track and the kills.
	rr = lw.call(lw.a.handleAdminMapHistory, http.MethodGet, lw.path("/map/history"), owner, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("history: %d %s", rr.Code, rr.Body.String())
	}
	h := decodeBody[liveMapHistoryDTO](t, rr)
	if h.Truncated || h.Map.Key != "chernarusplus" || len(h.Kills) != 4 || h.Kills[0].KillerID != lw.ghost || h.Kills[0].VictimID != lw.stranger || h.Kills[0].KillerName != "Ghost" || h.Kills[3].KillerID != lw.deelo {
		t.Fatalf("history = truncated=%v map=%+v kills=%+v", h.Truncated, h.Map, h.Kills)
	}
	tracks := map[string]liveMapHistoryTrackDTO{}
	for _, tr := range h.Tracks {
		tracks[tr.Gamertag] = tr
	}
	if len(h.Tracks) != 4 || len(tracks["Deelo"].Points) != 5 || tracks["Deelo"].Points[0].X != 4612 || tracks["Deelo"].Points[0].Type != "KILL" || tracks["Deelo"].Points[4].Type != "PLAYER_LIST" ||
		tracks["Deelo"].PlayerID != lw.deelo || len(tracks["Mate"].Points) != 2 || len(tracks["Stranger"].Points) != 4 || len(tracks["Ghost"].Points) != 1 {
		t.Fatalf("tracks = %+v", h.Tracks)
	}
	for _, tr := range h.Tracks {
		for i := 1; i < len(tr.Points); i++ {
			if tr.Points[i].T < tr.Points[i-1].T {
				t.Fatalf("%s track is not oldest first: %+v", tr.Gamertag, tr.Points)
			}
		}
	}
	if len(h.Intrusions) != 1 || h.Intrusions[0].ZoneID != zone.ID || h.Intrusions[0].CenterX != 9000 || h.Intrusions[0].ZoneName != "Airfield" {
		t.Fatalf("history intrusions = %+v", h.Intrusions)
	}
	if err := lw.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='LIVE_MAP_HISTORY_VIEWED'`, lw.f.InstallationID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("LIVE_MAP_HISTORY_VIEWED audit rows = %d err=%v", audits, err)
	}

	// An explicit window, and the window rules.
	from, to := lw.now.Add(-15*time.Minute).Format(time.RFC3339), lw.now.Format(time.RFC3339)
	narrow := decodeBody[liveMapHistoryDTO](t, lw.call(lw.a.handleAdminMapHistory, http.MethodGet, lw.path("/map/history?from="+from+"&to="+to), owner, nil, nil))
	if len(narrow.Kills) != 2 || narrow.From != from || narrow.To != to {
		t.Fatalf("narrow = %+v", narrow)
	}
	for _, q := range []string{"?from=" + lw.now.Add(-7*time.Hour).Format(time.RFC3339), "?from=" + to + "&to=" + from, "?from=yesterday"} {
		if rr := lw.call(lw.a.handleAdminMapHistory, http.MethodGet, lw.path("/map/history"+q), owner, nil, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", q, rr.Code)
		}
	}

	// Truncation: more samples than the cap keep the newest and say so.
	if _, err := lw.a.DB.Pool.Exec(ctx, `INSERT INTO player_location_events(guild_id, server_id, player_id, gamertag, x, z, event_type, observed_at)
SELECT $1, $2, $3, 'Stranger', 100 + (i % 50), 100, 'OTHER_ADM', $4::timestamptz + (i * interval '10 milliseconds') FROM generate_series(0, $5) AS i`,
		lw.guildID, lw.serverID, lw.stranger, lw.now.Add(-30*time.Minute), liveMapHistoryPoints+49); err != nil {
		t.Fatal(err)
	}
	full := decodeBody[liveMapHistoryDTO](t, lw.call(lw.a.handleAdminMapHistory, http.MethodGet, lw.path("/map/history"), owner, nil, nil))
	points := 0
	for _, tr := range full.Tracks {
		points += len(tr.Points)
	}
	if !full.Truncated || points != liveMapHistoryPoints {
		t.Fatalf("truncated=%v points=%d", full.Truncated, points)
	}
	// Newest-first truncation drops the 62 oldest samples: Deelo's kill fifty minutes ago and the
	// first 61 of the flood. Deelo's latest sample (two minutes ago) survives.
	for _, tr := range full.Tracks {
		switch tr.Gamertag {
		case "Deelo":
			if len(tr.Points) != 4 || tr.Points[0].Type != "PLAYER_LIST" || tr.Points[len(tr.Points)-1].X != 4650 {
				t.Fatalf("deelo after truncation: %+v", tr.Points)
			}
		case "Stranger":
			if len(tr.Points) != 4+liveMapHistoryPoints+50-61 {
				t.Fatalf("stranger kept %d samples", len(tr.Points))
			}
		}
	}
	raw, _ := json.Marshal(full.Map)
	if string(raw) != `{"key":"chernarusplus","name":"Chernarus","size":15360,"guessed":false}` {
		t.Fatalf("map json = %s", raw)
	}
}

// secondsOfDayApart is the distance between two clock readings, modulo midnight.
func secondsOfDayApart(a, b time.Time) int {
	sa := a.Hour()*3600 + a.Minute()*60 + a.Second()
	sb := b.Hour()*3600 + b.Minute()*60 + b.Second()
	d := sa - sb
	if d < 0 {
		d = -d
	}
	if d > 43200 {
		d = 86400 - d
	}
	return d
}
