package progress

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// MultiSink fans one snapshot out to several sinks (e.g. the run-status file plus
// a live terminal display). Nil sinks are skipped, so callers can compose
// optionally-present sinks without guarding each one.
func MultiSink(sinks ...Sink) Sink {
	return func(s Snapshot, force bool) {
		for _, k := range sinks {
			if k != nil {
				k(s, force)
			}
		}
	}
}

// liveThrottle bounds how often a non-forced (byte-count) update repaints the
// terminal; forced updates (state changes) always paint immediately.
const liveThrottle = 120 * time.Millisecond

// LiveSink returns a Sink that paints each snapshot in place on w — an interactive
// terminal — by formatting it with lines and rewinding the cursor over the previous
// frame before repainting, so the report updates without scrolling. Each line is
// truncated to the terminal width so a wrapped line never corrupts the cursor math;
// lines must therefore return a bounded number of rows that fits on screen. When the
// run reaches a terminal phase the region is erased, leaving the cursor on a clean
// line for the caller's final output.
func LiveSink(w io.Writer, lines func(Snapshot) []string) Sink {
	var (
		mu        sync.Mutex
		above     int  // rows above the cursor in the last painted frame
		painted   bool // a frame is currently on screen
		lastPaint time.Time
	)
	width := terminalWidth(w)
	erase := func() string {
		// Return to column 0 and clear from there to the end of the screen, moving
		// up over the frame's rows first. A single-line frame (above == 0) leaves
		// the cursor already on its only row, so no cursor-up is emitted — crucially
		// we must NOT write ESC[0A, because a real terminal treats the 0 parameter
		// as 1 and moves up a line, eating the row above (e.g. the shell prompt).
		if above == 0 {
			return "\r\033[J"
		}
		return fmt.Sprintf("\033[%dA\r\033[J", above)
	}
	return func(s Snapshot, force bool) {
		mu.Lock()
		defer mu.Unlock()
		if s.Phase.Terminal() {
			if painted {
				io.WriteString(w, erase())
				painted = false
			}
			return
		}
		now := time.Now()
		if !force && painted && now.Sub(lastPaint) < liveThrottle {
			return
		}
		frame := lines(s)
		var b strings.Builder
		if painted {
			b.WriteString(erase())
		} else {
			b.WriteString("\r\033[J")
		}
		for i, ln := range frame {
			b.WriteString(truncate(ln, width))
			if i < len(frame)-1 {
				b.WriteByte('\n')
			}
		}
		io.WriteString(w, b.String())
		if above = len(frame) - 1; above < 0 {
			above = 0
		}
		painted = true
		lastPaint = now
	}
}

// terminalWidth reports the usable column count for in-place rendering. It asks the
// terminal directly via TIOCGWINSZ on the output's file descriptor — the only
// reliable source, since interactive shells keep $COLUMNS as a shell variable and do
// NOT export it to child processes (so reading the env alone pins every run to the 80
// fallback, truncating wide terminals and wrapping narrow ones). $COLUMNS is still
// honored as a fallback (e.g. when w is not a terminal, as in tests), then 80.
func terminalWidth(w io.Writer) int {
	if f, ok := w.(interface{ Fd() uintptr }); ok {
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 20 {
			return int(ws.Col)
		}
	}
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 20 {
			return n
		}
	}
	return 80
}

// truncate clips s to at most width runes so it cannot wrap to a second terminal
// row (which would desync the cursor rewind).
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}
