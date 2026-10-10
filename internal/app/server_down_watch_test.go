package app

import (
	"testing"
	"time"

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
