package discord

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

func panelFields(e *discordgo.MessageEmbed) map[string]string {
	out := map[string]string{}
	for _, f := range e.Fields {
		out[f.Name] = f.Value
	}
	return out
}

func panelButtons(t *testing.T, data *discordgo.InteractionResponseData) map[string]discordgo.Button {
	t.Helper()
	out := map[string]discordgo.Button{}
	for _, c := range data.Components {
		row, ok := c.(discordgo.ActionsRow)
		if !ok {
			t.Fatalf("component %T is not a row", c)
		}
		for _, inner := range row.Components {
			if b, ok := inner.(discordgo.Button); ok {
				out[b.Label] = b
			}
		}
	}
	return out
}

func TestServerPanelButtonsFollowStaffLevel(t *testing.T) {
	srv := PanelServer{InstallationID: 7, Name: "Champion Chernarus #1", MapName: "Chernarus"}
	players := 34
	online := PanelStatus{State: "started", Players: &players, Capacity: 60, ReadAt: time.Now()}
	cases := []struct {
		level                  permissions.Level
		restart, stop, refresh bool // enabled
	}{
		{permissions.LevelGatekeeper, false, false, true},
		{permissions.LevelModerator, true, false, true},
		{permissions.LevelAdministrator, true, true, true},
		{permissions.LevelOwner, true, true, true},
	}
	for _, c := range cases {
		b := panelButtons(t, BuildServerPanel([]PanelServer{srv}, srv, online, c.level, nil, time.Now()))
		if !b["Restart"].Disabled != c.restart || !b["Stop"].Disabled != c.stop || !b["↻ Refresh"].Disabled != c.refresh {
			t.Errorf("%s: restart=%v stop=%v refresh=%v", levelLabel(c.level), !b["Restart"].Disabled, !b["Stop"].Disabled, !b["↻ Refresh"].Disabled)
		}
		if _, ok := b["Start"]; ok {
			t.Errorf("%s: an online server shows Start", levelLabel(c.level))
		}
	}
	data := BuildServerPanel([]PanelServer{srv}, srv, online, permissions.LevelModerator, nil, time.Now())
	e := data.Embeds[0]
	if e.Title != "🎛️ Server control" || e.Color != presentation.Green || e.Footer == nil || e.Footer.Text != presentation.FooterText(srv.Name, "Staff only") || !strings.HasSuffix(e.Footer.Text, " · Staff only") {
		t.Fatalf("embed: %+v", e)
	}
	if got := panelFields(e); got["Status"] != "🟢 Online" || got["Players"] != "34 / 60" || got["Map"] != "Chernarus" || !strings.HasPrefix(got["Your staff level"], "Moderator") {
		t.Fatalf("fields: %v", got)
	}
	if b := panelButtons(t, data); b["Restart"].CustomID != "srvpanel:ask:restart:7" || b["Stop"].CustomID != "srvpanel:ask:stop:7" || b["↻ Refresh"].CustomID != "srvpanel:refresh:7" {
		t.Fatalf("custom ids: %+v", b)
	}
}

func TestServerPanelStoppedBusyAndLocked(t *testing.T) {
	srv := PanelServer{InstallationID: 7, Name: "S"}
	now := time.Now()
	stopped := panelButtons(t, BuildServerPanel([]PanelServer{srv}, srv, PanelStatus{State: "stopped"}, permissions.LevelModerator, nil, now))
	if _, ok := stopped["Restart"]; ok {
		t.Fatal("a stopped server offers Start, not Restart")
	}
	if b := stopped["Start"]; b.Disabled || b.CustomID != "srvpanel:start:7" || !stopped["Stop"].Disabled {
		t.Fatalf("stopped: %+v", stopped)
	}
	busy := panelButtons(t, BuildServerPanel([]PanelServer{srv}, srv, PanelStatus{State: "restarting"}, permissions.LevelOwner, nil, now))
	if !busy["Restart"].Disabled || !busy["Stop"].Disabled || busy["↻ Refresh"].Disabled {
		t.Fatalf("restarting: %+v", busy)
	}
	lock := &panelLock{action: PanelRestart, at: now}
	data := BuildServerPanel([]PanelServer{srv}, srv, PanelStatus{State: "started"}, permissions.LevelOwner, lock, now)
	if b := panelButtons(t, data); !b["Restart"].Disabled || !b["Stop"].Disabled {
		t.Fatalf("locked: %+v", b)
	}
	if !strings.Contains(data.Embeds[0].Description, "A restart was sent") {
		t.Fatalf("locked description: %q", data.Embeds[0].Description)
	}
	// Nothing Nitrado did not give is shown as a placeholder.
	unknown := BuildServerPanel([]PanelServer{srv}, srv, PanelStatus{}, permissions.LevelOwner, nil, now).Embeds[0]
	if got := panelFields(unknown); len(got) != 1 || got["Your staff level"] == "" || !strings.Contains(unknown.Description, "status is unknown") || unknown.Color != presentation.Neutral {
		t.Fatalf("unknown status: %v %q", got, unknown.Description)
	}
	next := now.Add(3 * time.Hour)
	if got := panelFields(BuildServerPanel([]PanelServer{srv}, srv, PanelStatus{State: "started", NextRestart: &next}, permissions.LevelOwner, nil, now).Embeds[0]); got["Next scheduled restart"] != presentation.TimestampWithRelative(next) {
		t.Fatalf("next restart: %v", got)
	}
}

