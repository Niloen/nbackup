// Package lock provides a per-config exclusive lock so that only one mutating
// nb process operates on a catalog workdir (and the media it lands on) at a
// time. Rather than making the catalog concurrently writable, it serializes the
// whole mutating run.
//
// The lock is an advisory flock(2) on a `lock` file in the workdir. flock is
// tied to the open file description, so the kernel releases it automatically if
// the process exits or crashes — no stale lockfiles to clean up. Read-only
// commands do not take the lock: catalog writes land via atomic rename, so a
// reader always sees a complete old-or-new cache.
//
// Caveat: flock semantics are unreliable over NFS. A catalog workdir is expected
// to live on a local filesystem.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile is the advisory lock file kept in the workdir.
const LockFile = "lock"

// ErrHeld is returned when another nb process already holds the config lock.
var ErrHeld = errors.New("another nb process is operating on this config")

// Lock is a held exclusive lock. Release it (typically via defer) when the
// mutating operation completes.
type Lock struct {
	f *os.File
}

// Acquire takes the exclusive config lock for workdir, creating the workdir and
// lock file if needed. It is non-blocking: if another process holds the lock it
// returns ErrHeld immediately rather than waiting.
func Acquire(workdir string) (*Lock, error) {
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(workdir, LockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (workdir %s)", ErrHeld, workdir)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the lock file. Closing the descriptor releases the
// flock; the unlock is explicit so the intent is clear at call sites.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
