package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeTapeConfig writes a real config landing on a directory-emulated tape
// (auto-labeling a blank bay on first write) with a small volume_size, so a
// large-enough source splits across several parts within that one bay. When
// encrypt is true, part_size is set to fit inside volume_size — the atom
// bound an encrypted archive's whole parts must clear (an unset default
// bound is far bigger than this tiny tape and every bay fails to place it,
// per the gap this test exists to cover). It writes one large incompressible
// file (so plain framing/atom packing actually cuts more than once) and one
// small file (the selective-recovery target).
func writeTapeConfig(t *testing.T, encrypt bool) (cfgPath, base, src, bigContent, smallContent string) {
	t.Helper()
	base = t.TempDir()
	src = filepath.Join(base, "data")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	bigContent = "0123456789abcdefghijklmnopqrstuvwxyz"
	if err := os.WriteFile(filepath.Join(src, "big.bin"), []byte(strings.Repeat(bigContent, 8000)), 0o644); err != nil { // ~288 KiB
		t.Fatal(err)
	}
	smallContent = "the needle in the haystack"
	if err := os.WriteFile(filepath.Join(src, "small.txt"), []byte(smallContent), 0o644); err != nil {
		t.Fatal(err)
	}

	encryptBlock := ""
	partSize := ""
	if encrypt {
		pass := filepath.Join(base, "passphrase")
		if err := os.WriteFile(pass, []byte("correct horse battery staple\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		encryptBlock = fmt.Sprintf("encrypt:\n  scheme: gpg\n  passphrase_file: %s\n", pass)
		// An atomic (encrypted) part is placed whole against part_size — it must fit
		// inside the tape's volume_size, or every bay fails to place it (unset, the
		// default bound is far larger than this tiny tape).
		partSize = "part_size: 65536\n"
	}

	cfgPath = filepath.Join(base, "nbackup.yaml")
	cfg := fmt.Sprintf(`
landing: vtape
workdir: %s
state_dir: %s
auto_label: true
%scompress:
  scheme: none
%smedia:
  vtape: { type: tape, dir: %s, slots: 1, volume_size: 4194304 }
sources:
  default:
    localhost: [%s]
`, filepath.Join(base, "catalog"), filepath.Join(base, "state"), partSize, encryptBlock,
		filepath.Join(base, "vtape"), src)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, base, src, bigContent, smallContent
}

// TestTapeSplitAndSelectiveRecoverEndToEnd drives the real CLI against a
// directory-emulated tape whose small volume_size forces the archive to
// split into several parts (FRAMED-INVISIBLE, no encryption): dump, verify
// --deep (per-part structural check), a selective `recover --path` of the
// small file only (tape can't serve ranges, so this must correctly fall back
// to a whole-archive read rather than fail), and a whole-DLE `recover --all`.
func TestTapeSplitAndSelectiveRecoverEndToEnd(t *testing.T) {
	cfgPath, base, src, _, smallContent := writeTapeConfig(t, false)

	if _, err := runCmd(t, "-c", cfgPath, "load", "vtape", "1"); err != nil {
		t.Fatalf("load: %v", err)
	}
	if out, err := runCmd(t, "-c", cfgPath, "dump"); err != nil {
		t.Fatalf("dump: %v\n%s", err, out)
	}

	parts := countPartFiles(t, filepath.Join(base, "vtape"))
	if parts < 2 {
		t.Fatalf("archive landed as %d file(s) on tape, want a multi-part split", parts)
	}

	if out, err := runCmd(t, "-c", cfgPath, "verify", "--deep"); err != nil {
		t.Fatalf("verify --deep: %v\n%s", err, out)
	}

	// Tape is a streaming device: internal/media/tape/tape.go deliberately refuses
	// a sub-range read (ReadFile: "only the whole payload can be served"), so even a
	// multi-part split archive falls back to a whole-archive read here — the correct,
	// documented behavior for this medium, not a ranged read (that path is exercised
	// on disk in TestSelectiveRecoverRangedReadEndToEnd).
	selDest := filepath.Join(base, "selective")
	out, err := runCmd(t, "-c", cfgPath, "recover", "--dle", "localhost:"+src,
		"--path", "small.txt", "--dest", selDest, "--yes")
	if err != nil {
		t.Fatalf("selective recover: %v\n%s", err, out)
	}
	if !strings.Contains(out, "reading the whole") {
		t.Fatalf("tape cannot serve ranged reads, so selective recover should fall back to a whole-archive read:\n%s", out)
	}
	got, err := os.ReadFile(filepath.Join(selDest, "small.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != smallContent {
		t.Fatalf("selectively recovered content mismatch: %q", got)
	}

	dest := filepath.Join(base, "restored")
	if out, err := runCmd(t, "-c", cfgPath, "recover", "--all", "--dle", "localhost:"+src, "--dest", dest); err != nil {
		t.Fatalf("recover --all: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "small.txt")); err != nil || string(got) != smallContent {
		t.Fatalf("whole-DLE restore of small.txt: %v, got %q", err, got)
	}
}

// TestTapeEncryptedSplitEndToEnd drives the real CLI through the FRAMED-ATOMIC
// path (real gpg) against a directory-emulated tape sized so the encrypted
// archive splits into several atoms within the one bay: dump, verify --deep
// (per-atom decode), and recover --all. This is the combination that used to
// fail outright — an unset atom bound (part_size) can't fit any bay on a
// small tape, and the failure surfaced as a misleading "gpg: broken pipe"
// instead of the real "no further writable bay" cause (a case-sensitivity
// bug in isBrokenPipe, fixed alongside this test).
func TestTapeEncryptedSplitEndToEnd(t *testing.T) {
	if err := exec.Command("gpg", "--version").Run(); err != nil {
		t.Skipf("gpg unavailable: %v", err)
	}
	cfgPath, base, src, bigContent, smallContent := writeTapeConfig(t, true)

	if _, err := runCmd(t, "-c", cfgPath, "load", "vtape", "1"); err != nil {
		t.Fatalf("load: %v", err)
	}
	if out, err := runCmd(t, "-c", cfgPath, "dump"); err != nil {
		t.Fatalf("dump: %v\n%s", err, out)
	}

	parts := countPartFiles(t, filepath.Join(base, "vtape"))
	if parts < 2 {
		t.Fatalf("encrypted archive landed as %d atom(s) on tape, want a multi-atom split", parts)
	}
	raw, err := os.ReadFile(filepath.Join(base, "vtape", "slot-01", "000000"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), bigContent) {
		t.Fatal("on-tape atom still contains the plaintext — encryption did not run")
	}

	if out, err := runCmd(t, "-c", cfgPath, "verify", "--deep"); err != nil {
		t.Fatalf("verify --deep: %v\n%s", err, out)
	}

	dest := filepath.Join(base, "restored")
	if out, err := runCmd(t, "-c", cfgPath, "recover", "--all", "--dle", "localhost:"+src, "--dest", dest); err != nil {
		t.Fatalf("recover --all: %v\n%s", err, out)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "small.txt")); err != nil || string(got) != smallContent {
		t.Fatalf("restored small.txt: %v, got %q", err, got)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "big.bin")); err != nil || string(got) != strings.Repeat(bigContent, 8000) {
		t.Fatalf("restored big.bin mismatch: %v", err)
	}
}

// countPartFiles counts the numbered payload files the dir-backed tape wrote
// into its one occupied bay (slot-01/000000, 000001, …) — the emulated
// medium's on-disk part count, standing in for a real tape's file marks.
func countPartFiles(t *testing.T, tapeDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(tapeDir, "slot-01"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}
