// Package method is NBackup's dump-method abstraction, analogous to Amanda's
// Application API (amgtar, amstar, ampgsql, ...). A Method produces a raw backup
// stream and consumes one for restore; it knows nothing about compression,
// checksums, where bytes are stored, or configuration. It operates on a source
// path and is configured with generic options (supplied by a dumptype).
package method

import (
	"fmt"
	"io"
	"sort"
)

// BackupRequest describes one archive to produce.
type BackupRequest struct {
	SourcePath string // directory to archive
	Level      int    // 0 = full, >=1 = incremental
	BaseSnap   string // path to the base snapshot for incrementals; "" for a full
	OutSnap    string // path to write the updated snapshot for this level
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
	Restore(in io.Reader, destDir string) error
}

// Options are generic key/value parameters from a dumptype (e.g. "tar_path",
// "one-file-system").
type Options map[string]string

// Get returns the value for a key, or "".
func (o Options) Get(key string) string { return o[key] }

// Bool parses a boolean option, returning def when unset or unparseable.
func (o Options) Bool(key string, def bool) bool {
	switch o[key] {
	case "":
		return def
	case "true", "yes", "1", "on":
		return true
	case "false", "no", "0", "off":
		return false
	default:
		return def
	}
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
