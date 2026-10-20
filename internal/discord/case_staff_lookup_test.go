package discord

import (
	"errors"
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// A failed Discord read during the staff-channel privacy check is retryable when Discord could not
// answer (429, 5xx, network) and unsafe when it answered (403 missing access, 404 gone).
func TestCaseStaffLookupFailureClassification(t *testing.T) {
	rest := func(code int) error { return &discordgo.RESTError{Response: &http.Response{StatusCode: code}} }
	for _, err := range []error{rest(http.StatusTooManyRequests), rest(http.StatusBadGateway), errors.New("dial tcp: i/o timeout")} {
		got := caseStaffLookupFailed(err, "lookup")
		if !errors.Is(got, ErrCaseStaffLookupTransient) || errors.Is(got, ErrCaseStaffChannelUnsafe) {
			t.Fatalf("%v must be transient, got %v", err, got)
		}
	}
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		got := caseStaffLookupFailed(rest(code), "lookup")
		if !errors.Is(got, ErrCaseStaffChannelUnsafe) || errors.Is(got, ErrCaseStaffLookupTransient) {
			t.Fatalf("%d must be unsafe, got %v", code, got)
		}
	}
}
