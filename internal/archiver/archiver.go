// Package archiver is NBackup's archive-format abstraction, analogous to Amanda's
// Application API (amgtar, amstar, ampgsql, ...). An Archiver both produces a raw
// backup stream and consumes one for restore — it is the bidirectional handler for
// one archive format, knowing nothing about compression, checksums, where bytes are
// stored, or configuration. It operates on a source path and is configured with
// generic options (supplied by a named archiver definition). It also owns its own
// incremental state — the non-derivable, per-DLE/per-level base data an incremental
// builds on (GNU tar's listed-incremental snapshots, a dump database, ...) — keyed
// by DLE and level, so the generic layer never names a snapshot or a state file.
package archiver

import (
	"fmt"
	"io"
	"sort"
)

// BackupRequest describes one archive to produce. The Archiver resolves any
// incremental state it needs from DLE + BaseLevel itself; the request carries
// identity and levels, never file paths.
type BackupRequest struct {
	DLE        string   // DLE name; the key under which the Archiver stores incremental state
	SourcePath string   // directory to archive
	Level      int      // 0 = full, >=1 = incremental
	BaseLevel  int      // level whose state this incremental builds on; <0 for a full
	Exclude    []string // patterns to skip (content-dependent, so per-request, not Archiver config)
}

// BackupResult reports what was produced.
type BackupResult struct {
	Uncompressed int64    // raw stream size
	FileCount    int      // number of file members
	Members      []string // member paths
}

// Archiver is a pluggable archive-format program (Amanda's application): it backs
// up (produces a stream) and restores (consumes one), both directions.
type Archiver interface {
	Name() string
	// Check verifies the archiver's prerequisites (e.g. the tar binary).
	Check() error
	// Estimate returns the uncompressed bytes the request would archive.
	Estimate(r BackupRequest) (int64, error)
	// Backup writes the raw archive stream to out, updating the archiver's own
	// incremental state for (DLE, Level).
	Backup(r BackupRequest, out io.Writer) (*BackupResult, error)
	// HasBase reports whether the incremental state a dump at level+1 would build
	// on — the state left by a completed dump at the given level — is present. The
	// engine uses it to decide whether an incremental is dumpable (else the DLE is
	// forced to a full) and to gate level estimates. It is the archiver-neutral
	// replacement for "does the base snapshot exist".
	HasBase(dle string, level int) bool
	// Restore consumes a raw archive stream and writes into destDir. With no
	// members it restores the whole archive applying incremental deletions (a
	// chain restore); with members it extracts only those named entries and does
	// not delete (selected-file recovery).
	Restore(in io.Reader, destDir string, members []string) error
	// List reads a raw archive stream and returns its member paths without
	// extracting anything (amverify's `tar -t`). It writes nothing; it proves the
	// stream is a valid, listable archive end-to-end and yields the members to
	// compare against the seal. The returned paths use the same convention as
	// BackupResult.Members.
	List(in io.Reader) ([]string, error)
}

// Options are generic key/value parameters from an archiver definition (e.g.
// "tar_path", "state_dir", "one-file-system").
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

// Factory constructs an Archiver from options.
type Factory func(Options) (Archiver, error)

var factories = map[string]Factory{}

// Register registers an Archiver implementation under a type name.
func Register(name string, f Factory) { factories[name] = f }

// Open constructs the Archiver registered under the type name.
func Open(name string, opts Options) (Archiver, error) {
	f, ok := factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown archiver %q (known: %v)", name, Names())
	}
	return f(opts)
}

// Names lists registered archiver type names.
func Names() []string {
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
