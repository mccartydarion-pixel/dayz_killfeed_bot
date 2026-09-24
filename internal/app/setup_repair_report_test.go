package app

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/discord"
)

// /setup repair on a guild connected to many installations (Champions has 15): every installation's
// layout resolves to the same Discord channels. The report lists each channel once, the run is
// idempotent, and an unchanged route is never rewritten.

func renderAll(r discord.SetupLayoutResult, repair bool) (string, int) {
	e := discord.SetupLayoutEmbed(r, repair)
	var b strings.Builder
	b.WriteString(e.Title + "\n" + e.Description + "\n")
	size := len(e.Title) + len(e.Description)
	for _, f := range e.Fields {
		b.WriteString(f.Name + "\n" + f.Value + "\n")
		size += len(f.Name) + len(f.Value)
		if len(f.Value) > 1024 {
			return "FIELD_TOO_LONG:" + f.Name, size
		}
	}
	if e.Footer != nil {
		size += len(e.Footer.Text)
	}
	return b.String(), size
}

func repairRun(t *testing.T, g *layoutGuildFake, installs []*layoutRoutesFake) discord.SetupLayoutResult {
	t.Helper()
	var out discord.SetupLayoutResult
	for i, w := range installs {
		res, err := applyChannelLayout(context.Background(), g, w, channelLayoutInput{OrganizationID: 1, InstallationID: int64(i + 1), GuildID: "g",
			Existing: w.existing(), Producers: auditProducers(), Preserve: true, SyncPanels: panelsPosted(g, w)})
		if err != nil {
			t.Fatal(err)
		}
		out.Installations++
		addLayoutToSetupResult(&out, AutoSetupChannelsResponse{Destinations: res.Destinations, Retirable: res.Retirable, Summary: &res.Summary})
	}
	return out
}

func TestSetupRepairReportsEachChannelOnceAcrossInstallations(t *testing.T) {
	g := newLayoutGuildFake()
	installs := make([]*layoutRoutesFake, 15)
	for i := range installs {
		installs[i] = &layoutRoutesFake{routes: map[string]string{}}
	}

	first := repairRun(t, g, installs)
	chans := first.Channels()
	seen := map[string]bool{}
	for _, c := range chans {
		if seen[c.ID] {
			t.Fatalf("channel %s reported twice", c.ID)
		}
		seen[c.ID] = true
	}
	if len(chans) != g.createCalls-len(championCategories) {
		t.Fatalf("one report entry per real channel: %d entries, %d channels created", len(chans), g.createCalls-len(championCategories))
	}
	if first.Count(discord.SetupChannelCreated) != len(chans) || first.Count(discord.SetupChannelReused) != 0 {
		t.Fatalf("first run: every channel created once, none double-counted as reused: created=%d reused=%d",
			first.Count(discord.SetupChannelCreated), first.Count(discord.SetupChannelReused))
	}

	// Repeated repairs: nothing created, nothing rewritten, identical report every time.
	creates := g.createCalls
	for _, w := range installs {
		w.upserts = 0
	}
	var prev string
	for run := 0; run < 3; run++ {
		again := repairRun(t, g, installs)
		if g.createCalls != creates {
			t.Fatalf("run %d created %d duplicate channels", run, g.createCalls-creates)
		}
		for i, w := range installs {
			if w.upserts != 0 {
				t.Fatalf("run %d: installation %d rewrote %d unchanged routes", run, i+1, w.upserts)
			}
		}
		if again.Count(discord.SetupChannelReused) != len(chans) || again.Count(discord.SetupChannelCreated) != 0 || len(again.FailedSystems()) != 0 {
			t.Fatalf("run %d summary: reused=%d created=%d failed=%v", run, again.Count(discord.SetupChannelReused), again.Count(discord.SetupChannelCreated), again.FailedSystems())
		}
		text, size := renderAll(again, true)
		if prev != "" && text != prev {
			t.Fatalf("the repair report must be identical on every idempotent run:\n%s\n---\n%s", prev, text)
		}
		prev = text
		if size > 6000 || strings.HasPrefix(text, "FIELD_TOO_LONG") {
			t.Fatalf("embed exceeds Discord limits (%d): %s", size, text)
		}
		for _, name := range []string{"🔫・combat-feed", "hitfeed", "Online:"} {
			if strings.Contains(text, name) {
				t.Fatalf("no channel names or counter names are listed: %q in\n%s", name, text)
			}
		}
		for _, sys := range again.VerifiedSystems() {
			if n := strings.Count(text, "• "+sys+"\n") + strings.Count(text, "• "+sys+"\x00"); n > 1 {
				t.Fatalf("system %q listed %d times", sys, n)
			}
		}
		for _, want := range []string{"CHAMPIONS® DISCORD REPAIR", "Repair Complete", "Created: 0", "Reused: " + strconv.Itoa(len(chans)), "Updated: 0", "Failed: 0", "Combat Feed", "All required systems are configured."} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q:\n%s", want, text)
			}
		}
	}
}

func TestSetupRepairUpdatedAndLegacyAreDeduplicated(t *testing.T) {
	var r discord.SetupLayoutResult
	// Two installations report the same channel: one reused it, one moved a route onto it.
	r.AddChannel(discord.SetupChannel{ID: "c1", Name: "🔫・combat-feed", System: "Combat Feed", Outcome: discord.SetupChannelReused})
	r.AddChannel(discord.SetupChannel{ID: "c1", Name: "🔫・combat-feed", System: "Combat Feed", Outcome: discord.SetupChannelUpdated})
	r.AddVerifiedSystem("Combat Feed")
	r.AddVerifiedSystem("Combat Feed")
	for i := 0; i < 15; i++ {
		r.AddLegacy("old-1") // the same unused channel seen by every installation
	}
	if len(r.Channels()) != 1 || r.Count(discord.SetupChannelUpdated) != 1 || r.Count(discord.SetupChannelReused) != 0 {
		t.Fatalf("one channel, strongest outcome: %+v", r.Channels())
	}
	if r.LegacyChannels() != 1 || len(r.VerifiedSystems()) != 1 {
		t.Fatalf("legacy and systems deduplicated: legacy=%d systems=%v", r.LegacyChannels(), r.VerifiedSystems())
	}
	text, _ := renderAll(r, true)
	if !strings.Contains(text, "1 unused Champion channel detected.") || !strings.Contains(text, "Website → Setup → Discord Channels") {
		t.Fatalf("legacy notice:\n%s", text)
	}
	// A failure in any installation wins and is never also listed as verified.
	r.AddChannel(discord.SetupChannel{ID: "c1", System: "Combat Feed", Outcome: discord.SetupChannelFailed})
	text, _ = renderAll(r, true)
	if !strings.Contains(text, "Needs Attention") || strings.Contains(text, "Systems Verified") || !strings.Contains(text, "Failed: 1") {
		t.Fatalf("failure reported once, not as verified:\n%s", text)
	}
}
