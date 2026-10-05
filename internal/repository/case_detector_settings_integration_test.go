//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
)

func TestCaseDetectorSettingsDefaultsIdempotencyAndScope(t *testing.T) {
	repo, fx, _ := newZoneTestWorld(t)
	ctx := context.Background()
	settings := NewCaseDetectorSettingsRepository(repo.pool)
	all, err := settings.List(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID)
	if err != nil || len(all) != 8 {
		t.Fatalf("default settings: %d %v", len(all), err)
	}
	for _, s := range all {
		if s.Sensitivity != caseintel.SensitivityBalanced || s.Configured || s.Revision != 0 || s.UpdatedAt != nil {
			t.Fatalf("unsafe default: %+v", s)
		}
	}
	id := "CASE-LOGIN-001"
	first, err := settings.Set(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, id, caseintel.SensitivityStrict)
	if err != nil || first.Revision != 1 || !first.Configured {
		t.Fatalf("first setting: %+v %v", first, err)
	}
	same, err := settings.Set(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, id, caseintel.SensitivityStrict)
	if err != nil || same.Revision != 1 || !same.UpdatedAt.Equal(*first.UpdatedAt) {
		t.Fatalf("idempotent setting: %+v %v", same, err)
	}
	changed, err := settings.Set(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, id, caseintel.SensitivityRelaxed)
	if err != nil || changed.Revision != 2 || changed.Sensitivity != caseintel.SensitivityRelaxed {
		t.Fatalf("changed setting: %+v %v", changed, err)
	}
	all, err = settings.List(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID)
	if err != nil || len(all) != 8 {
		t.Fatalf("settings readback: %d %v", len(all), err)
	}
	for _, s := range all {
		if s.ModuleID == id {
			if s.Sensitivity != caseintel.SensitivityRelaxed || s.Revision != 2 || !s.Configured {
				t.Fatalf("readback: %+v", s)
			}
		} else if s.Sensitivity != caseintel.SensitivityBalanced || s.Configured {
			t.Fatalf("other module changed: %+v", s)
		}
	}
	if _, err = settings.Set(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, "CASE-MOV-001", caseintel.SensitivityStrict); err == nil {
		t.Fatal("ninth module accepted")
	}
	if _, err = settings.Set(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, id, caseintel.Sensitivity("ENABLED")); err == nil {
		t.Fatal("invalid sensitivity accepted")
	}
	if _, err = settings.Set(ctx, fx.InstallationID+999, fx.GuildRowID, fx.ServerRowID, id, caseintel.SensitivityStrict); err == nil {
		t.Fatal("foreign installation setting accepted")
	}
	foreign, err := settings.List(ctx, fx.InstallationID+999, fx.GuildRowID, fx.ServerRowID)
	if err != nil || len(foreign) != 8 {
		t.Fatalf("foreign scope read: %d %v", len(foreign), err)
	}
	for _, s := range foreign {
		if s.Configured {
			t.Fatalf("cross-installation setting leaked: %+v", s)
		}
	}
}
