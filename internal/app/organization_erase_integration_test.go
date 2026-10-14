//go:build integration

package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func (w *deleteWorld) erase(acting string, f installationFixture, body any, extra map[string]string) *httptest.ResponseRecorder {
	org := strconv.FormatInt(f.OrgID, 10)
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/api/saas/organizations/"+org+"/erase", body), acting),
		map[string]string{"organizationID": org})
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	w.a.handleEraseOrganization(rr, req)
	return rr
}

func (w *deleteWorld) setSubscription(t *testing.T, f installationFixture, providerSub, status string) {
	t.Helper()
	w.exec(t, `
INSERT INTO subscriptions(organization_id, provider, provider_subscription_id, plan, status)
VALUES($1, 'stripe', NULLIF($2,''), 'SURVIVOR', $3)
ON CONFLICT (organization_id) DO UPDATE SET provider = 'stripe', provider_subscription_id = NULLIF($2,''), status = $3`, f.OrgID, providerSub, status)
}

var eraseConfirm = map[string]string{"confirm": "DELETE"}

func TestEraseOrganizationRefusals(t *testing.T) {
	w := newDeleteWorld(t)
	target := w.a1
	suffix := time.Now().UnixNano()
	admin := syncUser(t, w.a, fmt.Sprintf("erase-admin-%d", suffix), "Org Admin")
	w.exec(t, `INSERT INTO organization_members(organization_id, user_id, role) VALUES($1,$2,'ADMIN')`, target.OrgID, mustAppUserID(t, w.a, admin.DiscordUserID))

	// Not the OWNER: an ADMIN of the organization, and another organization's owner.
	if rr := w.erase(admin.DiscordUserID, target, eraseConfirm, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("ADMIN erase: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.erase(w.b1.OwnerDiscordID, target, eraseConfirm, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("outsider erase: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	// A read-only "view as" session can never erase.
	if rr := w.erase(target.OwnerDiscordID, target, eraseConfirm, map[string]string{impersonatorHeader: adminFounderID}); rr.Code != http.StatusForbidden {
		t.Fatalf("view-as erase: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	// Missing or wrong confirmation.
	for _, body := range []any{nil, map[string]string{}, map[string]string{"confirm": "delete"}, map[string]string{"confirm": "Fixture Org"}} {
		rr := w.erase(target.OwnerDiscordID, target, body, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("confirm %v: want 400, got %d %s", body, rr.Code, rr.Body.String())
		}
		assertErrorCode(t, rr, codeInvalidRequest)
	}
	// A live paid subscription (ACTIVE or PAST_DUE) blocks the erase.
	for _, status := range []string{"ACTIVE", "PAST_DUE"} {
		w.setSubscription(t, target, "sub_erase_test", status)
		rr := w.erase(target.OwnerDiscordID, target, eraseConfirm, nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s subscription: want 409, got %d %s", status, rr.Code, rr.Body.String())
		}
		assertErrorCode(t, rr, codeSubscriptionActive)
	}
	if n := w.count(t, `SELECT COUNT(*) FROM organizations WHERE id=$1`, target.OrgID); n != 1 {
		t.Fatal("a refused erase must change nothing")
	}
	// Once the subscription is canceled the owner may erase.
	w.setSubscription(t, target, "sub_erase_test", "CANCELED")
	if rr := w.erase(target.OwnerDiscordID, target, eraseConfirm, nil); rr.Code != http.StatusOK {
		t.Fatalf("canceled subscription: want 200, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestEraseOrganizationRemovesItsDataAndNothingElse(t *testing.T) {
	w := newDeleteWorld(t)
	target, other := w.a1, w.b1
	serverID := w.connectServer(t, target)
	linkID := w.linkPlayer(t, target)
	guild := w.guildID(t, target)
	w.exec(t, `INSERT INTO nitrado_connections(organization_id, credential_ciphertext, credential_nonce, credential_key_version, status) VALUES($1,'\x01','\x02',1,'CONNECTED')`, target.OrgID)
	invoice := fmt.Sprintf("in_erase_%d", time.Now().UnixNano())
	w.exec(t, `INSERT INTO billing_transactions(organization_id, provider, provider_invoice_id, status, amount_cents, currency, stripe_event_id)
VALUES($1,'stripe',$2,'PAID',999,'usd',$2)`, target.OrgID, invoice)
	w.setSubscription(t, target, "sub_old", "CANCELED")
	// The other organization has its own server, credential and linked player.
	otherServer := w.connectServer(t, other)
	otherLink := w.linkPlayer(t, other)
	w.exec(t, `INSERT INTO nitrado_connections(organization_id, credential_ciphertext, credential_nonce, credential_key_version, status) VALUES($1,'\x01','\x02',1,'CONNECTED')`, other.OrgID)
	otherBefore := w.orgFootprint(t, other)
	ownerUserID := mustAppUserID(t, w.a, target.OwnerDiscordID)
	w.exec(t, `INSERT INTO trial_grants(user_id, organization_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, ownerUserID, target.OrgID)

	rr := w.erase(target.OwnerDiscordID, target, eraseConfirm, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("erase: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	if got := decodeBody[map[string]bool](t, rr); !got["erased"] {
		t.Fatalf("want {\"erased\":true}, got %s", rr.Body.String())
	}

	for name, q := range map[string]string{
		"organization":        `SELECT COUNT(*) FROM organizations WHERE id=$1`,
		"members":             `SELECT COUNT(*) FROM organization_members WHERE organization_id=$1`,
		"installations":       `SELECT COUNT(*) FROM installations WHERE organization_id=$1`,
		"guild connections":   `SELECT COUNT(*) FROM discord_guild_connections WHERE organization_id=$1`,
		"nitrado credentials": `SELECT COUNT(*) FROM nitrado_connections WHERE organization_id=$1`,
		"subscription":        `SELECT COUNT(*) FROM subscriptions WHERE organization_id=$1`,
		"owned servers":       `SELECT COUNT(*) FROM game_servers WHERE organization_id=$1`,
	} {
		if n := w.count(t, q, target.OrgID); n != 0 {
			t.Errorf("%s: %d row(s) left after erase", name, n)
		}
	}
	if n := w.count(t, `SELECT COUNT(*) FROM installations WHERE id=$1`, target.InstallationID); n != 0 {
		t.Errorf("installation %d survived the erase", target.InstallationID)
	}
	// The guild was only this organization's, so its gameplay data went with it.
	if n := w.count(t, `SELECT COUNT(*) FROM guilds WHERE id=$1`, guild); n != 0 {
		t.Errorf("unshared guild's gameplay data was kept")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM player_links WHERE id=$1`, linkID); n != 0 {
		t.Errorf("unshared guild's player link was kept")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM game_servers WHERE id=$1 AND active`, serverID); n != 0 {
		t.Errorf("the erased organization's server is still active")
	}
	// Payment records are kept, detached from the erased organization.
	if n := w.count(t, `SELECT COUNT(*) FROM billing_transactions WHERE provider_invoice_id=$1 AND organization_id IS NULL`, invoice); n != 1 {
		t.Errorf("payment record was not kept (detached): %d", n)
	}
	// The one-trial marker is kept, so erasing and starting over never grants a second trial.
	if n := w.count(t, `SELECT COUNT(*) FROM trial_grants WHERE user_id=$1`, ownerUserID); n != 1 {
		t.Errorf("trial marker was not kept: %d", n)
	}
	// Owner account survives; the erase is audited.
	_ = mustAppUserID(t, w.a, target.OwnerDiscordID)
	if a := w.lastAudit(t, "organization.erased", target.OrgID); a == nil || a.Actor != target.OwnerDiscordID {
		t.Errorf("erase was not audited: %+v", a)
	}

	// The other organization is untouched.
	otherAfter := w.orgFootprint(t, other)
	for k, v := range otherBefore {
		if otherAfter[k] != v {
			t.Errorf("other organization's %s changed: %d -> %d", k, v, otherAfter[k])
		}
	}
	if n := w.count(t, `SELECT COUNT(*) FROM game_servers WHERE id=$1 AND active AND organization_id=$2`, otherServer, other.OrgID); n != 1 {
		t.Errorf("other organization's server was touched")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM nitrado_connections WHERE organization_id=$1`, other.OrgID); n != 1 {
		t.Errorf("other organization's Nitrado credential was touched")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM player_links WHERE id=$1`, otherLink); n != 1 {
		t.Errorf("other organization's player link was touched")
	}
}

// The bot's own configured guild (DISCORD_GUILD_ID) keeps its gameplay data.
func TestEraseOrganizationKeepsTheConfiguredGuildsGameplayData(t *testing.T) {
	w := newDeleteWorld(t)
	target := w.a1
	linkID := w.linkPlayer(t, target)
	guild := w.guildID(t, target)
	w.a.Config.DiscordGuildID = target.DiscordGuildID

	if rr := w.erase(target.OwnerDiscordID, target, eraseConfirm, nil); rr.Code != http.StatusOK {
		t.Fatalf("erase: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	if n := w.count(t, `SELECT COUNT(*) FROM guilds WHERE id=$1`, guild); n != 1 {
		t.Fatal("the configured guild must be kept")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM player_links WHERE id=$1`, linkID); n != 1 {
		t.Fatal("the configured guild's player links must be kept")
	}
}

// A guild another organization still uses (here: runs a game server on) keeps its gameplay data.
func TestEraseOrganizationKeepsASharedGuildsGameplayData(t *testing.T) {
	w := newDeleteWorld(t)
	target, other := w.a1, w.b1
	linkID := w.linkPlayer(t, target)
	guild := w.guildID(t, target)
	w.exec(t, `
INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, active, organization_id)
VALUES($1,'nitrado',$2,'dayz','xbox','Shared Server','CONNECTED',TRUE,$3)`, guild, fmt.Sprintf("svc-shared-%d", time.Now().UnixNano()), other.OrgID)

	if rr := w.erase(target.OwnerDiscordID, target, eraseConfirm, nil); rr.Code != http.StatusOK {
		t.Fatalf("erase: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	if n := w.count(t, `SELECT COUNT(*) FROM player_links WHERE id=$1`, linkID); n != 1 {
		t.Fatal("a guild another organization still connects must keep its gameplay data")
	}
}

func TestPlayerUnlinksOwnIdentity(t *testing.T) {
	w := newDeleteWorld(t)
	f := w.a1
	linkID := w.linkPlayer(t, f)
	otherLink := w.linkPlayer(t, w.b1)
	players := w.count(t, `SELECT COUNT(*) FROM players WHERE id=(SELECT player_id FROM player_links WHERE id=$1)`, linkID)

	call := func(acting string, extra map[string]string) *httptest.ResponseRecorder {
		req := withActingUser(saasRequest(http.MethodPost, "/api/saas/player/identity/unlink", nil), acting)
		for k, v := range extra {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		w.a.handleUnlinkPlayerIdentity(rr, req)
		return rr
	}
	w.a.Links = nil
	if rr := call(f.OwnerDiscordID, nil); rr.Code != http.StatusInternalServerError {
		t.Fatalf("no link store: want 500, got %d", rr.Code)
	}
	w.a.Links = repository.NewLinkRepository(w.a.DB.Pool)
	if rr := call(f.OwnerDiscordID, map[string]string{impersonatorHeader: adminFounderID}); rr.Code != http.StatusForbidden {
		t.Fatalf("view-as unlink: want 403, got %d", rr.Code)
	}
	rr := call(f.OwnerDiscordID, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("unlink: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	if got := decodeBody[map[string]int64](t, rr); got["unlinked"] != 1 {
		t.Fatalf("want unlinked=1, got %s", rr.Body.String())
	}
	if n := w.count(t, `SELECT COUNT(*) FROM player_links WHERE id=$1 AND status='UNLINKED'`, linkID); n != 1 {
		t.Fatal("link was not unlinked")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM player_links WHERE id=$1 AND status='VERIFIED'`, otherLink); n != 1 {
		t.Fatal("another user's link was touched")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM players WHERE id=(SELECT player_id FROM player_links WHERE id=$1)`, linkID); n != players {
		t.Fatal("gameplay identity (players row) must be kept")
	}
	if rr := call(f.OwnerDiscordID, nil); decodeBody[map[string]int64](t, rr)["unlinked"] != 0 {
		t.Fatalf("second unlink should change nothing: %s", rr.Body.String())
	}
}

// Records the database refuses to delete (here a C.A.S.E. add-on billing row, RESTRICT) make the
// erase a 409 CONFLICT that changes nothing; a live add-on subscription is SUBSCRIPTION_ACTIVE.
func TestEraseOrganizationRefusedByKeptRecords(t *testing.T) {
	w := newDeleteWorld(t)
	target := w.a1
	serverID := w.connectServer(t, target)
	w.exec(t, `INSERT INTO case_addon_subscriptions(organization_id,installation_id,game_server_id,tier,status,provider_subscription_id,provider)
VALUES($1,$2,$3,'CASE_WATCH','PAST_DUE',$4,'stripe')`, target.OrgID, target.InstallationID, serverID, fmt.Sprintf("sub_addon_%d", time.Now().UnixNano()))
	rr := w.erase(target.OwnerDiscordID, target, eraseConfirm, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("live add-on: want 409, got %d %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeSubscriptionActive)

	w.exec(t, `UPDATE case_addon_subscriptions SET status='CANCELED' WHERE organization_id=$1`, target.OrgID)
	rr = w.erase(target.OwnerDiscordID, target, eraseConfirm, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("kept add-on records: want 409, got %d %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeConflict)
	if n := w.count(t, `SELECT COUNT(*) FROM installations WHERE organization_id=$1`, target.OrgID); n != 1 {
		t.Fatal("a refused erase must change nothing")
	}
	if n := w.count(t, `SELECT COUNT(*) FROM game_servers WHERE id=$1 AND active AND organization_id=$2`, serverID, target.OrgID); n != 1 {
		t.Fatal("a refused erase must leave the server active (the reconciler restarts its worker)")
	}
}

// With CHAMPION_REQUIRE_SIGNED_IDENTITY=true an unsigned acting user is refused and a correctly
// signed request succeeds.
func TestSignedIdentityRequiredEndToEnd(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	f := buildInstallationFixture(t, a, verifier)
	a.Config.RequireSignedIdentity = true
	const path = "/api/saas/organizations"

	rr := httptest.NewRecorder()
	a.handleListOrganizations(rr, withActingUser(saasRequest(http.MethodGet, path, nil), f.OwnerDiscordID))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: want 401, got %d %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeUnauthorized)

	req := withActingUser(saasRequest(http.MethodGet, path+"?x=1", nil), f.OwnerDiscordID)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set(identityTimestampHeader, ts)
	req.Header.Set(identitySignatureHeader, signIdentity(a.Config.WebsiteAPISecret, ts, http.MethodGet, path, f.OwnerDiscordID, "", ""))
	rr = httptest.NewRecorder()
	a.handleListOrganizations(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("signed: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	if list := decodeBody[[]OrganizationSummary](t, rr); !containsOrgID(list, f.OrgID) {
		t.Fatalf("signed request did not list the owner's organization: %+v", list)
	}

	// Signed for one user, replayed as another: refused even with require off.
	a.Config.RequireSignedIdentity = false
	req.Header.Set(actingUserHeader, "123456789012345678")
	rr = httptest.NewRecorder()
	a.handleListOrganizations(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("tampered acting user: want 401, got %d", rr.Code)
	}
}
