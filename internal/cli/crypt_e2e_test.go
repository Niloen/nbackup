package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/transform/compress"
)

// writeCryptConfig writes a real config for one local gnutar DLE with the given
// compress scheme and gpg symmetric encryption (a passphrase file — no keyring
// setup needed). It returns the config path, the source dir, its one file's
// content, and the base dir.
func writeCryptConfig(t *testing.T, scheme string) (cfgPath, base, content string) {
	t.Helper()
	base = t.TempDir()
	src := filepath.Join(base, "data")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	content = "the quick brown fox jumps over the lazy dog, repeatedly, so gzip/zstd has something to squeeze"
	if err := os.WriteFile(filepath.Join(src, "secret.txt"), []byte(strings.Repeat(content, 50)), 0o644); err != nil {
		t.Fatal(err)
	}
	pass := filepath.Join(base, "passphrase")
	if err := os.WriteFile(pass, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`
landing: disk
workdir: %s
state_dir: %s
compress:
  scheme: %s
encrypt:
  scheme: gpg
  passphrase_file: %s
media:
  disk: { type: disk, path: %s }
sources:
  default:
    localhost: [%s]
`, filepath.Join(base, "catalog"), filepath.Join(base, "state"), scheme, pass,
		filepath.Join(base, "runs"), src)
	cfgPath = filepath.Join(base, "nbackup.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, base, content
}

// TestEncryptedArchiveEndToEnd drives the real CLI through the full
// compress→gpg pipeline for every compiled compression scheme: dump (the
// on-medium bytes must be gpg ciphertext, never the plaintext, proving
// encryption actually ran and sits outermost per ARCHITECTURE.md), verify
// --deep (decrypt + decompress + structural check), and recover --all (the
// restored file must match byte-for-byte — proving the reverse pipeline
// decrypts and decompresses correctly).
func TestEncryptedArchiveEndToEnd(t *testing.T) {
	if err := exec.Command("gpg", "--version").Run(); err != nil {
		t.Skipf("gpg unavailable: %v", err)
	}
	for _, scheme := range []string{"none", "gzip", "zstd"} {
		t.Run(scheme, func(t *testing.T) {
			if scheme != "none" {
				if err := compress.Check(scheme, compress.Options{}); err != nil {
					t.Skipf("%s unavailable: %v", scheme, err)
				}
			}
			cfgPath, base, content := writeCryptConfig(t, scheme)

			if _, err := runCmd(t, "-c", cfgPath, "check"); err != nil {
				t.Fatalf("check: %v", err)
			}
			if out, err := runCmd(t, "-c", cfgPath, "dump"); err != nil {
				t.Fatalf("dump: %v\n%s", err, out)
			}

			archive := findArchivePayload(t, base)
			raw, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), content) {
				t.Fatalf("on-medium archive %s still contains the plaintext — encryption did not run", archive)
			}
			if out, err := runCmd(t, "-c", cfgPath, "verify", "--deep"); err != nil {
				t.Fatalf("verify --deep: %v\n%s", err, out)
			}

			dest := filepath.Join(base, "restored-"+scheme)
			if _, err := runCmd(t, "-c", cfgPath, "recover", "--all",
				"--dle", "localhost:"+filepath.Join(base, "data"), "--dest", dest); err != nil {
				t.Fatalf("recover: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dest, "secret.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != strings.Repeat(content, 50) {
				t.Fatal("restored file content mismatch")
			}
		})
	}
}

// findArchivePayload locates the one committed archive's raw payload file
// under runs/ — named "{seq}-{host}-{path}-L{n}.{ext}", distinct from its
// sibling index/commit/header sidecars.
func findArchivePayload(t *testing.T, base string) string {
	t.Helper()
	var archive string
	err := filepath.WalkDir(filepath.Join(base, "runs"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := filepath.Base(p)
		if strings.Contains(name, "-L") && !strings.HasSuffix(name, ".hdr") &&
			!strings.Contains(name, "-index") && !strings.Contains(name, "-commit") {
			archive = p
		}
		return nil
	})
	if err != nil || archive == "" {
		t.Fatalf("no archive payload found under runs/ (err=%v)", err)
	}
	return archive
}
