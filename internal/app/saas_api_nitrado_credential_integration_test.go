//go:build integration

package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Phase 6.26P: before any real Nitrado token is used on staging, the connect path is proven with a
// synthetic token against a fake Nitrado API: OWNER/ADMIN may connect, a MEMBER is denied before
// Nitrado is contacted, an unconfigured cipher fails safely without persisting anything, the stored
// envelope decrypts only with the configured key, and the token never reaches the logs.
func TestNitradoConnectRolesCipherAndSecretHygiene(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	completeDiscordStep(t, a, fixture)
	a.saasNitradoConnectLimiter = newSaaSRateLimiter(time.Hour, 100)
	var nitradoCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nitradoCalls.Add(1)
		_, _ = w.Write([]byte(psServiceJSON))
	}))
	t.Cleanup(srv.Close)
	a.saasNitradoClientFactory = func(token string) *nitrado.Client { return nitrado.NewClient(srv.URL, token, nil) }
	const token = "synthetic-nitrado-token-6-26P-do-not-use"
	ctx := context.Background()
	member := func(role string) string {
		u := syncUser(t, a, fmt.Sprintf("nitrado-%s-%d", strings.ToLower(role), time.Now().UnixNano()), role)
		if _, err := a.DB.Pool.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,$3)`,
			fixture.OrgID, u.ID, role); err != nil {
			t.Fatal(err)
		}
		return u.DiscordUserID
	}
	connect := func(actor string) *httptest.ResponseRecorder {
		req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", nitradoConnectRequest{Token: token}), actor),
			map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10)})
		rr := httptest.NewRecorder()
		a.handleNitradoConnect(rr, req)
		return rr
	}
	stored := func() bool {
		env, err := a.SaaSCredentials.GetForOrganizationOnly(ctx, fixture.OrgID)
		if err != nil {
			t.Fatal(err)
		}
		return env != nil
	}

	// MEMBER: denied before any Nitrado call; nothing stored.
	if rr := connect(member("MEMBER")); rr.Code != http.StatusForbidden || nitradoCalls.Load() != 0 || stored() {
		t.Fatalf("MEMBER connect: %d calls=%d stored=%v %s", rr.Code, nitradoCalls.Load(), stored(), rr.Body.String())
	}
	// Unconfigured cipher: a safe, fixed error; no Nitrado call; nothing stored.
	cipher := a.CredentialCipher
	a.CredentialCipher = nil
	rr := connect(fixture.OwnerDiscordID)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "credential encryption is not configured") ||
		strings.Contains(rr.Body.String(), token) || nitradoCalls.Load() != 0 || stored() {
		t.Fatalf("unconfigured cipher: %d calls=%d stored=%v %s", rr.Code, nitradoCalls.Load(), stored(), rr.Body.String())
	}
	a.CredentialCipher = cipher

	// ADMIN: allowed; the envelope is ciphertext and decrypts back only with the configured key.
	if rr := connect(member("ADMIN")); rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), token) {
		t.Fatalf("ADMIN connect: %d %s", rr.Code, rr.Body.String())
	}
	env, err := a.SaaSCredentials.GetForOrganizationOnly(ctx, fixture.OrgID)
	if err != nil || env == nil || len(env.Ciphertext) == 0 || len(env.Nonce) == 0 || bytes.Contains(env.Ciphertext, []byte(token)) {
		t.Fatalf("stored envelope: %+v %v", env, err)
	}
	var raw []byte
	// The whole row, every column rendered as text: the token appears nowhere in plaintext.
	if err := a.DB.Pool.QueryRow(ctx, `SELECT convert_to(row_to_json(c)::text,'UTF8') FROM nitrado_connections c WHERE organization_id=$1`,
		fixture.OrgID).Scan(&raw); err != nil || len(raw) == 0 || bytes.Contains(raw, []byte(token)) {
		t.Fatalf("credentials row: err=%v plaintext=%v", err, bytes.Contains(raw, []byte(token)))
	}
	plain, err := a.CredentialCipher.Decrypt(env.Ciphertext, env.Nonce, env.KeyVersion)
	if err != nil || string(plain) != token {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := a.CredentialCipher.Decrypt(env.Ciphertext, env.Nonce, env.KeyVersion+1); err == nil {
		t.Fatal("an unknown key version must not decrypt")
	}
	// OWNER may reconnect (replaces the envelope).
	if rr := connect(fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("OWNER reconnect: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("token leaked into logs:\n%s", logs.String())
	}
}
