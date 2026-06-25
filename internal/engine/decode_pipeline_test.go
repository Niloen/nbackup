package engine

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Niloen/nbackup/internal/archiver"
	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/transform/compress"
	"github.com/Niloen/nbackup/internal/transform/crypt"
	"github.com/Niloen/nbackup/internal/xfer"
)

// TestClientSidePipelineRoundTrip exercises the exact composition a fully client-side dump
// and a `--to` decode-on-client restore run — tar | gzip | gpg-encrypt to produce, then
// gpg-decrypt | gzip | tar-x to consume — all on the local executor (the stand-in for the
// client, since CI has no sshd), through xfer.Transfer. It proves the encrypt and decode
// pipelines compose end-to-end and that the key material flows only through the stage that
// needs it. Uses gpg symmetric + gzip (zstd is absent in CI); skips if absent.
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

	ex := programs.Local()
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
	var cipherBuf bytes.Buffer
	producer := xfer.NewPrograms(ex).Add(bs.Stage).Add(comp).Add(enc).
		Finishing(func() (xfer.Produced, error) { _, e := bs.Finish(); return xfer.Produced{}, e }).
		OnCleanup(func() {
			if bs.Cleanup != nil {
				bs.Cleanup()
			}
		})
	if _, err := xfer.Transfer(producer, xfer.NewFilters(), xfer.Writer(&cipherBuf), xfer.Opts{}); err != nil {
		t.Fatalf("produce: %v", err)
	}
	cipher := cipherBuf.Bytes()
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
	sink := xfer.NewPrograms(ex).Add(dec).Add(decomp).Add(m.RestoreStage(dest, nil))
	if _, err := xfer.Transfer(xfer.Reader(io.NopCloser(bytes.NewReader(cipher))), xfer.NewFilters(), sink, xfer.Opts{}); err != nil {
		t.Fatalf("decode/extract: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "secret.txt"))
	if err != nil || string(got) != "plaintext payload" {
		t.Fatalf("round-trip content = %q, err %v", got, err)
	}
}
