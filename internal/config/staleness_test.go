package config

import (
	"testing"
	"time"
)

// TestStalenessWindowDefaultsDisabled pins the deliberate choice to have no
// non-zero default (unlike DrillWindow): nb dump attempts every DLE every run, so
// there is no schedule to derive a threshold from, and guessing one risks false
// alarms on a legitimately irregular cron cadence.
func TestStalenessWindowDefaultsDisabled(t *testing.T) {
	var c Config
	window, ok := c.StalenessWindow()
	if ok || window != 0 {
		t.Errorf("StalenessWindow() = (%v, %v), want (0, false) when unset", window, ok)
	}
}

func TestStalenessWindowParsesConfigured(t *testing.T) {
	c := Config{Staleness: StalenessConfig{Window: "3d"}}
	window, ok := c.StalenessWindow()
	if !ok || window != 3*24*time.Hour {
		t.Errorf("StalenessWindow() = (%v, %v), want (72h, true)", window, ok)
	}
}

func TestValidateStalenessRejectsBadDuration(t *testing.T) {
	c := Config{Staleness: StalenessConfig{Window: "not-a-duration"}}
	if err := c.validateStaleness(); err == nil {
		t.Error("validateStaleness() = nil, want error for unparseable window")
	}
}

func TestValidateStalenessAcceptsUnset(t *testing.T) {
	var c Config
	if err := c.validateStaleness(); err != nil {
		t.Errorf("validateStaleness() = %v, want nil for unset window", err)
	}
}
