package nitrado

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rate-limit telemetry (docs/NITRADO_POLLING.md). Nitrado reports a per-token budget on every API
// response (X-RateLimit-Limit / -Remaining / -Reset) and answers 429 when it is spent. This file
// records those headers and counts requests per token, so the real limit and headroom are known
// from normal traffic: nothing here ever sends a request of its own. Tokens are identified by a
// short SHA-256 prefix; the token itself is never stored or logged.

// lowHeadroom is the share of the budget below which polling slows down and a warning is logged.
const lowHeadroom = 0.20

// healthyHeadroom is the share of the budget above which busy servers may poll at the fast rate.
const healthyHeadroom = 0.50

// budgetLogEvery is how often a per-token budget summary is logged.
const budgetLogEvery = 5 * time.Minute

// TokenKey is the non-reversible identifier a token is tracked under.
func TokenKey(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

// Budget is what Nitrado last said about one token's rate limit.
type Budget struct {
	Known     bool
	Limit     int
	Remaining int
	Reset     time.Time
	SeenAt    time.Time
}

// Headroom is Remaining/Limit (0..1), or -1 when unknown.
func (b Budget) Headroom() float64 {
	if !b.Known || b.Limit <= 0 {
		return -1
	}
	return float64(b.Remaining) / float64(b.Limit)
}

// Low reports whether the budget is known and below lowHeadroom (and the window has not reset).
func (b Budget) Low(now time.Time) bool {
	h := b.Headroom()
	if h < 0 || h >= lowHeadroom {
		return false
	}
	return b.Reset.IsZero() || now.Before(b.Reset)
}

// Healthy reports whether the budget is known and at or above healthyHeadroom.
func (b Budget) Healthy(now time.Time) bool {
	if !b.Reset.IsZero() && !now.Before(b.Reset) {
		return true // the window has reset since the last reading
	}
	return b.Headroom() >= healthyHeadroom
}

// TokenUsage is one token's budget and request counts, for the Owner Hub and logs.
type TokenUsage struct {
	Token          string         `json:"token"` // TokenKey, never the token
	Limit          int            `json:"limit"`
	Remaining      int            `json:"remaining"`
	ResetAt        *time.Time     `json:"resetAt,omitempty"`
	SeenAt         *time.Time     `json:"seenAt,omitempty"`
	HeadersSeen    bool           `json:"headersSeen"`
	RequestsLastHr int            `json:"requestsLastHour"`
	RateLimited    int            `json:"rateLimitedLastHour"` // 429 responses
	ByOperation    map[string]int `json:"byOperationLastHour"`
	Downloads      int            `json:"signedDownloadsLastHour"` // signed-URL fetches (separate host)
}

type minuteBucket struct {
	minute    int64
	requests  int
	limited   int
	downloads int
	ops       map[string]int
}

type tokenStats struct {
	budget  Budget
	buckets [60]minuteBucket
	lastLog time.Time
}

type rateRegistry struct {
	mu     sync.Mutex
	tokens map[string]*tokenStats
	now    func() time.Time
}

var rateLimits = &rateRegistry{tokens: map[string]*tokenStats{}, now: time.Now}

func (r *rateRegistry) stats(key string) *tokenStats {
	s := r.tokens[key]
	if s == nil {
		s = &tokenStats{}
		r.tokens[key] = s
	}
	return s
}

func (s *tokenStats) bucket(now time.Time) *minuteBucket {
	m := now.Unix() / 60
	b := &s.buckets[m%60]
	if b.minute != m {
		*b = minuteBucket{minute: m, ops: map[string]int{}}
	}
	return b
}

// observe records one API response for a token.
func (r *rateRegistry) observe(key, op string, resp *http.Response) {
	if key == "" || resp == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	s := r.stats(key)
	b := s.bucket(now)
	b.requests++
	b.ops[op]++
	if resp.StatusCode == http.StatusTooManyRequests {
		b.limited++
	}
	if budget, ok := parseRateHeaders(resp.Header, now); ok {
		wasLow := s.budget.Low(now)
		s.budget = budget
		if budget.Low(now) && !wasLow {
			slog.Warn("component=nitrado", "event", "rate_budget_low", "token", key, "limit", budget.Limit, "remaining", budget.Remaining, "reset_at", budget.Reset.UTC().Format(time.RFC3339))
		}
	}
	if now.Sub(s.lastLog) >= budgetLogEvery {
		s.lastLog = now
		u := s.usage(key, now)
		slog.Info("component=nitrado", "event", "rate_budget", "token", key, "headers_seen", u.HeadersSeen, "limit", u.Limit, "remaining", u.Remaining, "requests_last_hour", u.RequestsLastHr, "rate_limited_last_hour", u.RateLimited, "signed_downloads_last_hour", u.Downloads)
	}
}

// observeDownload counts one signed-URL fetch (served by a different host, outside the API budget
// as far as Nitrado documents, but counted so total load is visible).
func (r *rateRegistry) observeDownload(key string, status int) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b := r.stats(key).bucket(now)
	b.downloads++
	if status == http.StatusTooManyRequests {
		b.limited++
	}
}

