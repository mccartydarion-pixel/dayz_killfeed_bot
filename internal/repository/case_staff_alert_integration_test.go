//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
)

func TestCaseStaffAlertQueue(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	repo := NewCaseStaffAlertRepository(pool)
	ctx := context.Background()
	player := seedPlayer("Raider")
	server := CaseAlertServer{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}

	settings, err := repo.GetSettings(ctx, server.InstallationID, server.GuildID, server.ServerID)
	if err != nil || settings.Enabled {
		t.Fatalf("staff alerts must start off: %+v %v", settings, err)
	}
	if got, err := repo.EnabledServers(ctx); err != nil || len(got) != 0 {
		t.Fatalf("enabled servers before opt-in: %+v %v", got, err)
	}
	if _, err := repo.SetSettings(ctx, server.InstallationID, server.GuildID+999999, server.ServerID, true, nil); err == nil {
		t.Fatal("settings accepted a mismatched guild")
	}
	if _, err := repo.SetSettings(ctx, server.InstallationID, server.GuildID, server.ServerID, true, &fx.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.EnabledServers(ctx); err != nil || len(got) != 1 || got[0] != server {
		t.Fatalf("enabled servers: %+v %v", got, err)
	}

	finding := caseintel.Finding{DetectorID: "CASE-LOGIN-001", PlayerID: player, PlayerName: "Raider", EvidenceIDs: []int64{1, 2},
		Scope: caseintel.Core8Scope{GuildID: server.GuildID, InstallationID: server.InstallationID, ServerID: server.ServerID},
		Tier:  caseintel.TierSuspicious, Behavior: "Reconnected again and again", EventAt: time.Now().UTC(),
		IncidentKey: strings.Repeat("c", 64)}
	if ok, err := repo.Enqueue(ctx, server, finding); err != nil || !ok {
		t.Fatalf("enqueue: %v %v", ok, err)
	}
	if ok, err := repo.Enqueue(ctx, server, finding); err != nil || ok {
		t.Fatalf("the same incident was queued twice: %v %v", ok, err)
	}
	foreign := finding
	foreign.Scope.InstallationID++
	foreign.IncidentKey = strings.Repeat("d", 64)
	if _, err := repo.Enqueue(ctx, server, foreign); err == nil {
		t.Fatal("a finding from another installation was queued")
	}
	bad := finding
	bad.IncidentKey = "not-a-key"
	if _, err := repo.Enqueue(ctx, server, bad); err == nil {
		t.Fatal("a malformed incident key was queued")
	}

	due, err := repo.ClaimDue(ctx, 10, time.Minute)
	if err != nil || len(due) != 1 {
		t.Fatalf("claim: %+v %v", due, err)
	}
	d := due[0]
	if d.Attempts != 1 || d.OrganizationID != fx.OrgID || d.DiscordGuildID == "" || d.Finding.IncidentKey != finding.IncidentKey || d.Finding.PlayerName != "Raider" {
		t.Fatalf("claimed delivery: %+v", d)
	}
	if again, err := repo.ClaimDue(ctx, 10, time.Minute); err != nil || len(again) != 0 {
		t.Fatalf("a leased alert was claimed twice: %+v %v", again, err)
	}
	if err := repo.MarkRetry(ctx, d.ID, "DISCORD_REJECTED", 0); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSent(ctx, d.ID, "c", "m"); err == nil {
		t.Fatal("marked sent without a lease")
	}
	due, err = repo.ClaimDue(ctx, 10, time.Minute)
	if err != nil || len(due) != 1 || due[0].Attempts != 2 {
		t.Fatalf("retry claim: %+v %v", due, err)
	}
	if err := repo.MarkSent(ctx, d.ID, "alerts", "msg-1"); err != nil {
		t.Fatal(err)
	}
	recent, err := repo.Recent(ctx, server.InstallationID, server.GuildID, server.ServerID, 10)
	if err != nil || len(recent) != 1 || recent[0].Status != CaseAlertStatusSent || recent[0].PlayerName != "Raider" || recent[0].SentAt == nil {
		t.Fatalf("recent: %+v %v", recent, err)
	}

	// An alert that keeps failing gives up after the last attempt.
	second := finding
	second.IncidentKey = strings.Repeat("e", 64)
	if _, err := repo.Enqueue(ctx, server, second); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < CaseAlertMaxAttempts; i++ {
		due, err := repo.ClaimDue(ctx, 10, time.Minute)
		if err != nil || len(due) != 1 {
			t.Fatalf("attempt %d claim: %+v %v", i+1, due, err)
		}
		if err := repo.MarkRetry(ctx, due[0].ID, "DISCORD_REJECTED", 0); err != nil {
			t.Fatal(err)
		}
	}
	if more, err := repo.ClaimDue(ctx, 10, time.Minute); err != nil || len(more) != 0 {
		t.Fatalf("claimed after the last attempt: %+v %v", more, err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM case_alert_deliveries WHERE incident_key=$1`, second.IncidentKey).Scan(&status); err != nil || status != CaseAlertStatusDead {
		t.Fatalf("status after last attempt: %q %v", status, err)
	}

	// A crashed send (expired lease) is retried; on the last attempt it dies.
	third := finding
	third.IncidentKey = strings.Repeat("f", 64)
	if _, err := repo.Enqueue(ctx, server, third); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE case_alert_deliveries SET status='LEASED',lease_until=NOW()-INTERVAL '1 minute',attempts=$2
 WHERE incident_key=$1`, third.IncidentKey, CaseAlertMaxAttempts); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.ClaimDue(ctx, 10, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("expired final lease was claimed: %+v %v", got, err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM case_alert_deliveries WHERE incident_key=$1`, third.IncidentKey).Scan(&status); err != nil || status != CaseAlertStatusDead {
		t.Fatalf("expired final lease status: %q %v", status, err)
	}
}
