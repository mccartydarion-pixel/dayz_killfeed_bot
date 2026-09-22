package billing

import (
	"net/http"
	"net/url"
	"strings"
)

// DefaultOrigin is Champion's production site (docs/BILLING.md "Safe return URL"). It is always
// index 0 of the configured allowlist and is used whenever a request's Origin/Referer is absent or
// does not match a configured origin.
const DefaultOrigin = "https://championshp.vip"

// ParseAllowedOrigins parses a comma-separated list of allowed site origins (each
// "scheme://host[:port]", no path). DefaultOrigin is always included even if raw is empty or
// doesn't mention it, so production never depends on an operator remembering to list it.
// Malformed entries are dropped rather than rejected outright - a typo'd dev origin should not
// prevent the service from starting.
func ParseAllowedOrigins(raw string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(o string) {
		o = strings.TrimSpace(o)
		if o == "" || seen[o] {
			return
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
			return
		}
		seen[o] = true
		out = append(out, u.Scheme+"://"+u.Host)
	}
	add(DefaultOrigin)
	for _, part := range strings.Split(raw, ",") {
		add(part)
	}
	return out
}

// ResolveOrigin picks the site origin a redirect should use: the request's Origin header if it
// exactly matches an allowed entry, else its Referer's scheme+host if that matches, else
// allowed[0] (the production default). The website is therefore never asked to assert its own
// origin - Champion decides it server-side from an allowlist, which is what makes the resulting
// redirect safe regardless of what a caller claims.
func ResolveOrigin(r *http.Request, allowed []string) string {
	if len(allowed) == 0 {
		allowed = []string{DefaultOrigin}
	}
	if o := normalizeOrigin(r.Header.Get("Origin")); o != "" {
		for _, a := range allowed {
			if o == a {
				return a
			}
		}
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
			o := u.Scheme + "://" + u.Host
			for _, a := range allowed {
				if o == a {
					return a
				}
			}
		}
	}
	return allowed[0]
}

func normalizeOrigin(o string) string {
	o = strings.TrimSpace(o)
	if o == "" {
		return ""
	}
	u, err := url.Parse(o)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// parseRootRelativePath validates that path is a safe, root-relative client path - it must start
// with "/", must not be scheme-relative ("//evil.example"), must not contain a backslash escape, and
// must not parse into anything carrying a scheme or host - and returns its parsed form. Shared by
// SafeReturnURL (which turns a validated path into an absolute redirect URL) and DeriveCancelPath
// (which needs the validated path's own components, not yet joined to an origin), so both apply
// exactly the same rules; neither performs any string-level substitution on unvalidated input.
func parseRootRelativePath(path string) (*url.URL, bool) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\\") {
		return nil, false
	}
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.Scheme != "" {
		return nil, false
	}
	return u, true
}

// SafeReturnURL builds an absolute URL from a server-chosen origin and a client-supplied path. The
// path must be root-relative ("/dashboard/..."), must not be scheme-relative ("//evil.example") or
// contain a scheme/backslash escape, and must not itself carry a fragment that could be used to
// smuggle a redirect past the allowlist check performed on origin. def is used when path is empty.
// This is the only function in the codebase allowed to turn user input into a Stripe
// success_url/cancel_url/return_url - callers must not build one by string concatenation elsewhere.
func SafeReturnURL(origin, path, def string) (string, bool) {
	if path == "" {
		path = def
	}
	u, ok := parseRootRelativePath(path)
	if !ok {
		return "", false
	}
	return strings.TrimRight(origin, "/") + u.String(), true
}

// DeriveCancelPath returns returnPath's own root-relative path and query, with the query's
// "checkout" parameter (if any) overwritten to "cancelled" - every other query parameter is
// preserved. It lets Checkout build a same-page cancel URL from the single returnPath the Champion
// website contract sends for success, without ever concatenating strings: the path is validated by
// the same parseRootRelativePath rules as SafeReturnURL, and the query is rewritten via net/url, not
// substring replacement. ok is false when returnPath is empty or fails that validation, in which case
// the caller should fall back to its own configured cancel path.
func DeriveCancelPath(returnPath string) (string, bool) {
	u, ok := parseRootRelativePath(returnPath)
	if !ok {
		return "", false
	}
	q := u.Query()
	q.Set("checkout", "cancelled")
	u.RawQuery = q.Encode()
	return u.String(), true
}
