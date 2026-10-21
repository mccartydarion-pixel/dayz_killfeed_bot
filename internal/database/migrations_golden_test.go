package database

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// migrationRegistryGoldenPath pins every registered migration: one line per migration, in
// execution order, "<name> <sha256 of its SQL>".
const migrationRegistryGoldenPath = "testdata/migration_registry.golden"

func migrationRegistryLines() []string {
	lines := make([]string, 0, len(migrations))
	for _, m := range migrations {
		sum := sha256.Sum256([]byte(m.SQL))
		lines = append(lines, m.Name+" "+hex.EncodeToString(sum[:]))
	}
	return lines
}

// A migration is identified by its name in schema_migrations and runs exactly once, so an applied
// migration must never be renamed, reordered, removed or have its SQL edited: production would
// either re-run it or silently diverge from a fresh database. This test pins the ordered list of
// names and a hash of every SQL body, so moving migrations between files (or any other refactor)
// cannot alter one unnoticed.
//
// Adding a migration: append it to the registry, then run
//
//	UPDATE_MIGRATION_GOLDEN=1 go test ./internal/database -run TestMigrationRegistryMatchesGolden
//
// which only ever APPENDS lines for new migrations. It refuses to rewrite an existing line; a
// mismatch on an existing line means an applied migration was changed and must be restored.
func TestMigrationRegistryMatchesGolden(t *testing.T) {
	raw, err := os.ReadFile(migrationRegistryGoldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			golden = append(golden, line)
		}
	}
	got := migrationRegistryLines()
	if len(got) < len(golden) {
		t.Fatalf("registry has %d migrations but %d are pinned: a migration was removed", len(got), len(golden))
	}
	for i, want := range golden {
		if got[i] != want {
			t.Fatalf("migration %d changed (name, position or SQL):\n  pinned:   %s\n  registry: %s\nrestore it; applied migrations are frozen", i, want, got[i])
		}
	}
	if len(got) == len(golden) {
		return
	}
	if os.Getenv("UPDATE_MIGRATION_GOLDEN") != "1" {
		t.Fatalf("%d new migration(s) starting at %q are not pinned; run with UPDATE_MIGRATION_GOLDEN=1 to append them to %s",
			len(got)-len(golden), strings.Fields(got[len(golden)])[0], migrationRegistryGoldenPath)
	}
	if err := os.WriteFile(migrationRegistryGoldenPath, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("append golden: %v", err)
	}
}
