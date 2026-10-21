package database

import (
	"regexp"
	"strconv"
	"testing"
)

// historicalNumberPairs are the only migration numbers ever shared by two migrations. C.A.S.E.
// Core (main) and C.A.S.E. billing (PR #94) were developed in parallel and each applied its own
// migrations under these numbers: production recorded the first name of each pair, the isolated
// staging database the second. schema_migrations keys a migration by its FULL name, so renaming
// either one would re-run its SQL on the database that already applied it. Both names are
// therefore frozen, and the pair runs in exactly this order. No other number may ever be shared.
var historicalNumberPairs = map[int][2]string{
	56: {"0056_case_shadow_evaluations", "0056_case_addon_subscriptions"},
	63: {"0063_case_review_outbox_skeleton", "0063_case_plan_changes"},
	64: {"0064_case_build_evidence", "0064_case_invoice_coverage"},
}

// The migration registry is shared by every Champion workstream (Shop, C.A.S.E., Live Sync, ...).
// Every name must be unique, every number must be unique (except the frozen historical pairs
// above), and numbers must never decrease in execution (slice) order.
func TestMigrationRegistryNumbersAreUniqueAndOrdered(t *testing.T) {
	re := regexp.MustCompile(`^(\d{4})_[a-z0-9_]+$`)
	names := map[string]bool{}
	numbers := map[int][]string{}
	last := 0
	for i, m := range migrations {
		mm := re.FindStringSubmatch(m.Name)
		if mm == nil {
			t.Fatalf("migration %d has a malformed name %q", i, m.Name)
		}
		if names[m.Name] {
			t.Fatalf("duplicate migration name %s", m.Name)
		}
		names[m.Name] = true
		n, _ := strconv.Atoi(mm[1])
		if n < last || (n == last && len(numbers[n]) == 0) {
			t.Fatalf("migration %s (number %d) runs after number %d: numbers must not decrease in execution order", m.Name, n, last)
		}
		numbers[n] = append(numbers[n], m.Name)
		last = n
		if m.SQL == "" {
			t.Fatalf("migration %s has no SQL", m.Name)
		}
	}
	for n, used := range numbers {
		if len(used) == 1 {
			continue
		}
		pair, frozen := historicalNumberPairs[n]
		if !frozen || len(used) != 2 || used[0] != pair[0] || used[1] != pair[1] {
			t.Errorf("migration number %04d is used by %v; only the frozen historical pairs may share a number", n, used)
		}
	}
	for n, pair := range historicalNumberPairs {
		if len(numbers[n]) != 2 {
			t.Errorf("historical pair %04d %v must stay registered under its original names, found %v", n, pair, numbers[n])
		}
	}
}
