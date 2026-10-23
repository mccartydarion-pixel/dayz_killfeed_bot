//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// /server panel uses the website's staff levels, Nitrado credential, rate limits and audit log.
func TestServerPanelActsByStaffLevel(t *testing.T) {
	w := newClientAdminWorld(t)
	calls := withFakeNitradoActions(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)
	w.a.ServerPanels = repository.NewServerPanelRepository(w.a.DB.Pool)
	ctx := context.Background()
	b := newServerPanelBackend(w.a)

	servers, err := b.PanelServers(ctx, w.f.DiscordGuildID)
	if err != nil || len(servers) != 1 || servers[0].InstallationID != w.f.InstallationID {
		t.Fatalf("servers: %+v %v", servers, err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO shop_delivery_settings(installation_id,organization_id,map_key) VALUES($1,$2,'enoch')`, w.f.InstallationID, w.f.OrgID); err != nil {
		t.Fatal(err)
	}
	if servers, err = b.PanelServers(ctx, w.f.DiscordGuildID); err != nil || servers[0].MapName != "Livonia" {
		t.Fatalf("the panel names the server's map: %+v %v", servers, err)
	}
	if other, err := b.PanelServers(ctx, "no-such-guild"); err != nil || len(other) != 0 {
		t.Fatalf("another Discord server's servers: %+v %v", other, err)
	}

	inst := w.f.InstallationID
	modRole := fmt.Sprintf("panel-mod-%d", time.Now().UnixNano())
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO installation_role_permissions(installation_id,discord_role_id,permission_level) VALUES($1,$2,'MODERATOR')`, inst, modRole); err != nil {
		t.Fatal(err)
	}
	owner := discord.PanelCaller{DiscordGuildID: w.f.DiscordGuildID, DiscordUserID: w.f.OwnerDiscordID}
	mod := discord.PanelCaller{DiscordGuildID: w.f.DiscordGuildID, DiscordUserID: "panel-mod-user", RoleIDs: []string{modRole, "unmapped"}}
	nobody := discord.PanelCaller{DiscordGuildID: w.f.DiscordGuildID, DiscordUserID: "panel-nobody", RoleIDs: []string{"unmapped"}}
	discordAdmin := discord.PanelCaller{DiscordGuildID: w.f.DiscordGuildID, DiscordUserID: "panel-discord-admin", DiscordAdmin: true}
	for _, c := range []struct {
		who  discord.PanelCaller
		want permissions.Level
	}{{owner, permissions.LevelOwner}, {mod, permissions.LevelModerator}, {nobody, permissions.LevelNone}, {discordAdmin, permissions.LevelAdministrator}} {
		if got, err := b.PanelLevel(ctx, c.who, inst); err != nil || got != c.want {
			t.Errorf("%s: level %v, want %v (%v)", c.who.DiscordUserID, got, c.want, err)
		}
	}
	elsewhere := mod
	elsewhere.DiscordGuildID = "no-such-guild"
	if _, err := b.PanelLevel(ctx, elsewhere, inst); !errors.Is(err, discord.ErrPanelServerNotFound) {
		t.Fatalf("an installation of another Discord server: %v", err)
	}

	*calls = nil
	if err := b.PanelAct(ctx, mod, inst, discord.PanelStop, "testing"); !errors.Is(err, discord.ErrPanelForbidden) {
		t.Fatalf("a Moderator stopping the server: %v", err)
	}
	if err := b.PanelAct(ctx, nobody, inst, discord.PanelRestart, "testing"); !errors.Is(err, discord.ErrPanelForbidden) {
		t.Fatalf("a member without a staff role restarting: %v", err)
	}
	if err := b.PanelAct(ctx, mod, inst, discord.PanelRestart, "lag after airdrop"); err != nil {
		t.Fatal(err)
	}
	restart := "POST /services/" + w.providerServiceID + "/gameservers/restart"
	if len(*calls) != 1 || (*calls)[0] != restart {
		t.Fatalf("nitrado calls: %v", *calls)
	}
	if err := b.PanelAct(ctx, mod, inst, discord.PanelRestart, "again"); !errors.Is(err, discord.ErrPanelRateLimited) {
		t.Fatalf("a second restart within 5 minutes: %v", err)
	}
	if err := b.PanelAct(ctx, owner, inst, discord.PanelStop, "maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := b.PanelAct(ctx, discordAdmin, inst, discord.PanelStart, ""); err != nil {
		t.Fatal(err)
	}
	if got := (*calls)[len(*calls)-2:]; got[0] != "POST /services/"+w.providerServiceID+"/gameservers/stop" || got[1] != restart {
		t.Fatalf("stop then start: %v", *calls)
	}

	var action, actor, reason, result, source string
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT action,actor_discord_id,COALESCE(reason,''),result,after_state->>'source' FROM admin_audit_log
WHERE installation_id=$1 AND action='SERVER_RESTART' ORDER BY id DESC LIMIT 1`, inst).Scan(&action, &actor, &reason, &result, &source); err != nil {
		t.Fatal(err)
	}
	if actor != "panel-mod-user" || reason != "lag after airdrop" || result != "success" || source != "discord_panel" {
		t.Fatalf("audit: %s %s %q %s %s", action, actor, reason, result, source)
	}
	var ownerActor *int64
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT actor_user_id FROM admin_audit_log WHERE installation_id=$1 AND action='SERVER_STOP' ORDER BY id DESC LIMIT 1`, inst).Scan(&ownerActor); err != nil || ownerActor == nil {
		t.Fatalf("the owner's stop names their Champion account: %v %v", ownerActor, err)
	}
	var starts int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='SERVER_START'`, inst).Scan(&starts); err != nil || starts != 1 {
		t.Fatalf("start audit rows: %d %v", starts, err)
	}

	if _, err := b.PanelStatus(ctx, w.f.DiscordGuildID, inst, true); err != nil {
		t.Fatalf("status: %v", err)
	}
}

// The panel's whitelist, ban list and priority queue edit Nitrado's lists, keep Champion's records
// and write the website's audit actions, each at its own staff level.
func TestServerPanelEditsPlayerLists(t *testing.T) {
	w := newClientAdminWorld(t)
	withFakeNitradoActions(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)
	w.a.ServerPanels = repository.NewServerPanelRepository(w.a.DB.Pool)
	ctx := context.Background()
	b := newServerPanelBackend(w.a)
	inst := w.f.InstallationID

	gateRole := fmt.Sprintf("panel-gate-%d", time.Now().UnixNano())
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO installation_role_permissions(installation_id,discord_role_id,permission_level) VALUES($1,$2,'GATEKEEPER')`, inst, gateRole); err != nil {
		t.Fatal(err)
	}
	gate := discord.PanelCaller{DiscordGuildID: w.f.DiscordGuildID, DiscordUserID: "panel-gatekeeper", RoleIDs: []string{gateRole}}
	owner := discord.PanelCaller{DiscordGuildID: w.f.DiscordGuildID, DiscordUserID: w.f.OwnerDiscordID}

	if err := b.PanelListChange(ctx, gate, inst, discord.PanelWhitelist, "RustyAxe_77", "", true); err != nil {
		t.Fatal(err)
	}
	if names, err := b.PanelListNames(ctx, gate, inst, discord.PanelWhitelist); err != nil || len(names) != 1 || names[0] != "RustyAxe_77" {
		t.Fatalf("whitelist after add: %v %v", names, err)
	}
	var recorded int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM installation_access_entries WHERE installation_id=$1 AND list_type='WHITELIST' AND identifier='RustyAxe_77' AND removed_at IS NULL AND created_by_user_id IS NULL`, inst).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("Champion's record of a whitelist add by staff without an account: %d %v", recorded, err)
	}
	// Adding again changes nothing and is not an error.
	if err := b.PanelListChange(ctx, gate, inst, discord.PanelWhitelist, "rustyaxe_77", "", true); err != nil {
		t.Fatal(err)
	}
	if err := b.PanelListChange(ctx, gate, inst, discord.PanelBanlist, "Griefer", "raiding spawns", true); !errors.Is(err, discord.ErrPanelForbidden) {
		t.Fatalf("a Gatekeeper banning: %v", err)
	}
	if _, err := b.PanelListNames(ctx, gate, inst, discord.PanelPriority); !errors.Is(err, discord.ErrPanelForbidden) {
		t.Fatalf("a Gatekeeper reading the priority queue: %v", err)
	}
	if err := b.PanelListChange(ctx, gate, inst, discord.PanelWhitelist, "two\nlines", "", true); !errors.Is(err, discord.ErrPanelBadName) {
		t.Fatalf("a name on two lines: %v", err)
	}
	// Removed by any capitalisation: the list's own spelling is removed, and so is Champion's record.
	if err := b.PanelListChange(ctx, gate, inst, discord.PanelWhitelist, "RUSTYAXE_77", "", false); err != nil {
		t.Fatal(err)
	}
	if names, _ := b.PanelListNames(ctx, gate, inst, discord.PanelWhitelist); len(names) != 0 {
		t.Fatalf("whitelist after remove: %v", names)
	}
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM installation_access_entries WHERE installation_id=$1 AND list_type='WHITELIST' AND removed_at IS NULL`, inst).Scan(&recorded); err != nil || recorded != 0 {
		t.Fatalf("Champion's record after remove: %d %v", recorded, err)
	}

	if err := b.PanelListChange(ctx, owner, inst, discord.PanelBanlist, "Griefer", "raiding spawns", true); err != nil {
		t.Fatal(err)
	}
	if names, _ := b.PanelListNames(ctx, owner, inst, discord.PanelBanlist); len(names) != 1 || names[0] != "Griefer" {
		t.Fatalf("ban list: %v", names)
	}
	for _, name := range []string{"VIP_One", "VIP_Two"} {
		if err := b.PanelListChange(ctx, owner, inst, discord.PanelPriority, name, "supporter", true); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.PanelListChange(ctx, owner, inst, discord.PanelPriority, "vip_one", "", false); err != nil {
		t.Fatal(err)
	}
	if names, _ := b.PanelListNames(ctx, owner, inst, discord.PanelPriority); len(names) != 1 || names[0] != "VIP_Two" {
		t.Fatalf("priority queue: %v", names)
	}

	want := map[string]int{"WHITELIST_ADD": 1, "WHITELIST_REMOVE": 1, "BANLIST_ADD": 1, "PRIORITY_ADD": 2, "PRIORITY_REMOVE": 1}
	rows, err := w.a.DB.Pool.Query(ctx, `SELECT action,COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND after_state->>'source'='discord_panel' AND result='success' GROUP BY action`, inst)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var action string
		var n int
		if err := rows.Scan(&action, &n); err != nil {
			t.Fatal(err)
		}
		got[action] = n
	}
	for action, n := range want {
		if got[action] != n {
			t.Errorf("audit %s: %d rows, want %d (all: %v)", action, got[action], n, got)
		}
	}
}

