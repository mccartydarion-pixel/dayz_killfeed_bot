package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/server"
)

const (
	adminTestStaff    = "900000000000000002"
	adminTestStranger = "900000000000000003"
)

// fakeStaffStore is an in-memory platform_staff table.
type fakeStaffStore struct {
	mu      sync.Mutex
	members map[string]repository.PlatformStaffMember
	err     error
	writes  int
}

func newFakeStaffStore(ids ...string) *fakeStaffStore {
	f := &fakeStaffStore{members: map[string]repository.PlatformStaffMember{}}
	for _, id := range ids {
		f.members[id] = repository.PlatformStaffMember{DiscordID: id, AddedBy: adminTestAdmin, AddedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	}
	return f
}

func (f *fakeStaffStore) ListPlatformStaff(context.Context) ([]repository.PlatformStaffMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []repository.PlatformStaffMember{}
	for _, m := range f.members {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DiscordID < out[j].DiscordID })
	return out, f.err
}

func (f *fakeStaffStore) GetPlatformStaff(_ context.Context, id string) (*repository.PlatformStaffMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if m, ok := f.members[id]; ok {
		return &m, nil
	}
	return nil, nil
}

func (f *fakeStaffStore) UpsertPlatformStaff(_ context.Context, id, note, addedBy string) (*repository.PlatformStaffMember, repository.PlatformStaffMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if old, ok := f.members[id]; ok {
		before := old
		old.Note = note
		f.members[id] = old
		return &before, old, nil
	}
	m := repository.PlatformStaffMember{DiscordID: id, Note: note, AddedBy: addedBy, AddedAt: time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC)}
	f.members[id] = m
	return nil, m, nil
}

func (f *fakeStaffStore) RemovePlatformStaff(_ context.Context, id string) (repository.PlatformStaffMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	m, ok := f.members[id]
	if !ok {
		return m, repository.ErrPlatformStaffNotFound
	}
	delete(f.members, id)
	return m, nil
}

