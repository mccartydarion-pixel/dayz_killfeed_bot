//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCaseBaseShadowReadsBuildsAndBasesAndCannotNotify(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	admin := zoneActor(t, w, "base-shadow-admin")
	w.mapRole(admin, "base-shadow-admin-role", "ADMINISTRATOR")
	evidence := repository.NewCaseEvidenceRepository(w.a.DB.Pool)
	source := "dayzps/config/DayZServer_PS4_x64_2026-10-01_02-00-00.ADM"
	f := func(v float64) *float64 { return &v }
	record := func(offset int64, dayzID, action string, x, z float64) {
		in := repository.CaseEvidenceInput{GuildID: w.guildID, ServerID: w.serverID, SourceID: source, SourceEndOffset: offset,
			LineSHA256: fmt.Sprintf("%064x", offset), EventType: "BUILD_ACTION", ADMClock: "02:30:00",
			Subject: repository.CaseEvidencePerson{DayZID: dayzID, Name: dayzID, X: f(x), Z: f(z)},
			Build:   &repository.CaseBuildEvidence{Action: action, Object: "Wall", Target: "Fence", Tool: "Hammer"}}
		if err := evidence.RecordCaseEvidence(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	record(100, "base-owner", "Built", 1005, 2005)
	record(200, "base-intruder", "Built", 1010, 2010)
	record(300, "base-intruder", "Dismantled", 1010, 2010) // a raid, not boosting
	var owner int64
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT id FROM players WHERE guild_id=$1 AND dayz_player_id='base-owner'`, w.guildID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES ($1,$2,$3,$4,'chernarusplus','North Base',1000,2000,50)`, w.f.InstallationID, w.guildID, w.serverID, owner); err != nil {
		t.Fatal(err)
	}

	repo := repository.NewCaseBaseRegistrationRepository(w.a.DB.Pool)
	builds, truncated, err := repo.ListBaseShadowBuilds(ctx, w.guildID, w.serverID, 500)
	if err != nil || truncated || len(builds) != 2 {
		t.Fatalf("builds (dismantles excluded): %d %v %v", len(builds), truncated, err)
	}
	if b := builds[0]; b.Action != "Built" || b.Object != "Wall" || b.Event.SubjectX == nil || *b.Event.SubjectX != 1010 {
		t.Fatalf("build row: %+v", b)
	}
	bases, err := repo.ListBaseShadowBases(ctx, w.f.InstallationID, w.guildID, w.serverID)
	if err != nil || len(bases) != 1 || !bases[0].Verified || bases[0].OwnerPlayerID != owner || bases[0].RegisteredAt.IsZero() {
		t.Fatalf("bases: %+v %v", bases, err)
	}

	path := w.path("/case/bases/shadow")
	if denied := w.call(w.a.handleCaseBaseShadow, http.MethodGet, path, admin, nil, nil); denied.Code != http.StatusForbidden {
		t.Fatalf("admin read base shadow: %d", denied.Code)
	}
	rr := w.call(w.a.handleCaseBaseShadow, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner read: %d %s", rr.Code, rr.Body.String())
	}
	out := decodeBody[struct {
		BaseShadow caseintel.ShadowBaseReport `json:"baseShadow"`
	}](t, rr).BaseShadow
	// No learned clock, build logging off and no live ADM worker in this test
	// world: the run reports why it cannot conclude and never notifies.
	if out.Mode != "SHADOW_OBSERVATION_ONLY" || out.BuildActions != 2 || out.BasesChecked != 1 || out.TrustedEvents != 0 ||
		out.Result.CanNotify || out.Result.Enforcement != "DISABLED" || len(out.Result.Findings) != 0 || out.Result.Status != "INSUFFICIENT_EVIDENCE" {
		t.Fatalf("unsafe base shadow: %+v", out)
	}
}
