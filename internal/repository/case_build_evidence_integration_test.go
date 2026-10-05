//go:build integration

package repository

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

func TestCaseBuildEvidenceReplayAndScopedReadback(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("disposable integration database required")
		}
		t.Skip("TEST_DATABASE_URL unavailable")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("disposable database authorization required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("case_build_evidence_%d", time.Now().UnixNano())
	if _, err = admin.Pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		admin.Close()
	})
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	db, err := database.Connect(ctx, url+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	one := func(query string, args ...any) int64 {
		t.Helper()
		var id int64
		if e := db.Pool.QueryRow(ctx, query, args...).Scan(&id); e != nil {
			t.Fatal(e)
		}
		return id
	}
	user := one(`INSERT INTO app_users(discord_user_id,discord_username)
 VALUES('case-build-fixture','fixture') RETURNING id`)
	org := one(`INSERT INTO organizations(name,slug,owner_user_id)
 VALUES('Case build fixture','case-build-fixture',$1) RETURNING id`, user)
	guild := one(`INSERT INTO guilds(discord_guild_id) VALUES('case-build-guild') RETURNING id`)
	server := one(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'qa-fixture','build-one','dayz','PLAYSTATION','ACTIVE',$2) RETURNING id`, guild, org)
	other := one(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'qa-fixture','build-two','dayz','PLAYSTATION','ACTIVE',$2) RETURNING id`, guild, org)
	x, z, alt := 100.5, 200.5, 5.5
	item := CaseEvidenceInput{
		GuildID: guild, ServerID: server, SourceID: "test/build.ADM", SourceEndOffset: 100,
		LineSHA256: strings.Repeat("a", 64), EventType: "BUILD_ACTION", ADMClock: "16:31:00",
		Subject: CaseEvidencePerson{DayZID: "b001", Name: "Builder", X: &x, Z: &z, Altitude: &alt},
		Build:   &CaseBuildEvidence{Action: "Built", Object: "Fence Wall", Target: "Fence", Tool: "Hatchet"},
	}
	repo := NewCaseEvidenceRepository(db.Pool)
	if err = repo.RecordCaseEvidence(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err = repo.RecordCaseEvidence(ctx, item); err != nil {
		t.Fatalf("same-line replay: %v", err)
	}
	rows, err := repo.ListCaseEvidence(ctx, guild, server, nil, nil, nil, 10)
	if err != nil || len(rows) != 1 || rows[0].BuildAction != "Built" || rows[0].BuildObject != "Fence Wall" ||
		rows[0].BuildTarget != "Fence" || rows[0].BuildTool != "Hatchet" || rows[0].SubjectX == nil ||
		*rows[0].SubjectX != x || rows[0].SubjectZ == nil || *rows[0].SubjectZ != z {
		t.Fatalf("build readback incorrect: %+v %v", rows, err)
	}
	id := rows[0].ID
	exact, err := repo.ListCaseEvidence(ctx, guild, server, nil, nil, &id, 10)
	if err != nil || len(exact) != 1 || exact[0].ID != id {
		t.Fatalf("exact scoped readback: %+v %v", exact, err)
	}
	foreign, err := repo.ListCaseEvidence(ctx, guild, other, nil, nil, &id, 10)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("other server saw evidence: %+v %v", foreign, err)
	}
	mismatch := item
	mismatch.Build = &CaseBuildEvidence{Action: "Built", Object: "Other", Target: "Fence", Tool: "Hatchet"}
	if err = repo.RecordCaseEvidence(ctx, mismatch); err == nil {
		t.Fatal("same source/hash with changed tuple accepted")
	}
	changed := item
	changed.LineSHA256 = strings.Repeat("b", 64)
	if err = repo.RecordCaseEvidence(ctx, changed); err == nil {
		t.Fatal("changed source line accepted")
	}
	missing := item
	missing.SourceEndOffset = 200
	missing.Build = nil
	if err = repo.RecordCaseEvidence(ctx, missing); err == nil {
		t.Fatal("missing tuple accepted")
	}
	otherItem := item
	otherItem.ServerID = other
	if err = repo.RecordCaseEvidence(ctx, otherItem); err != nil {
		t.Fatal(err)
	}
	rows, err = repo.ListCaseEvidence(ctx, guild, other, nil, nil, nil, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("other server write: %+v %v", rows, err)
	}
}