func newStaffTestApp(t *testing.T) (*App, *fakeStaffStore, *server.Server) {
	t.Helper()
	srv, err := server.New(&config.Config{Port: "0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := newAdminTestApp(adminTestAdmin)
	store := newFakeStaffStore(adminTestStaff)
	a.platformStaff = store
	a.HTTPServer = srv
	a.registerAdminAPI()
	return a, store, srv
}

func adminDo(srv *server.Server, method, path, acting string, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+adminTestSecret)
	req.Header.Set("Content-Type", "application/json")
	if acting != "" {
		req.Header.Set(actingUserHeader, acting)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func adminErrorOf(t *testing.T, rr *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("not an error body: %s", rr.Body.String())
	}
	return body.Error.Code, body.Error.Message
}

var adminPathParam = regexp.MustCompile(`\{[^}]+\}`)

// The structural guarantee: every route registered under /api/admin is walked, and every one
// that is not a GET refuses a platform staff identity with the fixed message - whatever its
// handler does, and including routes added after this test was written.
func TestEveryAdminWriteRouteRefusesPlatformStaff(t *testing.T) {
	a, store, srv := newStaffTestApp(t)
	if len(a.adminRoutes) < 40 {
		t.Fatalf("only %d admin routes were recorded; registration is not going through adminHandle", len(a.adminRoutes))
	}
	seen := map[string]bool{}
	writes, reads := 0, 0
	for _, pattern := range a.adminRoutes {
		if seen[pattern] {
			t.Fatalf("admin route %q is registered twice", pattern)
		}
		seen[pattern] = true
		method, path, _ := strings.Cut(pattern, " ")
		// A flag key the route knows and a snowflake-shaped id, so the request reaches as far as it can.
		path = strings.ReplaceAll(path, "{flag}", "map_rotation")
		path = strings.ReplaceAll(path, "{discordID}", "900000000000000004")
		path = adminPathParam.ReplaceAllString(path, "1")
		body := map[string]any{"reason": "walking the routes", "discordId": "900000000000000004", "enabled": true}

		stranger := adminDo(srv, method, path, adminTestStranger, body)
		if stranger.Code != http.StatusForbidden {
			t.Errorf("%s as a stranger: %d, want 403", pattern, stranger.Code)
		}
		anonymous := adminDo(srv, method, path, "", body)
		if anonymous.Code != http.StatusUnauthorized {
			t.Errorf("%s with no acting user: %d, want 401", pattern, anonymous.Code)
		}

		staff := adminDo(srv, method, path, adminTestStaff, body)
		if method == http.MethodGet {
			reads++
			if staff.Code == http.StatusForbidden || staff.Code == http.StatusUnauthorized {
				t.Errorf("%s as staff: %d %s - staff may read everything", pattern, staff.Code, staff.Body.String())
			}
			continue
		}
		writes++
		if staff.Code != http.StatusForbidden {
			t.Errorf("%s as staff: %d %s, want 403", pattern, staff.Code, staff.Body.String())
			continue
		}
		if code, msg := adminErrorOf(t, staff); code != codeForbidden || msg != adminStaffWriteDenied {
			t.Errorf("%s as staff: %s %q, want FORBIDDEN %q", pattern, code, msg, adminStaffWriteDenied)
		}
		// The same request from the owner gets past the role gate (whatever the handler then says).
		owner := adminDo(srv, method, path, adminTestAdmin, body)
		if owner.Code == http.StatusForbidden || owner.Code == http.StatusUnauthorized {
			t.Errorf("%s as owner: %d %s - the owner must get past the role gate", pattern, owner.Code, owner.Body.String())
		}
	}
	if writes < 20 || reads < 20 {
		t.Fatalf("walked %d writes and %d reads; expected the whole admin surface", writes, reads)
	}
	if _, stillThere := store.members[adminTestStaff]; !stillThere {
		t.Fatal("the staff member must still be on the list")
	}
	// A method no route declares can never be a way in either.
	for _, method := range []string{http.MethodPatch, http.MethodPut, http.MethodDelete, http.MethodPost} {
		if rr := adminDo(srv, method, "/api/admin/overview", adminTestStaff, nil); rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/admin/overview as staff: %d, want 405", method, rr.Code)
		}
	}
}

// The rule is in the wrapper, not in the handlers: a write handler wrapped by adminRoute never
// runs for staff, for any non-read method.
func TestAdminRouteRunsOnlyReadsForStaff(t *testing.T) {
	a, _ := newAdminTestApp(adminTestAdmin)
	a.platformStaff = newFakeStaffStore(adminTestStaff)
	for _, tc := range []struct {
		method, acting string
		wantRun        bool
		wantRole       string
	}{
		{http.MethodGet, adminTestStaff, true, adminRoleStaff},
		{http.MethodHead, adminTestStaff, true, adminRoleStaff},
		{http.MethodPost, adminTestStaff, false, ""},
		{http.MethodPut, adminTestStaff, false, ""},
		{http.MethodPatch, adminTestStaff, false, ""},
		{http.MethodDelete, adminTestStaff, false, ""},
		{http.MethodOptions, adminTestStaff, false, ""},
		{"PURGE", adminTestStaff, false, ""},
		{http.MethodGet, adminTestAdmin, true, adminRoleOwner},
		{http.MethodPost, adminTestAdmin, true, adminRoleOwner},
		{http.MethodDelete, adminTestAdmin, true, adminRoleOwner},
		{http.MethodGet, adminTestStranger, false, ""},
		{http.MethodPost, adminTestStranger, false, ""},
	} {
		ran, role := false, ""
		h := a.adminRoute(func(w http.ResponseWriter, _ *http.Request, admin adminIdentity) {
			ran, role = true, admin.Role
			w.WriteHeader(http.StatusNoContent)
		})
		req := httptest.NewRequest(tc.method, "/api/admin/anything", nil)
		req.Header.Set("Authorization", "Bearer "+adminTestSecret)
		req.Header.Set(actingUserHeader, tc.acting)
		rr := httptest.NewRecorder()
		h(rr, req)
		if ran != tc.wantRun || role != tc.wantRole {
			t.Errorf("%s as %s: ran=%v role=%q, want ran=%v role=%q (status %d)", tc.method, tc.acting, ran, role, tc.wantRun, tc.wantRole, rr.Code)
		}
		if !tc.wantRun && rr.Code != http.StatusForbidden {
			t.Errorf("%s as %s: %d, want 403", tc.method, tc.acting, rr.Code)
		}
	}
}

// Nothing reaches /api/admin except through adminHandle, and nothing but adminHandle uses
// adminRoute: otherwise a route could exist that the walk above never sees.
func TestAdminRoutesAreRegisteredOnlyThroughAdminHandle(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	handles := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok && (fn.Name.Name == "adminHandle" || fn.Name.Name == "adminRoute") {
				return false // the helpers themselves
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = f.Sel.Name
			case *ast.Ident:
				name = f.Name
			}
			if name == "adminRoute" {
				t.Errorf("%s: adminRoute is used directly; register the route with adminHandle", fset.Position(call.Pos()))
			}
			if name == "adminHandle" {
				handles++
				return true
			}
			if name != "Handle" && name != "HandleFunc" && name != "h" {
				return true
			}
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, _ := strconv.Unquote(lit.Value); strings.Contains(s, "/api/admin") {
						t.Errorf("%s: %q is registered without adminHandle", fset.Position(call.Pos()), s)
					}
				}
			}
			return true
		})
	}
	if handles < 40 {
		t.Fatalf("found %d adminHandle calls; the scan is not seeing the admin routes", handles)
	}
	a := &App{}
	for _, bad := range []string{"/api/admin/x", "GET /api/saas/x", "GET /api/adminx", " /api/admin/x"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("adminHandle(%q) must refuse a pattern that is not METHOD /api/admin/...", bad)
				}
			}()
			a.adminHandle(bad, a.handleAdminMe)
		}()
	}
}

