package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSelectiveRecoverRangedReadEndToEnd proves a real ranged read through the
// CLI: unlike tape (which refuses sub-range reads outright — see
// TestTapeSplitAndSelectiveRecoverEndToEnd), a disk medium's payload reads
// through the store with a byte range, so a small `frame_size` gives the
// FRAMED-INVISIBLE archive several decode-restart points and a selective
// `recover --path` of a file positioned after the first several frames should
// fetch only the frames covering it, not the whole archive.
func TestSelectiveRecoverRangedReadEndToEnd(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "data")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// big.bin sorts before small.txt, so several frame_size-bounded frames of
	// incompressible data precede the target member in stream order.
	big := make([]byte, 300*1024)
	x := uint32(12345)
	for i := range big {
		x = x*1664525 + 1013904223
		big[i] = byte(x >> 24)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	smallContent := "the needle in the haystack"
	if err := os.WriteFile(filepath.Join(src, "small.txt"), []byte(smallContent), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(base, "nbackup.yaml")
	cfg := fmt.Sprintf(`
landing: disk
workdir: %s
state_dir: %s
frame_size: 16KiB
compress:
  scheme: none
media:
  disk: { type: disk, path: %s }
sources:
  default:
    localhost: [%s]
`, filepath.Join(base, "catalog"), filepath.Join(base, "state"), filepath.Join(base, "runs"), src)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := runCmd(t, "-c", cfgPath, "dump"); err != nil {
		t.Fatalf("dump: %v\n%s", err, out)
	}

	selDest := filepath.Join(base, "selective")
	out, err := runCmd(t, "-c", cfgPath, "recover", "--dle", "localhost:"+src,
		"--path", "small.txt", "--dest", selDest, "--yes")
	if err != nil {
		t.Fatalf("selective recover: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ranged read: fetched") {
		t.Fatalf("selective recover of a late member with several frames ahead of it should report a ranged read:\n%s", out)
	}
	got, err := os.ReadFile(filepath.Join(selDest, "small.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != smallContent {
		t.Fatalf("selectively recovered content mismatch: %q", got)
	}
}
