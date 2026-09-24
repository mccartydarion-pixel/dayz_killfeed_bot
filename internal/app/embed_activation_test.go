package app

import (
	"context"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func validKillTemplate(enabled bool) *embedtemplates.Stored {
	return &embedtemplates.Stored{Config: embedtemplates.Config{Enabled: enabled, RouteKey: "KILLFEED", Color: "#D4AF37",
		Title: embedtemplates.Text{Enabled: true, Template: "{{killer}} eliminated {{victim}}"}, Fields: []embedtemplates.Field{}}}
}

// The three separate states - saved, selected, runtime - and the four conditions for ACTIVE.
func TestEmbedActivationStatusMatrix(t *testing.T) {
	on := &App{EmbedRenderer: embedrender.New(embedrender.Options{Source: nilSource{}, Enabled: true})}
	off := &App{} // the operator rollout switch is off (no renderer wired)

	cases := []struct {
		name    string
		app     *App
		route   string
		stored  *embedtemplates.Stored
		mode    string
		runtime string
		reason  string
		can     bool
	}{
		{"custom, everything in place", on, "KILLFEED", validKillTemplate(true), repository.EmbedModeCustom, embedRuntimeActive, "", true},
		{"default selected", on, "KILLFEED", validKillTemplate(true), repository.EmbedModeDefault, embedRuntimeDefault, "", true},
		{"no selection row = default", on, "KILLFEED", validKillTemplate(true), "", embedRuntimeDefault, "", true},
		{"global switch off", off, "KILLFEED", validKillTemplate(true), repository.EmbedModeCustom, embedRuntimeBlocked, embedReasonGlobalDisabled, true},
		{"template disabled", on, "KILLFEED", validKillTemplate(false), repository.EmbedModeCustom, embedRuntimeBlocked, embedReasonTemplateDisabled, false},
		{"nothing saved", on, "KILLFEED", nil, repository.EmbedModeCustom, embedRuntimeBlocked, embedReasonNoTemplate, false},
		{"unsupported route", on, "SERVER_STATUS", nil, repository.EmbedModeDefault, embedRuntimeDefault, "", false},
	}
	for _, c := range cases {
		st := c.app.embedActivationStatus(c.route, c.stored, c.mode)
		if st.Runtime != c.runtime || st.BlockedReason != c.reason || st.CanActivate != c.can {
			t.Errorf("%s: got runtime=%s reason=%s can=%v (%+v)", c.name, st.Runtime, st.BlockedReason, st.CanActivate, st)
		}
		if st.TemplateSaved != (c.stored != nil) {
			t.Errorf("%s: templateSaved is reported separately", c.name)
		}
	}
	if st := on.embedActivationStatus("SERVER_STATUS", nil, ""); st.ActivationUnavailable != embedReasonRouteUnsupported || st.RouteSupported {
		t.Fatalf("an unsupported route cannot be activated and says why: %+v", st)
	}
	invalid := validKillTemplate(true)
	invalid.Config.Color = "not-a-color"
	if st := on.embedActivationStatus("KILLFEED", invalid, repository.EmbedModeCustom); st.TemplateValid || st.BlockedReason != embedReasonTemplateInvalid {
		t.Fatalf("an invalid stored template is never active: %+v", st)
	}
}

// nilSource is an embedrender.Source with no templates (only Enabled() matters here).
type nilSource struct{}

func (nilSource) ResolveTemplate(context.Context, int64, int64, string) (int64, *embedtemplates.Config, error) {
	return 0, nil, nil
}
