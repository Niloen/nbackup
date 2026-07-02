package restorer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClearDirContentsMissingDirIsClean: a restore can fail before its
// destination was ever created (e.g. a spanned read aborts unattended); rolling
// back must not then complain "could not clean partial restore ... no such file
// or directory" about a directory that holds nothing.
func TestClearDirContentsMissingDirIsClean(t *testing.T) {
	if err := clearDirContents(filepath.Join(t.TempDir(), "never-created")); err != nil {
		t.Fatalf("missing dest is already clean, got: %v", err)
	}
}

// TestClearDirContentsRemovesEntries pins the normal rollback: contents go, the
// directory itself stays.
func TestClearDirContentsRemovesEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := clearDirContents(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dir not emptied: %v", entries)
	}
}

// TestDecryptHintDropsGpgAgentNoise: on a tty-less run gpg's agent prepends
// "problem with the agent: Inappropriate ioctl for device" to the real failure;
// the surfaced error must drop exactly that line, keep every other gpg line, and
// preserve the wrapped chain for errors.Is classification.
func TestDecryptHintDropsGpgAgentNoise(t *testing.T) {
	sentinel := errors.New("boom")
	base := fmt.Errorf("%w: gpg: exit status 2:\ngpg: problem with the agent: Inappropriate ioctl for device\ngpg: decryption failed: No secret key", sentinel)
	got := DecryptHint("gpg", base)
	msg := got.Error()
	if strings.Contains(msg, "Inappropriate ioctl for device") {
		t.Fatalf("agent noise not filtered:\n%s", msg)
	}
	if !strings.Contains(msg, "gpg: decryption failed: No secret key") {
		t.Fatalf("real gpg error was hidden:\n%s", msg)
	}
	if !strings.Contains(msg, "gpg-encrypted, so extraction needs the key") {
		t.Fatalf("hint missing:\n%s", msg)
	}
	if !errors.Is(got, sentinel) {
		t.Fatal("error chain broken by the noise filter")
	}
}

// TestDecryptHintKeepsCleanErrors: an error without the agent line passes
// through the hint wrapper untouched (no line surgery on real gpg output).
func TestDecryptHintKeepsCleanErrors(t *testing.T) {
	base := errors.New("gpg: decryption failed: Bad session key")
	got := DecryptHint("gpg", base)
	if !strings.Contains(got.Error(), "Bad session key") || !errors.Is(got, base) {
		t.Fatalf("clean error mangled: %v", got)
	}
}
