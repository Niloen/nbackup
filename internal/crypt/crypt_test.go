package crypt

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestRoundTrip checks encrypt -> decrypt reproduces the input for every scheme
// whose binary is available (none always is; gztest needs gzip).
func TestRoundTrip(t *testing.T) {
	payload := []byte(strings.Repeat("secrets and lies\n", 4000))
	for _, scheme := range []string{"none", "", "gztest"} {
		scheme := scheme
		t.Run("scheme="+scheme, func(t *testing.T) {
			if err := Check(scheme, Options{}); err != nil {
				t.Skipf("scheme unavailable: %v", err)
			}

			var ciphertext bytes.Buffer
			ew, err := Encrypt(scheme, &ciphertext, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ew.Write(payload); err != nil {
				t.Fatal(err)
			}
			if err := ew.Close(); err != nil {
				t.Fatal(err)
			}

			rc, err := Decrypt(scheme, bytes.NewReader(ciphertext.Bytes()), Options{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if err := rc.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("round trip mismatch: got %d bytes, want %d", len(got), len(payload))
			}
			// A real transform must not leave the payload in the clear.
			if scheme == "gztest" && bytes.Contains(ciphertext.Bytes(), []byte("secrets")) {
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
	payload := []byte("the launch codes are 0000")
	var ciphertext bytes.Buffer
	ew, err := Encrypt("gpg", &ciphertext, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ew.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := ew.Close(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext.Bytes(), []byte("launch codes")) {
		t.Fatal("ciphertext still contains the plaintext")
	}
	rc, err := Decrypt("gpg", bytes.NewReader(ciphertext.Bytes()), o)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
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
