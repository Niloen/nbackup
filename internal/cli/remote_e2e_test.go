package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sshLoopbackAvailable reports whether `ssh -o BatchMode=yes localhost true`
// succeeds without a password/passphrase prompt — the guard for the real SSH
// tests below (skipped wherever passwordless localhost SSH isn't set up, which
// is every sandbox but CI once it provisions one).
func sshLoopbackAvailable(t *testing.T) bool {
	t.Helper()
	return exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "localhost", "true").Run() == nil
}

// writeRemoteConfig writes a real config for one DLE on host "127.0.0.1"
// (anything but the literal "localhost" is remote per Config.RemoteHost, and
// the host string is also the literal ssh target), backed up over a real SSH
// loopback connection, with compress/encrypt placed at server or client.
func writeRemoteConfig(t *testing.T, at string) (cfgPath, base, content string) {
	t.Helper()
	base = t.TempDir()
	src := filepath.Join(base, "data")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	content = "remote payload, compressed and possibly encrypted before it leaves the client"
	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte(strings.Repeat(content, 50)), 0o644); err != nil {
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
  scheme: gzip
  at: %s
encrypt:
  scheme: gpg
  passphrase_file: %s
  at: %s
media:
  disk: { type: disk, path: %s }
hosts:
  "127.0.0.1":
    ssh:
      options: ["-o", "BatchMode=yes", "-o", "ConnectTimeout=3"]
sources:
  default:
    "127.0.0.1": [%s]
`, filepath.Join(base, "catalog"), filepath.Join(base, "state"), at, pass, at,
		filepath.Join(base, "runs"), src)
	cfgPath = filepath.Join(base, "nbackup.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, base, content
}

// TestRemoteTransformPlacementEndToEnd drives the real CLI over an actual SSH
// loopback connection ("127.0.0.1" dials localhost's own sshd, exactly as
// programs_test.go's SSH loopback tests do), for both transform
// placements: server (compress/encrypt run on the NBackup host, after the
// plaintext crosses the wire) and client (compress/encrypt run on the source
// client via the SSH pipeline, so only ciphertext crosses the wire — the
// posture ARCHITECTURE.md documents). Both must dump, verify --deep, and
// recover the exact original bytes; "at" only changes where the transform
// runs, never the artifact it produces.
func TestRemoteTransformPlacementEndToEnd(t *testing.T) {
	if !sshLoopbackAvailable(t) {
		t.Skip("passwordless SSH to localhost not available")
	}
	if err := exec.Command("gpg", "--version").Run(); err != nil {
		t.Skipf("gpg unavailable: %v", err)
	}

	for _, at := range []string{"server", "client"} {
		t.Run(at, func(t *testing.T) {
			cfgPath, base, content := writeRemoteConfig(t, at)

			if out, err := runCmd(t, "-c", cfgPath, "check"); err != nil {
				t.Fatalf("check: %v\n%s", err, out)
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
				t.Fatalf("on-medium archive %s still contains the plaintext regardless of at:%s", archive, at)
			}

			if out, err := runCmd(t, "-c", cfgPath, "verify", "--deep"); err != nil {
				t.Fatalf("verify --deep: %v\n%s", err, out)
			}

			dest := filepath.Join(base, "restored-"+at)
			// A client-placed symmetric passphrase never leaves the client, so a
			// server-side restore can't decrypt it — recovery must run on the client
			// (`--to host:path`), where the key lives. Server-placed encryption
			// decrypts locally, so a plain `--dest` restore suffices.
			recoverArgs := []string{"-c", cfgPath, "recover", "--all",
				"--dle", "127.0.0.1:" + filepath.Join(base, "data")}
			if at == "client" {
				recoverArgs = append(recoverArgs, "--to", "127.0.0.1:"+dest)
			} else {
				recoverArgs = append(recoverArgs, "--dest", dest)
			}
			if out, err := runCmd(t, recoverArgs...); err != nil {
				t.Fatalf("recover: %v\n%s", err, out)
			}
			got, err := os.ReadFile(filepath.Join(dest, "file.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != strings.Repeat(content, 50) {
				t.Fatalf("restored content mismatch for at:%s", at)
			}
		})
	}
}
