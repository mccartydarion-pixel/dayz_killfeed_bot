package nitrado

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadLogUsesFileServerDownload(t *testing.T) {
	var sawDownload bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RequestURI(), "file_server/download") {
			sawDownload = true
			if !strings.Contains(r.URL.Query().Get("file"), "DayZServer_x64.ADM") {
				t.Errorf("expected file query param to reference the ADM log, got %q", r.URL.Query().Get("file"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"success","data":{"token":{"url":"` + srvURLPlaceholder + `"}}}`))
			return
		}
		// The signed URL target returns the raw log bytes.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("player connected\nplayer killed\n"))
	}))
	defer srv.Close()

	// Point the signed URL back at our test server.
	srvURL := srv.URL
	_ = srvURL

	client := NewClient(srv.URL, "token", srv.Client())
	// Resolve the placeholder after server start by rebuilding with real URL.
	downloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"success","data":{"token":{"url":"` + srv.URL + `/signed"}}}`))
	}))
	defer downloadSrv.Close()

	client = NewClient(downloadSrv.URL, "token", downloadSrv.Client())
	content, err := client.ReadLog(context.Background(), "19806451", "/profile/DayZServer_x64.ADM")
	if err != nil {
		t.Fatalf("ReadLog returned unexpected error: %v", err)
	}
	if !sawDownload {
		t.Fatal("expected the file_server/download endpoint to be called")
	}
	if !strings.Contains(string(content), "player killed") {
		t.Fatalf("expected raw log content, got %q", string(content))
	}
}

// srvURLPlaceholder is replaced at runtime via the test server URL.
const srvURLPlaceholder = "http://127.0.0.1/signed"
