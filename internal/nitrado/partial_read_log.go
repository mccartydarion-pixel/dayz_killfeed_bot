package nitrado

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sync"
	"time"
)

// Partial-read failures used to be logged at debug only, so a service whose seek never worked
// looked exactly like one that was never tried (docs/NITRADO_POLLING.md). Each failure is now
// logged at WARN, at most once per service and method every partialFailLogEvery, with a reason
// that never contains a URL: signed download URLs carry credentials and must not be logged.

const partialFailLogEvery = 30 * time.Minute

var urlPattern = regexp.MustCompile(`https?://[^\s"']+`)

var partialFailLog = struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}{last: map[string]time.Time{}, now: time.Now}

// safeReason describes err without any URL: the API error's own fields, the transport error under
// a *url.Error (which otherwise prints the signed URL), and any remaining URL text replaced.
func safeReason(err error) string {
	if err == nil {
		return ""
	}
	var reason string
	var reqErr *RequestError
	var urlErr *url.Error
	switch {
	case errors.As(err, &reqErr):
		reason = fmt.Sprintf("%s status=%d kind=%s message=%s", reqErr.Op, reqErr.StatusCode, reqErr.Kind, reqErr.Message)
	case errors.As(err, &urlErr):
		reason = "transport: " + urlErr.Err.Error()
	default:
		reason = err.Error()
	}
	reason = urlPattern.ReplaceAllString(reason, "<url>")
	if len(reason) > 300 {
		reason = reason[:300] + "…"
	}
	return reason
}

// logPartialFailure logs one failed partial-read attempt, rate-limited per service and method.
func logPartialFailure(serviceID string, method Capability, err error) {
	key := serviceID + "|" + string(method)
	partialFailLog.mu.Lock()
	now := partialFailLog.now()
	if last, ok := partialFailLog.last[key]; ok && now.Sub(last) < partialFailLogEvery {
		partialFailLog.mu.Unlock()
		return
	}
	partialFailLog.last[key] = now
	partialFailLog.mu.Unlock()
	slog.Warn("component=nitrado", "event", "partial_read_failed", "service_id", serviceID, "method", string(method),
		"unsupported", errors.Is(err, errUnsupported), "reason", safeReason(err))
}