// withFakeNitradoSettings serves the connect-time services list, game settings (with their
// writes) and scheduled tasks.
func withFakeNitradoSettings(t *testing.T, a *App, tasksJSON string) *[]string {
	t.Helper()
	writes := &[]string{}
	config := map[string]string{"disableBaseDamage": "0", "wipe_on_next_restart": "0", "enableMouseAndKeyboard": "1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/services":
			_, _ = w.Write([]byte(psServiceJSON))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gameservers"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"gameserver": map[string]any{"settings": map[string]any{"config": config}}}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/gameservers/settings"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			config[body["key"]] = body["value"]
			*writes = append(*writes, body["category"]+"."+body["key"]+"="+body["value"])
			_, _ = w.Write([]byte(`{"status":"success"}`))
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			_, _ = w.Write([]byte(tasksJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	a.saasNitradoClientFactory = func(token string) *nitrado.Client { return nitrado.NewClient(srv.URL, token, nil) }
	return writes
}

// The panel's tools: game settings (never the storage wipe), the restart schedule, maintenance
// and posted panels, each at its staff level.
func TestServerPanelTools(t *testing.T) {
	w := newClientAdminWorld(t)
	next := time.Now().Add(3 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	writes := withFakeNitradoSettings(t, w.a, `{"data":{"tasks":[{"id":1,"action_method":"restart","next_run":"`+next+`"}]}}`)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)
	w.a.ServerPanels = repository.NewServerPanelRepository(w.a.DB.Pool)
	ctx := context.Background()
	b := newServerPanelBackend(w.a)
	inst := w.f.InstallationID

	modRole := fmt.Sprintf("panel-tools-mod-%d", time.Now().UnixNano())
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO installation_role_permissions(installation_id,discord_role_id,permission_level) VALUES($1,$2,'MODERATOR')`, inst, modRole); err != nil {
		t.Fatal(err)
	}
	mod := discord.PanelCaller{DiscordGuildID: w.f.DiscordGuildID, DiscordUserID: "panel-tools-mod", RoleIDs: []string{modRole}}
	owner := discord.PanelCaller{DiscordGuildID: w.f.DiscordGuildID, DiscordUserID: w.f.OwnerDiscordID}

	// Game settings: Administrator and up; the storage wipe is never offered or written.
	if _, err := b.PanelSettings(ctx, mod, inst); !errors.Is(err, discord.ErrPanelForbidden) {
		t.Fatalf("a Moderator reading game settings: %v", err)
	}
	settings, err := b.PanelSettings(ctx, owner, inst)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range settings {
		got[s.ID] = s.Enabled
	}
	if len(got) != 2 || got["disable_base_damage"] || !got["mouse_and_keyboard"] {
		t.Fatalf("settings: %+v", settings)
	}
	if err := b.PanelSetSetting(ctx, owner, inst, "wipe_on_next_restart", true); !errors.Is(err, discord.ErrPanelUnsupported) {
		t.Fatalf("the storage wipe from Discord: %v", err)
	}
	if err := b.PanelSetSetting(ctx, owner, inst, "disable_base_damage", true); err != nil {
		t.Fatal(err)
	}
	if err := b.PanelSetSetting(ctx, owner, inst, "disable_base_damage", true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*writes, ",") != "config.disableBaseDamage=1" {
		t.Fatalf("setting writes (a repeat writes nothing): %v", *writes)
	}

	// The restart schedule: any staff level.
	tasks, err := b.PanelSchedule(ctx, mod, inst)
	if err != nil || len(tasks) != 1 || tasks[0].Action != "restart" || tasks[0].Next == nil {
		t.Fatalf("schedule: %+v %v", tasks, err)
	}

	// Maintenance: Administrator and up; the switch takes effect at once.
	if err := b.PanelSetMaintenance(ctx, mod, inst, true); !errors.Is(err, discord.ErrPanelForbidden) {
		t.Fatalf("a Moderator switching maintenance: %v", err)
	}
	if w.a.serverInMaintenance(w.serverID) {
		t.Fatal("not in maintenance yet")
	}
	if err := b.PanelSetMaintenance(ctx, owner, inst, true); err != nil {
		t.Fatal(err)
	}
	if !w.a.serverInMaintenance(w.serverID) {
		t.Fatal("maintenance is on at once")
	}
	if servers, _ := b.PanelServers(ctx, w.f.DiscordGuildID); len(servers) != 1 || !servers[0].Maintenance {
		t.Fatalf("the panel shows maintenance: %+v", servers)
	}
	if err := b.PanelSetMaintenance(ctx, owner, inst, false); err != nil || w.a.serverInMaintenance(w.serverID) {
		t.Fatalf("maintenance off: %v", err)
	}

	// Posted panels: Administrator and up; posting again in a channel replaces the earlier one.
	if _, err := b.PanelPosted(ctx, mod, inst, "chan-1", "msg-0"); !errors.Is(err, discord.ErrPanelForbidden) {
		t.Fatalf("a Moderator posting the panel: %v", err)
	}
	if replaced, err := b.PanelPosted(ctx, owner, inst, "chan-1", "msg-1"); err != nil || replaced != "" {
		t.Fatalf("first post: %q %v", replaced, err)
	}
	if replaced, err := b.PanelPosted(ctx, owner, inst, "chan-1", "msg-2"); err != nil || replaced != "msg-1" {
		t.Fatalf("second post in the channel: %q %v", replaced, err)
	}
	if _, err := b.PanelPosted(ctx, owner, inst, "chan-2", "msg-3"); err != nil {
		t.Fatal(err)
	}
	posts, err := b.PanelPosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mine := map[string]string{}
	for _, p := range posts {
		if p.InstallationID == inst {
			mine[p.MessageID] = p.ChannelID + "@" + p.DiscordGuildID
		}
	}
	if len(mine) != 2 || mine["msg-2"] != "chan-1@"+w.f.DiscordGuildID || mine["msg-3"] != "chan-2@"+w.f.DiscordGuildID {
		t.Fatalf("posts: %v", mine)
	}
	if err := b.PanelPostGone(ctx, "msg-3"); err != nil {
		t.Fatal(err)
	}
	if posts, _ = b.PanelPosts(ctx); len(posts) != 1 || posts[0].MessageID != "msg-2" {
		t.Fatalf("posts after one is gone: %+v", posts)
	}

	var audits int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND after_state->>'source'='discord_panel'
AND action IN ('GAME_SETTING_SET','MAINTENANCE_MODE_CHANGE','SERVER_PANEL_POSTED') AND result='success'`, inst).Scan(&audits); err != nil || audits != 6 {
		t.Fatalf("audit rows: %d %v", audits, err)
	}
}
