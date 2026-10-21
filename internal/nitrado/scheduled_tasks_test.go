package nitrado

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func tasksTestClient(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/services/19806451/tasks") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "secret-test-token", srv.Client())
}

func TestListScheduledTasksDecodesWhitelistedFields(t *testing.T) {
	c := tasksTestClient(t, 200, `{"status":"success","data":{"tasks":[
		{"id":41,"service_id":19806451,"minute":"0","hour":"*/6","day":"*","month":"*","weekday":"*","next_run":"2026-10-04 00:00:00","last_run":"2026-10-03 18:00:00","method":"restart","action_method":"restart","action_data":null},
		{"id":"42","action_method":"backup","next_run":"2026-10-03 23:00:00"}]}}`)
	tasks, err := c.ListScheduledTasks(context.Background(), "19806451")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].ID != 41 || tasks[0].ActionMethod != "restart" || tasks[0].NextRun != "2026-10-04 00:00:00" || tasks[0].Hour != "*/6" ||
		tasks[1].ID != 42 || tasks[1].ActionMethod != "backup" {
		t.Fatalf("tasks = %+v", tasks)
	}
}

func TestListScheduledTasksToleratesNoTasksAndFailsOnErrors(t *testing.T) {
	c := tasksTestClient(t, 200, `{"status":"success","data":{}}`)
	tasks, err := c.ListScheduledTasks(context.Background(), "19806451")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %+v err = %v", tasks, err)
	}
	if _, err := tasksTestClient(t, 404, `{"status":"error"}`).ListScheduledTasks(context.Background(), "19806451"); err == nil {
		t.Fatal("a 404 did not fail")
	}
	if _, err := tasksTestClient(t, 200, `not json`).ListScheduledTasks(context.Background(), "19806451"); err == nil {
		t.Fatal("a malformed body did not fail")
	}
}

func TestGameserverFactsReadsClockSettings(t *testing.T) {
	c := liveTestClient(t, 200, `{"status":"success","data":{"gameserver":{"service_id":19806451,"status":"started","slots":18,"game":"dayzps",
		"settings":{"config":{"mission":"dayzOffline.chernarusplus","serverTime":"SystemTime","serverTimeAcceleration":"12","serverNightTimeAcceleration":"1","password":"never"}}}}}`)
	facts, err := c.GameserverFacts(context.Background(), "19806451")
	if err != nil {
		t.Fatal(err)
	}
	if facts.ServerTime != "SystemTime" || facts.ServerTimeAcceleration != "12" || facts.ServerNightTimeAcceleration != "1" || facts.Mission != "dayzOffline.chernarusplus" {
		t.Fatalf("facts = %+v", facts)
	}
}
