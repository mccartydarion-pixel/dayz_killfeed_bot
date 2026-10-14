package app

// Signed acting identity (docs/SAAS_HTTP_API.md "Signed identity").
//
// The website authenticates with "Authorization: Bearer <WEBSITE_API_SECRET>"
// and asserts WHO is acting with plain headers (X-Champion-Acting-User,
// X-Champion-Impersonator, X-Champion-Admin). The bearer alone lets anyone who
// holds the secret claim any identity, so the identity headers are also bound
// to the request with an HMAC:
//
//	X-Champion-Timestamp: <unix seconds>
//	X-Champion-Signature: v1=<lowercase hex HMAC-SHA256>
//
// The HMAC key is the same secret the request's bearer token was checked
// against. The signed (canonical) string is these seven fields joined with
// "\n", with no trailing newline:
//
//	v1
//	<timestamp, exactly as sent in X-Champion-Timestamp>
//	<HTTP method, uppercased>
//	<URL path: r.URL.Path, no query string>
//	<X-Champion-Acting-User value, or "">
//	<X-Champion-Impersonator value, or "">
//	<X-Champion-Admin value, or "">
//
// A signature is accepted only when the timestamp is within ±300 seconds of
// the server clock and the MAC matches (hmac.Equal, constant time).
//
// Policy: a request carrying any identity header must be signed. A signature
// that is present but wrong or expired is always 401. A missing signature is
// 401 when CHAMPION_REQUIRE_SIGNED_IDENTITY=true; otherwise (the rollout
// default) it is allowed with a warning logged at most once a minute. A request
// with no identity header needs no signature.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	adminAssertHeader       = "X-Champion-Admin"
	identityTimestampHeader = "X-Champion-Timestamp"
	identitySignatureHeader = "X-Champion-Signature"

	identitySignatureVersion = "v1"
	// identitySignatureMaxSkew is how far the signed timestamp may sit from
	// the server clock, in either direction.
	identitySignatureMaxSkew = 300 * time.Second
	// unsignedIdentityWarnEvery rate-limits the rollout warning.
	unsignedIdentityWarnEvery = time.Minute
)

var (
	errIdentitySignatureMissing = errors.New("identity signature missing")
	errIdentitySignatureInvalid = errors.New("identity signature invalid")
	errIdentitySignatureExpired = errors.New("identity signature expired")
)

// identityNow is the verifier's clock; a variable so tests can pin it.
var identityNow = time.Now

// lastUnsignedIdentityWarn holds the UnixNano of the last unsigned-identity
// warning, so the warning is logged at most once per unsignedIdentityWarnEvery.
var lastUnsignedIdentityWarn atomic.Int64

// identityCanonicalString builds the signed string documented at the top of
// this file.
func identityCanonicalString(ts, method, path, acting, impersonator, admin string) string {
	return strings.Join([]string{identitySignatureVersion, ts, strings.ToUpper(method), path, acting, impersonator, admin}, "\n")
}

// signIdentity returns the X-Champion-Signature value ("v1=<lowercase hex
// HMAC-SHA256>") for the canonical string built from these fields, keyed by
// secret. It is what the website computes; the verifier uses it too.
func signIdentity(secret, ts, method, path, acting, impersonator, admin string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(identityCanonicalString(ts, method, path, acting, impersonator, admin)))
	return identitySignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}

// requestIdentityHeaders returns the raw identity header values the request
// asserts, and whether it asserts any.
func requestIdentityHeaders(r *http.Request) (acting, impersonator, admin string, asserts bool) {
	acting = r.Header.Get(actingUserHeader)
	impersonator = r.Header.Get(impersonatorHeader)
	admin = r.Header.Get(adminAssertHeader)
	return acting, impersonator, admin, acting != "" || impersonator != "" || admin != ""
}

// verifyIdentitySignature checks the request's signature headers against
// secret at time now. It returns errIdentitySignatureMissing when no
// signature was sent (the caller applies the rollout policy), nil when the
// signature is valid, and errIdentitySignatureInvalid/Expired otherwise.
func verifyIdentitySignature(r *http.Request, secret string, now time.Time) error {
	sig := strings.TrimSpace(r.Header.Get(identitySignatureHeader))
	if sig == "" {
		return errIdentitySignatureMissing
	}
	if secret == "" || !strings.HasPrefix(sig, identitySignatureVersion+"=") {
		return errIdentitySignatureInvalid
	}
	ts := strings.TrimSpace(r.Header.Get(identityTimestampHeader))
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errIdentitySignatureInvalid
	}
	skew := now.Sub(time.Unix(sec, 0))
	if skew > identitySignatureMaxSkew || skew < -identitySignatureMaxSkew {
		return errIdentitySignatureExpired
	}
	acting, impersonator, admin, _ := requestIdentityHeaders(r)
	want := signIdentity(secret, ts, r.Method, r.URL.Path, acting, impersonator, admin)
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return errIdentitySignatureInvalid
	}
	return nil
}

// checkSignedIdentity applies the signed-identity policy to a request whose
// bearer token already matched secret. It reports whether the request may
// proceed; it never writes a response, so each route keeps its own 401 shape.
// Nothing about the header values is ever logged - only the path.
func checkSignedIdentity(r *http.Request, secret string, require bool) bool {
	_, _, _, asserts := requestIdentityHeaders(r)
	hasSig := strings.TrimSpace(r.Header.Get(identitySignatureHeader)) != ""
	if !asserts && !hasSig {
		return true
	}
	err := verifyIdentitySignature(r, secret, identityNow())
	switch {
	case err == nil:
		return true
	case errors.Is(err, errIdentitySignatureMissing):
		if require {
			slog.Warn("component=saas_api", "event", "unsigned_identity_rejected", "path", r.URL.Path)
			return false
		}
		warnUnsignedIdentity(r.URL.Path)
		return true
	default:
		reason := "invalid"
		if errors.Is(err, errIdentitySignatureExpired) {
			reason = "expired"
		}
		slog.Warn("component=saas_api", "event", "identity_signature_rejected", "reason", reason, "path", r.URL.Path)
		return false
	}
}

// warnUnsignedIdentity logs the rollout warning at most once per minute.
func warnUnsignedIdentity(path string) {
	now := time.Now().UnixNano()
	last := lastUnsignedIdentityWarn.Load()
	if last != 0 && now-last < int64(unsignedIdentityWarnEvery) {
		return
	}
	if !lastUnsignedIdentityWarn.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("component=saas_api", "event", "unsigned_identity", "path", path)
}

// requireSignedIdentity is checkSignedIdentity for the /api/saas and
// /api/admin routes keyed by WEBSITE_API_SECRET; on refusal it writes the
// UNAUTHORIZED envelope.
func (a *App) requireSignedIdentity(w http.ResponseWriter, r *http.Request) bool {
	secret, require := "", false
	if a.Config != nil {
		secret, require = a.Config.WebsiteAPISecret, a.Config.RequireSignedIdentity
	}
	if checkSignedIdentity(r, secret, require) {
		return true
	}
	writeSaaSError(w, codeUnauthorized, "missing or invalid identity signature")
	return false
}
