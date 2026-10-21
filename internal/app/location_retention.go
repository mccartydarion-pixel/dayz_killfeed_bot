package app

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultLocationRetentionDays is used when CHAMPION_LOCATION_RETENTION_DAYS is unset or invalid
// (task section 9's own suggested default).
const defaultLocationRetentionDays = 30

// locationRetentionDays parses CHAMPION_LOCATION_RETENTION_DAYS, failing closed to the default on
// anything unparsable or non-positive - a misconfigured value must never disable retention
// entirely (e.g. accidentally reading as 0, which would delete everything every sweep).
func locationRetentionDays() int {
	raw := strings.TrimSpace(os.Getenv("CHAMPION_LOCATION_RETENTION_DAYS"))
	if raw == "" {
		return defaultLocationRetentionDays
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultLocationRetentionDays
	}
	return v
}

// locationRetentionSweepInterval bounds how often the retention job runs - a data-volume cleanup
// job, not a latency-sensitive one, so a slow cadence (matching this codebase's other periodic-
// but-not-urgent jobs, e.g. runCompetitiveSchedulers' 45s tick) is appropriate; here even coarser
// since retention only needs to keep up with a day-scale growth rate, not a poll-scale one.
const locationRetentionSweepInterval = time.Hour

// runLocationRetention periodically deletes player_location_events older than the configured
// retention window (task section 9), in bounded batches so one sweep never holds a long-running
// lock. Never touches kills/deaths (task: "Do not delete kill/death history") - LocationRepository
// only ever targets player_location_events.
func (a *App) runLocationRetention(ctx context.Context) {
	if a.Locations == nil {
		return
	}
	days := locationRetentionDays()
	sweep := func() {
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		var total int64
		for {
			n, err := a.Locations.DeleteOlderThan(ctx, cutoff, 0)
			if err != nil {
				slog.Warn("component=location", "event", "retention_sweep_failed", "err", err.Error())
				return
			}
			total += n
			if n == 0 {
				break
			}
		}
		if total > 0 {
			slog.Info("component=location", "event", "retention_deleted", "count", total, "retention_days", days)
		}
	}
	sweep()
	ticker := time.NewTicker(locationRetentionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sweep()
		case <-ctx.Done():
			return
		}
	}
}
