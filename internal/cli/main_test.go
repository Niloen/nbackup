package cli

import (
	"os"
	"testing"
	"time"
)

// TestMain pins the test binary's local zone to UTC so the date helpers (today,
// ParseDate) and the past/future guards assert stable calendar days regardless of
// the machine the suite runs on. Those helpers reason in the *local* zone by design;
// fixing it here makes the assertions deterministic.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
