package app

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
)

const sigTestSecret = "identity-signature-test-secret-0123456789"

// signedRequest builds a request carrying the given identity headers, signed
// with secret at ts (an empty secret leaves it unsigned).
func signedRequest(method, target, secret string, ts time.Time, acting, impersonator, admin string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Authorization", "Bearer "+sigTestSecret)
	if acting != "" {
		r.Header.Set(actingUserHeader, acting)
	}
	if impersonator != "" {
		r.Header.Set(impersonatorHeader, impersonator)
	}
	if admin != "" {
		r.Header.Set(adminAssertHeader, admin)
	}
	if secret != "" {
		stamp := strconv.FormatInt(ts.Unix(), 10)
		r.Header.Set(identityTimestampHeader, stamp)
		r.Header.Set(identitySignatureHeader, signIdentity(secret, stamp, method, r.URL.Path, acting, impersonator, admin))
	}
	return r
}

func TestSignIdentityCanonicalString(t *testing.T) {
	got := identityCanonicalString("1700000000", "post", "/api/saas/x", "123", "", "true")
	want := "v1\n1700000000\nPOST\n/api/saas/x\n123\n\ntrue"
	if got != want {
		t.Fatalf("canonical = %q, want %q", got, want)
	}
	// Known-answer vector the website can check its implementation against.
	sig := signIdentity("secret", "1700000000", "GET", "/api/saas/me", "123456789012345678", "", "")
	if !strings.HasPrefix(sig, "v1=") || len(sig) != 3+64 || strings.ToLower(sig) != sig {
		t.Fatalf("signature shape wrong: %q", sig)
	}
	if sig != signIdentity("secret", "1700000000", "get", "/api/saas/me", "123456789012345678", "", "") {
		t.Fatal("method must be uppercased before signing")
	}
}

func TestVerifyIdentitySignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name string
		req  func() *http.Request
		want error
	}{
		{"valid", func() *http.Request {
			return signedRequest(http.MethodGet, "/api/saas/me?x=1", sigTestSecret, now, "111", "", "")
		}, nil},
		{"valid all headers", func() *http.Request {
			return signedRequest(http.MethodPost, "/api/admin/x", sigTestSecret, now.Add(-299*time.Second), "111", "222", "true")
		}, nil},
		{"wrong secret", func() *http.Request {
			return signedRequest(http.MethodGet, "/api/saas/me", "another-secret", now, "111", "", "")
		}, errIdentitySignatureInvalid},
		{"tampered acting user", func() *http.Request {
			r := signedRequest(http.MethodGet, "/api/saas/me", sigTestSecret, now, "111", "", "")
			r.Header.Set(actingUserHeader, "999")
			return r
		}, errIdentitySignatureInvalid},
		{"tampered impersonator added", func() *http.Request {
			r := signedRequest(http.MethodGet, "/api/saas/me", sigTestSecret, now, "111", "", "")
			r.Header.Set(impersonatorHeader, "222")
			return r
		}, errIdentitySignatureInvalid},
		{"tampered path", func() *http.Request {
			r := signedRequest(http.MethodGet, "/api/saas/me", sigTestSecret, now, "111", "", "")
			r.URL.Path = "/api/saas/organizations/1/erase"
			return r
		}, errIdentitySignatureInvalid},
		{"tampered method", func() *http.Request {
			r := signedRequest(http.MethodGet, "/api/saas/me", sigTestSecret, now, "111", "", "")
			r.Method = http.MethodPost
			return r
		}, errIdentitySignatureInvalid},
		{"expired timestamp", func() *http.Request {
			return signedRequest(http.MethodGet, "/api/saas/me", sigTestSecret, now.Add(-301*time.Second), "111", "", "")
		}, errIdentitySignatureExpired},
		{"future timestamp", func() *http.Request {
			return signedRequest(http.MethodGet, "/api/saas/me", sigTestSecret, now.Add(301*time.Second), "111", "", "")
		}, errIdentitySignatureExpired},
		{"garbage timestamp", func() *http.Request {
			r := signedRequest(http.MethodGet, "/api/saas/me", sigTestSecret, now, "111", "", "")
			r.Header.Set(identityTimestampHeader, "soon")
			return r
		}, errIdentitySignatureInvalid},
		{"missing version prefix", func() *http.Request {
			r := signedRequest(http.MethodGet, "/api/saas/me", sigTestSecret, now, "111", "", "")
			r.Header.Set(identitySignatureHeader, strings.TrimPrefix(r.Header.Get(identitySignatureHeader), "v1="))
			return r
		}, errIdentitySignatureInvalid},
		{"missing signature", func() *http.Request {
			return signedRequest(http.MethodGet, "/api/saas/me", "", now, "111", "", "")
		}, errIdentitySignatureMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyIdentitySignature(tc.req(), sigTestSecret, now)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCheckSignedIdentityPolicy(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	prev := identityNow
	identityNow = func() time.Time { return now }
	defer func() { identityNow = prev }()

	cases := []struct {
		name    string
		req     *http.Request
		require bool
		want    bool
	}{
		{"no identity headers, not required", signedRequest(http.MethodGet, "/api/saas/x", "", now, "", "", ""), false, true},
		{"no identity headers, required", signedRequest(http.MethodGet, "/api/saas/x", "", now, "", "", ""), true, true},
		{"unsigned identity, not required", signedRequest(http.MethodGet, "/api/saas/x", "", now, "111", "", ""), false, true},
		{"unsigned identity, required", signedRequest(http.MethodGet, "/api/saas/x", "", now, "111", "", ""), true, false},
		{"unsigned admin header, required", signedRequest(http.MethodGet, "/api/admin/x", "", now, "", "", "true"), true, false},
		{"signed identity, required", signedRequest(http.MethodGet, "/api/saas/x", sigTestSecret, now, "111", "", ""), true, true},
		{"bad signature, not required", signedRequest(http.MethodGet, "/api/saas/x", "wrong", now, "111", "", ""), false, false},
		{"expired signature, not required", signedRequest(http.MethodGet, "/api/saas/x", sigTestSecret, now.Add(-time.Hour), "111", "", ""), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkSignedIdentity(tc.req, sigTestSecret, tc.require); got != tc.want {
				t.Fatalf("checkSignedIdentity = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequireSaaSServiceAuthEnforcesSignedIdentity(t *testing.T) {
	now := time.Now()
	for _, require := range []bool{false, true} {
		a := &App{Config: &config.Config{WebsiteAPISecret: sigTestSecret, RequireSignedIdentity: require}}
		// Unsigned: allowed only while not required.
		rr := httptest.NewRecorder()
		ok := a.requireSaaSServiceAuth(rr, signedRequest(http.MethodGet, "/api/saas/me", "", now, "111", "", ""))
		if ok == require {
			t.Fatalf("require=%v: unsigned identity ok=%v", require, ok)
		}
		if require && (rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), codeUnauthorized)) {
			t.Fatalf("require=true: want 401 UNAUTHORIZED, got %d %s", rr.Code, rr.Body.String())
		}
		// Tampered: always refused.
		rr = httptest.NewRecorder()
		bad := signedRequest(http.MethodGet, "/api/saas/me", sigTestSecret, now, "111", "", "")
		bad.Header.Set(actingUserHeader, "222")
		if a.requireSaaSServiceAuth(rr, bad) || rr.Code != http.StatusUnauthorized {
			t.Fatalf("require=%v: tampered identity must be 401, got %d", require, rr.Code)
		}
		// Correctly signed: accepted.
		rr = httptest.NewRecorder()
		if !a.requireSaaSServiceAuth(rr, signedRequest(http.MethodGet, "/api/saas/me", sigTestSecret, now, "111", "", "")) {
			t.Fatalf("require=%v: signed identity refused: %d %s", require, rr.Code, rr.Body.String())
		}
	}
}

func TestUnsignedIdentityWarningIsRateLimitedAndPathOnly(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	lastUnsignedIdentityWarn.Store(0)
	defer lastUnsignedIdentityWarn.Store(0)

	for i := 0; i < 5; i++ {
		r := signedRequest(http.MethodGet, "/api/saas/me", "", time.Now(), "123456789012345678", "", "")
		if !checkSignedIdentity(r, sigTestSecret, false) {
			t.Fatal("unsigned identity must pass while not required")
		}
	}
	out := buf.String()
	if n := strings.Count(out, "event=unsigned_identity"); n != 1 {
		t.Fatalf("warning logged %d times, want 1: %s", n, out)
	}
	if !strings.Contains(out, "path=/api/saas/me") || strings.Contains(out, "123456789012345678") {
		t.Fatalf("warning must carry the path and never header values: %s", out)
	}
}
