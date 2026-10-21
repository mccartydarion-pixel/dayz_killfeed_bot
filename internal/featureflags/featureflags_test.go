package featureflags

import (
	"context"
	"testing"
	"time"
)

type memStore struct {
	rows    []Override
	servers map[int64]int64
	loads   int
}

func (m *memStore) ListOverrides(context.Context) ([]Override, error) { m.loads++; return m.rows, nil }
func (m *memStore) ServerInstallations(context.Context) (map[int64]int64, error) {
	return m.servers, nil
}

func TestOverrideWinsAndFallbackOtherwise(t *testing.T) {
	store := &memStore{rows: []Override{{InstallationID: 7, Flag: CustomEmbeds, Enabled: true}, {InstallationID: 8, Flag: CaseEvidence, Enabled: false}}, servers: map[int64]int64{30: 7, 40: 8}}
	r := New(store, time.Hour)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.Enabled(7, CustomEmbeds, false) {
		t.Fatal("override on must win over a false default")
	}
	if r.Enabled(8, CaseEvidence, true) {
		t.Fatal("override off must win over a true default")
	}
	if !r.Enabled(9, CustomEmbeds, true) || r.Enabled(9, CustomEmbeds, false) {
		t.Fatal("no override: the default applies")
	}
	if !r.EnabledForServer(30, CustomEmbeds, false) || r.EnabledForServer(40, CaseEvidence, true) || !r.EnabledForServer(99, CaseEvidence, true) {
		t.Fatal("server-scoped questions resolve through the owning installation")
	}
	if got := r.Overrides(7); len(got) != 1 || !got[CustomEmbeds] {
		t.Fatalf("overrides = %v", got)
	}
	var nilResolver *Resolver
	if !nilResolver.Enabled(7, CustomEmbeds, true) {
		t.Fatal("a nil resolver is the default")
	}
}

func TestStaleSnapshotRefreshesInBackgroundWithoutBlocking(t *testing.T) {
	store := &memStore{servers: map[int64]int64{}}
	r := New(store, 10*time.Millisecond)
	_ = r.Refresh(context.Background())
	store.rows = []Override{{InstallationID: 1, Flag: ShopCanary, Enabled: true}}
	time.Sleep(20 * time.Millisecond)
	if r.Enabled(1, ShopCanary, false) {
		t.Fatal("the first stale read answers from the old snapshot")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !r.Enabled(1, ShopCanary, false) {
		if time.Now().After(deadline) {
			t.Fatal("background refresh never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if store.loads < 2 {
		t.Fatalf("expected a background reload, loads=%d", store.loads)
	}
}

func TestCatalogKeysAreKnown(t *testing.T) {
	for _, d := range Catalog {
		if !Known(d.Key) {
			t.Fatal(d.Key)
		}
	}
	if Known("nope") {
		t.Fatal("unknown key")
	}
}

// For an installation of a platform owner's own organization, a flag marked OwnerDefaultOn is on
// with no override; an override still wins; and nothing changes for any other installation.
func TestOwnerAccessTurnsTheDefaultOnAndAnOverrideStillWins(t *testing.T) {
	store := &memStore{rows: []Override{{InstallationID: 7, Flag: MapRotation, Enabled: false}}, servers: map[int64]int64{30: 7, 40: 8, 50: 9}}
	r := New(store, time.Hour)
	r.SetOwnerInstallations(func(id int64) bool { return id == 7 || id == 8 })
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.Enabled(8, MapRotation, false) || !r.Enabled(8, CustomEmbeds, false) {
		t.Fatal("owner installation: map rotation and custom embeds are on with no override")
	}
	if r.Enabled(7, MapRotation, false) || r.Enabled(7, MapRotation, true) {
		t.Fatal("an explicit OFF override wins over owner access")
	}
	if !r.OwnerAccess(7, MapRotation) {
		t.Fatal("OwnerAccess reports the owner rule even when an override hides it")
	}
	if r.Enabled(9, MapRotation, false) || r.Enabled(9, CustomEmbeds, false) || r.OwnerAccess(9, MapRotation) {
		t.Fatal("a customer installation keeps the plain default")
	}
	for _, flag := range []string{ShopCanary, CaseEvidence, CaseBuildEvidence} {
		if r.Enabled(8, flag, false) || r.OwnerAccess(8, flag) || r.EnabledForServer(40, flag, false) {
			t.Fatalf("%s must never be switched on by owner access", flag)
		}
	}
	if !r.EnabledForServer(40, MapRotation, false) || r.EnabledForServer(50, MapRotation, false) {
		t.Fatal("server-scoped questions follow the owning installation")
	}
	if r.Enabled(8, "not_a_flag", false) || r.OwnerAccess(0, MapRotation) {
		t.Fatal("unknown flags and installation 0 never get owner access")
	}
	if got := r.Overrides(8); len(got) != 0 {
		t.Fatalf("owner access must not appear as a stored override: %v", got)
	}
	plain := New(store, time.Hour)
	_ = plain.Refresh(context.Background())
	if plain.Enabled(8, MapRotation, false) || plain.OwnerAccess(8, MapRotation) {
		t.Fatal("without a lookup nobody has owner access")
	}
}

// The list of flags owner access switches on is a decision, not an accident: adding a flag to
// the catalog must come with an explicit answer here.
func TestOwnerDefaultIsDecidedForEveryFlag(t *testing.T) {
	want := map[string]bool{CustomEmbeds: true, MapRotation: true, ShopCanary: false, CaseEvidence: false, CaseBuildEvidence: false}
	if len(Catalog) != len(want) {
		t.Fatalf("the catalog has %d flags and this test decides %d: decide whether owner access switches the new flag on", len(Catalog), len(want))
	}
	for _, d := range Catalog {
		on, ok := want[d.Key]
		if !ok {
			t.Fatalf("flag %s has no owner-access decision", d.Key)
		}
		if d.OwnerDefaultOn != on || OwnerDefaultOn(d.Key) != on {
			t.Fatalf("flag %s: OwnerDefaultOn=%v, want %v", d.Key, d.OwnerDefaultOn, on)
		}
		if on && d.RestartRequired {
			t.Fatalf("flag %s needs a worker restart and so cannot be switched on by owner access", d.Key)
		}
		if on != (d.OwnerDefaultNote == "") {
			t.Fatalf("flag %s: a flag owner access leaves off must say why, and one it switches on must not", d.Key)
		}
	}
}
