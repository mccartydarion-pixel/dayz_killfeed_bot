//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func seedCaseKill(t *testing.T, w *clientAdminWorld, serverID, killerID, victimID int64, eventAt *time.Time, fingerprint string) {
	t.Helper()
	_, err := w.a.DB.Pool.Exec(context.Background(), `
INSERT INTO kills(guild_id,server_id,session_id,event_fingerprint,killer_player_id,victim_player_id,weapon_raw,weapon_display,distance,event_time)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		w.guildID, serverID, "case-integration-session", fingerprint, killerID, victimID, "M4A1", "M4-A1", 125.5, eventAt)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCaseObservationRealRecordsScopedAndPermissionChecked(t *testing.T) {
	w := newClientAdminWorld(t)
	killerID := w.seedPlayer("CaseAlpha")
	victimID := w.seedPlayer("CaseBravo")
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	seedCaseKill(t, w, w.serverID, killerID, victimID, &now, fmt.Sprintf("case-observed-%d", now.UnixNano()))
	// A row with no ADM source timestamp must retain ingestion provenance and
	// must not inflate the event-timestamped 24-hour count.
	seedCaseKill(t, w, w.serverID, killerID, victimID, nil, fmt.Sprintf("case-no-source-time-%d", now.UnixNano()))
	if _, err := w.a.DB.Pool.Exec(context.Background(), `
INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,y,event_type,observed_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, w.guildID, w.serverID, killerID, "CaseAlpha", 100.0, 200.0, 5.0, "HIT", now); err != nil {
		t.Fatal(err)
	}

	// A second server within the SAME guild is the important multi-installation
	// isolation case. The overview must not read its records.
	var otherServerID int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `
INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status,organization_id)
VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3)
RETURNING id`, w.guildID, fmt.Sprintf("case-other-%d", time.Now().UnixNano()), w.f.OrgID).Scan(&otherServerID); err != nil {
		t.Fatal(err)
	}
	seedCaseKill(t, w, otherServerID, killerID, victimID, &now, fmt.Sprintf("case-other-server-%d", now.UnixNano()))

	path := w.path("/anti-cheat/overview")
	rr := w.call(w.a.handleAntiCheatOverview, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	out := decodeBody[caseObservationResponse](t, rr)
	if out.Mode != "OBSERVATION_ONLY" || out.ServerID != w.serverID || out.Status != "COLLECTING" {
		t.Fatalf("incorrect observation scope/status: %+v", out)
	}
	if out.Telemetry.KillEvents24h != 1 || out.Telemetry.LocationSamples24h != 1 || out.Telemetry.HitEvents24h != nil {
		t.Fatalf("must count only this server's source-timed kills / ADM locations; hits unmeasured: %+v", out.Telemetry)
	}
	if len(out.RecentEvents) != 2 {
		t.Fatalf("expected two local kills, not the other server's kill: %+v", out.RecentEvents)
	}
	if out.RecentEvents[0].TimestampSource != "INGESTED_AT" || out.RecentEvents[1].TimestampSource != "ADM_EVENT_TIME" {
		t.Fatalf("timestamp provenance not preserved: %+v", out.RecentEvents)
	}
	if out.RecentEvents[1].Killer.ID == nil || *out.RecentEvents[1].Killer.ID != killerID || out.RecentEvents[1].Victim.ID == nil || *out.RecentEvents[1].Victim.ID != victimID {
		t.Fatalf("player records not resolved correctly: %+v", out.RecentEvents[1])
	}
	if out.DetectorsEnabled || out.Enforcement != "DISABLED" || len(out.Cases) != 0 || len(out.Alerts) != 0 {
		t.Fatal("observation endpoint must never manufacture accusations")
	}

	stranger := syncUser(t, w.a, fmt.Sprintf("case-stranger-%d", time.Now().UnixNano()), "Stranger")
	denied := w.call(w.a.handleAntiCheatOverview, http.MethodGet, path, stranger.DiscordUserID, nil, nil)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("unauthorized actor must not read C.A.S.E. observations, got %d: %s", denied.Code, denied.Body.String())
	}
}

func TestCaseObservationNoRecordsIsHonestEmptyState(t *testing.T) {
	w := newClientAdminWorld(t)
	rr := w.call(w.a.handleAntiCheatOverview, http.MethodGet, w.path("/anti-cheat/overview"), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	out := decodeBody[caseObservationResponse](t, rr)
	if out.Status != "AWAITING_EVENTS" || out.Telemetry.KillEvents24h != 0 || out.Telemetry.LocationSamples24h != 0 ||
		out.Telemetry.LastKillAt != nil || out.Telemetry.LastLocationSampleAt != nil ||
		len(out.RecentEvents) != 0 || out.Telemetry.HitEvents24h != nil {
		t.Fatalf("unexpected fabricated observations: %+v", out)
	}
}
