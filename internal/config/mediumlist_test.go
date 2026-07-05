package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The common single-medium spelling stays a plain scalar.
func TestLanding_ScalarLoads(t *testing.T) {
	c, err := loadYAML(t, `
landing: s3
media:
  s3: { type: disk, path: /tmp/s3 }
sources:
  default:
    localhost: [/home]
`)
	if err != nil {
		t.Fatalf("scalar landing must load, got %v", err)
	}
	names, err := c.LandingNames()
	if err != nil || len(names) != 1 || names[0] != "s3" {
		t.Errorf("LandingNames() = %v, %v; want [s3]", names, err)
	}
}

// A list fans out; order is preserved and the first entry is the primary.
func TestLanding_ListLoadsPrimaryFirst(t *testing.T) {
	c, err := loadYAML(t, `
landing: [s3, gdrive]
media:
  s3:     { type: disk, path: /tmp/s3 }
  gdrive: { type: disk, path: /tmp/gd }
sources:
  default:
    localhost: [/home]
`)
	if err != nil {
		t.Fatalf("list landing must load, got %v", err)
	}
	names, err := c.LandingNames()
	if err != nil || len(names) != 2 || names[0] != "s3" || names[1] != "gdrive" {
		t.Errorf("LandingNames() = %v, %v; want [s3 gdrive]", names, err)
	}
	if got, err := c.LandingName(); err != nil || got != "s3" {
		t.Errorf("LandingName() = %q, %v; want primary s3", got, err)
	}
}

// A route must not repeat a medium — a duplicate would double-write one store.
func TestLanding_RejectsDuplicates(t *testing.T) {
	_, err := loadYAML(t, `
landing: [s3, s3]
media:
  s3: { type: disk, path: /tmp/s3 }
sources:
  default:
    localhost: [/home]
`)
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate landing entries must be rejected, got %v", err)
	}
}

// Every entry must be a defined medium, not just the first.
func TestLanding_RejectsUndefinedEntry(t *testing.T) {
	_, err := loadYAML(t, `
landing: [s3, nope]
media:
  s3: { type: disk, path: /tmp/s3 }
sources:
  default:
    localhost: [/home]
`)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("undefined landing entry must be rejected, got %v", err)
	}
}

// A holding medium is a write-path buffer, not a landing — anywhere in the list.
func TestLanding_RejectsHoldingEntry(t *testing.T) {
	_, err := loadYAML(t, `
landing: [s3, scratch]
media:
  s3:      { type: disk, path: /tmp/s3 }
  scratch: { type: disk, path: /tmp/s, holding: true }
sources:
  default:
    localhost: [/home]
`)
	if err == nil || !strings.Contains(err.Error(), "holding") {
		t.Errorf("holding medium in landing list must be rejected, got %v", err)
	}
}

// A dumptype override may be a list too, resolved per DLE with the config-wide
// route as the default.
func TestLanding_DumpTypeListOverride(t *testing.T) {
	c, err := loadYAML(t, `
landing: main
media:
  main: { type: disk, path: /tmp/m }
  a:    { type: disk, path: /tmp/a }
  b:    { type: disk, path: /tmp/b }
dumptypes:
  fanned: { landing: [a, b] }
sources:
  fanned:
    localhost: [/data]
  default:
    localhost: [/home]
`)
	if err != nil {
		t.Fatalf("dumptype list override must load, got %v", err)
	}
	fanned := DLE{Host: "localhost", Path: "/data", DumpType: "fanned"}
	plain := DLE{Host: "localhost", Path: "/home"}
	if got, err := c.LandingsFor(fanned); err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("LandingsFor(fanned) = %v, %v; want [a b]", got, err)
	}
	if got, err := c.LandingsFor(plain); err != nil || len(got) != 1 || got[0] != "main" {
		t.Errorf("LandingsFor(default) = %v, %v; want [main]", got, err)
	}
	if got, err := c.LandingFor(fanned); err != nil || got != "a" {
		t.Errorf("LandingFor(fanned) = %q, %v; want primary a", got, err)
	}
}

// A duplicate in a dumptype route is rejected like the config-wide one.
func TestLanding_DumpTypeRejectsDuplicates(t *testing.T) {
	_, err := loadYAML(t, `
landing: main
media:
  main: { type: disk, path: /tmp/m }
  a:    { type: disk, path: /tmp/a }
dumptypes:
  fanned: { landing: [a, a] }
sources:
  fanned:
    localhost: [/data]
`)
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate dumptype landing entries must be rejected, got %v", err)
	}
}

// A one-entry route marshals back to the scalar spelling (nb init round-trip);
// a longer route stays a sequence and re-loads to the same list.
func TestMediumList_MarshalRoundTrip(t *testing.T) {
	one, err := yaml.Marshal(struct {
		Landing MediumList `yaml:"landing"`
	}{Landing: MediumList{"s3"}})
	if err != nil || !strings.Contains(string(one), "landing: s3\n") {
		t.Errorf("one-entry list must marshal as a scalar, got %q, %v", one, err)
	}
	two, err := yaml.Marshal(struct {
		Landing MediumList `yaml:"landing"`
	}{Landing: MediumList{"s3", "gdrive"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back struct {
		Landing MediumList `yaml:"landing"`
	}
	if err := yaml.Unmarshal(two, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Landing) != 2 || back.Landing[0] != "s3" || back.Landing[1] != "gdrive" {
		t.Errorf("two-entry round trip = %v; want [s3 gdrive]", back.Landing)
	}
}

// A mapping under landing: is a config mistake with a clear message.
func TestMediumList_RejectsMapping(t *testing.T) {
	var out struct {
		Landing MediumList `yaml:"landing"`
	}
	err := yaml.Unmarshal([]byte("landing: {s3: true}"), &out)
	if err == nil || !strings.Contains(err.Error(), "media name") {
		t.Errorf("mapping must be rejected with a hint, got %v", err)
	}
}
