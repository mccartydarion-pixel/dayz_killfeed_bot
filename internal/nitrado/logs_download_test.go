package nitrado

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadLogUsesFileServerDownload(t *testing.T) {
	var sawDownload bool

	// The signed URL target that returns the raw log bytes.
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("player connected\nplayer killed\n"))
	}))
	defer fileSrv.Close()

	// The API server that resolves file_server/download into a signed URL.
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RequestURI(), "file_server/download") {
			t.Errorf("unexpected API path: %s", r.URL.RequestURI())
		}
		sawDownload = true
		if !strings.Contains(r.URL.Query().Get("file"), "DayZServer_x64.ADM") {
			t.Errorf("expected file query param to reference the ADM log, got %q", r.URL.Query().Get("file"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"success","data":{"token":{"url":%q}}}`, fileSrv.URL+"/signed")
	}))
	defer apiSrv.Close()

	client := NewClient(apiSrv.URL, "token", apiSrv.Client())
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

