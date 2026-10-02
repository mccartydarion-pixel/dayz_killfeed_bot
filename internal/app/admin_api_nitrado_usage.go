package app

import (
	"net/http"
	"os"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// handleAdminNitradoUsage is GET /api/admin/nitrado-usage: what Nitrado says each token's rate
// limit is (X-RateLimit-* headers on normal responses), how much is left, and how many requests
// this bot made with it in the last hour, by operation. Read-only and in-memory: it never calls
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
		"tokens": nitrado.RateLimitUsage(),
	})
}
