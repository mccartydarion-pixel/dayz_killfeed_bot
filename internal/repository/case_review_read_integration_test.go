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

// All writes in this test are synthetic and restricted to a fresh disposable
// schema. The repository under test issues SELECT only.
func TestCaseReviewReaderScopeAndPagination(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("disposable integration database required")
		}
		t.Skip("TEST_DATABASE_URL is absent")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("explicit disposable database authorization required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("case_review_read_%d", time.Now().UnixNano())
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
			t.Fatalf("%s: %v", query, e)
		}
		return id
	}
	owner := one(`INSERT INTO app_users(discord_user_id,discord_username) VALUES('case-review-reader','fixture') RETURNING id`)
	org := one(`INSERT INTO organizations(name,slug,owner_user_id) VALUES('Reader fixture','reader-fixture',$1) RETURNING id`, owner)
	guild := one(`INSERT INTO guilds(discord_guild_id) VALUES('case-reader-guild') RETURNING id`)
	conn := one(`INSERT INTO discord_guild_connections(organization_id,guild_id) VALUES($1,$2) RETURNING id`, org, guild)
	server := one(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'qa-fixture','reader-one','dayz','PLAYSTATION','ACTIVE',$2) RETURNING id`, guild, org)
	otherServer := one(`INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'qa-fixture','reader-two','dayz','PLAYSTATION','ACTIVE',$2) RETURNING id`, guild, org)
	inst := one(`INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id)
 VALUES($1,$2,$3) RETURNING id`, org, conn, server)
	foreignInst := one(`INSERT INTO installations(organization_id,discord_guild_connection_id,game_server_id)
 VALUES($1,$2,$3) RETURNING id`, org, conn, otherServer)
	hexA := strings.Repeat("a", 64)
	hexB := strings.Repeat("b", 64)
	hexC := strings.Repeat("c", 64)
	ev := one(`INSERT INTO case_evidence_events(guild_id,server_id,source_id,source_end_offset,line_sha256,event_type)
 VALUES($1,$2,'source-fixture',100,$3,'PLAYER_HIT') RETURNING id`, guild, server, hexA)
	add := func(s, i int64, fp string) int64 {
		t.Helper()
		return one(`INSERT INTO case_review_cases(guild_id,server_id,installation_id,discord_guild_connection_id,
 detector_id,detector_version,evidence_fingerprint,source_quality_ref,status)
 VALUES($1,$2,$3,$4,'TEST-FIXTURE','0.0.0',$5,$6,'PENDING_REVIEW') RETURNING id`,
			guild, s, i, conn, fp, hexC)
	}
	first := add(server, inst, hexA)
	second := add(server, inst, hexB)
	other := add(otherServer, foreignInst, hexA)
	if first <= 0 || second <= first || other <= second {
		t.Fatal("invalid fixture ordering")
	}
	if _, err = db.Pool.Exec(ctx, `INSERT INTO case_review_evidence(guild_id,server_id,installation_id,case_id,evidence_id)
 VALUES($1,$2,$3,$4,$5)`, guild, server, inst, first, ev); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, `INSERT INTO case_review_audit(guild_id,server_id,installation_id,case_id,
 action_key,actor_user_id,from_status,to_status,reason_code,note)
 VALUES($1,$2,$3,$4,$5,$6,'PENDING_REVIEW','DISMISSED','FIXTURE','not a real finding')`,
		guild, server, inst, first, hexC, owner); err != nil {
		t.Fatal(err)
	}
	reader := NewCaseReviewReader(db.Pool)
	scope := CaseReviewScope{GuildID: guild, ServerID: server, InstallationID: inst}
	records, err := reader.List(ctx, scope, nil, 50)
	if err != nil || len(records) != 2 {
		t.Fatalf("scoped list: %+v %v", records, err)
	}
	if records[0].ID != second || records[1].ID != first ||
		records[0].EvidenceCount != 0 || records[1].EvidenceCount != 1 ||
		records[1].AuditCount != 1 || records[1].Status != "PENDING_REVIEW" {
		t.Fatalf("incorrect exact-scope summaries: %+v", records)
	}
	page, err := reader.List(ctx, scope, &second, 1)
	if err != nil || len(page) != 1 || page[0].ID != first {
		t.Fatalf("cursor: %+v %v", page, err)
	}
	otherScope := CaseReviewScope{GuildID: guild, ServerID: otherServer, InstallationID: foreignInst}
	foreign, err := reader.List(ctx, otherScope, nil, 50)
	if err != nil || len(foreign) != 1 || foreign[0].ID != other {
		t.Fatalf("foreign scope: %+v %v", foreign, err)
	}
	for _, bad := range []CaseReviewScope{
		{GuildID: guild, ServerID: server, InstallationID: foreignInst},
		{GuildID: guild, ServerID: otherServer, InstallationID: inst},
		{GuildID: guild + 1000000, ServerID: server, InstallationID: inst},
	} {
		rows, e := reader.List(ctx, bad, nil, 50)
		if e != nil || len(rows) != 0 {
			t.Fatalf("scope leak for %+v: %+v %v", bad, rows, e)
		}
	}
	for _, bad := range []struct {
		scope  CaseReviewScope
		before *int64
		limit  int
	}{
		{CaseReviewScope{}, nil, 50}, {scope, nil, 0}, {scope, nil, 51},
		{scope, func() *int64 { n := int64(0); return &n }(), 50},
	} {
		if _, e := reader.List(ctx, bad.scope, bad.before, bad.limit); e == nil {
			t.Fatalf("unsafe request accepted: %+v", bad)
		}
	}
	var missing *CaseReviewReader
	if _, e := missing.List(ctx, scope, nil, 10); e == nil {
		t.Fatal("missing DB was accepted")
	}
}
