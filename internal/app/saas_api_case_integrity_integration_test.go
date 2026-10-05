//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCASEIntegrityServerIsolationAndAuthorization(t *testing.T) {
	w := newClientAdminWorld(t)
	path := w.path("/anti-cheat/integrity")
	// A newly created/quiet installation is unknown, never "no cheating".
	quiet := w.call(w.a.handleAntiCheatIntegrity, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil)
	if quiet.Code != http.StatusOK {
		t.Fatalf("quiet server read: %d %s", quiet.Code, quiet.Body.String())
	}
	quietOut := decodeBody[caseSourceIntegrity](t, quiet)
	if quietOut.EvidenceObservationStatus != "WORKER_UNAVAILABLE" ||
		quietOut.PipelineHealth.State != "DEGRADED" || !quietOut.PipelineHealth.ConclusionsSuspended ||
		quietOut.EvidenceAdmissibility.ObservationCount != 0 ||
		quietOut.EvidenceAdmissibility.SourceCount != 0 ||
		quietOut.EvidenceAdmissibility.MovementDetectorStatus != "BLOCKED" ||
		quietOut.EvidenceAdmissibility.SafeSpeedPairs != 0 ||
		quietOut.EvidenceAdmissibility.Enforcement != "DISABLED" ||
		quietOut.DetectorsEnabled || quietOut.ElapsedTimeTrusted ||
		quietOut.EvidenceAdmissibility.Coverage != "FILTERED_SOURCE_EVENTS_ONLY" {
		t.Fatalf("quiet server must stay unverified: %+v", quietOut)
	}
	sawNoEvents := false
	for _, blocker := range quietOut.EvidenceAdmissibility.Blockers {
		if blocker == "NO_OBSERVED_EVENTS" {
			sawNoEvents = true
		}
		if blocker == "NO_CHEATING" {
			t.Fatal("quiet server falsely classified as no cheating")
		}
	}
	if !sawNoEvents {
		t.Fatalf("missing quiet-server blocker: %+v", quietOut.EvidenceAdmissibility.Blockers)
	}
	repo := repository.NewCaseEvidenceRepository(w.a.DB.Pool)
	in := caseHitInput(w.guildID, w.serverID, 500, "dayzps/config/integrity.ADM", fmt.Sprintf("%064x", 500))
	if err := repo.RecordCaseEvidence(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	rr := w.call(w.a.handleAntiCheatIntegrity, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner read: %d %s", rr.Code, rr.Body.String())
	}
	out := decodeBody[caseSourceIntegrity](t, rr)
	if strings.Contains(rr.Body.String(), in.SourceID) || strings.Contains(rr.Body.String(), "dayzps/config/") {
		t.Fatal("canonical ADM source path leaked through quality response")
	}
	if out.EvidenceObservationStatus != "WORKER_UNAVAILABLE" || out.ServerID != w.serverID || out.EvidenceLines24h != 1 || out.LatestEvidenceOffset == nil || *out.LatestEvidenceOffset != 500 ||
		out.PipelineHealth.State != "DEGRADED" || !out.PipelineHealth.ConclusionsSuspended ||
		out.LatestEvidenceSourceRef == nil || *out.LatestEvidenceSourceRef == in.SourceID ||
		out.Continuity.CurrentSourceEvidenceStatus != "UNKNOWN" || len(out.Continuity.RecentSources) != 1 || out.Continuity.RecentSources[0].RecordedLines != 1 ||
		out.EvidenceAdmissibility.ObservationCount != 1 || out.EvidenceAdmissibility.SourceCount != 1 ||
		out.EvidenceAdmissibility.MovementDetectorStatus != "BLOCKED" || out.EvidenceAdmissibility.SafeSpeedPairs != 0 ||
		out.EvidenceAdmissibility.Enforcement != "DISABLED" || out.DetectorsEnabled || out.ElapsedTimeTrusted || out.MovementDetectorStatus != "BLOCKED" {
		t.Fatalf("integrity scope/contract: %+v", out)
	}
	// One guild may contain more than one installation. Never aggregate both.
	var otherServer int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `
 INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
 VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
		w.guildID, fmt.Sprintf("case-integrity-other-%d", time.Now().UnixNano()), w.f.OrgID).Scan(&otherServer); err != nil {
		t.Fatal(err)
	}
	other := caseHitInput(w.guildID, otherServer, 600, "dayzps/config/other.ADM", fmt.Sprintf("%064x", 600))
	if err := repo.RecordCaseEvidence(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	rr = w.call(w.a.handleAntiCheatIntegrity, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil)
	out = decodeBody[caseSourceIntegrity](t, rr)
	if out.EvidenceLines24h != 1 || *out.LatestEvidenceOffset != 500 || len(out.Continuity.RecentSources) != 1 || out.Continuity.RecentSources[0].RecordedLines != 1 || out.EvidenceAdmissibility.ObservationCount != 1 ||
		out.EvidenceAdmissibility.SourceCount != 1 {
		t.Fatal("foreign server evidence leaked")
	}
	stranger := syncUser(t, w.a, fmt.Sprintf("case-integrity-stranger-%d", time.Now().UnixNano()), "Stranger")
	denied := w.call(w.a.handleAntiCheatIntegrity, http.MethodGet, path, stranger.DiscordUserID, nil, nil)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("unauthorized integrity read: %d", denied.Code)
	}
}
