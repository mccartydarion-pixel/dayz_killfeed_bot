package billing

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseAllowedOriginsAlwaysIncludesDefault(t *testing.T) {
	for _, raw := range []string{"", "   ", "not a url", "ftp://bad.example"} {
		got := ParseAllowedOrigins(raw)
		if len(got) != 1 || got[0] != DefaultOrigin {
			t.Errorf("%q: %v", raw, got)
		}
	}
	got := ParseAllowedOrigins("http://localhost:3000, https://staging.example ,https://championshp.vip")
	want := map[string]bool{DefaultOrigin: true, "http://localhost:3000": true, "https://staging.example": true}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for _, o := range got {
		if !want[o] {
			t.Errorf("unexpected origin %q", o)
		}
	}
}

func TestParseAllowedOriginsRejectsPathsAndCredentials(t *testing.T) {
	got := ParseAllowedOrigins("https://evil.example/path,https://user:pass@evil2.example,https://evil3.example?x=1")
	if len(got) != 1 || got[0] != DefaultOrigin {
		t.Fatalf("path/credential/query origins must be dropped: %v", got)
	}
}

func TestResolveOrigin(t *testing.T) {
	allowed := ParseAllowedOrigins("http://localhost:3000")
	mk := func(origin, referer string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if referer != "" {
			r.Header.Set("Referer", referer)
		}
		return r
	}
	if got := ResolveOrigin(mk("http://localhost:3000", ""), allowed); got != "http://localhost:3000" {
		t.Errorf("matching Origin: %q", got)
	}
	if got := ResolveOrigin(mk("https://evil.example", ""), allowed); got != DefaultOrigin {
		t.Errorf("an unlisted Origin must fall back to the default, got %q", got)
	}
	if got := ResolveOrigin(mk("", "http://localhost:3000/billing/checkout"), allowed); got != "http://localhost:3000" {
		t.Errorf("matching Referer: %q", got)
	}
	if got := ResolveOrigin(mk("", ""), allowed); got != DefaultOrigin {
		t.Errorf("no Origin/Referer: %q", got)
	}
	// An unmatched Origin still falls back to a matching Referer - this only ever picks which
	// *allowlisted* origin to redirect the caller's own browser back to (the caller already needed
	// valid service+acting-user auth to reach this endpoint), so it is not a security boundary the
	// way it would be for CSRF/CORS checks on a public unauthenticated endpoint.
	if got := ResolveOrigin(mk("https://evil.example", "http://localhost:3000/x"), allowed); got != "http://localhost:3000" {
		t.Errorf("Origin unmatched, Referer matched: %q", got)
	}
	// Neither header matches any allowed origin: fall back to the production default, never to the
	// attacker-supplied value itself.
	if got := ResolveOrigin(mk("https://evil.example", "https://evil2.example/x"), allowed); got != DefaultOrigin {
		t.Errorf("neither header matches: %q", got)
	}
}

func TestSafeReturnURL(t *testing.T) {
	ok := func(path string) {
		t.Helper()
		u, ok := SafeReturnURL(DefaultOrigin, path, "/billing")
		if !ok {
			t.Errorf("%q: expected ok", path)
			return
		}
		if u[:len(DefaultOrigin)] != DefaultOrigin {
			t.Errorf("%q: %q does not start with the origin", path, u)
		}
	}
	bad := func(path string) {
		t.Helper()
		if _, ok := SafeReturnURL(DefaultOrigin, path, "/billing"); ok {
			t.Errorf("%q: expected rejection", path)
		}
	}
	ok("/dashboard/org/5/billing")
	ok("/billing?checkout=success")
	ok("")
	bad("//evil.example")
	bad("http://evil.example")
	bad("https://evil.example")
	bad("dashboard/org/5") // must start with "/"
	bad("/\\evil.example")
	bad(" /billing")
}

func TestSafeReturnURLUsesDefaultWhenPathEmpty(t *testing.T) {
	u, ok := SafeReturnURL(DefaultOrigin, "", "/billing?x=1")
	if !ok || u != DefaultOrigin+"/billing?x=1" {
		t.Fatalf("got %q ok=%v", u, ok)
	}
}

func TestDeriveCancelPath(t *testing.T) {
	cases := []struct {
		name       string
		returnPath string
		wantPath   string
		wantOK     bool
	}{
		{"overwrites an existing checkout=success", "/dashboard/subscription?checkout=success", "/dashboard/subscription?checkout=cancelled", true},
		{"adds checkout to a bare path", "/settings/org/9", "/settings/org/9?checkout=cancelled", true},
		{"preserves other query params", "/dashboard/subscription?checkout=success&ref=email", "/dashboard/subscription?checkout=cancelled&ref=email", true},
		{"empty returnPath", "", "", false},
		{"scheme-relative rejected, same as SafeReturnURL", "//evil.example", "", false},
		{"absolute URL rejected", "https://evil.example/x", "", false},
		{"backslash escape rejected", "/\\evil.example", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := DeriveCancelPath(c.returnPath)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, c.wantOK, got)
			}
			if ok && got != c.wantPath {
				t.Fatalf("got %q, want %q", got, c.wantPath)
			}
		})
	}
}
