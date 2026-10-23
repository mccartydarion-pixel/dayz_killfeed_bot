package discord

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

func TestServerPanelToolsRowFollowsStaffLevel(t *testing.T) {
	srv := PanelServer{InstallationID: 7, Name: "S"}
	gate := panelButtons(t, BuildServerPanel([]PanelServer{srv}, srv, PanelStatus{State: "started"}, permissions.LevelGatekeeper, nil, time.Now()))
	if !gate["Game settings"].Disabled || !gate["Logs"].Disabled || gate["Schedule"].Disabled || !gate["Maintenance: Off"].Disabled || !gate["Post here"].Disabled {
		t.Fatalf("gatekeeper tools: %+v", gate)
	}
	admin := panelButtons(t, BuildServerPanel([]PanelServer{srv}, srv, PanelStatus{State: "started"}, permissions.LevelAdministrator, nil, time.Now()))
	for _, label := range []string{"Game settings", "Logs", "Schedule", "Maintenance: Off", "Post here"} {
		if admin[label].Disabled {
			t.Errorf("an administrator's %q is disabled", label)
		}
	}
	if admin["Game settings"].CustomID != "srvpanel:settings:7" || admin["Logs"].CustomID != "srvpanel:logs:7" || admin["Schedule"].CustomID != "srvpanel:schedule:7" ||
		admin["Maintenance: Off"].CustomID != "srvpanel:maint:7" || admin["Post here"].CustomID != "srvpanel:post:7" {
		t.Fatalf("tool custom ids: %+v", admin)
	}
	srv.Maintenance = true
	data := BuildServerPanel([]PanelServer{srv}, srv, PanelStatus{State: "started"}, permissions.LevelAdministrator, nil, time.Now())
	if _, ok := panelButtons(t, data)["Maintenance: On"]; !ok || !strings.Contains(data.Embeds[0].Description, "Maintenance is on") {
		t.Fatalf("maintenance on: %q", data.Embeds[0].Description)
	}
	if rows := len(data.Components); rows > 5 {
		t.Fatalf("Discord allows 5 rows, the panel has %d", rows)
	}
}

func TestSharedServerPanel(t *testing.T) {
	srv := PanelServer{InstallationID: 7, Name: "Champion Chernarus"}
	data := BuildSharedServerPanel(srv, PanelStatus{State: "started"}, nil)
	e := data.Embeds[0]
	if _, ok := panelFields(e)["Your staff level"]; ok {
		t.Fatal("a shared panel shows no one's staff level")
	}
	if e.Footer == nil || !strings.Contains(e.Footer.Text, "updates every 2 minutes") {
		t.Fatalf("footer: %+v", e.Footer)
	}
	b := panelButtons(t, data)
	if _, ok := b["Post here"]; ok {
		t.Fatal("a shared panel can't be posted again from itself")
	}
	for label, btn := range b {
		if btn.Disabled {
			t.Errorf("shared %q is disabled: a click is checked against the clicker's level", label)
		}
	}
	for _, c := range data.Components {
		if _, ok := c.(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu); ok {
			t.Fatal("a shared panel has no server picker")
		}
	}
	busy := panelButtons(t, BuildSharedServerPanel(srv, PanelStatus{State: "restarting"}, nil))
	if !busy["Restart"].Disabled || !busy["Stop"].Disabled {
		t.Fatal("a restarting server's power buttons wait on a shared panel too")
	}
}

func TestPanelSettingsMessage(t *testing.T) {
	data := BuildPanelSettings(7, "S", []PanelSetting{
		{ID: "disable_base_damage", Label: "Disable base damage", Help: "No fence damage.", Enabled: true},
		{ID: "mouse_and_keyboard", Label: "Mouse & keyboard", Help: "Allow M&K.", Enabled: false},
	})
	desc := data.Embeds[0].Description
	if !strings.Contains(desc, "✅ **Disable base damage**") || !strings.Contains(desc, "⬜ **Mouse & keyboard**") || !strings.Contains(desc, "storage wipe is on the Champion website only") {
		t.Fatalf("settings: %q", desc)
	}
	menu := data.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.SelectMenu)
	if menu.CustomID != "srvpanel:setting:7" || menu.Options[0].Value != "disable_base_damage:off" || menu.Options[0].Label != "Turn off: Disable base damage" || menu.Options[1].Value != "mouse_and_keyboard:on" {
		t.Fatalf("menu: %+v", menu)
	}
	if empty := BuildPanelSettings(7, "S", nil); len(empty.Components) != 0 || !strings.Contains(empty.Embeds[0].Description, "offers none") {
		t.Fatalf("no settings: %+v", empty)
	}
}

