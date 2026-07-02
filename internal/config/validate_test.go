package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateWholeConfig feeds small real YAML strings through Load so the
// whole-config Validate path (config.go) is exercised end-to-end, not just its
// sub-validators. Each case adds one offending piece to an otherwise-valid config
// and asserts the load error names it.
func TestValidateWholeConfig(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "no sources",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
`,
			wantErr: "config has no sources",
		},
		{
			name: "empty path rejected",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
sources:
  default:
    localhost: [""]
`,
			wantErr: "host and path are required",
		},
		{
			name: "unknown dumptype",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
sources:
  weird:
    localhost: [/home]
`,
			wantErr: `unknown dumptype "weird"`,
		},
		{
			name: "landing not a defined medium",
			yaml: `
landing: ghost
media:
  disk: { type: disk, path: /tmp/x }
sources:
  default:
    localhost: [/home]
`,
			wantErr: `landing "ghost" is not a defined medium`,
		},
		{
			name: "cycle unparseable",
			yaml: `
cycle: notaduration
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
sources:
  default:
    localhost: [/home]
`,
			wantErr: "cycle:",
		},
		{
			name: "cycle non-positive",
			yaml: `
cycle: 0d
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
sources:
  default:
    localhost: [/home]
`,
			wantErr: "cycle must be positive",
		},
		{
			name: "media capacity unparseable",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x, capacity: hugely }
sources:
  default:
    localhost: [/home]
`,
			wantErr: "capacity:",
		},
		{
			name: "media throughput unparseable",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x, throughput: fastish }
sources:
  default:
    localhost: [/home]
`,
			wantErr: "throughput:",
		},
		{
			name: "dumptype names unknown archiver",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
dumptypes:
  db: { archiver: nope }
sources:
  default:
    localhost: [/home]
`,
			wantErr: `unknown archiver "nope"`,
		},
		{
			name: "dumptype landing not defined",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
dumptypes:
  db: { landing: ghost }
sources:
  default:
    localhost: [/home]
`,
			wantErr: `landing "ghost" is not a defined medium`,
		},
		{
			name: "dumptype landing is a holding medium",
			yaml: `
landing: disk
media:
  disk:    { type: disk, path: /tmp/x }
  scratch: { type: disk, path: /tmp/s, holding: true }
dumptypes:
  db: { landing: scratch }
sources:
  default:
    localhost: [/home]
`,
			wantErr: `landing "scratch" is a holding medium`,
		},
		{
			name: "sync rule missing to",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
sync:
  - last: 3
sources:
  default:
    localhost: [/home]
`,
			wantErr: "`to` is required",
		},
		{
			name: "sync target undefined",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
sync:
  - to: ghost
sources:
  default:
    localhost: [/home]
`,
			wantErr: `target "ghost" is not a defined medium`,
		},
		{
			name: "sync source equals target",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
  tape: { type: tape, dir: /tmp/t }
sync:
  - from: tape
    to: tape
sources:
  default:
    localhost: [/home]
`,
			wantErr: "source and target are the same medium",
		},
		{
			name: "drill unknown tier",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
drill:
  tier: bogus
sources:
  default:
    localhost: [/home]
`,
			wantErr: `unknown tier "bogus"`,
		},
		{
			name: "drill negative sample",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
drill:
  sample: -1
sources:
  default:
    localhost: [/home]
`,
			wantErr: "sample must not be negative",
		},
		{
			name: "drill window unparseable",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
drill:
  window: soon
sources:
  default:
    localhost: [/home]
`,
			wantErr: "window:",
		},
		{
			name: "drill source undefined",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
drill:
  from: ghost
sources:
  default:
    localhost: [/home]
`,
			wantErr: `source "ghost" is not a defined medium`,
		},
		{
			name: "transform placement invalid at",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
compress:
  scheme: none
  at: wherever
sources:
  default:
    localhost: [/home]
`,
			wantErr: `compress.at must be "server" or "client"`,
		},
		{
			name: "encrypt client without compress client",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
encrypt:
  scheme: gpg
  at: client
sources:
  default:
    app01: [/home]
`,
			wantErr: "encrypt.at: client requires compress.at: client",
		},
		{
			name: "transform client requires remote host",
			yaml: `
landing: disk
media:
  disk: { type: disk, path: /tmp/x }
compress:
  scheme: none
  at: client
sources:
  default:
    localhost: [/home]
`,
			wantErr: "requires a remote host",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load: got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateAcceptsFullConfig proves the branches above reject only what is
// wrong: a config touching every validated block loads clean.
func TestValidateAcceptsFullConfig(t *testing.T) {
	_, err := loadYAML(t, `
cycle: 14d
landing: disk
media:
  disk: { type: disk, path: /tmp/x, capacity: 500GB, throughput: 50MB/s, writers: 2 }
  tape: { type: tape, dir: /tmp/t }
dumptypes:
  db: { archiver: gnutar, landing: tape }
sync:
  - from: disk
    to: tape
    last: 4
drill:
  tier: structural
  sample: 1
  window: 30d
  from: disk
sources:
  default:
    localhost: [/home]
  db:
    localhost: [/var/lib/db]
`)
	if err != nil {
		t.Fatalf("valid full config must load, got %v", err)
	}
}

// TestLoadRejectsMalformedYAML covers the Load-level error handling: a syntax
// error, an unknown top-level key (cleanYAMLError), a secret-looking key (the
// env-ref hint), a missing file, and an empty file.
func TestLoadRejectsMalformedYAML(t *testing.T) {
	t.Run("syntax error", func(t *testing.T) {
		_, err := loadYAML(t, "sources: [unterminated\n")
		if err == nil || !strings.Contains(err.Error(), "parse config") {
			t.Fatalf("got %v, want a parse error", err)
		}
	})
	t.Run("unknown top-level key", func(t *testing.T) {
		_, err := loadYAML(t, "cyle: 7d\n")
		if err == nil || !strings.Contains(err.Error(), `unknown key "cyle"`) {
			t.Fatalf("got %v, want unknown-key error", err)
		}
	})
	t.Run("secret key gets env-ref hint", func(t *testing.T) {
		_, err := loadYAML(t, "password: hunter2\n")
		if err == nil || !strings.Contains(err.Error(), "password_env") {
			t.Fatalf("got %v, want a hint toward password_env", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
		if err == nil || !strings.Contains(err.Error(), "no config file at") {
			t.Fatalf("got %v, want a missing-file error", err)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		_, err := loadYAML(t, "   \n")
		if err == nil || !strings.Contains(err.Error(), "is empty") {
			t.Fatalf("got %v, want an empty-file error", err)
		}
	})
}
