package discord

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/linking"
)

// Each /link outcome must reach the player as its own message; only a real
// outage says LINK CHECK UNAVAILABLE.
func TestLinkErrorMessageDistinguishesOutcomes(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{linking.ErrInvalidUsername, "Invalid PlayStation username"},
		{linking.ErrPlayerNotFound, "Player not found"},
		{linking.ErrPlayerNotObserved, "Not observed on server"},
		{&linking.PlaytimeShortfallError{Observed: 3*time.Minute + 20*time.Second, Required: 5 * time.Minute}, "3m 20s of the required 5m"},
		{linking.ErrPlaytimeRequired, "More playtime required"},
		{linking.ErrInstallationNotConfigured, "Server not connected"},
		{linking.ErrNoConnectedServer, "Server not connected"},
		{fmt.Errorf("wrapped: %w", linking.ErrNoConnectedServer), "Server not connected"},
		{linking.ErrActivityUnavailable, "Link check unavailable"},
		{linking.ErrLinkCheckUnavailable, "Link check unavailable"},
		{linking.ErrAlreadyLinked, "Account already linked"},
		{linking.ErrPlayerClaimed, "Already linked"},
		{fmt.Errorf("wrapped: %w", linking.ErrPlayerNotObserved), "Not observed on server"},
		{errors.New("boom"), "Couldn't create a pending link"},
	}
	for _, tc := range cases {
		if got := linkErrorMessage(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("%v: message %q does not contain %q", tc.err, got, tc.want)
		}
	}
	for _, err := range []error{linking.ErrInstallationNotConfigured, linking.ErrNoConnectedServer, linking.ErrPlayerNotObserved, linking.ErrInvalidUsername, linking.ErrPlaytimeRequired, linking.ErrPlayerNotFound, linking.ErrAlreadyLinked, linking.ErrPlayerClaimed} {
		if strings.Contains(strings.ToLower(linkErrorMessage(err)), "unavailable") {
			t.Errorf("%v must not be reported as a link check that is unavailable", err)
		}
	}
}
