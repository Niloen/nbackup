package cli

import (
	"bufio"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
)

// swapWithStdin runs stdinOperator.Swap with injected stdin.
func swapWithStdin(t *testing.T, input string, req librarian.SwapRequest) (string, bool) {
	t.Helper()
	old := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(input))
	t.Cleanup(func() { stdinReader = old })
	return stdinOperator{}.Swap(req)
}

// TestSwapPromptRealDrive: on a REAL single drive the shelf is empty by
// construction (no addressable slots), so the prompt must ask for a physical
// insert and proceed on a bare Enter — never abort with "no reels in the room".
// Regression for the mhvtl road-test finding that made multi-tape restores
// impossible on real hardware.
func TestSwapPromptRealDrive(t *testing.T) {
	req := librarian.SwapRequest{Medium: "lto", Reason: "need volume \"T2\"", Need: "T2"}

	reel, ok := swapWithStdin(t, "\n", req) // operator inserted the tape, pressed Enter
	if !ok || reel != "" {
		t.Fatalf("bare Enter after a physical swap should proceed unnamed, got (%q, %v)", reel, ok)
	}
	if _, ok := swapWithStdin(t, "", req); ok { // EOF (Ctrl-D): abort
		t.Fatal("EOF must abort the swap")
	}
	if _, ok := swapWithStdin(t, "something\n", req); ok { // typed input: nothing to choose — abort
		t.Fatal("unexpected input must abort rather than guess")
	}
}

// TestReelDescNotEmptyForForeignOrBlank: a foreign (unparseable) or blank reel
// carries no id and no label, but it IS in the drive — the prompt must say so
// rather than claim "(empty)" (found testing by hand: a dd-zeroed tape showed
// 'in drive: (empty)' while the reason complained about its corrupt data).
func TestReelDescNotEmptyForForeignOrBlank(t *testing.T) {
	if got := reelDesc(media.VolumeStatus{Foreign: true}, "lto"); got != "(foreign)" {
		t.Fatalf("foreign reel rendered %q, want (foreign)", got)
	}
	if got := reelDesc(media.VolumeStatus{Blank: true}, "lto"); got != "(blank)" {
		t.Fatalf("blank reel rendered %q, want (blank)", got)
	}
	if got := reelDesc(media.VolumeStatus{}, "lto"); got != "(empty)" {
		t.Fatalf("a truly empty drive rendered %q, want (empty)", got)
	}
}
