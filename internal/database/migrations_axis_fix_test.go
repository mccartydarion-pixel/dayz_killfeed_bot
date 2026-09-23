package database

import (
	"strings"
	"testing"
)

// The ADM axis repair must be registered after the tables it rewrites and must only touch ADM
// rows whose y (the real north coordinate) was actually stored.
func TestADMLocationAxisFixMigrationRegistered(t *testing.T) {
	idx := map[string]int{}
	for i, m := range migrations {
		idx[m.Name] = i
	}
	fix, ok := idx["0044_player_location_events_adm_axis_fix"]
	if !ok {
		t.Fatal("expected 0044_player_location_events_adm_axis_fix to be registered")
	}
	if base, ok := idx["0041_player_location_events"]; !ok || fix <= base {
		t.Fatalf("axis fix (index %d) must run after 0041_player_location_events (index %d)", fix, base)
	}
	sql := strings.Join(strings.Fields(migrations[fix].SQL), " ")
	for _, want := range []string{"UPDATE player_location_events", "SET z = y, y = z", "source = 'ADM'", "y IS NOT NULL"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("expected axis fix SQL to contain %q, got %q", want, sql)
		}
	}
}
