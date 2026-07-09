package engine

import (
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/config"
)

// TestRestoreArchiverPrefersRecordedName pins the restore identity chain: an archive
// records its archiver TYPE (how to reverse the stream) and the config DEFINITION NAME
// it was resolved from (an inert lookup key — options are never stored in artifacts).
// Restore resolves load-bearing options from the CURRENT config by that name, so a DLE
// absent from config — a partition child, a removed source — restores with the right
// options; a missing/retyped definition falls back to the bare type, whose factory
// errors naming what it needs.
func TestRestoreArchiverPrefersRecordedName(t *testing.T) {
	cfg := &config.Config{
		Archivers: map[string]config.Archiver{
			"mypipe": {Type: "pipe", Options: map[string]string{
				"backup_command":  "cat {source}",
				"restore_command": "cat > {dest}",
			}},
			"plainfiles": {Type: "gnutar"},
		},
		Workdir:  t.TempDir(),
		StateDir: t.TempDir(),
	}
	tc := newToolchain(cfg)

	// Recorded name resolves the definition's options even though NO configured DLE
	// references it (the partition-child / removed-source case): pipe opens fine
	// because restore_command came from the named definition.
	if _, err := tc.restoreArchiver("pipe", "mypipe", "gone-dle", ""); err != nil {
		t.Fatalf("recorded name should resolve the definition's options: %v", err)
	}

	// No recorded name (a pre-name artifact) and no configured DLE: the bare-type
	// fallback — pipe's factory then errors, naming its load-bearing option.
	if _, err := tc.restoreArchiver("pipe", "", "gone-dle", ""); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("bare-type fallback should error naming the missing command, got: %v", err)
	}

	// A recorded name whose definition has since been RETYPED must not silently apply
	// mismatched options — it falls through to the fallback chain instead.
	if _, err := tc.restoreArchiver("pipe", "plainfiles", "gone-dle", ""); err == nil || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("a retyped definition must not be used; want the bare-type error, got: %v", err)
	}
}
