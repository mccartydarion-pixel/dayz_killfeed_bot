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

func TestCASEReviewHistoryExactCaseScopeAndRevocation(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	ownerID := mustAppUserID(t,w.a,w.f.OwnerDiscordID)
	addCase := func(serverID,installationID int64, fingerprint string) int64 {
		t.Helper()
		var id int64
		err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO case_review_cases
		 (guild_id,server_id,installation_id,discord_guild_connection_id,
		 detector_id,detector_version,evidence_fingerprint,source_quality_ref,status)
		 VALUES($1,$2,$3,$4,'FIXTURE','0.0.0',$5,$6,'PENDING_REVIEW') RETURNING id`,
			w.guildID,serverID,installationID,w.f.ConnectionID,fingerprint,
			strings.Repeat("f",64)).Scan(&id)
		if err != nil { t.Fatal(err) }
		return id
	}
	caseID := addCase(w.serverID,w.f.InstallationID,strings.Repeat("a",64))
	addAudit := func(serverID,installationID,caseID int64,key string) int64 {
		t.Helper()
		var id int64
		err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO case_review_audit
		 (guild_id,server_id,installation_id,case_id,action_key,actor_user_id,
		 from_status,to_status,reason_code,note)
		 VALUES($1,$2,$3,$4,$5,$6,'PENDING_REVIEW','DISMISSED','FIXTURE',$7)
		 RETURNING id`,w.guildID,serverID,installationID,caseID,key,ownerID,
			"private fixture note: do not expose").Scan(&id)
		if err != nil { t.Fatal(err) }
		return id
	}
	first := addAudit(w.serverID,w.f.InstallationID,caseID,strings.Repeat("b",64))
	second := addAudit(w.serverID,w.f.InstallationID,caseID,strings.Repeat("c",64))
	var otherServer, otherInstallation int64
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO game_servers
		(guild_id,provider,provider_service_id,game,platform,status,organization_id)
		VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
		w.guildID,fmt.Sprintf("case-history-other-%d",time.Now().UnixNano()),w.f.OrgID).
		Scan(&otherServer); err != nil { t.Fatal(err) }
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO installations
		(organization_id,discord_guild_connection_id,game_server_id)
		VALUES($1,$2,$3) RETURNING id`,w.f.OrgID,w.f.ConnectionID,otherServer).
		Scan(&otherInstallation); err != nil { t.Fatal(err) }
	foreignCase := addCase(otherServer,otherInstallation,strings.Repeat("d",64))
	_ = addAudit(otherServer,otherInstallation,foreignCase,strings.Repeat("e",64))
	path := w.path(fmt.Sprintf("/anti-cheat/cases/%d/history",caseID))
	call := func(path,actor,casePathID string) *caseReviewHistoryResponse {
		t.Helper()
		rr := w.call(w.a.handleAntiCheatCaseHistory,http.MethodGet,path,actor,nil,
			map[string]string{"caseID":casePathID})
		return &caseReviewHistoryResponse{code:rr.Code,body:rr.Body.String(),page:func()caseReviewHistoryPage{
			if rr.Code == http.StatusOK { return decodeBody[caseReviewHistoryPage](t,rr) }
			return caseReviewHistoryPage{}
		}()}
	}
	id := fmt.Sprint(caseID)
	firstPage := call(path+"?limit=1",w.f.OwnerDiscordID,id)
	if firstPage.code != http.StatusOK || firstPage.page.Mode != "NEUTRAL_REVIEW_HISTORY_ONLY" ||
		firstPage.page.ServerID != w.serverID || firstPage.page.CaseID != caseID ||
		len(firstPage.page.Items) != 1 || firstPage.page.Items[0].ID != second ||
		firstPage.page.NextCursor == nil || firstPage.page.DetectorsEnabled ||
		firstPage.page.AlertsEnabled || firstPage.page.Enforcement != "DISABLED" ||
		strings.Contains(firstPage.body,"private fixture note") {
		t.Fatalf("unsafe first history page: %+v",firstPage)
	}
	next := call(path+"?limit=1&before="+*firstPage.page.NextCursor,w.f.OwnerDiscordID,id)
	if next.code != http.StatusOK || len(next.page.Items) != 1 || next.page.Items[0].ID != first {
		t.Fatalf("history pagination: %+v",next)
	}
	foreign := call(w.path(fmt.Sprintf("/anti-cheat/cases/%d/history",foreignCase)),w.f.OwnerDiscordID,fmt.Sprint(foreignCase))
	if foreign.code != http.StatusOK || len(foreign.page.Items) != 0 {
		t.Fatalf("foreign case history leaked: %+v",foreign)
	}
	for _,bad := range []struct{suffix,pathID string}{
		{"","0"},{"","bad"},{"?before=0",id},{"?limit=51",id},
	} {
		result := call(path+bad.suffix,w.f.OwnerDiscordID,bad.pathID)
		if result.code != http.StatusBadRequest { t.Fatalf("accepted invalid history request: %+v",result) }
	}
	stranger := syncUser(t,w.a,fmt.Sprintf("case-history-outsider-%d",time.Now().UnixNano()),"Outsider")
	if denied := call(path,stranger.DiscordUserID,id); denied.code != http.StatusForbidden {
		t.Fatalf("outsider read: %+v",denied)
	}
	if _,err := w.a.DB.Pool.Exec(ctx,`UPDATE organization_members SET role='MEMBER'
		WHERE organization_id=$1 AND user_id=$2`,w.f.OrgID,ownerID); err != nil { t.Fatal(err) }
	revoked := call(path,w.f.OwnerDiscordID,id)
	if revoked.code == http.StatusOK {
		if len(revoked.page.Items) != 0 { t.Fatalf("revoked member saw audit: %+v",revoked) }
	} else if revoked.code != http.StatusForbidden { t.Fatalf("unexpected revoked result: %+v",revoked) }
}

type caseReviewHistoryResponse struct {
	code int
	body string
	page caseReviewHistoryPage
}
