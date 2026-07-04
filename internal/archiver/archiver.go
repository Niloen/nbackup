// Package archiver is NBackup's archive-format abstraction. An Archiver both
// produces a raw backup stream and consumes one for restore — it is the
// bidirectional handler for one archive format, knowing nothing about
// compression, checksums, where bytes are
// stored, or configuration. It operates on a source path and is configured with
// generic options (supplied by a named archiver definition). It also owns its own
// incremental state — the non-derivable, per-DLE/per-level base data an incremental
// builds on (GNU tar's listed-incremental snapshots, a dump database, ...) — keyed
// by DLE and level, so the generic layer never names a snapshot or a state file.
//
// The interface splits along two axes the reader should keep in mind:
//   - Streaming operations that the caller composes into a pipeline return a program stage
//     for it to run, never bytes: BackupSource (the produce side) hands back a stage plus
//     the Finish/Promote/Cleanup hooks one dump needs, and RestoreStage (the consume side)
//     hands back a bare stage — restore carries no hooks because it commits no state.
//   - Synchronous queries run their program themselves and return a value: Estimate (a size)
//     and List (the member paths). They do not compose as stages because they yield an
//     answer rather than passing bytes through.
//
// Companion files: the plugin registry (Factory/Register/Open/Names) is in registry.go and
// the config Options type is in options.go.
package archiver

import (
	"io"
	"strings"

	"github.com/Niloen/nbackup/internal/programs"
	"github.com/Niloen/nbackup/internal/record"
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
	Uncompressed int64           // raw stream size
	FileCount    int             // number of file members
	Members      []record.Member // members in stream order, each with its byte offset in the raw stream (Off -1 when the archiver cannot report offsets)
	// Unreadable lists source paths the archiver could not read (e.g. a permission-denied
	// file): the archive committed without them — a *partial* dump. Empty means complete.
	// A partial archive is still a valid, restorable stream of what was readable; the caller
	// warns and exits non-zero so the gap is loud, rather than discarding a usable backup.
	Unreadable []string
}

// CountFiles counts the file members in a list, excluding directories — the
// member convention (BackupResult.Members, List) marks a directory with a
// trailing slash. So one nested file counts as 1, not "2 entries" once its
// parent directory is included.
func CountFiles(members []record.Member) int {
	n := 0
	for _, m := range members {
		if !strings.HasSuffix(m.Path, "/") {
			n++
		}
	}
	return n
}

// BackupSource is the producing side of one archive as a pipeline source: the program
// stage that emits the raw archive stream (e.g. `tar --create`) plus the executor (host)
// it runs on, and a Finish hook the runner calls after the pipeline has drained to gather
// the result (member list, sizes) from the host's scratch. The caller assembles the full
// pipeline — appending the compress and encrypt stages and metering the tail — and runs
// it; the archiver does not stream the bytes itself. This is what lets a dump's
// tar+compress+encrypt fuse on one host (the client) so plaintext never leaves it.
// Stage's Stderr is pre-wired by the archiver to capture totals; Cleanup removes scratch.
//
// Promote commits this dump's incremental state into the archiver's library — the
// caller invokes it once the archive is durably committed, and only then. Until it is
// called the dump writes its new state to a side file, leaving the base a retry would
// build on untouched; a dump that fails (or whose archive never commits) is simply never
// promoted, so a killed tar can never corrupt the library. It is the rename half of the
// ".new"-then-promote pattern; nil for an archiver with no incremental state.
type BackupSource struct {
	Stage   programs.Cmd
	Exec    programs.Executor
	Finish  func() (*BackupResult, error)
	Promote func() error
	Cleanup func()
}

// Archiver is a pluggable archive-format program: it backs
// up (produces a stream) and restores (consumes one), both directions.
type Archiver interface {
	Name() string
	// Check verifies the archiver's prerequisites (e.g. the tar binary).
	Check() error
	// Estimate returns the uncompressed bytes the request would archive.
	Estimate(r BackupRequest) (int64, error)
	// BackupSource returns the producing pipeline source for one archive (see
	// BackupSource): the program stage that emits the raw stream, a Finish hook to
	// gather the result, and a Promote hook that commits the dump's new incremental
	// state for (DLE, Level) into the library — which the caller invokes only once the
	// archive is durably committed, so a failed dump never advances the state. The
	// caller runs the pipeline.
	BackupSource(r BackupRequest) (*BackupSource, error)
	// HasBase reports whether the incremental state a dump at level+1 would build
	// on — the state left by a completed dump at the given level — is present. The
	// engine uses it to decide whether an incremental is dumpable (else the DLE is
	// forced to a full) and to gate level estimates. It is the archiver-neutral
	// replacement for "does the base snapshot exist". A present-but-unusable base (e.g.
	// an empty snapshot a killed dump left behind) reports false, so a corrupt base
	// forces a full rather than silently producing a full-sized incremental.
	HasBase(dle string, level int) bool
	// RestoreStage returns the extractor as a program stage (extract from stdin into
	// destDir), so a decode→extract pipeline can run entirely on the host where the
	// bytes should land — letting a client-held key decrypt on the client and a
	// server-held key ship only compressed plaintext to a remote target. With no members
	// it restores the whole archive applying incremental deletions (a chain restore);
	// with members it extracts only those named entries and does not delete (selected-file
	// recovery). It is the engine's one extraction primitive — the caller composes it
	// into a programs pipeline and runs it; the archiver never streams bytes itself.
	RestoreStage(destDir string, members []string) programs.Cmd
	// List reads a raw archive stream and returns its members without
	// extracting anything (`tar -t`). It writes nothing; it proves the
	// stream is a valid, listable archive end-to-end and yields the members to
	// compare against the seal. The returned members use the same convention
	// (and offset semantics) as BackupResult.Members.
	List(in io.Reader) ([]record.Member, error)
}
