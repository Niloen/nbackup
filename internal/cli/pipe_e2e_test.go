package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePipeConfig writes a real config whose one dumptype uses the pipe archiver:
// the producer cats a source file, the consumer writes stdin to {dest}. It returns
// the config path, the source file, and the base dir.
func writePipeConfig(t *testing.T) (cfgPath, src, base string) {
	t.Helper()
	base = t.TempDir()
	src = filepath.Join(base, "app.db")
	if err := os.WriteFile(src, []byte("pipe payload bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(base, "nbackup.yaml")
	cfg := fmt.Sprintf(`
landing: disk
workdir: %s
state_dir: %s
compress:
  scheme: none
media:
  disk: { type: disk, path: %s }
archivers:
  filedump:
    type: pipe
    backup_command: "cat {source}"
    restore_command: "cat > {dest}"
    estimate_command: "wc -c < {source}"
    extension: .db
dumptypes:
  pipedump:
    archiver: filedump
sources:
  pipedump:
    localhost: [%s]
`, filepath.Join(base, "catalog"), filepath.Join(base, "state"),
		filepath.Join(base, "runs"), src)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, src, base
}

// TestPipeArchiverEndToEnd drives the pipe archiver through the real CLI:
// check (source probe is the archiver's, not `test -r` semantics), dump (full,
// opaque stream, metered size), verify --deep (checksum + the degraded structural
// proof — a clean decode drain, no member diff), recover --all (the consumer
// command interprets --dest; no directory guard), and a second dump (still a full
// — pipe never reports a base). The payload's on-medium name must carry the
// archiver's extension, not ".tar".
func TestPipeArchiverEndToEnd(t *testing.T) {
	cfgPath, src, base := writePipeConfig(t)

	if _, err := runCmd(t, "-c", cfgPath, "check"); err != nil {
		t.Fatalf("check: %v", err)
	}
	if _, err := runCmd(t, "-c", cfgPath, "dump"); err != nil {
		t.Fatalf("dump: %v", err)
	}

	// The payload lands under the archiver's extension (.db), never .tar.
	var payload string
	err := filepath.WalkDir(filepath.Join(base, "runs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".db") {
			payload = p
		}
		return err
	})
	if err != nil || payload == "" {
		t.Fatalf("no .db payload landed under runs/ (err=%v)", err)
	}

	out, err := runCmd(t, "-c", cfgPath, "verify", "--deep")
	if err != nil {
		t.Fatalf("verify --deep: %v\n%s", err, out)
	}

	// A named-DLE restore hands --dest to the consumer command verbatim ({dest}
	// is a file path here — no directory guard, no MkdirAll on it).
	dest := filepath.Join(base, "restored.db")
	if _, err := runCmd(t, "-c", cfgPath, "recover", "--all", "--dle", "localhost:"+src, "--dest", dest); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "pipe payload bytes" {
		t.Fatalf("restored = %q", got)
	}

	// A second dump is another full: pipe keeps no incremental state, so the
	// planner must never schedule an incremental against it.
	out, err = runCmd(t, "-c", cfgPath, "dump")
	if err != nil {
		t.Fatalf("second dump: %v\n%s", err, out)
	}
	if strings.Contains(out, "L1") {
		t.Fatalf("second pipe dump must stay a full:\n%s", out)
	}
}
