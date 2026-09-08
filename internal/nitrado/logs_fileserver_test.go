package nitrado

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListLogsUsesFileServerAPI(t *testing.T) {
	var sawPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPaths = append(sawPaths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")

		// Only the DayZ profile directory returns log files; others are empty.
		if strings.Contains(r.URL.RequestURI(), "profile") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"success","data":{"entries":[
				{"type":"file","path":"/profile/DayZServer_x64.ADM","name":"DayZServer_x64.ADM","size":20480,"modified_at":1757370000},
				{"type":"file","path":"/profile/settings.xml","name":"settings.xml","size":1200,"modified_at":1757360000}
			]}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"success","data":{"entries":[
			{"type":"dir","path":"/profile","name":"profile","modified_at":1757360000}
		]}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	logs, err := client.ListLogs(context.Background(), "19806451")
	if err != nil {
		t.Fatalf("ListLogs returned unexpected error: %v", err)
	}

	// Must have hit the documented file server list endpoint, not the service payload.
	joined := strings.Join(sawPaths, " ")
	if !strings.Contains(joined, "/gameservers/file_server/list") {
		t.Fatalf("expected file_server/list API to be used, got: %s", joined)
	}

	if len(logs) != 1 {
		t.Fatalf("expected 1 log file (settings.xml filtered out), got %d", len(logs))
	}
	if logs[0].Name != "DayZServer_x64.ADM" {
		t.Fatalf("expected DayZServer_x64.ADM, got %q", logs[0].Name)
	}
	if logs[0].Path == "" {
		t.Fatal("expected a real path to be set on the discovered log")
	}
	if logs[0].Modified.IsZero() {
		t.Fatal("expected modified_at to be converted to a time")
	}
}

func TestListLogsPermissionDeniedSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":"error","message":"forbidden"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", srv.Client())
	_, err := client.ListLogs(context.Background(), "1")
	if err == nil {
		t.Fatal("expected an error when all file server calls are forbidden")
	}
	reqErr, ok := err.(*RequestError)
	if !ok {
		t.Fatalf("expected RequestError, got %T", err)
	}
	if reqErr.Kind != KindPermission {
		t.Fatalf("expected kind=permission, got %s", reqErr.Kind)
	}
}
