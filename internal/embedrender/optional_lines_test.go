package embedrender

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

// Optional description lines: a line that uses an absent variable is omitted entirely - no empty
// "Hit:" / "Damage:" labels - while the rest of the description is kept. Preview (report), test
// send and live rendering all go through the same Render.

func hitTemplate() embedtemplates.Config {
	cfg := killTemplate()
	cfg.Description.Template = "**{{killer}}** eliminated **{{victim}}**\nWeapon: {{weapon}}\nHit: {{hit_zone}} | Damage: {{damage}}\n\nDistance: {{distance}}"
	cfg.Footer.Text = "Champion Killfeed\non {{server_name}}"
	return cfg
}

func TestLineWithAbsentHitDataIsOmittedEntirely(t *testing.T) {
	v := full() // no hit_zone, no damage (a standard kill without a correlated lethal hit)
	e, rep, err := RenderWithReport(hitTemplate(), "KILLFEED", v, at)
	if err != nil {
		t.Fatal(err)
	}
	want := "**Alice** eliminated **Bob**\nWeapon: M4-A1\n\nDistance: 87m"
	if e.Description != want {
		t.Fatalf("description:\n%q\nwant\n%q", e.Description, want)
	}
	if strings.Contains(e.Description, "Hit:") || strings.Contains(e.Description, "Damage:") {
		t.Fatal("no empty hit/damage labels")
	}
	if rep.OmittedLines != 1 || !reflect.DeepEqual(rep.AbsentVariables, []string{"damage", "hit_zone"}) {
		t.Fatalf("report: %+v", rep)
	}
}

func TestLineWithHitDataIsKeptWhenPresent(t *testing.T) {
	v := full()
	v["hit_zone"], v["damage"] = "Head", "22.1"
	e := mustRender(t, hitTemplate(), v)
	if !strings.Contains(e.Description, "Hit: Head | Damage: 22.1") {
		t.Fatalf("real hit data is shown: %q", e.Description)
	}
	// One of the two present, one absent: the line still omits (never "Damage: " alone).
	delete(v, "damage")
	if e := mustRender(t, hitTemplate(), v); strings.Contains(e.Description, "Hit:") {
		t.Fatalf("a partially available line is omitted: %q", e.Description)
	}
}

func TestFooterLinesFollowTheSameRule(t *testing.T) {
	v := full()
	delete(v, "server_name")
	e := mustRender(t, hitTemplate(), v)
	if e.Footer == nil || e.Footer.Text != "Champion Killfeed" {
		t.Fatalf("footer keeps its other lines: %+v", e.Footer)
	}
}

func TestEscapedNewlineIsNotExpandedTwice(t *testing.T) {
	cfg := killTemplate()
	cfg.Description.Template = `Path: a\\nb {{weapon}}`
	e := mustRender(t, cfg, full())
	if e.Description != `Path: a\nb M4-A1` {
		t.Fatalf("an escaped \\n stays literal through line splitting: %q", e.Description)
	}
}

func TestPreviewReportAndLiveRenderAgree(t *testing.T) {
	v := full()
	live, err := RenderEvent(hitTemplate(), "KILLFEED", v, at)
	if err != nil {
		t.Fatal(err)
	}
	preview, _, err := RenderEventWithReport(hitTemplate(), "KILLFEED", v, at)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live, preview) {
		t.Fatalf("preview and live differ:\n%+v\n%+v", live, preview)
	}
}
