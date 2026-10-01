//go:build integration

package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCaseShadowVerdictsReviewScore(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	admin := zoneActor(t, w, "verdict-admin")
	w.mapRole(admin, "verdict-admin-role", "ADMINISTRATOR")
	var player int64
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,'verdict-player','Verdict Player') RETURNING id`, w.guildID).Scan(&player); err != nil {
		t.Fatal(err)
	}
	path := w.path("/case/shadow-verdicts")
	key := func(c string) string { return strings.Repeat(c, 64) }
	save := func(actor, detector, k, verdict string) int {
		return w.call(w.a.handleSaveCaseShadowVerdict, http.MethodPut, path, actor,
			map[string]any{"detectorId": detector, "incidentKey": k, "playerId": player, "verdict": verdict, "note": "  console crash  "}, nil).Code
	}
	type view struct {
		Summary repository.CaseVerdictSummary  `json:"summary"`
		Items   []repository.CaseShadowVerdict `json:"items"`
	}
	read := func(actor, detector string) (int, view) {
		rr := w.call(w.a.handleGetCaseShadowVerdicts, http.MethodGet, path+"?detectorId="+detector, actor, nil, nil)
		if rr.Code != http.StatusOK {
			return rr.Code, view{}
		}
		return rr.Code, decodeBody[view](t, rr)
	}

	if code := save(admin, "CASE-LOGIN-001", key("a"), "FALSE_ALARM"); code != http.StatusOK {
		t.Fatalf("staff could not review logins: %d", code)
	}
	if code := save(w.f.OwnerDiscordID, "CASE-LOGIN-001", key("b"), "SUSPICIOUS"); code != http.StatusOK {
		t.Fatalf("owner could not review logins: %d", code)
	}
	if code := save(admin, "CASE-BASE-001", key("c"), "FALSE_ALARM"); code != http.StatusForbidden {
		t.Fatalf("admin reviewed the owner-only Base Boosting test: %d", code)
	}
	for name, body := range map[string]map[string]any{
		"bad key":       {"detectorId": "CASE-LOGIN-001", "incidentKey": "nope", "playerId": player, "verdict": "FALSE_ALARM"},
		"bad verdict":   {"detectorId": "CASE-LOGIN-001", "incidentKey": key("d"), "playerId": player, "verdict": "BAN"},
		"other module":  {"detectorId": "CASE-TELEPORT-001", "incidentKey": key("d"), "playerId": player, "verdict": "FALSE_ALARM"},
		"unknown field": {"detectorId": "CASE-LOGIN-001", "incidentKey": key("d"), "playerId": player, "verdict": "FALSE_ALARM", "enable": true},
		"long note":     {"detectorId": "CASE-LOGIN-001", "incidentKey": key("d"), "playerId": player, "verdict": "FALSE_ALARM", "note": strings.Repeat("x", 301)},
	} {
		if rr := w.call(w.a.handleSaveCaseShadowVerdict, http.MethodPut, path, w.f.OwnerDiscordID, body, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s accepted: %d", name, rr.Code)
		}
	}
	// Changing a verdict replaces it rather than counting twice.
	if code := save(admin, "CASE-LOGIN-001", key("a"), "SUSPICIOUS"); code != http.StatusOK {
		t.Fatalf("change verdict: %d", code)
	}
	code, got := read(admin, "CASE-LOGIN-001")
	if code != http.StatusOK || got.Summary.Reviewed != 2 || got.Summary.Suspicious != 2 || got.Summary.FalseAlarms != 0 ||
		len(got.Items) != 2 || got.Items[0].Note != "console crash" || got.Summary.FirstReviewAt == nil || got.Summary.DaysReviewing != 0 {
		t.Fatalf("score: %d %+v", code, got)
	}
	if code, _ := read(admin, "CASE-BASE-001"); code != http.StatusForbidden {
		t.Fatalf("admin read Base Boosting review: %d", code)
	}
	if code, got := read(w.f.OwnerDiscordID, "CASE-BASE-001"); code != http.StatusOK || got.Summary.Reviewed != 0 {
		t.Fatalf("base review: %d %+v", code, got)
	}
}
