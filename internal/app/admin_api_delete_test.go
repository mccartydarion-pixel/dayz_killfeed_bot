package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The delete routes are registered through adminHandle (so the role gate applies), and the
// two that change something are not GETs (so platform staff are refused by adminRoute; the
// route walk in admin_api_staff_test.go proves that for every write).
func TestDeleteRoutesAreRegisteredUnderAdmin(t *testing.T) {
	a, _, _ := newStaffTestApp(t)
	want := map[string]bool{
		"GET /api/admin/organizations/{organizationID}/delete-check": false,
		"POST /api/admin/organizations/{organizationID}/delete":      false,
		"GET /api/admin/installations/{installationID}/delete-check": false,
		"POST /api/admin/installations/{installationID}/delete":      false,
	}
	for _, p := range a.adminRoutes {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("%s is not registered through adminHandle", p)
		}
	}
}

// Nothing outside admin_api_delete.go may call the delete write model or its handlers: an
// organization owner's routes (/api/saas) must have no path to them.
func TestDeleteWriteModelIsOnlyReachableFromTheAdminAPI(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "admin_api_delete.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"DeleteEmptyOrganization(", "DeleteInstallation(", "handleOwnerDeleteOrganization", "handleOwnerDeleteInstallation"} {
			if strings.Contains(string(src), name) {
				t.Errorf("%s references %s; deletes must stay behind /api/admin", f, name)
			}
		}
	}
}

func TestDeleteRequestNeedsReasonAndConfirmation(t *testing.T) {
	_, _, srv := newStaffTestApp(t)
	for name, body := range map[string]map[string]any{
		"no reason":       {"confirm": "Alpha"},
		"no confirmation": {"reason": "accidental"},
		"blank confirm":   {"reason": "accidental", "confirm": "   "},
	} {
		for _, path := range []string{"/api/admin/organizations/1/delete", "/api/admin/installations/1/delete"} {
			rr := adminDo(srv, http.MethodPost, path, adminTestAdmin, body)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s %s: %d %s, want 400", path, name, rr.Code, rr.Body.String())
			}
		}
	}
}

func TestDeleteBlockedMessageListsEveryReason(t *testing.T) {
	msg := deleteBlockedMessage("This community", []repository.DeleteBlocker{
		{Code: repository.DeleteBlockerServerConnected, Message: "It has 1 installation(s) with a game server connected."},
		{Code: repository.DeleteBlockerLiveSubscription, Message: "It has a live paid subscription in Stripe."},
	})
	if msg != "This community cannot be deleted: It has 1 installation(s) with a game server connected. It has a live paid subscription in Stripe." {
		t.Fatalf("message: %q", msg)
	}
}
