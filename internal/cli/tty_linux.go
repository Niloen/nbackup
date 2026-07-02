package cli

import "golang.org/x/sys/unix"

// ioctlGetTermios is the get-termios ioctl isTerminal probes with: TCGETS on Linux.
const ioctlGetTermios = unix.TCGETS
