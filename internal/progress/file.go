package progress

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Niloen/nbackup/internal/fsx"
)

// flushInterval bounds how often byte-count updates rewrite the status file;
// state transitions bypass it (force=true), so the file is always current at
// start, finish, and phase changes.
const flushInterval = time.Second

// NewFileSink returns a Sink that persists snapshots to dir/StatusFileName,
// throttling byte-only updates to flushInterval. now supplies the throttle clock
// (injectable for tests); pass nil for time.Now. The directory must already
// exist (it is the catalog workdir).
func NewFileSink(dir string, now func() time.Time) Sink {
	if now == nil {
		now = time.Now
	}
	// The catalog workdir may not exist yet (the catalog creates it lazily on its
	// first save); ensure it so the very first status write lands.
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, StatusFileName)
	var last time.Time
	return func(s Snapshot, force bool) {
		t := now()
		if !force && !last.IsZero() && t.Sub(last) < flushInterval {
			return
		}
		last = t
		if err := writeStatus(path, s); err != nil {
			// Progress reporting must never break a backup; a stderr note is enough.
			fmt.Fprintf(os.Stderr, "warning: write run status: %v\n", err)
		}
	}
}

// writeStatus serializes the snapshot to a temp file and atomically renames it
// over path, so a concurrent `nb status` reader never sees a half-written file.
func writeStatus(path string, s Snapshot) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, data, 0o644)
}

// Load reads the run-status file from a catalog workdir. It returns
// os.ErrNotExist (wrapped) when no run has written one, which callers treat as
// "no run in progress".
func Load(dir string) (Snapshot, error) {
	path := filepath.Join(dir, StatusFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return Snapshot{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// IsNotExist reports whether a Load error means simply "no status file yet".
func IsNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
