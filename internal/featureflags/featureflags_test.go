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
