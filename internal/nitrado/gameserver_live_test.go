package nitrado

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func liveTestClient(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/services/19806451/gameservers") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "secret-test-token", srv.Client())
}

func TestGameserverLiveReadsQueryPlayerCounts(t *testing.T) {
	c := liveTestClient(t, 200, `{"status":"success","data":{"gameserver":{"status":"started","slots":18,
		"username":"ni123_1","password":"never-decoded","query":{"server_name":"Champions","player_current":2,"player_max":18}}}}`)
	live, err := c.GameserverLive(context.Background(), "19806451")
	if err != nil {
		t.Fatal(err)
	}
	if !live.Running() || live.PlayerCurrent == nil || *live.PlayerCurrent != 2 || live.Capacity() != 18 {
		t.Fatalf("unexpected live status: %+v", live)
	}
}

func TestGameserverLiveZeroPlayersIsKnownZero(t *testing.T) {
	c := liveTestClient(t, 200, `{"data":{"gameserver":{"status":"started","slots":18,"query":{"player_current":0,"player_max":18}}}}`)
	live, err := c.GameserverLive(context.Background(), "19806451")
	if err != nil {
		t.Fatal(err)
	}
	if live.PlayerCurrent == nil || *live.PlayerCurrent != 0 {
		t.Fatalf("expected a known zero, got %+v", live)
	}
}

// Nitrado serializes a missing query as []: that is unknown, never zero.
func TestGameserverLiveEmptyQueryArrayIsUnknown(t *testing.T) {
	c := liveTestClient(t, 200, `{"data":{"gameserver":{"status":"restarting","slots":18,"query":[]}}}`)
	live, err := c.GameserverLive(context.Background(), "19806451")
	if err != nil {
		t.Fatal(err)
	}
	if live.PlayerCurrent != nil || live.Running() || live.Stopped() || live.Capacity() != 18 {
		t.Fatalf("expected unknown count while restarting, got %+v", live)
	}
}

func TestGameserverLiveMissingPlayerCurrentIsUnknown(t *testing.T) {
	c := liveTestClient(t, 200, `{"data":{"gameserver":{"status":"started","slots":18,"query":{"server_name":"Champions"}}}}`)
	live, err := c.GameserverLive(context.Background(), "19806451")
	if err != nil {
		t.Fatal(err)
	}
	if live.PlayerCurrent != nil {
		t.Fatalf("expected unknown player count, got %d", *live.PlayerCurrent)
	}
}

func TestGameserverLiveAPIUnavailableIsError(t *testing.T) {
	c := liveTestClient(t, 503, `{"status":"error","message":"maintenance"}`)
	if _, err := c.GameserverLive(context.Background(), "19806451"); err == nil {
		t.Fatal("expected an error when Nitrado is unavailable")
	}
}
