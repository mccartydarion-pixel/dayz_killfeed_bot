package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/ownerops"
)

func TestServerDownStateAlertsOncePerOutage(t *testing.T) {
	start := time.Date(2026, 10, 10, 3, 39, 0, 0, time.UTC)
	lastGrowth := start
	var st serverDownState
	at := func(d time.Duration) *discord.AdminAlert {
		now := start.Add(d)
		return st.step(ownerops.DownInput{Now: now, LastGrowth: lastGrowth, LastListing: now.Add(-30 * time.Second)}, 1, 2)
	}

	if a := at(5 * time.Minute); a != nil {
		t.Fatalf("a restart-length pause alerted: %+v", a)
	}
	down := at(20 * time.Minute)
	if down == nil || down.Severity != discord.AlertCritical || down.Kind != discord.AlertKindServerDown || down.GuildRowID != 1 || down.ServerID != 2 {
		t.Fatalf("no down alert at 20 minutes: %+v", down)
	}
	for _, d := range []time.Duration{21 * time.Minute, 2 * time.Hour, 9 * time.Hour} {
		if a := at(d); a != nil {
			t.Fatalf("a second alert at %s: %+v", d, a)
		}
	}

	// A check that cannot see the server changes nothing.
	blind := start.Add(9*time.Hour + time.Minute)
	if a := st.step(ownerops.DownInput{Now: blind, LastGrowth: lastGrowth, LastListing: blind, ListingFailed: true}, 1, 2); a != nil || !st.alerted {
		t.Fatalf("an unknown check changed the state: %+v alerted=%v", a, st.alerted)
	}

	lastGrowth = start.Add(9*time.Hour + 38*time.Minute)
	back := at(9*time.Hour + 39*time.Minute)
	if back == nil || back.Severity != discord.AlertResolved {
		t.Fatalf("no recovery alert: %+v", back)
	}
	if a := at(9*time.Hour + 40*time.Minute); a != nil || st.alerted {
		t.Fatalf("after recovery: %+v alerted=%v", a, st.alerted)
	}
}

func TestServerDownAfterSetting(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 20 * time.Minute, "45": 45 * time.Minute, "3": 10 * time.Minute, "junk": 20 * time.Minute} {
		t.Setenv("SERVER_DOWN_ALERT_MINUTES", raw)
		if got, on := serverDownAfter(); !on || got != want {
			t.Errorf("%q: %s on=%v, want %s on", raw, got, on, want)
		}
	}
	for _, raw := range []string{"0", "off", "OFF", "-5"} {
		t.Setenv("SERVER_DOWN_ALERT_MINUTES", raw)
		if _, on := serverDownAfter(); on {
			t.Errorf("%q should switch the alert off", raw)
		}
	}
}

func TestRoughDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{20 * time.Minute: "20 minutes", time.Hour: "1 hour", 9*time.Hour + 38*time.Minute: "9 hours 38 minutes", 10 * time.Second: "1 minute"} {
		if got := roughDuration(d); got != want {
			t.Errorf("roughDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestServerDownIsAnOperationalAlert(t *testing.T) {
	embed := discord.BuildAdminAlertEmbed(discord.AdminAlert{Kind: discord.AlertKindServerDown, Severity: discord.AlertCritical, Headline: "Game server looks down"}, "Chernarus")
	if embed == nil || embed.Description == "" {
		t.Fatal("no embed")
	}
	if !discord.OperationalAdminAlertKind(discord.AlertKindServerDown) {
		t.Fatal("SERVER_DOWN must be accepted by the staff alert publisher")
	}
}

func TestServerDownDMCarriesTheAlertAndPingsNobody(t *testing.T) {
	msg := serverDownDM(discord.AdminAlert{Kind: discord.AlertKindServerDown, Severity: discord.AlertCritical, Headline: "Game server looks down", Detail: "The logs are still."}, "Chernarus")
	if len(msg.Embeds) != 1 || !strings.Contains(msg.Embeds[0].Description, "Game server looks down") {
		t.Fatalf("embed = %+v", msg.Embeds)
	}
	if msg.AllowedMentions == nil || len(msg.AllowedMentions.Parse) != 0 {
		t.Fatal("the DM must not allow mentions")
	}
	var named bool
	for _, f := range msg.Embeds[0].Fields {
		named = named || f.Value == "Chernarus"
	}
	if !named {
		t.Fatal("the DM must name the server")
	}
}

type fakeAlertTransport struct {
	sends    []string // channel ids
	edits    []string // "channel/message"
	failEdit bool
	next     int
}

func (f *fakeAlertTransport) Send(channelID string, _ *discordgo.MessageSend) (string, error) {
	f.sends = append(f.sends, channelID)
	f.next++
	return fmt.Sprintf("m%d", f.next), nil
}

func (f *fakeAlertTransport) Edit(channelID, messageID string, _ *discordgo.MessageEmbed) error {
	if f.failEdit {
		return errors.New("unknown message")
	}
	f.edits = append(f.edits, channelID+"/"+messageID)
	return nil
}

func newTestNotifier(tr *fakeAlertTransport) *serverDownNotifier {
	return &serverDownNotifier{transport: tr, targets: func() []string { return []string{"staff", "dm"} }, serverName: func() string { return "Chernarus" }}
}

func TestServerDownNotifierEditsItsOwnMessages(t *testing.T) {
	tr := &fakeAlertTransport{}
	n := newTestNotifier(tr)
	down := discord.AdminAlert{Kind: discord.AlertKindServerDown, Severity: discord.AlertCritical, Headline: "Game server looks down"}
	back := discord.AdminAlert{Kind: discord.AlertKindServerDown, Severity: discord.AlertResolved, Headline: "Game server is back"}

	n.notify(down)
	if strings.Join(tr.sends, ",") != "staff,dm" || len(tr.edits) != 0 {
		t.Fatalf("down: sends=%v edits=%v", tr.sends, tr.edits)
	}
	n.notify(back)
	if strings.Join(tr.sends, ",") != "staff,dm" || strings.Join(tr.edits, ",") != "staff/m1,dm/m2" {
		t.Fatalf("back must edit the two down messages and send nothing: sends=%v edits=%v", tr.sends, tr.edits)
	}

	// The next outage is a new pair of messages, edited in turn.
	n.notify(down)
	n.notify(back)
	if len(tr.sends) != 4 || strings.Join(tr.edits[2:], ",") != "staff/m3,dm/m4" {
		t.Fatalf("second outage: sends=%v edits=%v", tr.sends, tr.edits)
	}
}

func TestServerDownNotifierSendsWhenItHasNothingToEdit(t *testing.T) {
	// After a bot restart the ids are gone: "back" is posted once in each place.
	tr := &fakeAlertTransport{}
	n := newTestNotifier(tr)
	n.notify(discord.AdminAlert{Kind: discord.AlertKindServerDown, Severity: discord.AlertResolved, Headline: "Game server is back"})
	if strings.Join(tr.sends, ",") != "staff,dm" || len(n.sent) != 0 {
		t.Fatalf("sends=%v sent=%v", tr.sends, n.sent)
	}

	// A message that can no longer be edited (deleted) is replaced by a new one.
	tr = &fakeAlertTransport{}
	n = newTestNotifier(tr)
	n.notify(discord.AdminAlert{Kind: discord.AlertKindServerDown, Severity: discord.AlertCritical, Headline: "Game server looks down"})
	tr.failEdit = true
	n.notify(discord.AdminAlert{Kind: discord.AlertKindServerDown, Severity: discord.AlertResolved, Headline: "Game server is back"})
	if len(tr.sends) != 4 {
		t.Fatalf("sends=%v, want the two alerts and two replacements", tr.sends)
	}
}

func TestGameServerStatusForTheBoard(t *testing.T) {
	boot := time.Date(2026, 10, 10, 16, 36, 30, 0, time.UTC)
	now := boot.Add(20 * time.Minute)
	in := ownerops.DownInput{Now: now, LastGrowth: now.Add(-time.Minute), LastListing: now, BootAt: boot, RunLength: 68 * time.Minute}
	g, known := gameServerStatus(in, serverDownState{})
	if !known || g.State != ownerops.GameOnline || !g.StartedAt.Equal(boot) || !g.NextRestart.Equal(boot.Add(68*time.Minute).Truncate(time.Minute)) {
		t.Fatalf("online: %+v known=%v", g, known)
	}
	// An open alert keeps the board on "down", even when this check cannot see the server.
	since := now.Add(-time.Hour)
	g, known = gameServerStatus(ownerops.DownInput{Now: now, LastGrowth: since, LastListing: now, ListingFailed: true}, serverDownState{alerted: true, downSince: since})
	if !known || g.State != ownerops.GameDown || !g.DownSince.Equal(since) {
		t.Fatalf("down: %+v known=%v", g, known)
	}
	if _, known := gameServerStatus(ownerops.DownInput{Now: now}, serverDownState{}); known {
		t.Fatal("nothing known must not change the board")
	}
}

func TestKnownStartsKeepsEachStartOnce(t *testing.T) {
	a := time.Date(2026, 10, 10, 9, 17, 46, 0, time.UTC)
	starts := knownStarts(nil, a)
	starts = knownStarts(starts, a)
	starts = knownStarts(starts, time.Time{})
	starts = knownStarts(starts, a.Add(68*time.Minute))
	if len(starts) != 2 {
		t.Fatalf("starts = %v", starts)
	}
}
