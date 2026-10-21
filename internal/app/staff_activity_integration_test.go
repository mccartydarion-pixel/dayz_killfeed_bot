//go:build integration

package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestStaffActivity(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	w.a.StaffActivity = repository.NewStaffActivityRepository(w.a.DB.Pool)
	inst := w.f.InstallationID
	record := func(actor, action, result string) {
		t.Helper()
		if err := w.a.AdminAudit.Record(ctx, repository.AuditEntry{OrganizationID: w.f.OrgID, InstallationID: &inst, ActorDiscordID: actor, Action: action, Target: "player:1", Result: result, BeforeState: []byte(`{"secret":"x"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	record("mod-1", "WARNING_ISSUED", "success")
	record("mod-1", "WARNING_ISSUED", "success")
	record("mod-1", "CASE_REVIEW_QUEUE_VIEWED", "success")
	record("admin-1", "ZONE_CREATE", "success")
	record("admin-1", "ZONE_BAN_ADD", "denied")
	record("admin-1", "SOMETHING_NEW", "success")

	get := func(query string) map[string]any {
		t.Helper()
		rr := w.call(w.a.handleStaffActivity, http.MethodGet, w.path("/staff-activity"+query), w.f.OwnerDiscordID, nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, rr.Code, rr.Body.String())
		}
		if s := rr.Body.String(); strings.Contains(s, "secret") {
			t.Fatal("snapshots must never be returned")
		}
		return decodeBody[map[string]any](t, rr)
	}
	all := get("")
	staff := all["staff"].([]any)
	if len(staff) != 2 {
		t.Fatalf("staff: %+v", staff)
	}
	first := staff[0].(map[string]any)
	if first["actions"].(float64) != 3 || first["failed"].(float64) != 1 || first["byCategory"].(map[string]any)["ZONES"].(float64) != 1 {
		t.Fatalf("summary (admin-1 has 3, one failed, the ban counts as moderation): %+v", first)
	}
	count := func(q string) int { return len(get(q)["items"].([]any)) }
	if n := count("?category=MODERATION"); n != 3 {
		t.Fatalf("moderation: %d", n)
	}
	if n := count("?category=ZONES"); n != 1 {
		t.Fatalf("zones excludes zone bans: %d", n)
	}
	if n := count("?category=VIEWS"); n != 1 {
		t.Fatalf("views: %d", n)
	}
	if n := count("?category=OTHER"); n != 1 {
		t.Fatalf("other: %d", n)
	}
	if n := count("?actor=mod-1"); n != 3 {
		t.Fatalf("actor filter: %d", n)
	}
	page := get("?limit=2")
	next := int64(page["nextBefore"].(float64))
	if n := count("?limit=10&before=" + strconv.FormatInt(next, 10)); n != 4 {
		t.Fatalf("paging: %d", n)
	}
	if rr := w.call(w.a.handleStaffActivity, http.MethodGet, w.path("/staff-activity?category=NOPE"), w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad category: %d", rr.Code)
	}
	if rr := w.call(w.a.handleStaffActivity, http.MethodGet, w.path("/staff-activity"), "stranger", nil, nil); rr.Code == http.StatusOK {
		t.Fatal("non-staff read staff activity")
	}
}
