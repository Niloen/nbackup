package cli

import (
	"bufio"
	"strings"
	"testing"

	"github.com/Niloen/nbackup/internal/engine"
	"github.com/Niloen/nbackup/internal/librarian"
	"github.com/Niloen/nbackup/internal/media"
)

// withStdin points the shared stdinReader at scripted input for one test and
// restores it after, so the prompt-reading code paths can be driven deterministically.
func withStdin(t *testing.T, input string) {
	t.Helper()
	orig := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(input))
	t.Cleanup(func() { stdinReader = orig })
}

// swapReq builds a write-side swap request (no Need) with a shelf of reels.
func swapReq(shelf ...media.VolumeStatus) librarian.SwapRequest {
	return librarian.SwapRequest{Medium: "lto", Reason: "drive empty", Shelf: shelf}
}

func TestStdinOperatorDefaultAcceptOnEnter(t *testing.T) {
	// A blank reel makes suggestReel offer a default; a bare Enter accepts it.
	withStdin(t, "\n")
	out := captureStdout(t, func() {
		id, ok := stdinOperator{}.Swap(swapReq(media.VolumeStatus{ID: "reel-01", Blank: true}))
		if !ok || id != "reel-01" {
			t.Fatalf("Enter should accept the suggested reel, got %q ok=%v", id, ok)
		}
	})
	if !strings.Contains(out, "Enter = reel-01") {
		t.Errorf("prompt should offer the default reel, got:\n%s", out)
	}
}

func TestStdinOperatorExplicitID(t *testing.T) {
	withStdin(t, "reel-02\n")
	captureStdout(t, func() {
		id, ok := stdinOperator{}.Swap(swapReq(
			media.VolumeStatus{ID: "reel-01", Blank: true},
			media.VolumeStatus{ID: "reel-02", Label: "DAILY-02", Pool: "lto"},
		))
		if !ok || id != "reel-02" {
			t.Fatalf("typed id should select that reel, got %q ok=%v", id, ok)
		}
	})
}

func TestStdinOperatorExplicitLabel(t *testing.T) {
	withStdin(t, "DAILY-02\n")
	captureStdout(t, func() {
		id, ok := stdinOperator{}.Swap(swapReq(
			media.VolumeStatus{ID: "reel-01", Blank: true},
			media.VolumeStatus{ID: "reel-02", Label: "DAILY-02", Pool: "lto"},
		))
		if !ok || id != "reel-02" {
			t.Fatalf("typed label should select its reel, got %q ok=%v", id, ok)
		}
	})
}

func TestStdinOperatorAbortNoDefault(t *testing.T) {
	// No blank reel and no needed label → no suggestion, so an empty line aborts.
	withStdin(t, "\n")
	out := captureStdout(t, func() {
		id, ok := stdinOperator{}.Swap(swapReq(
			media.VolumeStatus{ID: "reel-02", Label: "DAILY-02", Pool: "lto"},
		))
		if ok || id != "" {
			t.Fatalf("empty line with no default should abort, got %q ok=%v", id, ok)
		}
	})
	if !strings.Contains(out, "Enter aborts") {
		t.Errorf("prompt should say Enter aborts, got:\n%s", out)
	}
}

func TestStdinOperatorEOFAborts(t *testing.T) {
	// EOF (unattended stdin) aborts even with a default present.
	withStdin(t, "")
	captureStdout(t, func() {
		id, ok := stdinOperator{}.Swap(swapReq(media.VolumeStatus{ID: "reel-01", Blank: true}))
		if ok || id != "" {
			t.Fatalf("EOF should abort, got %q ok=%v", id, ok)
		}
	})
}

func TestStdinOperatorEmptyShelf(t *testing.T) {
	// An empty shelf is a REAL drive (no addressable slots): the prompt asks for
	// the physical swap and a bare Enter proceeds unnamed — the librarian
	// identifies the inserted reel by its label read. (It used to abort with "no
	// reels in the room", which made multi-reel work impossible on real hardware.)
	withStdin(t, "\n")
	out := captureStdout(t, func() {
		id, ok := stdinOperator{}.Swap(swapReq())
		if !ok || id != "" {
			t.Fatalf("Enter after a physical swap should proceed unnamed, got %q ok=%v", id, ok)
		}
	})
	if !strings.Contains(out, "insert") {
		t.Errorf("expected a physical-insert prompt, got:\n%s", out)
	}
	// EOF (unattended / Ctrl-D) still aborts.
	withStdin(t, "")
	captureStdout(t, func() {
		if _, ok := (stdinOperator{}).Swap(swapReq()); ok {
			t.Fatal("EOF must abort the physical-swap prompt")
		}
	})
}