func TestServerPanelPicksAmongServers(t *testing.T) {
	a := PanelServer{InstallationID: 1, Name: "Chernarus #1", MapName: "Chernarus"}
	b := PanelServer{InstallationID: 2, Name: "Livonia PvE", MapName: "Livonia"}
	one := BuildServerPanel([]PanelServer{a}, a, PanelStatus{State: "started"}, permissions.LevelOwner, nil, time.Now())
	if len(one.Components) != 3 {
		t.Fatalf("one server needs no picker: %d rows", len(one.Components))
	}
	two := BuildServerPanel([]PanelServer{a, b}, b, PanelStatus{State: "started"}, permissions.LevelOwner, nil, time.Now())
	menu, ok := two.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
	if !ok || menu.CustomID != serverPanelPickID || len(menu.Options) != 2 || menu.Options[0].Default || !menu.Options[1].Default || menu.Options[1].Value != "2" {
		t.Fatalf("picker: %+v", two.Components[0])
	}
}

func TestServerPanelForms(t *testing.T) {
	stop := BuildServerPanelForm(PanelStop, 9)
	if stop.CustomID != "srvpanel:do:stop:9" || len(stop.Components) != 2 {
		t.Fatalf("stop form: %+v", stop)
	}
	restart := BuildServerPanelForm(PanelRestart, 9)
	if restart.CustomID != "srvpanel:do:restart:9" || len(restart.Components) != 1 {
		t.Fatalf("restart form: %+v", restart)
	}
	for _, f := range []*discordgo.InteractionResponseData{stop, restart} {
		if len([]rune(f.Title)) > 45 {
			t.Fatalf("form title over Discord's 45 characters: %q", f.Title)
		}
	}
	for in, want := range map[string]bool{"restart:9": true, "start:12": true, "stop:3": true, "wipe:3": false, "restart:x": false, "restart:0": false, "restart": false} {
		if _, _, ok := parsePanelTarget(in); ok != want {
			t.Errorf("parsePanelTarget(%q) ok=%v", in, ok)
		}
	}
}

func TestServerPanelPowerLock(t *testing.T) {
	now := time.Now()
	h := NewServerPanelHandler(nil)
	h.now = func() time.Time { return now }
	if !h.takeLock(5, PanelRestart) || h.takeLock(5, PanelStop) {
		t.Fatal("a second power action while the first is running must be refused")
	}
	if !h.takeLock(6, PanelStop) {
		t.Fatal("the lock is per server")
	}
	now = now.Add(panelPowerLock)
	if _, locked := h.lockOf(5); locked || !h.takeLock(5, PanelStart) {
		t.Fatal("the lock ends after panelPowerLock")
	}
	h.releaseLock(5)
	if _, locked := h.lockOf(5); locked {
		t.Fatal("a failed action releases the lock")
	}
}

func TestPanelActionCapabilities(t *testing.T) {
	for action, want := range map[PanelAction]permissions.Level{PanelRestart: permissions.LevelModerator, PanelStart: permissions.LevelModerator, PanelStop: permissions.LevelAdministrator} {
		c, ok := PanelActionCapability(action)
		if !ok || !permissions.Allows(want, c) || permissions.Allows(want-1, c) {
			t.Errorf("%s needs %s", action, levelLabel(want))
		}
	}
}

