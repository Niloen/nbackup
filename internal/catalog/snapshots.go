package catalog

import (
	"fmt"
	"os"
	"path/filepath"
)

// The GNU tar snapshot library is the catalog's one piece of non-derivable local
// state: per-DLE, per-level .snar files in the workdir that let an incremental name
// only what changed since its base level. Unlike the slot index it cannot be
// rebuilt from the media — losing a .snar forces the next run of that DLE to a full.
// The catalog owns the path layout; the engine reads and writes the files.

// DirSnapshots holds per-DLE, per-level GNU tar snapshot files.
const DirSnapshots = "snapshots"

// SnapshotPath is the local location of a DLE's snapshot for a given level.
func (c *Catalog) SnapshotPath(dleName string, level int) string {
	return filepath.Join(c.workdir, DirSnapshots, dleName, fmt.Sprintf("L%d.snar", level))
}

// SnapshotExists reports whether a snapshot file exists for the level.
func (c *Catalog) SnapshotExists(dleName string, level int) bool {
	_, err := os.Stat(c.SnapshotPath(dleName, level))
	return err == nil
}
