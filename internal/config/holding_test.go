package config

import (
	"strings"
	"testing"
)

// A disk marked holding: true, buffering a tape landing, loads and resolves.
func TestHolding_ValidLoads(t *testing.T) {
	c, err := loadYAML(t, `
landing: lto
media:
  lto:     { type: tape, dir: /tmp/v }
  scratch: { type: disk, path: /tmp/s, capacity: 500GB, holding: true }
sources:
  default:
    localhost: [/home]
`)
	if err != nil {
		t.Fatalf("valid holding config must load, got %v", err)
	}
	if name, ok := c.HoldingMedium(); !ok || name != "scratch" {
		t.Errorf("HoldingMedium() = %q,%v; want scratch,true", name, ok)
	}
}

// (A holding disk's medium-type capability — concurrent writes + per-archive reclaim — is a
// media-layer property the engine checks; see the engine package's holding tests. config
// validates only the structural rules below, free of medium-type knowledge.)

// At most one holding medium.
func TestHolding_RejectsTwo(t *testing.T) {
	_, err := loadYAML(t, `
landing: lto
media:
  lto: { type: tape, dir: /tmp/v }
  a:   { type: disk, path: /tmp/a, holding: true }
  b:   { type: disk, path: /tmp/b, holding: true }
sources:
  default:
    localhost: [/home]
`)
	if err == nil || !strings.Contains(err.Error(), "at most one holding disk") {
		t.Fatalf("want at-most-one error, got %v", err)
	}
}

// The holding disk may not be the landing.
func TestHolding_RejectsLanding(t *testing.T) {
	_, err := loadYAML(t, `
landing: scratch
media:
  lto:     { type: tape, dir: /tmp/v }
  scratch: { type: disk, path: /tmp/s, holding: true }
sources:
  default:
    localhost: [/home]
`)
	if err == nil || !strings.Contains(err.Error(), "both the landing and a holding disk") {
		t.Fatalf("want landing-conflict error, got %v", err)
	}
}
