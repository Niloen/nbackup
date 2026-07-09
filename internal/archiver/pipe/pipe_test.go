package pipe

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
)

func open(t *testing.T, opts archiver.Options) archiver.Archiver {
	t.Helper()
	m, err := archiver.Open("pipe", opts, programs.Local(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestBackupRestoreRoundtrip drives the producer stage and the consumer stage the
// way the dumper/restorer do (RunPipe over the executor) and checks the bytes
// survive — including a source path with a shell metacharacter, which must ride
// the {source}/{dest} substitution as one quoted word.
func TestBackupRestoreRoundtrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "it's data.bin")
	if err := os.WriteFile(src, []byte("payload bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := open(t, archiver.Options{
		"backup_command":  "cat {source}",
		"restore_command": "cat > {dest}",
	})
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}

	bs, err := m.BackupSource(archiver.BackupRequest{DLE: "app", Scope: archiver.Scope{Source: src}, Level: 0, BaseLevel: -1})
	if err != nil {
		t.Fatal(err)
	}
	out, wait, err := bs.Exec.RunPipe(context.Background(), nil, bs.Stage)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	out.Close()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if string(stream) != "payload bytes" {
		t.Fatalf("backup stream = %q", stream)
	}
	res, err := bs.Finish()
	if err != nil {
		t.Fatal(err)
	}
	// An opaque stream reports no members, count, or size (the caller meters it).
	if res.Uncompressed != 0 || res.FileCount != 0 || res.Members != nil {
		t.Fatalf("opaque result should be empty, got %+v", res)
	}
	if bs.Promote != nil {
		t.Fatal("pipe has no incremental state to promote")
	}

	dest := filepath.Join(t.TempDir(), "restored's.bin")
	rout, rwait, err := programs.Local().RunPipe(context.Background(), strings.NewReader(string(stream)), m.RestoreStage(dest, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, rout); err != nil {
		t.Fatal(err)
	}
	rout.Close()
	if err := rwait(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload bytes" {
		t.Fatalf("restored = %q", got)
	}
}

// TestFullOnly pins the capability posture: no base ever, an incremental request
// and exclude patterns are loud errors, and the opaque-stream capabilities are all
// withdrawn (no list, no splice, no directory destination, no stock recipe drift).
func TestFullOnly(t *testing.T) {
	m := open(t, archiver.Options{"backup_command": "true", "restore_command": "cat > {dest}"})
	if m.HasBase("app", 0) {
		t.Error("pipe must never report a base")
	}
	if _, err := m.BackupSource(archiver.BackupRequest{DLE: "app", Level: 1, BaseLevel: 0}); err == nil {
		t.Error("an incremental request must be rejected")
	}
	if _, err := m.BackupSource(archiver.BackupRequest{DLE: "app", Scope: archiver.Scope{Exclude: []string{"*.log"}}, Level: 0, BaseLevel: -1}); err == nil {
		t.Error("exclude patterns must be rejected")
	}
	if m.CanList() {
		t.Error("an opaque stream must not claim listing")
	}
	if _, err := m.List(strings.NewReader("x")); err == nil {
		t.Error("List must refuse")
	}
	if m.SpliceTrailer() != nil {
		t.Error("an opaque stream must not claim splicing")
	}
	if m.DestIsDir() {
		t.Error("the destination is the consumer command's, not a directory the generic layer owns")
	}
	if err := m.CheckSource("anything"); err != nil {
		t.Errorf("CheckSource has nothing to probe: %v", err)
	}
}

// TestOptions covers factory validation and the extension normalization.
func TestOptions(t *testing.T) {
	if _, err := archiver.Open("pipe", archiver.Options{"restore_command": "cat"}, programs.Local(), ""); err == nil || !strings.Contains(err.Error(), "backup_command") {
		t.Errorf("missing backup_command should error, got %v", err)
	}
	if _, err := archiver.Open("pipe", archiver.Options{"backup_command": "true"}, programs.Local(), ""); err == nil || !strings.Contains(err.Error(), "restore_command") {
		t.Errorf("missing restore_command should error, got %v", err)
	}
	base := archiver.Options{"backup_command": "true", "restore_command": "cat"}
	if got := open(t, base).Ext(); got != ".raw" {
		t.Errorf("default extension = %q, want .raw", got)
	}
	withExt := archiver.Options{"backup_command": "true", "restore_command": "cat", "extension": "sqlite"}
	if got := open(t, withExt).Ext(); got != ".sqlite" {
		t.Errorf("extension should normalize to a leading dot, got %q", got)
	}
}

// TestEstimate covers the optional estimate_command: absent = 0 (unknown, not an
// error), present = its stdout parsed as a byte count, garbage = a loud error.
func TestEstimate(t *testing.T) {
	req := archiver.BackupRequest{DLE: "app", Scope: archiver.Scope{Source: "src"}, Level: 0, BaseLevel: -1}
	m := open(t, archiver.Options{"backup_command": "true", "restore_command": "cat"})
	if n, err := m.Estimate(req); n != 0 || err != nil {
		t.Errorf("no estimate_command should report 0, got %d %v", n, err)
	}
	m = open(t, archiver.Options{"backup_command": "true", "restore_command": "cat", "estimate_command": "echo 4096"})
	if n, err := m.Estimate(req); n != 4096 || err != nil {
		t.Errorf("estimate = %d %v, want 4096", n, err)
	}
	m = open(t, archiver.Options{"backup_command": "true", "restore_command": "cat", "estimate_command": "echo not-a-number"})
	if _, err := m.Estimate(req); err == nil {
		t.Error("a non-numeric estimate must error")
	}
}

// TestStockExtract pins the stock tier's tail: the consumer command with the
// drill's destination riding in as the script's "$1".
func TestStockExtract(t *testing.T) {
	m := open(t, archiver.Options{"backup_command": "true", "restore_command": "sqlite3 {dest}"})
	if got := m.StockExtract(); got != `sqlite3 "$1"` {
		t.Errorf("StockExtract = %q", got)
	}
}
