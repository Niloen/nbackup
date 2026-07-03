// Package fsx holds tiny filesystem helpers shared across packages.
package fsx

import "os"

// WriteFileAtomic writes data to a sibling temp file (path + ".tmp") and renames
// it over path, so a concurrent reader never observes a half-written file. The
// temp file lives in the same directory as path, keeping the rename atomic.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
