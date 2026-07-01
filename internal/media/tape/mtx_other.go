//go:build !linux

package tape

import (
	"fmt"
	"runtime"
)

// A SCSI media changer (mtx) is driven only on Linux (it needs the real tape drives,
// whose byte I/O is the Linux st(4) backend). The file-backed library (dir:) works
// everywhere; openMtxLoader refuses up front elsewhere.
func openMtxLoader(control string, nodes []string, block int) (loader, error) {
	return nil, fmt.Errorf("tape changers (changer:) are supported only on Linux, not %s; use a file-backed library (dir:) instead", runtime.GOOS)
}
