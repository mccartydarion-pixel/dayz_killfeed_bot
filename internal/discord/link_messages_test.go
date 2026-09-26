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
		{linking.ErrInvalidUsername, "INVALID PLAYSTATION USERNAME"},
		{linking.ErrPlayerNotFound, "PLAYER NOT FOUND"},
		{linking.ErrPlayerNotObserved, "NOT OBSERVED ON SERVER"},
		{&linking.PlaytimeShortfallError{Observed: 3*time.Minute + 20*time.Second, Required: 5 * time.Minute}, "3m 20s of the required 5m"},
		{linking.ErrPlaytimeRequired, "MORE PLAYTIME REQUIRED"},
		{linking.ErrInstallationNotConfigured, "SERVER NOT CONNECTED"},
		{linking.ErrNoConnectedServer, "SERVER NOT CONNECTED"},
		{fmt.Errorf("wrapped: %w", linking.ErrNoConnectedServer), "SERVER NOT CONNECTED"},
		{linking.ErrActivityUnavailable, "LINK CHECK UNAVAILABLE"},
		{linking.ErrLinkCheckUnavailable, "LINK CHECK UNAVAILABLE"},
		{linking.ErrAlreadyLinked, "ACCOUNT ALREADY LINKED"},
		{linking.ErrPlayerClaimed, "ALREADY LINKED"},
		{fmt.Errorf("wrapped: %w", linking.ErrPlayerNotObserved), "NOT OBSERVED ON SERVER"},
		{errors.New("boom"), "Could not create a pending link"},
	}
	for _, tc := range cases {
		if got := linkErrorMessage(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("%v: message %q does not contain %q", tc.err, got, tc.want)
		}
	}
	for _, err := range []error{linking.ErrInstallationNotConfigured, linking.ErrNoConnectedServer, linking.ErrPlayerNotObserved, linking.ErrInvalidUsername, linking.ErrPlaytimeRequired, linking.ErrPlayerNotFound, linking.ErrAlreadyLinked, linking.ErrPlayerClaimed} {
		if strings.Contains(linkErrorMessage(err), "UNAVAILABLE") {
			t.Errorf("%v must not be reported as LINK CHECK UNAVAILABLE", err)
		}
	}
}