func (s *tokenStats) usage(key string, now time.Time) TokenUsage {
	u := TokenUsage{Token: key, ByOperation: map[string]int{}, HeadersSeen: s.budget.Known}
	if s.budget.Known {
		u.Limit, u.Remaining = s.budget.Limit, s.budget.Remaining
		seen := s.budget.SeenAt
		u.SeenAt = &seen
		if !s.budget.Reset.IsZero() {
			reset := s.budget.Reset
			u.ResetAt = &reset
		}
	}
	cur := now.Unix() / 60
	for _, b := range s.buckets {
		if b.minute == 0 || cur-b.minute >= 60 {
			continue
		}
		u.RequestsLastHr += b.requests
		u.RateLimited += b.limited
		u.Downloads += b.downloads
		for op, n := range b.ops {
			u.ByOperation[op] += n
		}
	}
	return u
}

func (r *rateRegistry) budget(key string) Budget {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.tokens[key]; s != nil {
		return s.budget
	}
	return Budget{}
}

// RateLimitUsage returns every tracked token's budget and last-hour request counts.
func RateLimitUsage() []TokenUsage {
	rateLimits.mu.Lock()
	defer rateLimits.mu.Unlock()
	now := rateLimits.now()
	out := make([]TokenUsage, 0, len(rateLimits.tokens))
	for key, s := range rateLimits.tokens {
		out = append(out, s.usage(key, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestsLastHr > out[j].RequestsLastHr })
	return out
}

// RateBudget is this client's token budget as Nitrado last reported it.
func (c *Client) RateBudget() Budget {
	if c == nil {
		return Budget{}
	}
	return rateLimits.budget(c.tokenKey)
}

// parseRateHeaders reads X-RateLimit-Limit/-Remaining/-Reset. Reset may be a Unix timestamp, a
// number of seconds until reset, or an RFC 3339 / HTTP date; anything else leaves Reset zero.
func parseRateHeaders(h http.Header, now time.Time) (Budget, bool) {
	limit, err1 := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Limit")))
	remaining, err2 := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Remaining")))
	if err1 != nil || err2 != nil || limit <= 0 || remaining < 0 {
		return Budget{}, false
	}
	b := Budget{Known: true, Limit: limit, Remaining: remaining, SeenAt: now}
	raw := strings.TrimSpace(h.Get("X-RateLimit-Reset"))
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		if n > 1_000_000_000 {
			b.Reset = time.Unix(n, 0)
		} else {
			b.Reset = now.Add(time.Duration(n) * time.Second)
		}
	} else if t, err := time.Parse(time.RFC3339, raw); err == nil {
		b.Reset = t
	} else if t, err := http.ParseTime(raw); err == nil {
		b.Reset = t
	}
	return b, true
}

// operationLabel turns an API path into a stable label without ids or query:
// "/services/123/gameservers/file_server/list?dir=/x" -> "gameservers/file_server/list".
func operationLabel(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p == "" || p == "services" {
			continue
		}
		if _, err := strconv.ParseInt(p, 10, 64); err == nil {
			continue
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "root"
	}
	return strings.Join(parts, "/")
}
