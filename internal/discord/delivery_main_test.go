package discord

import (
	"os"
	"testing"
	"time"
)

// TestMain keeps delivery retry backoff out of test wall-clock time; tests
// that need real sleeps set deliverySleep themselves.
func TestMain(m *testing.M) {
	deliverySleep = func(time.Duration) {}
	os.Exit(m.Run())
}
