//go:build integration

package app

import (
	"net/http"
	"testing"
)

func TestCaseSetupChecklist(t *testing.T) {
	w := newClientAdminWorld(t)
	admin := zoneActor(t, w, "setup-admin")
	w.mapRole(admin, "setup-admin-role", "ADMINISTRATOR")
	path := w.path("/case/setup")
	if rr := w.call(w.a.handleGetCaseSetup, http.MethodGet, path, admin, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin: %d", rr.Code)
	}
	first := w.call(w.a.handleGetCaseSetup, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("get: %d %s", first.Code, first.Body.String())
	}
	got := decodeBody[caseSetupResponse](t, first)
	keys := []string{"discord", "feed", "clock", "evidence", "alerts", "detectors"}
	if len(got.Steps) != len(keys) {
		t.Fatalf("steps: %+v", got.Steps)
	}
	for i, k := range keys {
		if got.Steps[i].Key != k || got.Steps[i].State == "" || got.Steps[i].Detail == "" {
			t.Fatalf("step %d: %+v", i, got.Steps[i])
		}
	}
	if got.Steps[2].State != setupWait || got.Steps[4].State != setupAction || got.Steps[5].State != setupTesting || len(got.Detectors) == 0 {
		t.Fatalf("clock not learned, alerts off, detectors in testing: %+v", got.Steps)
	}
	if got.Evidence.Running || got.Evidence.SelfServe {
		t.Fatalf("evidence: %+v", got.Evidence)
	}

	evPath := w.path("/case/setup/evidence")
	t.Setenv("CASE_EVIDENCE_ENABLED", "true")
	t.Setenv("CASE_EVIDENCE_SELF_SERVE", "")
	if rr := w.call(w.a.handleSetCaseEvidence, http.MethodPut, evPath, w.f.OwnerDiscordID, map[string]any{"enabled": true}, nil); rr.Code != http.StatusConflict {
		t.Fatalf("self-serve off must refuse: %d", rr.Code)
	}
	t.Setenv("CASE_EVIDENCE_SELF_SERVE", "true")
	if rr := w.call(w.a.handleSetCaseEvidence, http.MethodPut, evPath, admin, map[string]any{"enabled": true}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin set: %d", rr.Code)
	}
	if rr := w.call(w.a.handleSetCaseEvidence, http.MethodPut, evPath, w.f.OwnerDiscordID, map[string]any{"enabled": true, "x": 1}, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rr.Code)
	}
	if rr := w.call(w.a.handleSetCaseEvidence, http.MethodPut, evPath, w.f.OwnerDiscordID, map[string]any{"enabled": true}, nil); rr.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rr.Code, rr.Body.String())
	}
	after := decodeBody[caseSetupResponse](t, w.call(w.a.handleGetCaseSetup, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if !after.Evidence.OwnerChoice || !after.Evidence.SelfServe || after.Evidence.Running || after.Steps[3].Action != "TOGGLE_EVIDENCE" {
		t.Fatalf("owner choice saved, not running until restart: %+v %+v", after.Evidence, after.Steps[3])
	}
	var audits int
	if err := w.a.DB.Pool.QueryRow(t.Context(), `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='CASE_EVIDENCE_OPTIN_SAVED'`, w.f.InstallationID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audits: %d %v", audits, err)
	}
}
