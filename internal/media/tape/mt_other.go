//go:build !linux

package tape

import (
	"fmt"
	"io"
	"runtime"
)

// On non-Linux platforms a real tape drive is not supported: positioning a drive
// means driving an OS-specific tape ioctl interface (Amanda carries a separate
// implementation per OS for the same reason), and only the Linux st(4) backend is
// implemented. The file-backed library/station (dir:) works everywhere. The type
// below exists only so the package compiles; openMT refuses up front.
type mtDevice struct{}

func openMT(dev string, block int) (*mtDevice, error) {
	return nil, fmt.Errorf("real tape drives (device:) are supported only on Linux, not %s; use a file-backed library (dir:) instead", runtime.GOOS)
}

func (m *mtDevice) count() (int, error)                     { return 0, errUnsupported }
func (m *mtDevice) bytesUsed() int64                        { return 0 }
func (m *mtDevice) reset() error                            { return errUnsupported }
func (m *mtDevice) appendWriter() (deviceWriter, error)     { return nil, errUnsupported }
func (m *mtDevice) readFile(pos int) (io.ReadCloser, error) { return nil, errUnsupported }

var errUnsupported = fmt.Errorf("real tape drives are supported only on Linux")
