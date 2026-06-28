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
	if got := c.HoldingMedia(); len(got) != 1 || got[0] != "scratch" {
		t.Errorf("HoldingMedia() = %v; want [scratch]", got)
	}
}

// (A holding disk's medium-type capability — concurrent writes + per-archive reclaim — is a
// media-layer property the engine checks; see the engine package's holding tests. config
// validates only the structural rules below, free of medium-type knowledge.)

// Several holding media are allowed; HoldingMedia returns them sorted (deterministic allocation
// and drain order).
func TestHolding_AllowsMultiple(t *testing.T) {
	c, err := loadYAML(t, `
landing: lto
media:
  lto: { type: tape, dir: /tmp/v }
  b:   { type: disk, path: /tmp/b, holding: true }
  a:   { type: disk, path: /tmp/a, holding: true }
sources:
  default:
    localhost: [/home]
`)
	if err != nil {
		t.Fatalf("multiple holding disks must load, got %v", err)
	}
	got := c.HoldingMedia()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("HoldingMedia() = %v; want [a b]", got)
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
