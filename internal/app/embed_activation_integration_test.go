//go:build integration

package app

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// Runtime activation through the real API and the real killfeed path (PostgreSQL): saving never
// makes a template live; Custom Embed does; Champion Default restores the untouched default card
// without touching the saved template; unsupported routes and missing templates are refused with
// their reason; one kill is always exactly one card.
func TestKillfeedActivationEndToEnd(t *testing.T) {
	w := newRuntimeWorld(t, time.Hour)
	q, cap := w.server(w.serverA, w.renderer)
	owner := w.fixture.OwnerDiscordID

	// Saved, not activated: still the default card, and the API says so.
	if rr := w.admin.put(w.fixture.OrgID, w.fixture.InstallationID, "KILLFEED", owner, killTemplateBody("SAVED")); rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	var saved EmbedTemplateResponse
	rr := w.admin.get(w.fixture.OrgID, w.fixture.InstallationID, "KILLFEED", owner)
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Activation.TemplateSaved || saved.Activation.Mode != "DEFAULT" || saved.Activation.Runtime != embedRuntimeDefault || !saved.Activation.CanActivate {
		t.Fatalf("saved / selected / runtime reported separately: %+v", saved.Activation)
	}
	w.kill(q, "11:00:01")
	if card, ev := cap.last(); !isDefaultKillCard(card, ev) {
		t.Fatal("a saved template is not used before it is activated")
	}

	// Activate: the next kill is the custom card.
	rr = w.admin.activate(w.fixture.OrgID, w.fixture.InstallationID, "KILLFEED", owner, "CUSTOM")
	var st EmbedActivationDTO
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &st) != nil || st.Runtime != embedRuntimeActive {
		t.Fatalf("activate: %d %s", rr.Code, rr.Body.String())
	}
	w.kill(q, "11:00:02")
	if card, ev := cap.last(); isDefaultKillCard(card, ev) || card.Title != "SAVED Hunter eliminated Target" {
		t.Fatalf("custom card after activation: %+v", card)
	}

	// Deactivate: the untouched default card, and the template is still saved.
	if rr := w.admin.activate(w.fixture.OrgID, w.fixture.InstallationID, "KILLFEED", owner, "DEFAULT"); rr.Code != http.StatusOK {
		t.Fatalf("deactivate: %d", rr.Code)
	}
	w.kill(q, "11:00:03")
	if card, ev := cap.last(); !isDefaultKillCard(card, ev) {
		t.Fatal("Champion Default restores the original card")
	}
	if rr := w.admin.get(w.fixture.OrgID, w.fixture.InstallationID, "KILLFEED", owner); rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &saved) != nil || !saved.Customized {
		t.Fatal("deactivation keeps the saved template")
	}
	if cap.count() != 3 || w.killsPersisted(w.serverA) != 3 {
		t.Fatalf("one kill = one card, no duplicates: cards=%d rows=%d", cap.count(), w.killsPersisted(w.serverA))
	}

	// Refusals carry the reason.
	if rr := w.admin.activate(w.fixture.OrgID, w.fixture.InstallationID, "SERVER_STATUS", owner, "CUSTOM"); rr.Code == http.StatusOK {
		t.Fatalf("an unsupported route cannot be activated: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.admin.activate(w.fixture.OrgID, w.fixture.InstallationID, "HITFEED", owner, "CUSTOM"); rr.Code != http.StatusConflict {
		t.Fatalf("no saved template: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.admin.activate(w.fixture.OrgID, w.fixture.InstallationID, "KILLFEED", owner, "LIVE"); rr.Code != http.StatusBadRequest {
		t.Fatalf("an unknown mode is rejected: %d", rr.Code)
	}
}
