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

// The holding disk must be a disk/cloud medium, not tape.
func TestHolding_RejectsTape(t *testing.T) {
	_, err := loadYAML(t, `
landing: disk
media:
  disk: { type: disk, path: /tmp/d }
  vault: { type: tape, dir: /tmp/v, holding: true }
sources:
  default:
    localhost: [/home]
`)
	if err == nil || !strings.Contains(err.Error(), "requires a disk or cloud medium") {
		t.Fatalf("want disk/cloud requirement, got %v", err)
	}
}

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
