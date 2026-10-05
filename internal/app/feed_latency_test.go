package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The server clock is only ever what Live Sync learned: unknown stays unknown, and a later
// "not learned" answer never erases an offset that was known.
func TestServerClockNeverInventsAnOffset(t *testing.T) {
	c := &serverClock{}
	if _, known := c.offset(); known {
		t.Fatal("a fresh clock must be unknown")
	}
	c.set(nil)
	if _, known := c.offset(); known {
		t.Fatal("'not learned' must not become a known offset")
	}
	minutes := -240
	c.set(&minutes)
	if got, known := c.offset(); !known || got != -240 {
		t.Fatalf("offset = %d known=%v, want -240 true", got, known)
	}
	c.set(nil)
	if got, known := c.offset(); !known || got != -240 {
		t.Fatalf("after a 'not learned' read: %d known=%v, want the last known offset", got, known)
	}
	var none *serverClock
	if _, known := none.offset(); known {
		t.Fatal("nil clock must be unknown")
	}
}

func TestServerUTCOffsetSourceWithoutDatabaseIsUnknown(t *testing.T) {
	a := &App{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, known := a.serverUTCOffsetSource(ctx, repository.GameServer{ID: 1, GuildID: 1})(); known {
		t.Fatal("without a database the offset must be unknown")
	}
}

// GET /api/runtime/status carries feedLatency only once there is something to show, with the
// documented field names (docs/PERFORMANCE.md section 19, docs/runtime-status-api.md).
func TestRuntimeStatusFeedLatencyShape(t *testing.T) {
	empty, err := json.Marshal(RuntimeStatusResponse{OK: true, FeedLatency: killfeed.FeedLatency.SnapshotServer(-1)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "feedLatency") {
		t.Fatalf("feedLatency must be absent when no card was delivered: %s", empty)
	}

	const serverID = 990071
	read := time.Now().Add(-2 * time.Second)
	killfeed.FeedLatency.Record("KILLFEED", killfeed.FeedTiming{ServerID: serverID, LoggedAt: read.Add(-time.Minute), ReadAt: read}, read.Add(time.Second), time.Now())
	body, err := json.Marshal(RuntimeStatusResponse{OK: true, FeedLatency: killfeed.FeedLatency.SnapshotServer(serverID)})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		FeedLatency []map[string]json.RawMessage `json:"feedLatency"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || len(decoded.FeedLatency) != 1 {
		t.Fatalf("feedLatency: %v %s", err, body)
	}
	for _, key := range []string{"serverId", "feed", "deliveredSinceStart", "cardsWithoutServerClock", "cardsWithImplausibleServerClock", "lastDeliveredAt", "lastHour", "last24Hours"} {
		if _, ok := decoded.FeedLatency[0][key]; !ok {
			t.Fatalf("feedLatency entry lacks %q: %s", key, body)
		}
	}
	var lastHour struct {
		Cards                      int                 `json:"cards"`
		GameLogToBotReadMs         killfeed.StageStats `json:"gameLogToBotReadMs"`
		BotReadToQueuedMs          killfeed.StageStats `json:"botReadToQueuedMs"`
		QueuedToDiscordAcceptedMs  killfeed.StageStats `json:"queuedToDiscordAcceptedMs"`
		BotReadToDiscordAcceptedMs killfeed.StageStats `json:"botReadToDiscordAcceptedMs"`
		GameLogToDiscordAcceptedMs killfeed.StageStats `json:"gameLogToDiscordAcceptedMs"`
	}
	if err := json.Unmarshal(decoded.FeedLatency[0]["lastHour"], &lastHour); err != nil {
		t.Fatal(err)
	}
	if lastHour.Cards != 1 || lastHour.GameLogToBotReadMs.P50Ms != 60_000 || lastHour.BotReadToQueuedMs.P50Ms != 1000 ||
		lastHour.GameLogToDiscordAcceptedMs.Samples != 1 || lastHour.BotReadToDiscordAcceptedMs.MaxMs < 2000 {
		t.Fatalf("lastHour: %+v", lastHour)
	}
	for _, key := range []string{`"p50Ms"`, `"p90Ms"`, `"p99Ms"`, `"maxMs"`, `"samples"`} {
		if !strings.Contains(string(body), key) {
			t.Fatalf("stage statistics lack %s: %s", key, body)
		}
	}
}

// GET /api/admin/nitrado-usage lists every server feed's latency next to the poll timing.
func TestAdminNitradoUsageIncludesFeedLatency(t *testing.T) {
	const serverID = 990072
	now := time.Now()
	killfeed.FeedLatency.Record("DEATH_FEED", killfeed.FeedTiming{ServerID: serverID, ReadAt: now.Add(-time.Second)}, now, now)
	a := &App{Config: &config.Config{}}
	rr := httptest.NewRecorder()
	a.handleAdminNitradoUsage(rr, httptest.NewRequest(http.MethodGet, "/api/admin/nitrado-usage", nil), adminIdentity{})
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		FeedLatency []killfeed.FeedLatencySnapshot `json:"feedLatency"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, s := range payload.FeedLatency {
		if s.ServerID == serverID && s.Feed == "DEATH_FEED" && s.LastHour.Cards == 1 && s.CardsWithoutServerClock == 1 {
			return
		}
	}
	t.Fatalf("server %d DEATH_FEED not in feedLatency: %s", serverID, rr.Body.String())
}
