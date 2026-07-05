package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Niloen/nbackup/internal/restorer"
	"github.com/Niloen/nbackup/internal/sizeutil"
	"golang.org/x/term"
)

// extractProgress paints a live, in-place progress bar while a selective recovery
// pulls bytes off a medium, so an operator sees the extract tracking the plan
// instead of a silent pause on a large ranged read. It implements
// restorer.ReadProgress: Reading starts an archive's bar (its expected egress the
// denominator), Pulled advances it, Finished erases the transient line so the next
// stdout log line prints cleanly over it. Repaints are throttled so a fast local
// read does not flood the terminal.
type extractProgress struct {
	w      io.Writer
	fd     int    // terminal fd, for the current width
	total  int64  // grand total egress across all archives (0 = unknown), for the overall tally
	done   int64  // egress pulled by already-finished archives
	kind   string // the current archive's read strategy ("RANGED"/"WHOLE"), the plan's READ column
	label  string
	expect int64 // encoded bytes the current archive is expected to pull
	cur    int64 // bytes pulled in the current archive
	last   time.Time
	shown  bool // a transient line is currently on screen
}

// newExtractProgress returns a live progress reporter painting to stderr, or nil when
// stderr is not a terminal (a pipe, file, or cron log) — non-interactive callers keep
// the clean, stdout-only transcript. total is the whole selection's planned egress.
func newExtractProgress(total int64) restorer.ReadProgress {
	if !stderrIsTerminal() {
		return nil
	}
	return &extractProgress{w: os.Stderr, fd: int(os.Stderr.Fd()), total: total}
}

func (p *extractProgress) Reading(kind, label string, expect int64) {
	p.kind, p.label, p.expect, p.cur = kind, label, expect, 0
	p.last = time.Time{}
	p.paint(true)
}

func (p *extractProgress) Pulled(delta int64) {
	p.cur += delta
	p.paint(false)
}

func (p *extractProgress) Finished() {
	p.done += p.cur
	p.clear()
}

// paint redraws the bar in place, throttled to ~15 fps unless forced (the first paint
// of an archive), so a fast read does not spend more time drawing than reading.
func (p *extractProgress) paint(force bool) {
	now := time.Now()
	if !force && now.Sub(p.last) < 66*time.Millisecond {
		return
	}
	p.last = now

	frac := 0.0
	if p.expect > 0 {
		frac = float64(p.cur) / float64(p.expect)
		if frac > 1 {
			frac = 1
		}
	}
	overall := ""
	if p.total > 0 && p.total != p.expect { // more than the current archive in flight
		overall = fmt.Sprintf("  overall %s / %s", sizeutil.FormatBytes(p.done+p.cur), sizeutil.FormatBytes(p.total))
	}
	head := fmt.Sprintf("  %-6s %s %3.0f%%  %s / %s%s  ",
		p.kind, bar(frac, 22), frac*100, sizeutil.FormatBytes(p.cur), sizeutil.FormatBytes(p.expect), overall)
	line := head + fitLabel(p.label, p.width()-len([]rune(head)))
	fmt.Fprintf(p.w, "\r%s\033[K", line)
	p.shown = true
}

// clear erases the transient bar line and returns the cursor to column 0 without a
// newline, so the next stdout log line ("  ranged read: fetched …") overwrites it
// cleanly rather than leaving a stale bar behind.
func (p *extractProgress) clear() {
	if p.shown {
		fmt.Fprint(p.w, "\r\033[K")
		p.shown = false
	}
}

// width returns the terminal's current column count, or 80 when it can't be read.
func (p *extractProgress) width() int {
	if w, _, err := term.GetSize(p.fd); err == nil && w > 0 {
		return w
	}
	return 80
}

// bar renders a fixed-width [====>   ] meter for frac in [0,1].
func bar(frac float64, width int) string {
	filled := int(frac * float64(width))
	if filled > width {
		filled = width
	}
	arrow := ""
	if filled < width {
		arrow = ">"
		filled--
	}
	if filled < 0 {
		filled = 0
	}
	return "[" + strings.Repeat("=", filled) + arrow + strings.Repeat(" ", width-filled-len(arrow)) + "]"
}

// fitLabel trims an archive label to fit the remaining columns, keeping its tail (the
// run id and level) and eliding the front, so a long host:path never wraps the bar.
func fitLabel(s string, max int) string {
	if max < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[len(r)-max:])
	}
	return "…" + string(r[len(r)-(max-1):])
}
