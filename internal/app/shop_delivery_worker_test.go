package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/deliveryworker"
)

type fakeWorkerStore struct {
	claims   [][]int64
	owners   []string
	give     []repository.ShopAutoInstallation
	released []int64
}

func (f *fakeWorkerStore) ClaimInstallations(_ context.Context, _ time.Time, owner string, lease time.Duration, allowed []int64) ([]repository.ShopAutoInstallation, error) {
	f.claims, f.owners = append(f.claims, allowed), append(f.owners, owner)
	if lease < 2*shopWorkerInterval {
		return nil, errors.New("the lease must outlive a pass interval")
	}
	return f.give, nil
}

func (f *fakeWorkerStore) ReleaseLease(_ context.Context, inst int64, _ string) error {
	f.released = append(f.released, inst)
	return nil
}

func TestTheDeliveryWorkerDoesNotStartWhileItsLockIsClosed(t *testing.T) {
	for _, cfg := range []config.ShopAutoDelivery{
		{},
		config.ParseShopAutoDelivery("true", "11", ""),
		config.ParseShopAutoDelivery("enabled", "", ""),
	} {
		a := &App{Config: &config.Config{ShopAutoDelivery: cfg}}
		if a.startShopDeliveryWorker(context.Background()) {
			t.Fatalf("the worker started with lock %+v", cfg)
		}
	}
	// An open lock without a database still starts nothing.
	a := &App{Config: &config.Config{ShopAutoDelivery: config.ParseShopAutoDelivery("enabled", "11", "")}}
	if a.startShopDeliveryWorker(context.Background()) {
		t.Fatal("the worker started without a database")
	}
}

func TestTheRunnerOnlyWorksListedInstallationsAndAlwaysReleasesItsLease(t *testing.T) {
	store := &fakeWorkerStore{give: []repository.ShopAutoInstallation{{OrganizationID: 1, InstallationID: 11, GameServerID: 1}, {OrganizationID: 1, InstallationID: 12, GameServerID: 2}}}
	asked := []int64{}
	r := &shopWorkerRunner{worker: deliveryworker.New(deliveryworker.Config{Name: "t", ReportOnly: true}, nil, nil, nil, nil), store: store,
		allowed: []int64{11, 12}, owner: "worker:t", now: time.Now,
		server: func(_ context.Context, inst repository.ShopAutoInstallation) (deliveryworker.Server, error) {
			asked = append(asked, inst.InstallationID)
			return nil, errors.New("no credential")
		}}
	r.tick(context.Background())
	if len(store.claims) != 1 || len(store.claims[0]) != 2 || store.owners[0] != "worker:t" {
		t.Fatalf("claims = %v owners = %v", store.claims, store.owners)
	}
	if len(asked) != 2 || len(store.released) != 2 || store.released[0] != 11 || store.released[1] != 12 {
		t.Fatalf("asked=%v released=%v, want every claimed installation released even when its server is unavailable", asked, store.released)
	}
}