func TestStdinOperatorUnknownChoice(t *testing.T) {
	withStdin(t, "nope\n")
	out := captureStdout(t, func() {
		id, ok := stdinOperator{}.Swap(swapReq(media.VolumeStatus{ID: "reel-01", Blank: true}))
		if ok || id != "" {
			t.Fatalf("an unknown choice should abort, got %q ok=%v", id, ok)
		}
	})
	if !strings.Contains(out, `no reel "nope" in the room`) {
		t.Errorf("expected unknown-reel message, got:\n%s", out)
	}
}

// suggestReel prefers the needed label (a read), then the expected tape, then the
// first blank — the three write/read cases the operator prompt seeds its default from.
func TestSuggestReel(t *testing.T) {
	shelf := []media.VolumeStatus{
		{ID: "b1", Blank: true},
		{ID: "want", Label: "DAILY-07", Pool: "lto"},
		{ID: "exp", Label: "DAILY-03", Pool: "lto"},
	}
	if got := suggestReel(librarian.SwapRequest{Need: "DAILY-07", Shelf: shelf}); got != "want" {
		t.Errorf("read: suggestReel = %q, want the needed-label reel", got)
	}
	if got := suggestReel(librarian.SwapRequest{Need: "MISSING", Shelf: shelf}); got != "" {
		t.Errorf("read for an absent label should suggest nothing, got %q", got)
	}
	if got := suggestReel(librarian.SwapRequest{Expect: "DAILY-03", Shelf: shelf}); got != "exp" {
		t.Errorf("write: suggestReel should offer the expected tape, got %q", got)
	}
	if got := suggestReel(librarian.SwapRequest{Shelf: shelf}); got != "b1" {
		t.Errorf("write with no expectation should offer the first blank, got %q", got)
	}
}

// confirmRead: an unpriced (local) medium or a zero-byte read always proceeds without
// a prompt; any priced non-zero read prompts when interactive regardless of the dollar
// amount (a cheap ranged read can still pull a large frame); --yes or a non-terminal
// stdin proceeds; and at an interactive prompt the typed answer decides.
func TestConfirmRead(t *testing.T) {
	material := engine.ReadEstimate{Priced: true, Bytes: 1 << 30, Cost: 5, Medium: "s3", Provider: "aws-s3"}
	// A priced read whose charge is a fraction of a cent: below any old dollar gate,
	// yet it must still prompt — the operator is confirming the pull, not the charge.
	cheap := engine.ReadEstimate{Priced: true, Bytes: 1 << 10, Cost: 0.001, Medium: "s3", Provider: "aws-s3"}

	orig := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = orig })

	stdinIsTerminal = func() bool { return false }
	captureStdout(t, func() {
		if !confirmRead(engine.ReadEstimate{Priced: false}, false) {
			t.Error("unpriced read must proceed")
		}
		if !confirmRead(engine.ReadEstimate{Priced: true, Bytes: 0, Cost: 5}, false) {
			t.Error("zero-byte read must proceed")
		}
		if !confirmRead(material, false) {
			t.Error("a non-interactive run must proceed (print-and-go, never block)")
		}
	})

	// Interactive now: --yes skips the prompt; otherwise the typed answer decides.
	stdinIsTerminal = func() bool { return true }
	captureStdout(t, func() {
		if !confirmRead(material, true) {
			t.Error("--yes must skip the prompt and proceed")
		}
	})
	// A cheap priced read still reaches the prompt: declining aborts it.
	withStdin(t, "n\n")
	captureStdout(t, func() {
		if confirmRead(cheap, false) {
			t.Error("a cheap priced read must still prompt; declining must abort")
		}
	})
	for _, tc := range []struct {
		in   string
		want bool
	}{{"y\n", true}, {"yes\n", true}, {"n\n", false}, {"\n", false}, {"", false}} {
		withStdin(t, tc.in)
		captureStdout(t, func() {
			if got := confirmRead(material, false); got != tc.want {
				t.Errorf("confirmRead with input %q = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
