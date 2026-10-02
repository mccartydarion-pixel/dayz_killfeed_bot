package app

import (
	"net/http"
	"os"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// feedDelivery is one Discord route's delivery timing: bot noticed the line -> card posted.
type feedDelivery struct {
	Route                 string `json:"route"`
	Delivered             int64  `json:"delivered"`
	LatencySamples        int64  `json:"latencySamples"`
	AvgQueueWaitMs        int64  `json:"avgQueueWaitMs"`
	MaxQueueWaitMs        int64  `json:"maxQueueWaitMs"`
	LastDetectToDeliverMs int64  `json:"lastDetectToDeliverMs"`
	MaxDetectToDeliverMs  int64  `json:"maxDetectToDeliverMs"`
}

// handleAdminNitradoUsage is GET /api/admin/nitrado-usage: what Nitrado says each token's rate
// limit is (X-RateLimit-* headers on normal responses), how much is left, and how many requests
// this bot made with it in the last hour, by operation. It also shows the three parts of a kill's
// trip to Discord that can be measured: how often Nitrado writes each log (writeGap), how long
// until the bot notices a write (detectLag), and noticed -> posted per feed (delivery). Read-only and in-memory: it never calls
// Nitrado, and tokens appear only as a short hash (docs/NITRADO_POLLING.md).
func (a *App) handleAdminNitradoUsage(w http.ResponseWriter, _ *http.Request, _ adminIdentity) {
	pollEnv := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
		"polling": map[string]string{
			"baseInterval": pollEnv("NITRADO_POLL_INTERVAL", "10s"),
			"fastInterval": pollEnv("NITRADO_POLL_INTERVAL_FAST", "3s"),
			"deltaReads":   pollEnv("NITRADO_DELTA_READ_MODE", "off"),
		},
		"tokens":   nitrado.RateLimitUsage(),
		"timing":   killfeed.PollTimings(),
		"delivery": deliveries(),
	})
}

func deliveries() []feedDelivery {
	out := []feedDelivery{}
	for _, r := range discord.Deliveries.Snapshot() {
		if r.LatencySamples == 0 {
			continue
		}
		out = append(out, feedDelivery{Route: r.Route, Delivered: r.Delivered, LatencySamples: r.LatencySamples, AvgQueueWaitMs: r.AvgQueueWaitMs,
			MaxQueueWaitMs: r.MaxQueueWaitMs, LastDetectToDeliverMs: r.LastDetectToDeliverMs, MaxDetectToDeliverMs: r.MaxDetectToDeliverMs})
	}
	return out
}
