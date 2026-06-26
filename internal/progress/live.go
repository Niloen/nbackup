package progress

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
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
	width := terminalWidth()
	erase := func() string {
		// Move to the top of the frame (ESC[0A is a harmless no-op), return to
		// column 0, and clear from there to the end of the screen.
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

// terminalWidth reports the usable column count for in-place rendering, from
// $COLUMNS when set (the shell exports it) and 80 otherwise.
func terminalWidth() int {
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
