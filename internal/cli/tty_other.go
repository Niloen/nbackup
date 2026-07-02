//go:build !linux

package cli

import "golang.org/x/sys/unix"

// ioctlGetTermios is the get-termios ioctl isTerminal probes with: TIOCGETA on
// darwin and the BSDs (Linux calls it TCGETS — see tty_linux.go).
const ioctlGetTermios = unix.TIOCGETA
