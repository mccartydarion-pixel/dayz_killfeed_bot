package app

import (
	"testing"
	"time"
)

// A retention window comes from CHAMPION_RETENTION_DAYS_<TABLE> and fails closed to the default:
// an unset, unparsable or non-positive value must never shorten a window to zero.
func TestRetentionDaysEnvOverrideFailsClosed(t *testing.T) {
	if got := retentionDays("combat_anomaly_flags", 14); got != 14 {
		t.Fatalf("unset: got %d", got)
	}
	t.Setenv("CHAMPION_RETENTION_DAYS_COMBAT_ANOMALY_FLAGS", "30")
	if got := retentionDays("combat_anomaly_flags", 14); got != 30 {
		t.Fatalf("override: got %d", got)
	}
	for _, bad := range []string{"0", "-3", "soon", " "} {
		t.Setenv("CHAMPION_RETENTION_DAYS_COMBAT_ANOMALY_FLAGS", bad)
		if got := retentionDays("combat_anomaly_flags", 14); got != 14 {
			t.Fatalf("%q must fall back to the default, got %d", bad, got)
		}
	}
	t.Setenv("CHAMPION_RETENTION_DAYS_LIVE_SYNC_RECORDS", "7")
	if got := liveSyncRetention(); got != 7*24*time.Hour {
		t.Fatalf("live sync override: got %v", got)
	}
	t.Setenv("CHAMPION_RETENTION_DAYS_LIVE_SYNC_RECORDS", "")
	if got := liveSyncRetention(); got != time.Duration(defaultDiagnosticsRetentionDays)*24*time.Hour {
		t.Fatalf("live sync default: got %v", got)
	}
}

// Every pruned table is a plain identifier with a positive default window, and none of the
// tables that feed cases, leaderboards, ledgers or audits is on the list.
func TestDataRetentionTablesAreDiagnosticsOnly(t *testing.T) {
	protected := map[string]bool{"kills": true, "deaths": true, "case_evidence_events": true, "players": true, "point_transactions": true,
		"platform_audit_log": true, "admin_audit_log": true, "player_daily_activity": true, "server_hourly_activity": true, "shop_purchases": true}
	if len(dataRetentionTables) == 0 {
		t.Fatal("no tables")
	}
	for _, tb := range dataRetentionTables {
		if protected[tb.Table] {
			t.Fatalf("%s must never be pruned", tb.Table)
		}
		if tb.DefaultDays <= 0 || tb.TimeColumn == "" || tb.Table == "" {
			t.Fatalf("bad entry %+v", tb)
		}
	}
}
