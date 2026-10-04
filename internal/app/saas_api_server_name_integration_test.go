//go:build integration

package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// PUT /server/name (docs/SERVER_NAME_SYNC.md): a typed name is custom and the Nitrado sync leaves
// it alone; the Nitrado name itself, or an empty name, hands the name back to the sync.
func TestSetServerNameMarksCustomAndClearsIt(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	names := repository.NewServerNameRepository(w.a.DB.Pool)
	put := func(name string) (int, map[string]any) {
		t.Helper()
		rr := w.call(w.a.handleSetServerName, http.MethodPut, w.path("/server/name"), w.f.OwnerDiscordID, setServerNameRequest{Name: name}, nil)
		if rr.Code != http.StatusOK {
			return rr.Code, nil
		}
		return rr.Code, decodeBody[map[string]any](t, rr)
	}

	// No Nitrado name known yet: an empty name is refused as before.
	if code, _ := put("   "); code != http.StatusBadRequest {
		t.Fatalf("empty name without a Nitrado name: got %d", code)
	}
	if code, _ := put(string(make([]byte, 101))); code != http.StatusBadRequest {
		t.Fatalf("an over-long name must be refused, got %d", code)
	}

	code, body := put("  Owner's pick ")
	if code != http.StatusOK || body["name"] != "Owner's pick" || body["displayNameCustom"] != true || body["providerName"] != nil {
		t.Fatalf("rename: %d %v", code, body)
	}
	// The sync records the Nitrado name and does not overwrite the owner's.
	if changed, err := names.RecordProviderName(ctx, w.serverID, "Nitrado name"); err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	srv, err := w.a.SaaSServers.GetScoped(ctx, w.f.OrgID, w.serverID)
	if err != nil || srv == nil {
		t.Fatalf("get server: %v", err)
	}
	summary := toDayZServerSummary(*srv)
	if summary.DisplayName != "Owner's pick" || !summary.DisplayNameCustom || summary.ProviderName == nil || *summary.ProviderName != "Nitrado name" {
		t.Fatalf("summary: %+v", summary)
	}

	// Typing the Nitrado name is not custom.
	code, body = put("Nitrado name")
	if code != http.StatusOK || body["displayNameCustom"] != false || body["providerName"] != "Nitrado name" {
		t.Fatalf("nitrado name: %d %v", code, body)
	}
	// Custom again, then cleared: back to the Nitrado name.
	if code, body = put("Second pick"); code != http.StatusOK || body["displayNameCustom"] != true {
		t.Fatalf("second rename: %d %v", code, body)
	}
	code, body = put("")
	if code != http.StatusOK || body["name"] != "Nitrado name" || body["displayNameCustom"] != false {
		t.Fatalf("clear: %d %v", code, body)
	}
	if changed, err := names.RecordProviderName(ctx, w.serverID, "Nitrado renamed"); err != nil || !changed {
		t.Fatalf("after a clear the sync follows Nitrado again: changed=%v err=%v", changed, err)
	}

	var audits int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='SERVER_NAME_EDIT' AND result='success'`, w.f.InstallationID).Scan(&audits); err != nil || audits != 4 {
		t.Fatalf("expected 4 audited renames, got %d (%v)", audits, err)
	}
}
