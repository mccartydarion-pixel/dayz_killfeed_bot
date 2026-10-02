package app

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Data retention for append-only diagnostics tables (docs/PERFORMANCE.md "Data growth and
// retention"). Each table below is only ever read within a short window, so rows older than its
// retention are dead weight. The sweep runs hourly, deletes in bounded batches (never a long
// lock) and logs what it removed. Kills, deaths, C.A.S.E. evidence, ledgers, audit logs and the
// activity rollups are not on this list and never will be pruned by it.
//
// Every window is overridable with CHAMPION_RETENTION_DAYS_<TABLE> (the table name upper-cased,
// e.g. CHAMPION_RETENTION_DAYS_COMBAT_ANOMALY_FLAGS). A value that is unset, unparsable or not
// positive falls back to the default, so a typo can never turn into "delete everything".

// dataRetentionTable is one pruned table and its default window.
type dataRetentionTable struct {
	repository.RetentionTable
	DefaultDays int
}

// dataRetentionTables is the compile-time list of what the sweep prunes.
var dataRetentionTables = []dataRetentionTable{
	// One row per PvP kill, read only for the last ten minutes of the same killer/victim pair
	// (AnomalyRepository.ObservePair).
	{RetentionTable: repository.RetentionTable{Table: "combat_anomaly_flags", TimeColumn: "created_at"}, DefaultDays: defaultDiagnosticsRetentionDays},
}

const (
	// defaultDiagnosticsRetentionDays is the window for a table that only serves diagnostics.
	defaultDiagnosticsRetentionDays = 14
	dataRetentionSweepInterval      = time.Hour
	// dataRetentionMaxBatches caps one sweep; the backlog that is left waits for the next hour.
	dataRetentionMaxBatches = 200
)

// retentionDays returns the configured window for table: CHAMPION_RETENTION_DAYS_<TABLE> when it
// is a positive integer, otherwise def.
func retentionDays(table string, def int) int {
	raw := strings.TrimSpace(os.Getenv("CHAMPION_RETENTION_DAYS_" + strings.ToUpper(table)))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// runDataRetention sweeps every table in dataRetentionTables once at start-up and then hourly,
// until ctx ends.
func (a *App) runDataRetention(ctx context.Context) {
	if a.DB == nil || a.DB.Pool == nil {
		return
	}
	repo := repository.NewDataRetentionRepository(a.DB.Pool)
	sweep := func() {
		for _, t := range dataRetentionTables {
			days := retentionDays(t.Table, t.DefaultDays)
			cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
			var total int64
			for i := 0; i < dataRetentionMaxBatches; i++ {
				n, err := repo.PruneOlderThan(ctx, t.RetentionTable, cutoff, repository.MaxRetentionBatch)
				if err != nil {
					if ctx.Err() == nil {
						slog.Warn("component=retention", "event", "sweep_failed", "table", t.Table, "err", err.Error())
					}
					break
				}
				total += n
				if n == 0 {
					break
				}
			}
			if total > 0 {
				slog.Info("component=retention", "event", "rows_deleted", "table", t.Table, "count", total, "retention_days", days)
			}
		}
	}
	sweep()
	ticker := time.NewTicker(dataRetentionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
