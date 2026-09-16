package nitrado

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListLogsFiltersADMOnly verifies that only .ADM files become candidates and
// that discovery does not emit a per-file INFO dump (summary-only logging).
func TestListLogsFiltersADMOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Root returns the config dir; config returns mixed files.
		if r.URL.Query().Get("dir") == "" || r.URL.Query().Get("dir") == "/" {
			_, _ = w.Write([]byte(`{"status":"success","data":{"entries":[
				{"type":"dir","path":"/games/ni_1/ftproot/dayzps/config","name":"config","modified_at":1757370000}
			]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"entries":[
			{"type":"file","path":"/games/ni_1/ftproot/dayzps/config/DayZServer_PS4_x64_2026-09-08_22-00.ADM","name":"DayZServer_PS4_x64_2026-09-08_22-00.ADM","size":629,"modified_at":1757371000},
			{"type":"file","path":"/games/ni_1/ftproot/dayzps/config/DayZServer_PS4_x64_2026-09-08_22-05.ADM","name":"DayZServer_PS4_x64_2026-09-08_22-05.ADM","size":900,"modified_at":1757371300},
			{"type":"file","path":"/games/ni_1/ftproot/dayzps/config/DayZServer_PS4_x64_2026-09-08_22-00.RPT","name":"DayZServer_PS4_x64_2026-09-08_22-00.RPT","size":5000,"modified_at":1757371000},
			{"type":"file","path":"/games/ni_1/ftproot/dayzps/config/script_2026_09_08.log","name":"script_2026_09_08.log","size":300,"modified_at":1757371200}
		]}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	logs, err := client.ListLogs(context.Background(), "19806451")
	if err != nil {
		t.Fatalf("ListLogs returned unexpected error: %v", err)
	}

	// Only the two .ADM files should be candidates (RPT and script log excluded).
	if len(logs) != 2 {
		t.Fatalf("expected 2 ADM candidates, got %d: %+v", len(logs), logs)
	}
	// Newest (highest modified_at) must be first.
	if !strings.HasSuffix(logs[0].Name, "22-05.ADM") {
		t.Fatalf("expected newest ADM first, got %q", logs[0].Name)
	}
	for _, lf := range logs {
		if !strings.HasSuffix(strings.ToLower(lf.Name), ".adm") {
			t.Fatalf("non-ADM candidate leaked: %q", lf.Name)
		}
	}
}

// TestListLogsFindsFileOnlyVisibleViaGameserverDetails reproduces a live
// Nitrado behavior: a server can expose a second mount (observed live as
// "noftp") that never appears as a child of any directory the recursive tree
// walk from "/" can reach, yet is the mount the documented Gameserver Details
// endpoint (game_specific.path) names as current. A log rotated only into
// that mount must still be discovered.
func TestListLogsFindsFileOnlyVisibleViaGameserverDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if strings.HasSuffix(r.URL.Path, "/gameservers") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"gameserver":{"game_specific":{"path":"/games/ni_1/noftp/dayzps/"}}}}`))
			return
		}

		switch r.URL.Query().Get("dir") {
		case "", "/":
			// The enumerable mount: only reachable via "/", holds only the stale file.
			_, _ = w.Write([]byte(`{"status":"success","data":{"entries":[
				{"type":"dir","path":"/games/ni_1/ftproot/dayzps/config","name":"config","modified_at":1757370000}
			]}}`))
		case "/games/ni_1/ftproot/dayzps/config":
			_, _ = w.Write([]byte(`{"status":"success","data":{"entries":[
				{"type":"file","path":"/games/ni_1/ftproot/dayzps/config/DayZServer_PS4_x64_2026-09-08_22-00.ADM","name":"DayZServer_PS4_x64_2026-09-08_22-00.ADM","size":629,"modified_at":1757371000}
			]}}`))
		case "/games/ni_1/noftp/dayzps/config":
			// The unlisted mount named by game_specific.path: holds the live file.
			_, _ = w.Write([]byte(`{"status":"success","data":{"entries":[
				{"type":"file","path":"/games/ni_1/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-08_23-00.ADM","name":"DayZServer_PS4_x64_2026-09-08_23-00.ADM","size":1200,"modified_at":1757375000}
			]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"entries":[]}}`))
		}
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	logs, err := client.ListLogs(context.Background(), "19806451")
	if err != nil {
		t.Fatalf("ListLogs returned unexpected error: %v", err)
	}

	if len(logs) != 2 {
		t.Fatalf("expected 2 ADM candidates across both mounts, got %d: %+v", len(logs), logs)
	}
	if !strings.HasSuffix(logs[0].Name, "23-00.ADM") {
		t.Fatalf("expected the file only visible via game_specific.path to be selected as newest, got %q", logs[0].Name)
	}
}
