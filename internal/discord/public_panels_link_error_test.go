package discord

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/linking"
)

// TestLinkErrorMessageDistinguishesOutcomes: only a real backend failure may
// read as LINK CHECK UNAVAILABLE; ordinary verification outcomes and a missing
// server each have their own message.
func TestLinkErrorMessageDistinguishesOutcomes(t *testing.T) {
	cases := map[error]string{
		linking.ErrLinkCheckUnavailable: "LINK CHECK UNAVAILABLE",
		linking.ErrNoConnectedServer:    "SERVER NOT CONNECTED",
		linking.ErrPlayerNotFound:       "PLAYER NOT FOUND",
		linking.ErrPlaytimeRequired:     "MORE PLAYTIME REQUIRED",
		linking.ErrAlreadyLinked:        "ACCOUNT ALREADY LINKED",
		linking.ErrPlayerClaimed:        "ALREADY LINKED",
	}
	for err, want := range cases {
		got := linkErrorMessage(fmt.Errorf("wrapped: %w", err))
		if !strings.Contains(got, want) {
			t.Errorf("%v: got %q, want it to contain %q", err, got, want)
		}
		if err != linking.ErrLinkCheckUnavailable && strings.Contains(got, "UNAVAILABLE") {
			t.Errorf("%v must not be reported as unavailable: %q", err, got)
		}
	}
}
