package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdinIsTerminal reports whether stdin is an interactive terminal (vs a pipe/cron),
// so an unspecified `nb drill` defaults to unattended in a non-interactive context and a
// manual tape station only prompts when someone can answer. It uses a real TTY ioctl, not
// an os.ModeCharDevice test: /dev/null (a common cron stdin) is itself a character device,
// so the looser test wrongly reported `nb dump </dev/null` as interactive — printing a swap
// prompt into the log and then erroring, instead of the clean unattended path.
// A var (not a plain func) so tests can script the interactive-vs-cron decision
// the prompt paths (confirmRead, the swap operator) gate on.
var stdinIsTerminal = func() bool {
	return isTerminal(os.Stdin.Fd())
}

// isTerminal reports whether fd refers to a real terminal, via the get-termios ioctl
// that underlies isatty(3) (TCGETS on Linux, TIOCGETA on darwin/BSD — see tty_*.go).
// Unlike an os.ModeCharDevice test it returns false for /dev/null and other character
// devices — only a tty answers it.
func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), ioctlGetTermios)
	return err == nil
}

// stderrIsTerminal reports whether stderr is an interactive terminal, so live
// in-place progress is only painted when someone is watching (not into a pipe,
// file, or cron log). Same real-TTY check as stdinIsTerminal (not os.ModeCharDevice).
func stderrIsTerminal() bool {
	return isTerminal(os.Stderr.Fd())
}
