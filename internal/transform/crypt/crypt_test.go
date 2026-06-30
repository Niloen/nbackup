package crypt

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/programs"
)

// runFilter runs one filter command over src through the live programs path — the same
// RunPipe the engine drives — returning its output. An empty command (the none
// identity) passes src through unchanged.
func runFilter(t *testing.T, cmd programs.Cmd, src []byte) []byte {
	t.Helper()
	if cmd.Name == "" {
		return src
	}
	out, wait, err := programs.Local().RunPipe(context.Background(), bytes.NewReader(src), cmd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	return got
}

// register a deterministic, non-identity scheme backed by gzip so the
// encrypt->decrypt plumbing is exercised without a real cipher in CI (the test
// env has no gpg). It stands in for a stream cipher: reversible, and its output
// differs from its input.
func init() {
	register(Spec{
		Name:        "gztest",
		encryptArgv: func(o Options) []string { return []string{"gzip", "-c"} },
		decryptArgv: func(o Options) []string { return []string{"gzip", "-dc"} },
	})
}

// TestRoundTrip checks Forward (encrypt) -> Reverse (decrypt) reproduces the input for
// every scheme whose binary is available (none always is; gztest needs gzip), driving the
// same Filter the engine uses.
func TestRoundTrip(t *testing.T) {
	payload := []byte(strings.Repeat("secrets and lies\n", 4000))
	for _, scheme := range []string{"none", "", "gztest"} {
		scheme := scheme
		t.Run("scheme="+scheme, func(t *testing.T) {
			if err := Check(scheme, Options{}); err != nil {
				t.Skipf("scheme unavailable: %v", err)
			}
			f, err := Filter(scheme, Options{})
			if err != nil {
				t.Fatal(err)
			}
			ciphertext := runFilter(t, f.Forward, payload)
			got := runFilter(t, f.Reverse, ciphertext)
			if !bytes.Equal(got, payload) {
				t.Errorf("round trip mismatch: got %d bytes, want %d", len(got), len(payload))
			}
			// A real transform must not leave the payload in the clear.
			if scheme == "gztest" && bytes.Contains(ciphertext, []byte("secrets")) {
				t.Error("ciphertext still contains the plaintext")
			}
		})
	}
}

// TestGPGRoundTrip exercises the real gpg symmetric path when gpg is installed.
func TestGPGRoundTrip(t *testing.T) {
	pass := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(pass, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := Options{PassphraseFile: pass}
	if err := Check("gpg", o); err != nil {
		t.Skipf("gpg unavailable: %v", err)
	}
	f, err := Filter("gpg", o)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("the launch codes are 0000")
	ciphertext := runFilter(t, f.Forward, payload)
	if bytes.Contains(ciphertext, []byte("launch codes")) {
		t.Fatal("ciphertext still contains the plaintext")
	}
	got := runFilter(t, f.Reverse, ciphertext)
	if !bytes.Equal(got, payload) {
		t.Errorf("gpg round trip mismatch: got %q", got)
	}
}

// TestCheck rejects unknown schemes and gpg without a key reference.
func TestCheck(t *testing.T) {
	if err := Check("aes-rot13", Options{}); err == nil {
		t.Error("expected an error for an unknown scheme")
	}
	if err := Check("gpg", Options{}); err == nil {
		t.Error("expected an error for gpg with no recipient or passphrase file")
	}
}

// TestCheckUnreadablePassphraseFile fails check on a mode-000 passphrase file:
// os.Stat would pass it (it only needs +x on the parent dir), so the dump would
// fail mid-run with gpg "Permission denied". Check must catch it up front by
// trying to open the file the way gpg will.
func TestCheckUnreadablePassphraseFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files, so the unreadable case can't be reproduced")
	}
	pass := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(pass, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pass, 0o000); err != nil {
		t.Fatal(err)
	}
	err := Check("gpg", Options{PassphraseFile: pass})
	if err == nil {
		t.Fatal("expected an error for an unreadable passphrase file")
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("error should name the file as unreadable, got: %v", err)
	}
}

// TestGPGDecryptLoopback locks in that symmetric decrypt runs gpg with
// --pinentry-mode loopback (matching encrypt). Without it, a tty-less restore
// emits "problem with the agent: Inappropriate ioctl" and the misleading
// "decryption failed: No secret key". The passphrase must travel via
// --passphrase-file, never as an argv literal.
func TestGPGDecryptLoopback(t *testing.T) {
	pass := filepath.Join(t.TempDir(), "pass")
	o := Options{PassphraseFile: pass}
	cmd, ok, err := DecryptCmd("gpg", o)
	if err != nil || !ok {
		t.Fatalf("DecryptCmd: ok=%v err=%v", ok, err)
	}
	argv := append([]string{cmd.Name}, cmd.Args...)
	if !containsPair(argv, "--pinentry-mode", "loopback") {
		t.Errorf("decrypt argv missing --pinentry-mode loopback: %v", argv)
	}
	if !containsPair(argv, "--passphrase-file", pass) {
		t.Errorf("decrypt argv must pass the passphrase via --passphrase-file: %v", argv)
	}
	for _, a := range argv {
		if a == "correct horse battery staple" {
			t.Error("passphrase must never appear as an argv literal")
		}
	}
}

// containsPair reports whether argv has want immediately followed by next.
func containsPair(argv []string, want, next string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == want && argv[i+1] == next {
			return true
		}
	}
	return false
}