func TestAdminMeReportsTheRole(t *testing.T) {
	_, _, srv := newStaffTestApp(t)
	for acting, role := range map[string]string{adminTestAdmin: adminRoleOwner, adminTestStaff: adminRoleStaff} {
		rr := adminDo(srv, http.MethodGet, "/api/admin/me", acting, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("me as %s: %d %s", acting, rr.Code, rr.Body.String())
		}
		var got map[string]string
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		if len(got) != 2 || got["role"] != role || got["discordId"] != acting {
			t.Fatalf("me as %s = %v", acting, got)
		}
	}
	rr := adminDo(srv, http.MethodGet, "/api/admin/me", adminTestStranger, nil)
	if code, _ := adminErrorOf(t, rr); rr.Code != http.StatusForbidden || code != codeForbidden {
		t.Fatalf("me as a stranger: %d %s", rr.Code, rr.Body.String())
	}
}

// An owner is an owner because of the allowlist alone: being on the staff list as well, or a
// staff list that cannot be read, never demotes or locks out the owner.
func TestOwnerNeverDependsOnTheStaffList(t *testing.T) {
	a, store, srv := newStaffTestApp(t)
	store.members[adminTestAdmin] = repository.PlatformStaffMember{DiscordID: adminTestAdmin}
	store.err = errors.New("database is down")
	rr := adminDo(srv, http.MethodGet, "/api/admin/me", adminTestAdmin, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"OWNER"`) {
		t.Fatalf("owner with a broken staff list: %d %s", rr.Code, rr.Body.String())
	}
	// Staff and strangers are both refused while the list cannot be read (fail closed).
	for _, id := range []string{adminTestStaff, adminTestStranger} {
		if rr := adminDo(srv, http.MethodGet, "/api/admin/overview", id, nil); rr.Code != http.StatusInternalServerError {
			t.Fatalf("%s with a broken staff list: %d, want 500", id, rr.Code)
		}
	}
	// No staff list at all (no database): nobody is staff.
	a.platformStaff = nil
	if rr := adminDo(srv, http.MethodGet, "/api/admin/overview", adminTestStaff, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("staff with no staff list: %d, want 403", rr.Code)
	}
}

func TestStaffListManagement(t *testing.T) {
	_, store, srv := newStaffTestApp(t)
	type listBody struct {
		Staff []staffMemberDTO `json:"staff"`
	}
	list := func(rr *httptest.ResponseRecorder) []staffMemberDTO {
		t.Helper()
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		var b listBody
		if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil || b.Staff == nil {
			t.Fatalf("not a staff list: %s", rr.Body.String())
		}
		return b.Staff
	}
	// Owner and staff both read the list.
	for _, acting := range []string{adminTestAdmin, adminTestStaff} {
		got := list(adminDo(srv, http.MethodGet, "/api/admin/staff", acting, nil))
		if len(got) != 1 || got[0].DiscordID != adminTestStaff || got[0].AddedBy != adminTestAdmin || got[0].AddedAt != "2026-10-01T12:00:00Z" {
			t.Fatalf("list as %s = %+v", acting, got)
		}
	}
	// Add.
	got := list(adminDo(srv, http.MethodPost, "/api/admin/staff", adminTestAdmin, map[string]any{"discordId": " " + adminTestStranger + " ", "note": "  Support lead ", "reason": "joined the team"}))
	if len(got) != 2 || got[1].DiscordID != adminTestStranger || got[1].Note != "Support lead" || got[1].AddedBy != adminTestAdmin || got[1].AddedAt != "2026-10-04T09:30:00Z" {
		t.Fatalf("after add: %+v", got)
	}
	// The new member can read at once, and still cannot write.
	if rr := adminDo(srv, http.MethodGet, "/api/admin/overview", adminTestStranger, nil); rr.Code != http.StatusOK {
		t.Fatalf("new staff read: %d", rr.Code)
	}
	if rr := adminDo(srv, http.MethodPost, "/api/admin/staff", adminTestStranger, map[string]any{"discordId": "900000000000000009", "reason": "x"}); rr.Code != http.StatusForbidden {
		t.Fatalf("staff adding staff: %d", rr.Code)
	}
	// Adding someone already on the list updates the note and keeps who added them and when.
	got = list(adminDo(srv, http.MethodPost, "/api/admin/staff", adminTestAdmin, map[string]any{"discordId": adminTestStranger, "note": "Billing", "reason": "changed role"}))
	if len(got) != 2 || got[1].Note != "Billing" || got[1].AddedAt != "2026-10-04T09:30:00Z" {
		t.Fatalf("after note update: %+v", got)
	}
	// No note is fine.
	got = list(adminDo(srv, http.MethodPost, "/api/admin/staff", adminTestAdmin, map[string]any{"discordId": adminTestStranger, "reason": "cleared the note"}))
	if got[1].Note != "" {
		t.Fatalf("note should be empty: %+v", got)
	}
	writesBefore := store.writes
	for name, tc := range map[string]struct {
		body   map[string]any
		status int
		code   string
	}{
		"no reason":       {map[string]any{"discordId": "900000000000000009"}, 400, codeInvalidRequest},
		"blank reason":    {map[string]any{"discordId": "900000000000000009", "reason": "   "}, 400, codeInvalidRequest},
		"long reason":     {map[string]any{"discordId": "900000000000000009", "reason": strings.Repeat("r", 501)}, 400, codeInvalidRequest},
		"missing id":      {map[string]any{"reason": "x"}, 400, codeInvalidRequest},
		"short id":        {map[string]any{"discordId": "12345678901234", "reason": "x"}, 400, codeInvalidRequest},
		"long id":         {map[string]any{"discordId": "123456789012345678901", "reason": "x"}, 400, codeInvalidRequest},
		"non-numeric id":  {map[string]any{"discordId": "90000000000000000a", "reason": "x"}, 400, codeInvalidRequest},
		"long note":       {map[string]any{"discordId": "900000000000000009", "note": strings.Repeat("n", 121), "reason": "x"}, 400, codeInvalidRequest},
		"platform owner":  {map[string]any{"discordId": adminTestAdmin, "reason": "x"}, 409, codeConflict},
		"120 runes is ok": {map[string]any{"discordId": "900000000000000009", "note": strings.Repeat("é", 120), "reason": "x"}, 200, ""},
	} {
		rr := adminDo(srv, http.MethodPost, "/api/admin/staff", adminTestAdmin, tc.body)
		if rr.Code != tc.status {
			t.Errorf("%s: %d %s, want %d", name, rr.Code, rr.Body.String(), tc.status)
			continue
		}
		if tc.code != "" {
			if code, msg := adminErrorOf(t, rr); code != tc.code || msg == "" {
				t.Errorf("%s: %s %q", name, code, msg)
			}
		}
	}
	if store.writes != writesBefore+1 {
		t.Fatalf("only the valid request may write; writes went from %d to %d", writesBefore, store.writes)
	}
	if _, msg := adminErrorOf(t, adminDo(srv, http.MethodPost, "/api/admin/staff", adminTestAdmin, map[string]any{"discordId": adminTestAdmin, "reason": "x"})); !strings.Contains(msg, "already has full access") {
		t.Fatalf("owner refusal message: %q", msg)
	}
	// Remove.
	if rr := adminDo(srv, http.MethodDelete, "/api/admin/staff/"+adminTestStranger, adminTestAdmin, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("remove without a reason: %d", rr.Code)
	}
	if rr := adminDo(srv, http.MethodDelete, "/api/admin/staff/"+adminTestStranger, adminTestStaff, map[string]any{"reason": "x"}); rr.Code != http.StatusForbidden {
		t.Fatalf("staff removing staff: %d", rr.Code)
	}
	got = list(adminDo(srv, http.MethodDelete, "/api/admin/staff/"+adminTestStranger, adminTestAdmin, map[string]any{"reason": "left the team"}))
	for _, m := range got {
		if m.DiscordID == adminTestStranger {
			t.Fatalf("still on the list: %+v", got)
		}
	}
	// Access is gone on the very next request.
	if rr := adminDo(srv, http.MethodGet, "/api/admin/overview", adminTestStranger, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("removed staff read: %d", rr.Code)
	}
	for _, id := range []string{adminTestStranger, "900000000000000777", "not-an-id"} {
		rr := adminDo(srv, http.MethodDelete, "/api/admin/staff/"+id, adminTestAdmin, map[string]any{"reason": "x"})
		if code, _ := adminErrorOf(t, rr); rr.Code != http.StatusNotFound || code != codeNotFound {
			t.Fatalf("remove unknown %s: %d %s", id, rr.Code, rr.Body.String())
		}
	}
}

// The admin log names the role next to the acting admin, for reads, writes and refused writes.
func TestAdminLogIncludesTheRole(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	_, _, srv := newStaffTestApp(t)
	adminDo(srv, http.MethodGet, "/api/admin/overview", adminTestStaff, nil)
	adminDo(srv, http.MethodGet, "/api/admin/overview", adminTestAdmin, nil)
	adminDo(srv, http.MethodPost, "/api/admin/staff", adminTestStaff, map[string]any{"discordId": adminTestStranger, "reason": "x"})
	adminDo(srv, http.MethodPost, "/api/admin/staff", adminTestAdmin, map[string]any{"discordId": adminTestStranger, "reason": "x"})
	out := buf.String()
	for _, want := range []string{
		"event=admin_read acting_admin_discord_id=" + adminTestStaff + " role=STAFF",
		"event=admin_read acting_admin_discord_id=" + adminTestAdmin + " role=OWNER",
		"event=admin_write_denied acting_admin_discord_id=" + adminTestStaff + " role=STAFF",
		"event=admin_write acting_admin_discord_id=" + adminTestAdmin + " role=OWNER",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q:\n%s", want, out)
		}
	}
}

func TestIsDiscordSnowflake(t *testing.T) {
	for s, want := range map[string]bool{
		"123456789012345": true, "12345678901234567890": true, "900000000000000001": true,
		"": false, "12345678901234": false, "123456789012345678901": false, "12345678901234a": false, " 123456789012345": false, "１２３４５６７８９０１２３４５": false,
	} {
		if isDiscordSnowflake(s) != want {
			t.Errorf("isDiscordSnowflake(%q) = %v", s, !want)
		}
	}
}
