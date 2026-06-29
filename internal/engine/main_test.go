package engine

import (
	"os"
	"testing"
	"time"
)

// TestMain pins the test binary's local zone to UTC so tests that drive Run with a
// fixed instant assert a stable slot date. Slot dates are the run's *local* calendar
// day (see localDay); fixing the zone keeps that day deterministic regardless of the
// machine the suite runs on. TestLocalDay covers the cross-zone behavior directly.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
