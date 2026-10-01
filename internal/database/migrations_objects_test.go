package database

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	createdObjectRe = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?(?:TABLE|INDEX|(?:OR\s+REPLACE\s+)?FUNCTION|(?:CONSTRAINT\s+)?TRIGGER|VIEW|TYPE|SEQUENCE)\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	touchedObjectRe = regexp.MustCompile(`(?i)\b(?:ALTER\s+TABLE(?:\s+IF\s+EXISTS)?|DROP\s+(?:TABLE|INDEX|FUNCTION|TRIGGER|VIEW|TYPE|SEQUENCE)(?:\s+IF\s+EXISTS)?|UPDATE|DELETE\s+FROM|INSERT\s+INTO|TRUNCATE(?:\s+TABLE)?|\bON)\s+([a-z_][a-z0-9_]*)`)
)

func sqlObjects(re *regexp.Regexp, sql string) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(sql, -1) {
		out[strings.ToLower(m[1])] = true
	}
	return out
}

// The Shop delivery ledger migrations (0054/0055) and every other workstream's migrations (including
// the C.A.S.E. Phase 6 migrations that follow them) must not create, replace, alter or drop each
// other's objects: applying the combined sequence can never silently replace a database object.
func TestLedgerMigrationObjectsAreDisjoint(t *testing.T) {
	isLedger := func(name string) bool { return strings.Contains(name, "_shop_delivery_attempt") }
	ledgerCreated := map[string]string{}
	ledgerTouched := map[string]bool{}
	for _, m := range migrations {
		if !isLedger(m.Name) {
			continue
		}
		for o := range sqlObjects(createdObjectRe, m.SQL) {
			ledgerCreated[o] = m.Name
		}
		for o := range sqlObjects(touchedObjectRe, m.SQL) {
			ledgerTouched[o] = true
		}
	}
	for _, want := range []string{"shop_delivery_attempts", "shop_delivery_attempt_events", "uq_shop_deliveries_id_tenant",
		"shop_delivery_attempt_guard", "trg_shop_delivery_exposure_guard", "shop_delivery_exposure_guard"} {
		if _, ok := ledgerCreated[want]; !ok {
			t.Fatalf("the object parser missed %s (found %v)", want, ledgerCreated)
		}
	}
	otherCreated := map[string]string{}
	for _, m := range migrations {
		if isLedger(m.Name) {
			continue
		}
		for o := range sqlObjects(createdObjectRe, m.SQL) {
			if owner, clash := ledgerCreated[o]; clash {
				t.Errorf("%s creates %s, which %s owns", m.Name, o, owner)
			}
			otherCreated[o] = m.Name
		}
		for o := range sqlObjects(touchedObjectRe, m.SQL) {
			if owner, clash := ledgerCreated[o]; clash {
				t.Errorf("%s alters or drops %s, which %s owns", m.Name, o, owner)
			}
		}
	}
	// The ledger may attach its own index and trigger to shop_deliveries (and read Shop tables inside
	// its functions); it never alters or replaces any other pre-existing object.
	var foreign []string
	for o := range ledgerTouched {
		if _, own := ledgerCreated[o]; own || o == "shop_deliveries" {
			continue
		}
		if _, existing := otherCreated[o]; existing {
			foreign = append(foreign, o)
		}
	}
	sort.Strings(foreign)
	if len(foreign) > 0 {
		t.Fatalf("the ledger migrations touch other workstreams' objects: %v", foreign)
	}
}
