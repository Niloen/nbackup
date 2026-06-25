package engine

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/hostexec"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
)

// runStages runs the stages through RunGrouped and returns the final stdout bytes.
func runStages(t *testing.T, stdin io.Reader, stages ...hostexec.Stage) ([]byte, error) {
	t.Helper()
	out, wait, err := hostexec.RunGrouped(stdin, stages...)
	if err != nil {
		return nil, err
	}
	data, rerr := io.ReadAll(out)
	out.Close()
	if werr := wait(); werr != nil {
		return data, werr
	}
	return data, rerr
}

// TestClientSidePipelineRoundTrip exercises the exact stage composition a fully
// client-side dump and a `--to` decode-on-client restore run — tar | gzip | gpg-encrypt to
// produce, then gpg-decrypt | gzip | tar-x to consume — all through hostexec on the local
// executor (the stand-in for the client, since CI has no sshd). It proves the encrypt and
// decode pipelines compose end-to-end and that the key material flows only through the
// stage that needs it. Uses gpg symmetric + gzip (zstd is absent in CI); skips if absent.
func TestClientSidePipelineRoundTrip(t *testing.T) {
	pass := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(pass, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eopts := crypt.Options{PassphraseFile: pass}
	if err := crypt.Check("gpg", eopts); err != nil {
		t.Skipf("gpg unavailable: %v", err)
	}
	if err := compress.Check("gzip", compress.Options{}); err != nil {
		t.Skipf("gzip unavailable: %v", err)
	}

	ex := hostexec.Local()
	m, err := archiver.Open("gnutar", archiver.Options{"state_dir": t.TempDir()}, ex)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Check(); err != nil {
		t.Skipf("GNU tar unavailable: %v", err)
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "secret.txt"), []byte("plaintext payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Produce: tar | gzip | gpg -c  (the fully client-side dump pipeline).
	bs, err := m.BackupSource(archiver.BackupRequest{DLE: "app-data", SourcePath: src, Level: 0, BaseLevel: -1})
	if err != nil {
		t.Fatal(err)
	}
	comp, _, err := compress.CompressCmd("gzip", compress.Options{})
	if err != nil {
		t.Fatal(err)
	}
	enc, _, err := crypt.EncryptCmd("gpg", eopts)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := runStages(t, nil,
		hostexec.Stage{Cmd: bs.Stage, Exec: ex},
		hostexec.Stage{Cmd: comp, Exec: ex},
		hostexec.Stage{Cmd: enc, Exec: ex},
	)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	if _, err := bs.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if bs.Cleanup != nil {
		bs.Cleanup()
	}
	if bytes.Contains(cipher, []byte("plaintext payload")) {
		t.Fatal("ciphertext still contains the plaintext — encryption did not run")
	}

	// Consume: gpg -d | gzip -d | tar -x  (the decode-on-client restore pipeline).
	dec, _, err := crypt.DecryptCmd("gpg", eopts)
	if err != nil {
		t.Fatal(err)
	}
	decomp, _, err := compress.DecompressCmd("gzip", compress.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := runStages(t, bytes.NewReader(cipher),
		hostexec.Stage{Cmd: dec, Exec: ex},
		hostexec.Stage{Cmd: decomp, Exec: ex},
		hostexec.Stage{Cmd: m.RestoreStage(dest, nil), Exec: ex},
	); err != nil {
		t.Fatalf("decode/extract: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "secret.txt"))
	if err != nil || string(got) != "plaintext payload" {
		t.Fatalf("round-trip content = %q, err %v", got, err)
	}
}
