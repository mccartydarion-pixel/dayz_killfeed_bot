package nitrado

import (
	"context"
	"strings"
	"testing"
)

func TestServerNamePrecedence(t *testing.T) {
	svc := func(name, serverName string) Service {
		return Service{Details: ServiceDetails{Name: name, ServerName: serverName}}
	}
	cases := []struct {
		label string
		gs    GameserverName
		svc   Service
		want  string
	}{
		{"hostname setting wins over everything", GameserverName{Hostname: "Setting", QueryName: "Query"}, svc("List", "Legacy"), "Setting"},
		{"query name when the setting is missing", GameserverName{QueryName: "Query"}, svc("List", "Legacy"), "Query"},
		{"service list name when the gameserver read gave nothing", GameserverName{}, svc("List", "Legacy"), "List"},
		{"legacy server_name last", GameserverName{}, svc("", "Legacy"), "Legacy"},
		{"a blank candidate is skipped", GameserverName{Hostname: " \t ", QueryName: "Query"}, svc("", ""), "Query"},
		{"nothing reported", GameserverName{}, svc("", ""), ""},
	}
	for _, c := range cases {
		if got := ServerName(c.gs, c.svc); got != c.want {
			t.Errorf("%s: got %q, want %q", c.label, got, c.want)
		}
	}
}

func TestCleanServerName(t *testing.T) {
	if got := CleanServerName("  My\tServer\r\n  [PVP]\x00 "); got != "My Server [PVP]" {
		t.Fatalf("got %q", got)
	}
	long := CleanServerName(strings.Repeat("é", 80)) // 160 bytes
	if len(long) > MaxServerNameLen || len(long) != 100 || strings.ContainsRune(long, '�') {
		t.Fatalf("cap must be %d bytes on a character boundary, got %d bytes", MaxServerNameLen, len(long))
	}
	if got := CleanServerName("bad\xff"); got != "" {
		t.Fatalf("invalid UTF-8 must yield no name, got %q", got)
	}
}

func TestGameserverNameReadsHostnameAndQueryOnly(t *testing.T) {
	c := liveTestClient(t, 200, `{"status":"success","data":{"gameserver":{"service_id":19806451,"status":"started",
		"username":"ni123_1","password":"never-decoded",
		"settings":{"config":{"hostname":"Champions | PVP","password":"secret","admin-password":"secret2"}},
		"query":{"server_name":"Champions (old)","player_current":2}}}}`)
	got, err := c.GameserverName(context.Background(), "19806451")
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "Champions | PVP" || got.QueryName != "Champions (old)" || !got.BelongsTo("19806451") || got.BelongsTo("1") {
		t.Fatalf("unexpected: %+v", got)
	}
}

// Nitrado serializes empty objects as [] and a stopped server has no query.
func TestGameserverNameToleratesEmptyBlocks(t *testing.T) {
	c := liveTestClient(t, 200, `{"data":{"gameserver":{"service_id":19806451,"settings":[],"query":[]}}}`)
	got, err := c.GameserverName(context.Background(), "19806451")
	if err != nil || got.Hostname != "" || got.QueryName != "" {
		t.Fatalf("got %+v err %v", got, err)
	}
	c = liveTestClient(t, 200, `{"data":{"gameserver":{"service_id":19806451,"settings":{"config":{"hostname":42}}}}}`)
	if got, err := c.GameserverName(context.Background(), "19806451"); err != nil || got.Hostname != "" {
		t.Fatalf("a non-string hostname must be ignored: %+v err %v", got, err)
	}
}

func TestGameserverNameFailureIsAnError(t *testing.T) {
	c := liveTestClient(t, 503, `{}`)
	if _, err := c.GameserverName(context.Background(), "19806451"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestResolveServerNameFallsBackToTheServiceList(t *testing.T) {
	svc := Service{ID: "19806451", Details: ServiceDetails{Name: "List name"}}
	// The gameserver read fails: the list name is used.
	if got := liveTestClient(t, 500, `{}`).ResolveServerName(context.Background(), svc); got != "List name" {
		t.Fatalf("got %q", got)
	}
	// A response that names another service is not trusted.
	other := liveTestClient(t, 200, `{"data":{"gameserver":{"service_id":7,"settings":{"config":{"hostname":"Someone else"}}}}}`)
	if got := other.ResolveServerName(context.Background(), svc); got != "List name" {
		t.Fatalf("got %q", got)
	}
	ok := liveTestClient(t, 200, `{"data":{"gameserver":{"service_id":19806451,"settings":{"config":{"hostname":"Host"}}}}}`)
	if got := ok.ResolveServerName(context.Background(), svc); got != "Host" {
		t.Fatalf("got %q", got)
	}
}
