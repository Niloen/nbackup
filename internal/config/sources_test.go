package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// findDLE returns the DLE with the given host+path, or fails.
func findDLE(t *testing.T, c *Config, host, path string) DLE {
	t.Helper()
	for _, d := range c.Sources {
		if d.Host == host && d.Path == path {
			return d
		}
	}
	t.Fatalf("no source %s:%s in %v", host, path, c.Sources)
	return DLE{}
}

// TestSourcesParseForms covers the three source forms: a plain scalar path, a scalar path
// with a wildcard (a selection), and the {path, partition} mapping (a partition).
func TestSourcesParseForms(t *testing.T) {
	c, err := loadYAML(t, baseMedia+`
sources:
  default:
    fileserver:
      - /var/log
      - /srv/web-*
      - path: /data
        partition: "*"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if d := findDLE(t, c, "fileserver", "/var/log"); d.Partition != "" {
		t.Errorf("/var/log: want no partition, got %q", d.Partition)
	}
	if d := findDLE(t, c, "fileserver", "/srv/web-*"); d.Partition != "" {
		t.Errorf("/srv/web-*: want no partition (wildcard lives in the path), got %q", d.Partition)
	}
	if d := findDLE(t, c, "fileserver", "/data"); d.Partition != "*" {
		t.Errorf("/data: want partition %q, got %q", "*", d.Partition)
	}
}

// TestPartitionBasePathIsCleaned: a trailing-slash base normalizes at decode so slugs and
// carve-prefix math stay canonical. Scalar sources are never cleaned (conninfo safety).
func TestPartitionBasePathIsCleaned(t *testing.T) {
	c, err := loadYAML(t, baseMedia+`
sources:
  default:
    fileserver:
      - path: /data/
        partition: "*"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if d := findDLE(t, c, "fileserver", "/data"); d.Partition != "*" {
		t.Errorf("trailing-slash base not cleaned: %+v", c.Sources)
	}
}

// TestSourcesRoundTrip checks a config with all three forms marshals back to YAML that
// re-parses to the same DLE set — the nb init / config-rewrite path.
func TestSourcesRoundTrip(t *testing.T) {
	c, err := loadYAML(t, baseMedia+`
sources:
  default:
    fileserver:
      - /var/log
      - path: /data
        partition: "*"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	out, err := yaml.Marshal(c.Sources)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Sources
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-parse %q: %v", out, err)
	}
	if len(back) != len(c.Sources) {
		t.Fatalf("round-trip changed count: %d -> %d (%s)", len(c.Sources), len(back), out)
	}
	for i := range back {
		if back[i] != c.Sources[i] {
			t.Errorf("round-trip mismatch at %d: %+v != %+v", i, back[i], c.Sources[i])
		}
	}
}

func TestSourcesValidation(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{
			name: "partition base cannot be root",
			yaml: `
sources:
  default:
    fileserver:
      - path: /
        partition: "*"
`,
			wantErr: "cannot be the filesystem root",
		},
		{
			name: "partition must be relative",
			yaml: `
sources:
  default:
    fileserver:
      - path: /data
        partition: /abs
`,
			wantErr: "must be relative",
		},
		{
			name: "partition rejects doublestar",
			yaml: `
sources:
  default:
    fileserver:
      - path: /data
        partition: "**"
`,
			wantErr: "must not use **",
		},
		{
			name: "unknown key in source mapping",
			yaml: `
sources:
  default:
    fileserver:
      - path: /data
        split: "*"
`,
			wantErr: `unknown key "split"`,
		},
		{
			name: "wildcard partition base rejected",
			yaml: `
sources:
  default:
    fileserver:
      - path: /srv/web-*
        partition: "*"
`,
			wantErr: "must be a literal path",
		},
		{
			name: "duplicate partition base",
			yaml: `
sources:
  default:
    fileserver:
      - /data
      - path: /data
        partition: "*"
`,
			wantErr: "partition base",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, baseMedia+tc.yaml)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
