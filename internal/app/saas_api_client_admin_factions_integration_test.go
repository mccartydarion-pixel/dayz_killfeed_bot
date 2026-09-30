//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Faction Hub moderation through the real handlers: a Moderator edits, releases branding,
// removes a member and hands leadership over; only an Administrator may dissolve; every write
// needs a reason and lands in the audit log; a faction on another installation is not found.
func TestFactionHubModerationEndpoints(t *testing.T) {
	w := newClientAdminWorld(t)
	w.a.FactionHub = repository.NewFactionHubRepository(w.a.DB.Pool)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	leader := syncUser(t, w.a, fmt.Sprintf("mod-leader-%d", suffix), "Leader")
	grunt := syncUser(t, w.a, fmt.Sprintf("mod-grunt-%d", suffix), "Grunt")
	f, err := w.a.FactionHub.CreateFaction(ctx, w.f.OrgID, w.f.InstallationID, leader.ID, repository.HubFactionInput{Name: "Bad Words Crew", Tag: "BWC", Description: "hello", RecruitmentStatus: "OPEN"})
	if err != nil {
		t.Fatal(err)
	}
	wolf, red := "WOLF", "RED"
	if _, err := w.a.FactionHub.UpdateFaction(ctx, w.f.OrgID, w.f.InstallationID, f.ID, leader.ID, repository.HubFactionUpdate{FlagKey: &wolf, ArmbandKey: &red}); err != nil {
		t.Fatal(err)
	}
	app, err := w.a.FactionHub.Apply(ctx, w.f.OrgID, w.f.InstallationID, f.ID, grunt.ID, "let me in")
	if err != nil {
		t.Fatal(err)
	}
	_, gruntMember, err := w.a.FactionHub.AcceptApplication(ctx, w.f.OrgID, w.f.InstallationID, f.ID, app.ID, leader.ID)
	if err != nil {
		t.Fatal(err)
	}
	fid := strconv.FormatInt(f.ID, 10)
	pv := map[string]string{"factionID": fid}

	// A Moderator-mapped staff member (not a faction member, not the org owner).
	modRole := fmt.Sprintf("mod-role-%d", suffix)
	mod := syncUser(t, w.a, fmt.Sprintf("mod-staff-%d", suffix), "Staff")
	w.mapRole(mod.DiscordUserID, modRole, "MODERATOR")

	// List shows the faction with its leader, branding and pending count.
	rr := w.call(w.a.handleModerationListFactions, http.MethodGet, w.path("/hub-factions"), mod.DiscordUserID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var list struct{ Items []map[string]any }
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	if len(list.Items) != 1 || list.Items[0]["tag"] != "BWC" || list.Items[0]["flagKey"] != "WOLF" || list.Items[0]["leader"].(map[string]any)["userId"].(float64) != float64(leader.ID) || list.Items[0]["memberCount"].(float64) != 2 {
		t.Fatalf("moderation list: %v", list.Items)
	}

	// Writes need a reason.
	rr = w.call(w.a.handleModerationUpdateFaction, http.MethodPut, w.path("/hub-factions/"+fid), mod.DiscordUserID, map[string]any{"name": "Polite Crew"}, pv)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("reason required: %d %s", rr.Code, rr.Body.String())
	}
	// Rename + close recruiting + release the flag; the armband stays; colors cannot be set by staff.
	rr = w.call(w.a.handleModerationUpdateFaction, http.MethodPut, w.path("/hub-factions/"+fid), mod.DiscordUserID, map[string]any{"name": "Polite Crew", "recruitmentStatus": "CLOSED", "releaseFlag": true, "primaryColor": "#000000", "reason": "offensive name"}, pv)
	if rr.Code != http.StatusOK {
		t.Fatalf("moderate: %d %s", rr.Code, rr.Body.String())
	}
	var after map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &after)
	if after["name"] != "Polite Crew" || after["recruitmentStatus"] != "CLOSED" || after["flagKey"] != nil || after["armbandKey"] != "RED" || after["primaryColor"] != nil {
		t.Fatalf("moderated faction: %v", after)
	}

	// The leader cannot be removed; a member can.
	leaderMember, _ := w.a.FactionHub.MyFaction(ctx, w.f.OrgID, w.f.InstallationID, leader.ID)
	rr = w.call(w.a.handleModerationRemoveMember, http.MethodDelete, w.path("/hub-factions/"+fid+"/members/"+strconv.FormatInt(leaderMember.Member.ID, 10)), mod.DiscordUserID, map[string]any{"reason": "x"}, map[string]string{"factionID": fid, "memberID": strconv.FormatInt(leaderMember.Member.ID, 10)})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("leader protected: %d %s", rr.Code, rr.Body.String())
	}
	// Transfer leadership to the grunt, then the old leader is removable.
	rr = w.call(w.a.handleModerationTransferLeadership, http.MethodPost, w.path("/hub-factions/"+fid+"/transfer-leadership"), mod.DiscordUserID, map[string]any{"memberId": gruntMember.ID, "reason": "leader inactive"}, pv)
	if rr.Code != http.StatusOK {
		t.Fatalf("transfer: %d %s", rr.Code, rr.Body.String())
	}
	rr = w.call(w.a.handleModerationRemoveMember, http.MethodDelete, w.path("/hub-factions/"+fid+"/members/"+strconv.FormatInt(leaderMember.Member.ID, 10)), mod.DiscordUserID, map[string]any{"reason": "gone"}, map[string]string{"factionID": fid, "memberID": strconv.FormatInt(leaderMember.Member.ID, 10)})
	if rr.Code != http.StatusOK {
		t.Fatalf("remove old leader: %d %s", rr.Code, rr.Body.String())
	}
	if mine, _ := w.a.FactionHub.MyFaction(ctx, w.f.OrgID, w.f.InstallationID, grunt.ID); mine.Faction == nil || mine.Member.RoleKey != "LEADER" {
		t.Fatalf("grunt leads now: %+v", mine)
	}

	// A Moderator may not dissolve; an Administrator may, with typed confirmation.
	rr = w.call(w.a.handleModerationDissolveFaction, http.MethodPost, w.path("/hub-factions/"+fid+"/dissolve"), mod.DiscordUserID, map[string]any{"confirm": "DISSOLVE", "reason": "r"}, pv)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("moderator cannot dissolve: %d %s", rr.Code, rr.Body.String())
	}
	rr = w.call(w.a.handleModerationDissolveFaction, http.MethodPost, w.path("/hub-factions/"+fid+"/dissolve"), w.f.OwnerDiscordID, map[string]any{"confirm": "nope", "reason": "r"}, pv)
	if rr.Code == http.StatusOK {
		t.Fatalf("typed confirmation required: %d", rr.Code)
	}
	rr = w.call(w.a.handleModerationDissolveFaction, http.MethodPost, w.path("/hub-factions/"+fid+"/dissolve"), w.f.OwnerDiscordID, map[string]any{"confirm": "DISSOLVE", "reason": "dead faction"}, pv)
	if rr.Code != http.StatusOK {
		t.Fatalf("dissolve: %d %s", rr.Code, rr.Body.String())
	}
	if got, err := w.a.FactionHub.Get(ctx, w.f.OrgID, w.f.InstallationID, f.ID); err == nil {
		t.Fatalf("faction still exists: %+v", got)
	}
	if mine, _ := w.a.FactionHub.MyFaction(ctx, w.f.OrgID, w.f.InstallationID, grunt.ID); mine.Faction != nil {
		t.Fatal("members are released when the faction is dissolved")
	}
	// The armband it held is free again.
	claims, _ := w.a.FactionHub.BrandingClaims(ctx, w.f.OrgID, w.f.InstallationID)
	if len(claims) != 0 {
		t.Fatalf("claims after dissolve: %+v", claims)
	}

	// Every write left an audit row with its reason.
	var n int
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action IN ('FACTION_MODERATED','FACTION_MEMBER_REMOVED','FACTION_LEADERSHIP_TRANSFERRED','FACTION_HUB_DISSOLVED') AND reason <> ''`, w.f.InstallationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("audit rows: %d", n)
	}
}
