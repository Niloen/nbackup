// Package method is NBackup's dump-method abstraction, analogous to Amanda's
// Application API (amgtar, amstar, ampgsql, ...). A Method produces a raw backup
// stream and consumes one for restore; it knows nothing about compression,
// checksums, or where bytes are stored. Implementations register themselves so
// a DLE can select a method by name.
package method

import (
	"fmt"
	"io"
	"sort"

	"github.com/Niloen/nbackup/internal/dle"
)

// BackupRequest describes one archive to produce.
type BackupRequest struct {
	DLE      dle.DLE
	Level    int    // 0 = full, >=1 = incremental
	BaseSnap string // path to the base snapshot for incrementals; "" for a full
	OutSnap  string // path to write the updated snapshot for this level
}

// BackupResult reports what was produced.
type BackupResult struct {
	Uncompressed int64    // raw stream size
	FileCount    int      // number of file members
	Members      []string // member paths
}

// Method is a pluggable dump program.
type Method interface {
	Name() string
	// Check verifies the method's prerequisites (e.g. the tar binary).
	Check() error
	// Estimate returns the uncompressed bytes the request would archive.
	Estimate(r BackupRequest) (int64, error)
	// Backup writes the raw archive stream to out.
	Backup(r BackupRequest, out io.Writer) (*BackupResult, error)
	// Restore consumes a raw archive stream and writes into destDir.
	Restore(d dle.DLE, in io.Reader, destDir string) error
}

// Options carries method-specific configuration to a factory.
type Options struct {
	TarPath string // gnutar: GNU tar binary
}

// Factory constructs a Method from options.
type Factory func(Options) (Method, error)

var factories = map[string]Factory{}

// Register registers a Method implementation under a name.
func Register(name string, f Factory) { factories[name] = f }

// Open constructs the Method registered under name.
func Open(name string, opts Options) (Method, error) {
	f, ok := factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown dump method %q (known: %v)", name, Names())
	}
	return f(opts)
}

// Names lists registered method names.
func Names() []string {
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
