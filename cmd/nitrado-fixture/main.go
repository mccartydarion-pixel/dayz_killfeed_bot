// Command nitrado-fixture serves the read-only synthetic Nitrado API used by
// isolated staging (internal/nitrado/nitradofixture). Point a staging bot at
// it with APP_ENV=staging and NITRADO_API_BASE_URL. It holds no Nitrado
// credential and refuses every write.
//
// Environment:
//
//	PORT                        listen port (default 8080)
//	FIXTURE_SERVICE_ID          synthetic service ID (default 90000001)
//	FIXTURE_CONTROL_TOKEN       required X-Fixture-Token for /_fixture/* (recommended)
//	FIXTURE_KILLS_PER_MINUTE    optional background kill rate (default 0 = only on request)
package main

import (
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado/nitradofixture"
)

func main() {
	port := envOr("PORT", "8080")
	serviceID, err := strconv.ParseInt(envOr("FIXTURE_SERVICE_ID", "90000001"), 10, 64)
	if err != nil || serviceID <= 0 {
		slog.Error("FIXTURE_SERVICE_ID must be a positive integer")
		os.Exit(2)
	}
	token := os.Getenv("FIXTURE_CONTROL_TOKEN")
	if token == "" {
		slog.Warn("FIXTURE_CONTROL_TOKEN is not set: /_fixture control endpoints are open to anyone who can reach this service")
	}
	fx := nitradofixture.New(serviceID, token)
	if rate, _ := strconv.Atoi(os.Getenv("FIXTURE_KILLS_PER_MINUTE")); rate > 0 {
		go func() {
			t := time.NewTicker(time.Minute / time.Duration(rate))
			defer t.Stop()
			for range t.C {
				fx.AddKills(1)
			}
		}()
	}
	slog.Info("nitrado fixture listening", "port", port, "service_id", serviceID)
	srv := &http.Server{Addr: ":" + port, Handler: fx, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("nitrado fixture stopped", "err", err.Error())
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
