package nitradofixture

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/livemap"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

func TestFixtureControlRequiresTokenAndWritesAreRefused(t *testing.T) {
	fx := New(90000001, "secret-control")
	srv := httptest.NewServer(fx)
	defer srv.Close()
	do := func(method, path, token string) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		if token != "" {
			req.Header.Set("X-Fixture-Token", token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := do("POST", "/_fixture/kills?n=15", ""); got != http.StatusUnauthorized {
		t.Fatalf("control without token: %d", got)
	}
	if got := do("POST", "/_fixture/kills?n=15", "wrong"); got != http.StatusUnauthorized {
		t.Fatalf("control with a wrong token: %d", got)
	}
	if got := do("POST", "/_fixture/kills?n=15", "secret-control"); got != 200 || fx.Snapshot().Events != 15 {
		t.Fatalf("control with token: %d events=%d", got, fx.Snapshot().Events)
	}
	for _, p := range []string{"/services/90000001/gameservers/restart", "/services/90000001/gameservers/stop",
		"/services/90000001/gameservers/games/whitelist", "/services/90000001/gameservers/file_server/upload"} {
		if got := do("POST", p, ""); got != http.StatusForbidden {
			t.Fatalf("write %s: %d", p, got)
		}
	}
	if got := do("PUT", "/_files?file="+strings.ReplaceAll(fx.Snapshot().ADMPath, "/", "%2F"), ""); got != http.StatusForbidden {
		t.Fatalf("file write: %d", got)
	}
	if fx.Snapshot().RefusedWrites != 4 {
		t.Fatalf("refused writes %d, want 4", fx.Snapshot().RefusedWrites)
	}
}

// The live map reads the scheduled restart and the in-game clock settings from the fixture the
// way it does from Nitrado (docs/LIVE_MAP.md).
func TestFixtureServesScheduledTasksAndClockSettings(t *testing.T) {
	fx := New(90000001, "")
	srv := httptest.NewServer(fx)
	defer srv.Close()
	c := nitrado.NewClient(srv.URL, "fixture", srv.Client())
	tasks, err := c.ListScheduledTasks(context.Background(), "90000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].ActionMethod != "restart" || tasks[1].ActionMethod != "backup" {
		t.Fatalf("tasks = %+v", tasks)
	}
	next, ok := livemap.NextRestart([]livemap.Task{{ActionMethod: tasks[0].ActionMethod, NextRun: tasks[0].NextRun}}, time.Now())
	if !ok || next.Sub(time.Now()) > restartEvery || next.Before(time.Now()) {
		t.Fatalf("next restart = %v ok=%v", next, ok)
	}
	facts, err := c.GameserverFacts(context.Background(), "90000001")
	if err != nil {
		t.Fatal(err)
	}
	if facts.ServerTime != "SystemTime" || facts.ServerTimeAcceleration != "12" || facts.ServerNightTimeAcceleration != "1" {
		t.Fatalf("facts = %+v", facts)
	}
}
