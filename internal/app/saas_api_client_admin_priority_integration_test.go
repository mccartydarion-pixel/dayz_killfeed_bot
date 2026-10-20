//go:build integration

package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// fakeNitradoPriority is a Nitrado that holds one priority list: the gameserver read returns it,
// the settings write replaces it. present=false models a service with no priority setting.
type fakeNitradoPriority struct {
	mu      sync.Mutex
	present bool
	value   string
	writes  []map[string]string
}

func withFakeNitradoPriority(t *testing.T, a *App, present bool, value string) *fakeNitradoPriority {
	t.Helper()
	fake := &fakeNitradoPriority{present: present, value: value}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/services":
			_, _ = w.Write([]byte(psServiceJSON))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gameservers"):
			general := map[string]string{"expertMode": "false"}
			if fake.present {
				general["priority"] = fake.value
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"gameserver": map[string]any{"settings": map[string]any{"general": general}}}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/gameservers/settings"):
			body, _ := io.ReadAll(r.Body)
			var sent map[string]string
			_ = json.Unmarshal(body, &sent)
			fake.writes = append(fake.writes, sent)
			fake.value = sent["value"]
			_, _ = w.Write([]byte(`{"status":"success"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	a.saasNitradoClientFactory = func(token string) *nitrado.Client { return nitrado.NewClient(srv.URL, token, nil) }
	return fake
}

func TestPriorityListAddRemoveRoundTrip(t *testing.T) {
	w := newClientAdminWorld(t)
	fake := withFakeNitradoPriority(t, w.a, true, "Alpha\r\nBravo")
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	listRR := w.call(w.a.handleListPriority, http.MethodGet, w.path("/priority"), w.f.OwnerDiscordID, nil, nil)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list priority failed: %d %s", listRR.Code, listRR.Body.String())
	}
	list := decodeBody[priorityListDTO](t, listRR)
	if !list.Supported || strings.Join(list.Items, ",") != "Alpha,Bravo" || list.Limit != nitrado.MaxPriorityEntries {
		t.Fatalf("unexpected list: %+v", list)
	}

	// Adding keeps what Nitrado already held: the write replaces the whole value.
	addRR := w.call(w.a.handleAddPriority, http.MethodPost, w.path("/priority"), w.f.OwnerDiscordID, priorityRequest{Name: "  Charlie ", Reason: "donor"}, nil)
	if addRR.Code != http.StatusCreated {
		t.Fatalf("add priority failed: %d %s", addRR.Code, addRR.Body.String())
	}
	if len(fake.writes) != 1 || fake.writes[0]["category"] != "general" || fake.writes[0]["key"] != "priority" || fake.writes[0]["value"] != "Alpha\r\nBravo\r\nCharlie" {
		t.Fatalf("unexpected Nitrado write: %v", fake.writes)
	}

	// A name already listed (in any case) is not written again.
	dupRR := w.call(w.a.handleAddPriority, http.MethodPost, w.path("/priority"), w.f.OwnerDiscordID, priorityRequest{Name: "ALPHA"}, nil)
	if dupRR.Code != http.StatusOK || len(fake.writes) != 1 {
		t.Fatalf("a duplicate must not write: %d, writes=%d", dupRR.Code, len(fake.writes))
	}

	remRR := w.call(w.a.handleRemovePriority, http.MethodDelete, w.path("/priority/bravo"), w.f.OwnerDiscordID, nil, map[string]string{"name": "bravo"})
	if remRR.Code != http.StatusOK {
		t.Fatalf("remove priority failed: %d %s", remRR.Code, remRR.Body.String())
	}
	if got := decodeBody[priorityListDTO](t, remRR); strings.Join(got.Items, ",") != "Alpha,Charlie" {
		t.Fatalf("unexpected list after removal: %+v", got)
	}
	if fake.value != "Alpha\r\nCharlie" {
		t.Fatalf("Nitrado should hold the list without Bravo, got %q", fake.value)
	}

	// Removing a name that is not there succeeds without a write.
	goneRR := w.call(w.a.handleRemovePriority, http.MethodDelete, w.path("/priority/nobody"), w.f.OwnerDiscordID, nil, map[string]string{"name": "nobody"})
	if goneRR.Code != http.StatusOK || len(fake.writes) != 2 {
		t.Fatalf("removing an absent name must not write: %d, writes=%d", goneRR.Code, len(fake.writes))
	}

	auditRR := w.call(w.a.handleListAuditLog, http.MethodGet, w.path("/audit-log"), w.f.OwnerDiscordID, nil, nil)
	audit := auditRR.Body.String()
	if !strings.Contains(audit, "PRIORITY_ADD") || !strings.Contains(audit, "PRIORITY_REMOVE") {
		t.Fatalf("both changes must be audited, got %s", audit)
	}
}

func TestPriorityRejectsBadNamesWithoutCallingNitrado(t *testing.T) {
	w := newClientAdminWorld(t)
	fake := withFakeNitradoPriority(t, w.a, true, "Alpha")
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	for _, name := range []string{"", "   ", "two\nlines", strings.Repeat("a", 65)} {
		rr := w.call(w.a.handleAddPriority, http.MethodPost, w.path("/priority"), w.f.OwnerDiscordID, priorityRequest{Name: name}, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("name %q should be rejected, got %d %s", name, rr.Code, rr.Body.String())
		}
	}
	if len(fake.writes) != 0 || fake.value != "Alpha" {
		t.Fatalf("a rejected name must never reach Nitrado: %v", fake.writes)
	}
}

func TestPriorityNeverWritesWhenTheServiceHasNoPrioritySetting(t *testing.T) {
	w := newClientAdminWorld(t)
	fake := withFakeNitradoPriority(t, w.a, false, "")
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	listRR := w.call(w.a.handleListPriority, http.MethodGet, w.path("/priority"), w.f.OwnerDiscordID, nil, nil)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list should still answer: %d %s", listRR.Code, listRR.Body.String())
	}
	if list := decodeBody[priorityListDTO](t, listRR); list.Supported || len(list.Items) != 0 {
		t.Fatalf("expected an unsupported, empty list, got %+v", list)
	}
	addRR := w.call(w.a.handleAddPriority, http.MethodPost, w.path("/priority"), w.f.OwnerDiscordID, priorityRequest{Name: "Alpha"}, nil)
	if addRR.Code == http.StatusCreated || addRR.Code == http.StatusOK {
		t.Fatalf("add must fail when the setting is absent, got %d", addRR.Code)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("nothing may be written to a service whose list could not be read: %v", fake.writes)
	}
}

func TestPriorityNeedsAdministrator(t *testing.T) {
	w := newClientAdminWorld(t)
	fake := withFakeNitradoPriority(t, w.a, true, "Alpha")
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	modUser := syncUser(t, w.a, fmt.Sprintf("ca-prio-mod-%d", time.Now().UnixNano()), "Mod")
	roleID := "discord-role-priority-moderator"
	if w.verifier.memberRoles == nil {
		w.verifier.memberRoles = map[string]map[string][]string{}
	}
	if w.verifier.memberRoles[w.f.DiscordGuildID] == nil {
		w.verifier.memberRoles[w.f.DiscordGuildID] = map[string][]string{}
	}
	w.verifier.memberRoles[w.f.DiscordGuildID][modUser.DiscordUserID] = []string{roleID}
	setRR := w.call(w.a.handleSetPermission, http.MethodPut, w.path("/permissions/"+roleID), w.f.OwnerDiscordID, setPermissionRequest{Level: "MODERATOR"}, map[string]string{"discordRoleID": roleID})
	if setRR.Code != http.StatusOK {
		t.Fatalf("mapping the moderator role failed: %d %s", setRR.Code, setRR.Body.String())
	}

	for name, rr := range map[string]*httptest.ResponseRecorder{
		"list":   w.call(w.a.handleListPriority, http.MethodGet, w.path("/priority"), modUser.DiscordUserID, nil, nil),
		"add":    w.call(w.a.handleAddPriority, http.MethodPost, w.path("/priority"), modUser.DiscordUserID, priorityRequest{Name: "Bravo"}, nil),
		"remove": w.call(w.a.handleRemovePriority, http.MethodDelete, w.path("/priority/Alpha"), modUser.DiscordUserID, nil, map[string]string{"name": "Alpha"}),
	} {
		if rr.Code != http.StatusForbidden {
			t.Fatalf("a Moderator must not %s the priority list, got %d %s", name, rr.Code, rr.Body.String())
		}
	}
	if len(fake.writes) != 0 {
		t.Fatalf("a forbidden request must never reach Nitrado: %v", fake.writes)
	}
}
