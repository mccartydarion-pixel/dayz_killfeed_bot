//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// deleteWorld is the owner world plus direct database helpers for the delete tests.
type deleteWorld struct {
	*adminWorld
	ctx context.Context
}

func newDeleteWorld(t *testing.T) *deleteWorld {
	t.Helper()
	return &deleteWorld{adminWorld: newOwnerWorld(t), ctx: context.Background()}
}

func (w *deleteWorld) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := w.a.DB.Pool.QueryRow(w.ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (w *deleteWorld) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := w.a.DB.Pool.Exec(w.ctx, query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func (w *deleteWorld) guildID(t *testing.T, f installationFixture) int64 {
	t.Helper()
	return w.count(t, `SELECT guild_id FROM discord_guild_connections WHERE id=$1`, f.ConnectionID)
}

// connectServer gives the fixture's installation a game server, the way setup does.
func (w *deleteWorld) connectServer(t *testing.T, f installationFixture) int64 {
	t.Helper()
	var serverID int64
	err := w.a.DB.Pool.QueryRow(w.ctx, `
INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, active, organization_id)
VALUES($1,'nitrado',$2,'dayz','xbox','Delete Test Server','CONNECTED',TRUE,$3) RETURNING id`,
		w.guildID(t, f), fmt.Sprintf("svc-%d", time.Now().UnixNano()), f.OrgID).Scan(&serverID)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.a.SaaSInstallations.SetGameServer(w.ctx, f.OrgID, f.InstallationID, serverID); err != nil {
		t.Fatal(err)
	}
	return serverID
}

// linkPlayer gives the fixture's owner a player and a verified link in the fixture's guild.
func (w *deleteWorld) linkPlayer(t *testing.T, f installationFixture) (linkID int64) {
	t.Helper()
	guild := w.guildID(t, f)
	var playerID int64
	if err := w.a.DB.Pool.QueryRow(w.ctx, `INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,'Survivor') RETURNING id`,
		guild, fmt.Sprintf("dz-%d", time.Now().UnixNano())).Scan(&playerID); err != nil {
		t.Fatal(err)
	}
	if err := w.a.DB.Pool.QueryRow(w.ctx, `INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,'VERIFIED') RETURNING id`,
		guild, playerID, f.OwnerDiscordID).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	return linkID
}

// orgFootprint is the row count of everything that belongs to one organization's fixture.
func (w *deleteWorld) orgFootprint(t *testing.T, f installationFixture) map[string]int64 {
	t.Helper()
	return map[string]int64{
		"organizations":               w.count(t, `SELECT COUNT(*) FROM organizations WHERE id=$1`, f.OrgID),
		"organization_members":        w.count(t, `SELECT COUNT(*) FROM organization_members WHERE organization_id=$1`, f.OrgID),
		"discord_guild_connections":   w.count(t, `SELECT COUNT(*) FROM discord_guild_connections WHERE organization_id=$1`, f.OrgID),
		"installations":               w.count(t, `SELECT COUNT(*) FROM installations WHERE organization_id=$1`, f.OrgID),
		"installation_setup_progress": w.count(t, `SELECT COUNT(*) FROM installation_setup_progress WHERE installation_id=$1`, f.InstallationID),
		"installation_settings":       w.count(t, `SELECT COUNT(*) FROM installation_settings WHERE installation_id=$1`, f.InstallationID),
		"subscriptions":               w.count(t, `SELECT COUNT(*) FROM subscriptions WHERE organization_id=$1`, f.OrgID),
	}
}

func (w *deleteWorld) deleteOrg(acting string, f installationFixture, confirm string) (int, string) {
	rr := w.post(w.a.handleOwnerDeleteOrganization, "/api/admin/organizations/x/delete", acting,
		map[string]string{"organizationID": strconv.FormatInt(f.OrgID, 10)}, map[string]string{"reason": "accidental community", "confirm": confirm})
	return rr.Code, rr.Body.String()
}

func (w *deleteWorld) deleteInstallation(acting string, f installationFixture, confirm string) (int, string) {
	rr := w.post(w.a.handleOwnerDeleteInstallation, "/api/admin/installations/x/delete", acting,
		map[string]string{"installationID": strconv.FormatInt(f.InstallationID, 10)}, map[string]string{"reason": "remove slot", "confirm": confirm})
	return rr.Code, rr.Body.String()
}

type deleteAuditRow struct {
	Actor, Action, TargetType, Reason, Result string
	TargetID, OrgID                           int64
	Before                                    map[string]any
}

func (w *deleteWorld) lastAudit(t *testing.T, action string, targetID int64) *deleteAuditRow {
	t.Helper()
	var row deleteAuditRow
	var before []byte
	err := w.a.DB.Pool.QueryRow(w.ctx, `
SELECT actor_discord_id, action, target_type, reason, result, COALESCE(target_id,0), COALESCE(organization_id,0), COALESCE(before_state,'{}'::jsonb)
FROM platform_audit_log WHERE action=$1 AND target_id=$2 ORDER BY id DESC LIMIT 1`, action, targetID).
		Scan(&row.Actor, &row.Action, &row.TargetType, &row.Reason, &row.Result, &row.TargetID, &row.OrgID, &before)
	if err != nil {
		return nil
	}
	if err := json.Unmarshal(before, &row.Before); err != nil {
		t.Fatalf("audit snapshot is not JSON: %v", err)
	}
	return &row
}

func TestOwnerDeletesAnEmptyOrganization(t *testing.T) {
	w := newDeleteWorld(t)
	target, other := w.a1, w.b1
	linkID := w.linkPlayer(t, target)
	guild := w.guildID(t, target)
	// A Nitrado token typed during setup, with no server ever selected.
	w.exec(t, `INSERT INTO nitrado_connections(organization_id, credential_ciphertext, credential_nonce, credential_key_version, status) VALUES($1,'\x01','\x02',1,'CONNECTED')`, target.OrgID)
	w.exec(t, `INSERT INTO installation_channel_routes(installation_id, route_key, channel_id) VALUES($1,'killfeed','123')`, target.InstallationID)
	otherBefore := w.orgFootprint(t, other)
	ownerUserID := mustAppUserID(t, w.a, target.OwnerDiscordID)
	orgID := strconv.FormatInt(target.OrgID, 10)

	// The check says it is deletable and changes nothing.
	rr := w.get(w.a.handleOwnerOrganizationDeleteCheck, "/api/admin/organizations/x/delete-check", adminFounderID, map[string]string{"organizationID": orgID})
	if rr.Code != http.StatusOK {
		t.Fatalf("delete-check: %d %s", rr.Code, rr.Body.String())
	}
	var check struct {
		Deletable         bool             `json:"deletable"`
		Name              string           `json:"name"`
		OwnerDiscordID    string           `json:"ownerDiscordId"`
		Installations     int64            `json:"installations"`
		GameServers       int64            `json:"gameServers"`
		NitradoCredential bool             `json:"nitradoCredential"`
		Removes           map[string]int64 `json:"removes"`
		Blockers          []any            `json:"blockers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &check); err != nil {
		t.Fatal(err)
	}
	if !check.Deletable || len(check.Blockers) != 0 || check.Name != "Fixture Org" || check.OwnerDiscordID != target.OwnerDiscordID || check.Installations != 1 || check.GameServers != 0 || !check.NitradoCredential {
		t.Fatalf("delete-check: %s", rr.Body.String())
	}
	if check.Removes["installations"] != 1 || check.Removes["organization_members"] != 1 || check.Removes["installation_channel_routes"] != 1 {
		t.Fatalf("delete-check removes: %v", check.Removes)
	}

	// Refusals first: none of them may change anything.
	if code, body := w.deleteOrg(adminFounderID, target, "fixture org"); code != http.StatusBadRequest {
		t.Fatalf("wrong-case confirmation: %d %s", code, body)
	}
	if code, body := w.deleteOrg(adminFounderID, target, "Some Other Name"); code != http.StatusBadRequest {
		t.Fatalf("wrong confirmation: %d %s", code, body)
	}
	if code, body := w.deleteOrg(adminFounderID, target, ""); code != http.StatusBadRequest {
		t.Fatalf("missing confirmation: %d %s", code, body)
	}
	if code, body := w.deleteOrg("900000000000000001", target, "Fixture Org"); code != http.StatusForbidden {
		t.Fatalf("a stranger: %d %s", code, body)
	}
	// The organization's own owner is not a platform admin either.
	if code, body := w.deleteOrg(target.OwnerDiscordID, target, "Fixture Org"); code != http.StatusForbidden {
		t.Fatalf("the organization owner: %d %s", code, body)
	}
	if code, body := w.deleteOrg("", target, "Fixture Org"); code != http.StatusUnauthorized {
		t.Fatalf("no acting user: %d %s", code, body)
	}
	if got := w.orgFootprint(t, target); got["organizations"] != 1 || got["installations"] != 1 || got["organization_members"] != 1 {
		t.Fatalf("a refused delete changed the organization: %v", got)
	}
	if w.lastAudit(t, "organization.deleted", target.OrgID) != nil {
		t.Fatal("a refused delete wrote a deleted audit row")
	}

	code, body := w.deleteOrg(adminFounderID, target, "  Fixture Org ")
	if code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}

	for table, n := range w.orgFootprint(t, target) {
		if n != 0 {
			t.Errorf("%s still has %d row(s) of the deleted organization", table, n)
		}
	}
	if n := w.count(t, `SELECT COUNT(*) FROM installation_channel_routes WHERE installation_id=$1`, target.InstallationID); n != 0 {
		t.Errorf("channel routes left behind: %d", n)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM nitrado_connections WHERE organization_id=$1 OR (organization_id IS NULL AND guild_id IS NULL)`, target.OrgID); n != 0 {
		t.Errorf("nitrado credential left behind: %d", n)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM trial_grants WHERE organization_id=$1`, target.OrgID); n != 0 {
		t.Errorf("trial marker left behind: %d", n)
	}

	// The user is still an account, now without any organization, and their player link is intact.
	if n := w.count(t, `SELECT COUNT(*) FROM app_users WHERE id=$1 AND banned_at IS NULL`, ownerUserID); n != 1 {
		t.Fatalf("the user account is gone")
	}
	orgs, err := w.a.SaaSOrganizations.ListForUser(w.ctx, ownerUserID)
	if err != nil || len(orgs) != 0 {
		t.Fatalf("the user still has organizations: %v %v", orgs, err)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM player_links WHERE id=$1 AND discord_user_id=$2 AND status='VERIFIED'`, linkID, target.OwnerDiscordID); n != 1 {
		t.Fatal("the player link was removed")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM guilds WHERE id=$1`, guild); n != 1 {
		t.Fatal("the guild row was removed")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM players WHERE guild_id=$1`, guild); n != 1 {
		t.Fatal("the player row was removed")
	}

	// The other organization (same name, different id) is untouched.
	if got := w.orgFootprint(t, other); fmt.Sprint(got) != fmt.Sprint(otherBefore) {
		t.Fatalf("the other organization changed: %v -> %v", otherBefore, got)
	}

	audit := w.lastAudit(t, "organization.deleted", target.OrgID)
	if audit == nil {
		t.Fatal("no audit row")
	}
	if audit.Actor != adminFounderID || audit.TargetType != "organization" || audit.OrgID != target.OrgID || audit.Reason != "accidental community" || audit.Result != "OK" {
		t.Fatalf("audit row: %+v", audit)
	}
	if audit.Before["name"] != "Fixture Org" || audit.Before["ownerDiscordId"] != target.OwnerDiscordID || audit.Before["members"] != float64(1) || audit.Before["installations"] != float64(1) {
		t.Fatalf("audit snapshot: %v", audit.Before)
	}
	// And it is readable through the audit endpoint after the organization is gone.
	if got := w.auditActions(t, "?organizationId="+orgID); len(got) == 0 || got[0] != "organization.deleted" {
		t.Fatalf("audit list: %v", got)
	}

	// Gone means gone: a second delete and the check both answer 404.
	if code, body := w.deleteOrg(adminFounderID, target, "Fixture Org"); code != http.StatusNotFound {
		t.Fatalf("second delete: %d %s", code, body)
	}
	rr = w.get(w.a.handleOwnerOrganizationDeleteCheck, "/api/admin/organizations/x/delete-check", adminFounderID, map[string]string{"organizationID": orgID})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("delete-check after delete: %d", rr.Code)
	}
}

func TestOwnerDeleteOrganizationRefusesWhenNotEmpty(t *testing.T) {
	w := newDeleteWorld(t)

	t.Run("a game server is connected", func(t *testing.T) {
		serverID := w.connectServer(t, w.a1)
		code, body := w.deleteOrg(adminFounderID, w.a1, "Fixture Org")
		if code != http.StatusConflict || !strings.Contains(body, "game server") {
			t.Fatalf("delete with a server: %d %s", code, body)
		}
		if got := w.orgFootprint(t, w.a1); got["organizations"] != 1 || got["installations"] != 1 {
			t.Fatalf("refused delete changed the organization: %v", got)
		}
		if n := w.count(t, `SELECT COUNT(*) FROM game_servers WHERE id=$1 AND active AND organization_id=$2`, serverID, w.a1.OrgID); n != 1 {
			t.Fatal("refused delete touched the game server")
		}
		refused := w.lastAudit(t, "organization.delete_refused", w.a1.OrgID)
		if refused == nil || refused.Result != "REFUSED" {
			t.Fatalf("refusal audit: %+v", refused)
		}
	})

	t.Run("a live paid subscription", func(t *testing.T) {
		w.exec(t, `UPDATE subscriptions SET provider='stripe', provider_customer_id='cus_del', provider_subscription_id='sub_del', plan='PRO', status='ACTIVE' WHERE organization_id=$1`, w.b1.OrgID)
		code, body := w.deleteOrg(adminFounderID, w.b1, "Fixture Org")
		if code != http.StatusConflict || !strings.Contains(body, "Cancel the subscription first") {
			t.Fatalf("delete with a live subscription: %d %s", code, body)
		}
		w.exec(t, `UPDATE subscriptions SET status='PAST_DUE' WHERE organization_id=$1`, w.b1.OrgID)
		if code, body := w.deleteOrg(adminFounderID, w.b1, "Fixture Org"); code != http.StatusConflict {
			t.Fatalf("delete with a past-due subscription: %d %s", code, body)
		}
		// Cancelled in Stripe, but it has paid: still kept for the accounts.
		w.exec(t, `UPDATE subscriptions SET status='CANCELED' WHERE organization_id=$1`, w.b1.OrgID)
		w.exec(t, `INSERT INTO billing_transactions(organization_id, provider, provider_invoice_id, status, amount_cents, currency, stripe_event_id) VALUES($1,'stripe',$2,'PAID',999,'usd',$3)`,
			w.b1.OrgID, fmt.Sprintf("in_%d", time.Now().UnixNano()), fmt.Sprintf("evt_%d", time.Now().UnixNano()))
		code, body = w.deleteOrg(adminFounderID, w.b1, "Fixture Org")
		if code != http.StatusConflict || !strings.Contains(body, "payment record") {
			t.Fatalf("delete with payment history: %d %s", code, body)
		}
		if got := w.orgFootprint(t, w.b1); got["organizations"] != 1 || got["subscriptions"] != 1 {
			t.Fatalf("refused delete changed the organization: %v", got)
		}
		// With the payment record gone and the subscription cancelled it is empty again.
		w.exec(t, `DELETE FROM billing_transactions WHERE organization_id=$1`, w.b1.OrgID)
		if code, body := w.deleteOrg(adminFounderID, w.b1, "Fixture Org"); code != http.StatusOK {
			t.Fatalf("delete after cancellation: %d %s", code, body)
		}
	})
}

func TestOwnerDeletesAnInstallation(t *testing.T) {
	w := newDeleteWorld(t)

	t.Run("an empty setup slot", func(t *testing.T) {
		f := w.b1
		if code, body := w.deleteInstallation(adminFounderID, f, "Fixture Org"); code != http.StatusBadRequest {
			t.Fatalf("wrong confirmation: %d %s", code, body)
		}
		if code, body := w.deleteInstallation(f.OwnerDiscordID, f, strconv.FormatInt(f.InstallationID, 10)); code != http.StatusForbidden {
			t.Fatalf("the organization owner: %d %s", code, body)
		}
		if n := w.count(t, `SELECT COUNT(*) FROM installations WHERE id=$1`, f.InstallationID); n != 1 {
			t.Fatal("a refused delete removed the installation")
		}
		code, body := w.deleteInstallation(adminFounderID, f, strconv.FormatInt(f.InstallationID, 10))
		if code != http.StatusOK {
			t.Fatalf("delete: %d %s", code, body)
		}
		got := w.orgFootprint(t, f)
		if got["installations"] != 0 || got["installation_setup_progress"] != 0 || got["installation_settings"] != 0 {
			t.Fatalf("installation left behind: %v", got)
		}
		if got["organizations"] != 1 || got["organization_members"] != 1 || got["discord_guild_connections"] != 1 || got["subscriptions"] != 1 {
			t.Fatalf("the organization itself was touched: %v", got)
		}
		audit := w.lastAudit(t, "installation.deleted", f.InstallationID)
		if audit == nil || audit.Actor != adminFounderID || audit.OrgID != f.OrgID || audit.TargetType != "installation" || audit.Before["organizationName"] != "Fixture Org" || audit.Before["ownerDiscordId"] != f.OwnerDiscordID {
			t.Fatalf("audit row: %+v", audit)
		}
		if code, _ := w.deleteInstallation(adminFounderID, f, strconv.FormatInt(f.InstallationID, 10)); code != http.StatusNotFound {
			t.Fatalf("second delete: %d", code)
		}
	})

	t.Run("a connected installation", func(t *testing.T) {
		f := w.a1
		serverID := w.connectServer(t, f)
		guild := w.guildID(t, f)
		linkID := w.linkPlayer(t, f)
		w.exec(t, `INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint) VALUES($1,$2,'s1',$3)`, guild, serverID, fmt.Sprintf("fp-%d", time.Now().UnixNano()))
		w.exec(t, `INSERT INTO installation_channel_routes(installation_id, route_key, channel_id) VALUES($1,'killfeed','123')`, f.InstallationID)
		confirm := strconv.FormatInt(f.InstallationID, 10)

		// Refused while the community pays through Stripe.
		w.exec(t, `UPDATE subscriptions SET provider='stripe', provider_subscription_id='sub_inst', status='ACTIVE' WHERE organization_id=$1`, f.OrgID)
		code, body := w.deleteInstallation(adminFounderID, f, confirm)
		if code != http.StatusConflict || !strings.Contains(body, "Cancel the subscription first") {
			t.Fatalf("delete with a live subscription: %d %s", code, body)
		}
		w.exec(t, `UPDATE subscriptions SET status='CANCELED' WHERE organization_id=$1`, f.OrgID)

		// Refused while it holds records that are not configuration.
		w.exec(t, `INSERT INTO hub_factions(organization_id, installation_id, game_server_id, name, tag, slug, created_by_user_id) VALUES($1,$2,$3,'Wolves','WLF','wolves',$4)`,
			f.OrgID, f.InstallationID, serverID, mustAppUserID(t, w.a, f.OwnerDiscordID))
		code, body = w.deleteInstallation(adminFounderID, f, confirm)
		if code != http.StatusConflict || !strings.Contains(body, "hub_factions") {
			t.Fatalf("delete with faction records: %d %s", code, body)
		}
		if n := w.count(t, `SELECT COUNT(*) FROM installations WHERE id=$1 AND game_server_id=$2`, f.InstallationID, serverID); n != 1 {
			t.Fatal("a refused delete changed the installation")
		}
		if n := w.count(t, `SELECT COUNT(*) FROM game_servers WHERE id=$1 AND active`, serverID); n != 1 {
			t.Fatal("a refused delete switched the game server off")
		}
		if refused := w.lastAudit(t, "installation.delete_refused", f.InstallationID); refused == nil || refused.Result != "REFUSED" {
			t.Fatalf("refusal audit: %+v", refused)
		}
		w.exec(t, `DELETE FROM hub_factions WHERE installation_id=$1`, f.InstallationID)

		code, body = w.deleteInstallation(adminFounderID, f, confirm)
		if code != http.StatusOK {
			t.Fatalf("delete: %d %s", code, body)
		}
		if n := w.count(t, `SELECT COUNT(*) FROM installations WHERE id=$1`, f.InstallationID); n != 0 {
			t.Fatal("installation still there")
		}
		if n := w.count(t, `SELECT COUNT(*) FROM installation_channel_routes WHERE installation_id=$1`, f.InstallationID); n != 0 {
			t.Fatal("configuration left behind")
		}
		// The game server is kept, switched off and released; gameplay data is all still there.
		if n := w.count(t, `SELECT COUNT(*) FROM game_servers WHERE id=$1 AND NOT active AND status='DISCONNECTED' AND organization_id IS NULL`, serverID); n != 1 {
			t.Fatal("the game server was not switched off and released")
		}
		active, err := w.a.DB.Pool.Query(w.ctx, `SELECT id FROM game_servers WHERE active AND id=$1`, serverID)
		if err != nil {
			t.Fatal(err)
		}
		if active.Next() {
			t.Fatal("the worker manager would still start this server")
		}
		active.Close()
		if n := w.count(t, `SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2`, guild, serverID); n != 1 {
			t.Fatal("kills were removed")
		}
		if n := w.count(t, `SELECT COUNT(*) FROM player_links WHERE id=$1`, linkID); n != 1 {
			t.Fatal("the player link was removed")
		}
		got := w.orgFootprint(t, f)
		if got["organizations"] != 1 || got["organization_members"] != 1 || got["discord_guild_connections"] != 1 {
			t.Fatalf("the organization itself was touched: %v", got)
		}
		audit := w.lastAudit(t, "installation.deleted", f.InstallationID)
		if audit == nil || audit.Before["gameServerId"] != float64(serverID) || audit.Before["gameServerName"] != "Delete Test Server" {
			t.Fatalf("audit row: %+v", audit)
		}

		// With its only server removed the community is empty, and can now be deleted.
		if code, body := w.deleteOrg(adminFounderID, f, "Fixture Org"); code != http.StatusOK {
			t.Fatalf("delete the now-empty organization: %d %s", code, body)
		}
		if n := w.count(t, `SELECT COUNT(*) FROM kills WHERE guild_id=$1`, guild); n != 1 {
			t.Fatal("deleting the organization removed kills")
		}
		if n := w.count(t, `SELECT COUNT(*) FROM game_servers WHERE id=$1`, serverID); n != 1 {
			t.Fatal("deleting the organization removed the game server row")
		}
	})
}

func TestAdminOrganizationsNoServerFilter(t *testing.T) {
	w := newDeleteWorld(t)
	w.connectServer(t, w.a1)
	rr := w.get(w.a.handleAdminListOrganizations, "/api/admin/organizations?noServer=true&limit=100", adminFounderID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var page struct {
		Items []struct {
			ID                     int64 `json:"id"`
			ServerCount            int   `json:"serverCount"`
			DiscordConnectionCount int   `json:"discordConnectionCount"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var sawEmpty bool
	for _, o := range page.Items {
		if o.ID == w.a1.OrgID {
			t.Fatal("an organization with a game server is in the no-server list")
		}
		if o.ServerCount != 0 {
			t.Fatalf("organization %d in the no-server list has serverCount %d", o.ID, o.ServerCount)
		}
		if o.ID == w.b1.OrgID {
			sawEmpty = true
			if o.DiscordConnectionCount != 1 {
				t.Fatalf("discordConnectionCount: %d", o.DiscordConnectionCount)
			}
		}
	}
	if !sawEmpty {
		t.Fatal("the organization without a server is missing from the no-server list")
	}
	rr = w.get(w.a.handleAdminGetOrganization, "/api/admin/organizations/x", adminFounderID, map[string]string{"organizationID": strconv.FormatInt(w.a1.OrgID, 10)})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"serverCount":1`) {
		t.Fatalf("detail serverCount: %d %s", rr.Code, rr.Body.String())
	}
}
