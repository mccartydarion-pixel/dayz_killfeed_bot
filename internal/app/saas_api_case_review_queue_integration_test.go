//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCASEReviewQueueExactInstallationAndRevocation(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	add := func(serverID, installationID, connectionID int64, fingerprint string) int64 {
		t.Helper()
		var id int64
		err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO case_review_cases
		 (guild_id,server_id,installation_id,discord_guild_connection_id,
		 detector_id,detector_version,evidence_fingerprint,source_quality_ref,status)
		 VALUES($1,$2,$3,$4,'FIXTURE','0.0.0',$5,$6,'PENDING_REVIEW') RETURNING id`,
			w.guildID, serverID, installationID, connectionID, fingerprint,
			strings.Repeat("f", 64)).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := add(w.serverID, w.f.InstallationID, w.f.ConnectionID, strings.Repeat("a", 64))
	second := add(w.serverID, w.f.InstallationID, w.f.ConnectionID, strings.Repeat("b", 64))
	var otherServer, otherInstallation int64
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO game_servers
		(guild_id,provider,provider_service_id,game,platform,status,organization_id)
		VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
		w.guildID, fmt.Sprintf("case-queue-other-%d", time.Now().UnixNano()), w.f.OrgID).
		Scan(&otherServer); err != nil {
		t.Fatal(err)
	}
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO installations
		(organization_id,discord_guild_connection_id,game_server_id)
		VALUES($1,$2,$3) RETURNING id`, w.f.OrgID, w.f.ConnectionID, otherServer).
		Scan(&otherInstallation); err != nil {
		t.Fatal(err)
	}
	foreign := add(otherServer, otherInstallation, w.f.ConnectionID, strings.Repeat("c", 64))
	path := w.path("/anti-cheat/cases")
	rr := w.call(w.a.handleAntiCheatCases, http.MethodGet, path+"?limit=1", w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner read: %d %s", rr.Code, rr.Body.String())
	}
	page := decodeBody[caseReviewQueuePage](t, rr)
	if page.Mode != "NEUTRAL_REVIEW_QUEUE_ONLY" || page.ServerID != w.serverID ||
		len(page.Items) != 1 || page.Items[0].ID != second || page.NextCursor == nil ||
		page.DetectorsEnabled || page.AlertsEnabled || page.Enforcement != "DISABLED" ||
		strings.Contains(rr.Body.String(), strings.Repeat("a", 64)) ||
		strings.Contains(rr.Body.String(), fmt.Sprintf(`"id":%d`, foreign)) {
		t.Fatalf("unsafe case page: %+v", page)
	}
	rr = w.call(w.a.handleAntiCheatCases, http.MethodGet, path+"?limit=1&before="+*page.NextCursor, w.f.OwnerDiscordID, nil, nil)
	page = decodeBody[caseReviewQueuePage](t, rr)
	if rr.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].ID != first {
		t.Fatalf("cursor crossed scope: %d %+v", rr.Code, page)
	}
	for _, suffix := range []string{"?before=0", "?before=invalid", "?limit=51"} {
		bad := w.call(w.a.handleAntiCheatCases, http.MethodGet, path+suffix, w.f.OwnerDiscordID, nil, nil)
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("accepted %q: %d", suffix, bad.Code)
		}
	}
	stranger := syncUser(t, w.a, fmt.Sprintf("case-queue-outsider-%d", time.Now().UnixNano()), "Outsider")
	denied := w.call(w.a.handleAntiCheatCases, http.MethodGet, path, stranger.DiscordUserID, nil, nil)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("outsider read: %d", denied.Code)
	}
	ownerID := mustAppUserID(t, w.a, w.f.OwnerDiscordID)
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE organization_members SET role='MEMBER'
		WHERE organization_id=$1 AND user_id=$2`, w.f.OrgID, ownerID); err != nil {
		t.Fatal(err)
	}
	revoked := w.call(w.a.handleAntiCheatCases, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil)
	if revoked.Code == http.StatusOK {
		got := decodeBody[caseReviewQueuePage](t, revoked)
		if len(got.Items) != 0 {
			t.Fatalf("revoked member saw cases: %+v", got)
		}
	} else if revoked.Code != http.StatusForbidden {
		t.Fatalf("unexpected revoked result: %d %s", revoked.Code, revoked.Body.String())
	}
}