func TestServerPanelListButtonsFollowStaffLevel(t *testing.T) {
	srv := PanelServer{InstallationID: 7, Name: "S"}
	for _, c := range []struct {
		level                     permissions.Level
		whitelist, bans, priority bool
	}{
		{permissions.LevelGatekeeper, true, false, false},
		{permissions.LevelModerator, true, true, false},
		{permissions.LevelAdministrator, true, true, true},
	} {
		b := panelButtons(t, BuildServerPanel([]PanelServer{srv}, srv, PanelStatus{State: "started"}, c.level, nil, time.Now()))
		if !b["Whitelist"].Disabled != c.whitelist || !b["Ban list"].Disabled != c.bans || !b["Priority queue"].Disabled != c.priority {
			t.Errorf("%s: whitelist=%v bans=%v priority=%v", levelLabel(c.level), !b["Whitelist"].Disabled, !b["Ban list"].Disabled, !b["Priority queue"].Disabled)
		}
		if b["Whitelist"].CustomID != "srvpanel:list:whitelist:7" || b["Ban list"].CustomID != "srvpanel:list:banlist:7" || b["Priority queue"].CustomID != "srvpanel:list:priority:7" {
			t.Fatalf("list custom ids: %+v", b)
		}
	}
}

func TestServerPanelListMessage(t *testing.T) {
	names := make([]string, 45)
	for i := range names {
		names[i] = "Player_" + strconv.Itoa(i)
	}
	names[0] = "*Bold*Name"
	data := BuildPanelList(PanelWhitelist, 7, "Champion Chernarus", names)
	e := data.Embeds[0]
	if e.Title != "📋 Whitelist" || !strings.Contains(e.Description, "**45** players") || !strings.Contains(e.Description, "\\*Bold\\*Name") ||
		!strings.Contains(e.Description, "…and 5 more") || strings.Contains(e.Description, "Player_44") {
		t.Fatalf("list message: %q", e.Description)
	}
	b := panelButtons(t, data)
	if b["Add"].CustomID != "srvpanel:ask:whitelist-add:7" || b["Remove"].CustomID != "srvpanel:ask:whitelist-remove:7" || b["Remove"].Disabled {
		t.Fatalf("list buttons: %+v", b)
	}
	empty := BuildPanelList(PanelBanlist, 7, "S", nil)
	if eb := panelButtons(t, empty); !eb["Unban"].Disabled || eb["Ban"].Disabled || eb["Ban"].Style != discordgo.DangerButton || !strings.Contains(empty.Embeds[0].Description, "Nobody is banned.") {
		t.Fatalf("empty ban list: %+v %q", eb, empty.Embeds[0].Description)
	}
}

func TestServerPanelListForms(t *testing.T) {
	reasonInput := func(f *discordgo.InteractionResponseData) *discordgo.TextInput {
		for _, row := range f.Components {
			for _, c := range row.(discordgo.ActionsRow).Components {
				if in := c.(discordgo.TextInput); in.CustomID == serverPanelReasonInput {
					return &in
				}
			}
		}
		return nil
	}
	ban := BuildPanelListForm(PanelBanlist, true, 4)
	if ban.CustomID != "srvpanel:do:banlist-add:4" || ban.Title != "Ban a player" || reasonInput(ban) == nil || !reasonInput(ban).Required {
		t.Fatalf("ban form: %+v", ban)
	}
	if wl := BuildPanelListForm(PanelWhitelist, true, 4); reasonInput(wl) == nil || reasonInput(wl).Required {
		t.Fatalf("a whitelist reason is optional: %+v", wl)
	}
	if rm := BuildPanelListForm(PanelPriority, false, 4); rm.CustomID != "srvpanel:do:priority-remove:4" || reasonInput(rm) != nil || len([]rune(rm.Title)) > 45 {
		t.Fatalf("priority remove form: %+v", rm)
	}
	for _, l := range []PanelList{PanelWhitelist, PanelBanlist, PanelPriority} {
		for _, add := range []bool{true, false} {
			if f := BuildPanelListForm(l, add, 1); len([]rune(f.Title)) > 45 {
				t.Errorf("form title over 45 characters: %q", f.Title)
			}
		}
	}
	for in, want := range map[string]bool{"whitelist-add:3": true, "banlist-remove:3": true, "priority-add:9": true, "whitelist-wipe:3": false, "ops-add:3": false, "whitelist-add:x": false, "whitelist:3": false} {
		if _, _, _, ok := parseListTarget(in); ok != want {
			t.Errorf("parseListTarget(%q) ok=%v", in, ok)
		}
	}
	if l, add, id, ok := parseListTarget("banlist-remove:12"); !ok || l != PanelBanlist || add || id != 12 {
		t.Fatalf("parse: %v %v %v %v", l, add, id, ok)
	}
	if _, _, ok := parseListShow("restart:3"); ok {
		t.Fatal("a power action is not a list")
	}
}
