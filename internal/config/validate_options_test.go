package config

import (
	"strings"
	"testing"

	// Register the gnutar archiver so its known option keys are available to
	// validateArchivers (the engine blank-imports it in real runs).
	_ "github.com/Niloen/nbackup/internal/archiver/gnutar"
)

func TestValidateArchivers(t *testing.T) {
	cases := []struct {
		name     string
		archiver Archiver
		wantErr  string
	}{
		{
			name:     "valid options",
			archiver: Archiver{Type: "gnutar", Options: map[string]string{"one-file-system": "true", "sparse": "no", "tar_path": "/usr/bin/tar"}},
		},
		{
			name:     "typo'd option rejected",
			archiver: Archiver{Type: "gnutar", Options: map[string]string{"one-file-sytem": "true"}},
			wantErr:  `archivers.default: unknown option "one-file-sytem" (accepted: one-file-system, sparse, tar_path)`,
		},
		{
			name:     "made-up option rejected",
			archiver: Archiver{Type: "gnutar", Options: map[string]string{"totaly-made-up": "yes"}},
			wantErr:  `unknown option "totaly-made-up"`,
		},
		{
			name:     "type defaults to definition name",
			archiver: Archiver{Options: map[string]string{"sparse": "true"}},
		},
		{
			name:     "unregistered type is skipped",
			archiver: Archiver{Type: "made-up-archiver", Options: map[string]string{"anything": "goes"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Key the definition under "gnutar" so an empty Type defaults to a
			// registered type, and under "default" otherwise for the error wording.
			key := "default"
			if tc.archiver.Type == "" {
				key = "gnutar"
			}
			c := &Config{Archivers: map[string]Archiver{key: tc.archiver}}
			err := c.validateArchivers()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateArchivers: unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateArchivers: got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateMinimumAge(t *testing.T) {
	base := func(minAge string) *Config {
		return &Config{
			Sources: []DLE{{Host: "app01", Path: "/home"}},
			Media:   map[string]Media{"disk": {Type: "disk", MinimumAge: minAge}},
		}
	}
	cases := []struct {
		name    string
		minAge  string
		wantErr string
	}{
		{name: "omitted defaults to one cycle", minAge: ""},
		{name: "positive value accepted", minAge: "14d"},
		{name: "zero accepted (no age floor, capacity-only retention)", minAge: "0d"},
		{name: "negative rejected by parser", minAge: "-3d", wantErr: `minimum_age: invalid duration`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.minAge).Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate: got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
