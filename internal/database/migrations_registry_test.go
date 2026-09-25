package database

import (
	"regexp"
	"strconv"
	"testing"
)

// The migration registry is shared by every Champion workstream (Shop, C.A.S.E., Live Sync, ...).
// schema_migrations keys a migration by its full name, so two workstreams that pick the same number
// would both run - in slice order, under a misleading number. Every name must therefore be unique,
// every number must be unique, and numbers must increase in execution (slice) order.
func TestMigrationRegistryNumbersAreUniqueAndOrdered(t *testing.T) {
	re := regexp.MustCompile(`^(\d{4})_[a-z0-9_]+$`)
	names := map[string]bool{}
	numbers := map[int]string{}
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
		if other, dup := numbers[n]; dup {
			t.Fatalf("migration number %04d is used by both %s and %s", n, other, m.Name)
		}
		numbers[n] = m.Name
		if n <= last {
			t.Fatalf("migration %s (number %d) runs after number %d: numbers must increase in execution order", m.Name, n, last)
		}
		last = n
		if m.SQL == "" {
			t.Fatalf("migration %s has no SQL", m.Name)
		}
	}
}