func TestPanelLogMessage(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, "14:00:"+string(rune('0'+i%10))+" | line "+strings.Repeat("x", i))
	}
	lines = append(lines, "evil ``` fence")
	log := PanelLog{Name: "DayZServer_x64.ADM", Size: 2 << 20, Text: strings.Join(lines, "\n") + "\n", Earlier: true}
	data := BuildPanelLog("S", log)
	desc := data.Embeds[0].Description
	if strings.Count(desc, "\n14:00") != panelLogLines-1 || strings.Contains(desc, "line x\n") || !strings.Contains(desc, "evil ''' fence") || !strings.Contains(desc, "2.0 MB") || !strings.Contains(desc, "end of the log") {
		t.Fatalf("log message: %q", desc)
	}
	if len(data.Files) != 1 || data.Files[0].Name != "DayZServer_x64.ADM.txt" {
		t.Fatalf("attachment: %+v", data.Files)
	}
	body, _ := io.ReadAll(data.Files[0].Reader)
	if string(body) != log.Text {
		t.Fatal("the attachment is the log text that was read")
	}
	if panelLogFileName("a/b\\c") != "a_b_c.txt" {
		t.Fatalf("file name: %q", panelLogFileName("a/b\\c"))
	}
}

func TestPanelScheduleMessage(t *testing.T) {
	now := time.Now()
	later, sooner := now.Add(6*time.Hour), now.Add(time.Hour)
	desc := BuildPanelSchedule("S", []PanelTask{{Action: "restart", Next: &later}, {Action: "backup"}, {Action: "restart", Next: &sooner}}).Embeds[0].Description
	first, second := strings.Index(desc, presentation.Timestamp(sooner, 'F')), strings.Index(desc, presentation.Timestamp(later, 'F'))
	if first < 0 || second < 0 || first > second || !strings.Contains(desc, "🗓️ **Backup** · no next run") {
		t.Fatalf("schedule: %q", desc)
	}
	if !strings.Contains(BuildPanelSchedule("S", nil).Embeds[0].Description, "no scheduled tasks") {
		t.Fatal("empty schedule")
	}
}

func TestPanelMaintenanceAsk(t *testing.T) {
	off := panelButtons(t, BuildPanelMaintenanceAsk(PanelServer{InstallationID: 3, Name: "S"}))
	if off["Turn maintenance on"].CustomID != "srvpanel:maint-on:3" {
		t.Fatalf("ask on: %+v", off)
	}
	on := panelButtons(t, BuildPanelMaintenanceAsk(PanelServer{InstallationID: 3, Name: "S", Maintenance: true}))
	if on["Turn maintenance off"].CustomID != "srvpanel:maint-off:3" {
		t.Fatalf("ask off: %+v", on)
	}
}

func TestServerStatusBoardShowsMaintenance(t *testing.T) {
	now := time.Now()
	e := BuildServerStatusEmbed([]ServerStatusSection{{ServerName: "S", Maintenance: true, Game: GameServerStatus{State: "ONLINE"}}}, now)
	if e.Color != presentation.Amber || !strings.HasPrefix(e.Fields[0].Value, "🛠️ **Under maintenance:**") {
		t.Fatalf("board: %d %q", e.Color, e.Fields[0].Value)
	}
	if down := BuildServerStatusEmbed([]ServerStatusSection{{ServerName: "S", Maintenance: true, Game: GameServerStatus{State: "DOWN"}}}, now); down.Color != presentation.ErrorRed {
		t.Fatal("a server that is down stays red")
	}
}

// panelBackendFake serves posted panels for the refresh test; the other methods are unused.
type panelBackendFake struct {
	ServerPanelBackend
	servers []PanelServer
	posts   []PanelPost
	gone    []string
}

func (f *panelBackendFake) PanelServers(context.Context, string) ([]PanelServer, error) {
	return f.servers, nil
}
func (f *panelBackendFake) PanelStatus(context.Context, string, int64, bool) (PanelStatus, error) {
	return PanelStatus{State: "started"}, nil
}
func (f *panelBackendFake) PanelPosts(context.Context) ([]PanelPost, error) { return f.posts, nil }
func (f *panelBackendFake) PanelPostGone(_ context.Context, id string) error {
	f.gone = append(f.gone, id)
	return nil
}

type panelEditorFake struct {
	edited  []string
	missing map[string]bool
}

func (f *panelEditorFake) ChannelMessageEditComplex(m *discordgo.MessageEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	if f.missing[m.ID] {
		return nil, &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusNotFound}, Message: &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage}}
	}
	if m.Embeds == nil || len(*m.Embeds) != 1 || (*m.Embeds)[0].Title != "🎛️ Server control" {
		return nil, errors.New("edit carries no panel")
	}
	f.edited = append(f.edited, m.ID)
	return &discordgo.Message{ID: m.ID}, nil
}

func TestSharedPanelsRefresh(t *testing.T) {
	backend := &panelBackendFake{
		servers: []PanelServer{{InstallationID: 1, Name: "A"}},
		posts: []PanelPost{
			{MessageID: "m1", ChannelID: "c1", DiscordGuildID: "g", InstallationID: 1},
			{MessageID: "m2", ChannelID: "c2", DiscordGuildID: "g", InstallationID: 1},
			{MessageID: "m3", ChannelID: "c3", DiscordGuildID: "g", InstallationID: 99},
		},
	}
	editor := &panelEditorFake{missing: map[string]bool{"m2": true}}
	NewServerPanelHandler(backend).refreshShared(context.Background(), editor)
	if len(editor.edited) != 1 || editor.edited[0] != "m1" {
		t.Fatalf("edited: %v", editor.edited)
	}
	if strings.Join(backend.gone, ",") != "m2,m3" {
		t.Fatalf("forgotten: %v (a deleted message and a server no longer set up)", backend.gone)
	}
}
