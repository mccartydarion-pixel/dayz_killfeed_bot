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

// All inserts are synthetic and are permitted only on the disposable CI DB.
// The feature under test, AuditCaseEvidenceAdmissibility, performs SELECT only.
func TestAdmissibilityReadsPersistedEvidenceWithinExactScope(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("disposable integration DB required")
		}
		t.Skip("no test database")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("explicit disposable DB authorization required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	token := time.Now().UnixNano()
	var guildID, serverID, otherServerID int64
	err = db.Pool.QueryRow(ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`,
		fmt.Sprintf("case-admissibility-ci-%d", token)).Scan(&guildID)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool.Exec(context.Background(), `DELETE FROM guilds WHERE id=$1`, guildID)
	insertServer := func(name string) int64 {
		t.Helper()
		var id int64
		err = db.Pool.QueryRow(ctx, `INSERT INTO game_servers
  (guild_id,provider,provider_service_id,game,platform,status,display_name)
  VALUES($1,'qa-fixture',$2,'dayz','PLAYSTATION','ACTIVE','C.A.S.E. CI fixture')
  RETURNING id`, guildID, name).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	serverID = insertServer(fmt.Sprintf("primary-%d", token))
	otherServerID = insertServer(fmt.Sprintf("foreign-%d", token))
	insert := func(server int64, source string, offset int64, clock string, x, z any) {
		t.Helper()
		_, err = db.Pool.Exec(ctx, `INSERT INTO case_evidence_events
  (guild_id,server_id,source_id,source_end_offset,line_sha256,event_type,adm_clock,subject_x,subject_z)
  VALUES($1,$2,$3,$4,$5,'PLAYER_HIT',$6,$7,$8)`,
			guildID, server, source, offset, strings.Repeat("a", 64), clock, x, z)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(serverID, "qa-source-a", 100, "23:59:59", 1.0, 2.0)
	insert(serverID, "qa-source-a", 90000, "00:00:00", 3.0, nil)
	insert(serverID, "qa-source-b", 10, "invalid", nil, nil)
	insert(otherServerID, "foreign-source", 999, "09:00:00", 1.0, 2.0)
	// The first hit carries one actor axis and a different target axis;
	// they must never become a fabricated complete coordinate pair.
	_, err = db.Pool.Exec(ctx, `UPDATE case_evidence_events SET
 actor_x=12,actor_z=NULL,target_x=NULL,target_z=25
 WHERE guild_id=$1 AND server_id=$2 AND source_id='qa-source-a' AND source_end_offset=100`, guildID, serverID)
	if err != nil {
		t.Fatal(err)
	}
	audit := NewCaseEvidenceRepository(db.Pool)
	out, err := audit.AuditCaseEvidenceAdmissibility(ctx, guildID, serverID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if out.ObservationCount != 3 || out.SourceCount != 2 || out.WindowTruncated ||
		out.ValidSourceAddresses != 3 || out.HitObservations != 3 || out.KillObservations != 0 ||
		out.BoundaryObservations != 0 || out.OtherObservations != 0 ||
		out.InvalidClockStrings != 1 || out.ClockDecreasesInSource != 1 ||
		out.CompleteCoordinatePairs != 1 || out.PartialCoordinatePairs != 1 || out.MissingCoordinatePairs != 1 ||
		out.ActorPartialCoordinatePairs != 1 || out.ActorCompleteCoordinatePairs != 0 ||
		out.TargetPartialCoordinatePairs != 1 || out.TargetCompleteCoordinatePairs != 0 ||
		out.MovementDetectorStatus != "BLOCKED" || out.SafeSpeedPairs != 0 || out.Enforcement != "DISABLED" {
		t.Fatalf("persisted ADM audit mismatch: %+v", out)
	}
	// PostgreSQL float8 can retain NaN; it must not count as a usable point.
	_, err = db.Pool.Exec(ctx, `UPDATE case_evidence_events SET subject_x='NaN'::float8
 WHERE guild_id=$1 AND server_id=$2 AND source_id='qa-source-a'
 AND source_end_offset=100`, guildID, serverID)
	if err != nil {
		t.Fatal(err)
	}
	nonFinite, err := audit.AuditCaseEvidenceAdmissibility(ctx, guildID, serverID, 3)
	if err != nil || nonFinite.NonFiniteCoordinateValues != 1 ||
		nonFinite.CompleteCoordinatePairs != 0 || nonFinite.PartialCoordinatePairs != 2 ||
		nonFinite.SafeSpeedPairs != 0 || nonFinite.MovementDetectorStatus != "BLOCKED" {
		t.Fatalf("persisted non-finite axis incorrectly accepted: %+v %v", nonFinite, err)
	}
	truncated, err := audit.AuditCaseEvidenceAdmissibility(ctx, guildID, serverID, 2)
	if err != nil || !truncated.WindowTruncated || truncated.ObservationCount != 2 {
		t.Fatalf("limit+1 edge missing: %+v %v", truncated, err)
	}
	foreign, err := audit.AuditCaseEvidenceAdmissibility(ctx, guildID, otherServerID, 3)
	if err != nil || foreign.ObservationCount != 1 || foreign.SourceCount != 1 {
		t.Fatalf("server isolation failed: %+v %v", foreign, err)
	}
	empty, err := audit.AuditCaseEvidenceAdmissibility(ctx, guildID+1000000, serverID, 3)
	if err != nil || empty.ObservationCount != 0 || empty.MovementDetectorStatus != "BLOCKED" {
		t.Fatalf("guild isolation failed: %+v %v", empty, err)
	}
	for _, scope := range [][3]int64{{0, serverID, 3}, {guildID, 0, 3}, {guildID, serverID, 0}, {guildID, serverID, 501}} {
		if _, err := audit.AuditCaseEvidenceAdmissibility(ctx, scope[0], scope[1], int(scope[2])); err == nil {
			t.Fatalf("accepted unsafe scope %v", scope)
		}
	}
}
