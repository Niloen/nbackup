// Package archiver is NBackup's archive-format abstraction. An Archiver both
// produces a raw backup stream and consumes one for restore — it is the
// bidirectional handler for one archive format, knowing nothing about
// compression, checksums, where bytes are
// stored, or configuration. It operates on an opaque source string it alone
// interprets (a directory for tar, a command argument for pipe, a database name
// for a future db archiver) and is configured with
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

// Scope is what one archive covers, in the archiver's own vocabulary: a Source to archive
// plus the Exclude patterns to leave out of it. It is the addressing half of a BackupRequest,
// factored out so an Expand result and a dump request share one excludes model. Base is the
// partition/enumeration root the archiver derived: "" for a plain source; equal to Source for
// a remainder ("the rest"); a strict prefix of Source for a match. It is transient plan-time
// provenance (grouping and the remainder guard) and is not recorded.
type Scope struct {
	Base string
	// Source names what to archive, in this archiver's own vocabulary: a directory
	// for a tree archiver (gnutar), the producer command's argument for pipe, a
	// database name or a dataset for a future db/zfs archiver. Opaque to the generic
	// layers — only the archiver interprets it (CheckSource is its readiness probe).
	Source  string
	Exclude []string // patterns to skip (content-dependent, so per-request, not Archiver config)
}

// Carves returns the Exclude entries that carve whole subtrees out of this scope — the
// leading-"/" (root-anchored) patterns a partition's remainder carries, as opposed to
// content globs ("*.log"). The convention is part of the Scope contract: Expand produces
// them, the archiver anchors them, and the dump records them (record.Archive.Carves) so
// the next plan can force a re-baseline when the carve set grows.
func (s Scope) Carves() []string {
	var out []string
	for _, p := range s.Exclude {
		if strings.HasPrefix(p, "/") {
			out = append(out, p)
		}
	}
	return out
}

// SourcePattern is the input to Expand: a Source pattern to resolve into concrete Scopes.
// Base is the named base of a partition (config's path:) — its presence means Expand also
// emits a remainder Scope covering everything under Base the matches don't. An empty Base is
// a selection (a wildcard Pattern → one Scope per match, no remainder) or a plain source (no
// wildcard → one Scope). Pattern is relative to Base when Base is set, else the whole source.
// Exclude carries the configured (dumptype) excludes to bake into every result Scope.
type SourcePattern struct {
	Base    string
	Pattern string
	Exclude []string
}

// BackupRequest describes one archive to produce. The Archiver resolves any
// incremental state it needs from DLE + BaseLevel itself; the request carries
// identity and levels, never file paths.
type BackupRequest struct {
	Scope            // Source, Exclude (and Base) — promoted, so r.Source / r.Exclude still work
	DLE       string // DLE name; the key under which the Archiver stores incremental state
	Level     int    // 0 = full, >=1 = incremental
	BaseLevel int    // level whose state this incremental builds on; <0 for a full
}

// BackupResult reports what was produced.
type BackupResult struct {
	Uncompressed int64           // raw stream size
	FileCount    int             // number of file members
	Members      []record.Member // members in stream order, each with its byte offset in the raw stream (Off -1 when the archiver cannot report offsets)
	// Units is the archive's content inventory in this archiver's own vocabulary
	// (postgres: tables with sizes; see record.Unit), sorted by Path. Nil when the
	// archiver reports none (gnutar, pipe) — inventory is a declared extra, like
	// member offsets.
	Units []record.Unit
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
	// CheckSource verifies one DLE's source is ready to back up, in this archiver's
	// own vocabulary — gnutar probes the directory's readability (`test -r`), a db
	// archiver would probe connectivity, pipe has nothing to probe (the producer
	// command owns its source). It is `nb check`'s per-DLE probe, replacing the
	// generic "is the path readable" that assumed every source is a filesystem path.
	CheckSource(source string) error
	// Expand resolves a SourcePattern into the concrete Scopes to dump, in this archiver's own
	// vocabulary. A wildcard-free pattern returns exactly one Scope (no I/O). A wildcard yields
	// one Scope per match; when SourcePattern.Base is set it also emits a remainder Scope
	// (Source == Base) carving out the matches with anchored excludes — a tree archiver only (a
	// discrete archiver like postgres has no remainder and returns just the matches). Each
	// returned Scope is complete: it carries the configured excludes (the remainder additionally
	// carries the carve excludes) and its Base (the enumeration/partition root), so a dump hands
	// a Scope straight into a BackupRequest. Called only at plan time; a failed enumeration is
	// returned as an error and fails the plan.
	Expand(p SourcePattern) ([]Scope, error)
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
	// dest), so a decode→extract pipeline can run entirely on the host where the
	// bytes should land — letting a client-held key decrypt on the client and a
	// server-held key ship only compressed plaintext to a remote target. dest is the
	// restore destination in this archiver's vocabulary (see DestIsDir). With no members
	// it restores the whole archive applying incremental deletions (a chain restore);
	// with members it extracts only those named entries and does not delete (selected-file
	// recovery). It is the engine's one extraction primitive — the caller composes it
	// into a programs pipeline and runs it; the archiver never streams bytes itself.
	RestoreStage(dest string, members []string) programs.Cmd
	// DestIsDir declares the restore destination a directory tree the GENERIC layer
	// owns the lifecycle of: it creates it, refuses a non-empty one for a whole-DLE
	// restore (the archiver's chain replay applies deletions, so restoring over an
	// existing tree would prune unrelated files), and rolls a failed chain back by
	// clearing it. false = the destination is opaque — the archiver's restore stage
	// alone interprets it (pipe hands it to the consumer command verbatim), so the
	// generic layer must neither create nor guard nor clear anything there.
	DestIsDir() bool
	// SourceIsPath declares that the DLE's source string is a local filesystem path
	// this archiver reads directly (gnutar: a directory tree) — so a preview may stat
	// it to warn about a missing/unreadable source before the run. false = the source
	// is an opaque reference the archiver alone interprets (postgres: a libpq
	// connection string; pipe: a token for the user's command), which the generic
	// layer must NOT stat — doing so mis-warns "source path is missing" for a perfectly
	// valid conninfo. Readiness of a non-path source is the archiver's own live probe
	// (postgres CheckSource connects), surfaced by `nb check`, not a filesystem stat.
	SourceIsPath() bool
	// Ext is the filename extension for this archiver's raw stream (gnutar: ".tar"),
	// recorded per-archive so a payload's on-medium name says what stock tool reads
	// it — the naming peer of the recorded compress/encrypt schemes.
	Ext() string
	// CanList declares whether List is available. An archiver whose stream has no
	// enumerable members (pipe: an opaque byte stream) reports false; structural
	// verify then degrades to proving the decode pipeline drains cleanly — the same
	// class of fallback ranged reads take for unreported offsets.
	CanList() bool
	// List reads a raw archive stream and returns its members without
	// extracting anything (`tar -t`). It writes nothing; it proves the
	// stream is a valid, listable archive end-to-end and yields the members to
	// compare against the seal. The returned members use the same convention
	// (and offset semantics) as BackupResult.Members. Only called when CanList.
	List(in io.Reader) ([]record.Member, error)
	// StockExtract is the documented no-NBackup extraction fragment for this
	// archiver's raw stream: an `sh` pipeline tail that reads the stream on stdin
	// and restores it into "$1" (gnutar: `tar --extract … -C "$1" -f -`; pipe: the
	// configured consumer command). The drill's stock tier composes it after the
	// stock decrypt/decompress stages — proving recovery needs no NBackup — so it
	// must use only the operator's own tools. "" = no stock recipe; the stock tier
	// then fails with that fact rather than silently exercising the wrong command.
	StockExtract() string
	// SpliceTrailer declares whether this archiver's streams can be SPLICED — a
	// synthetic stream assembled from whole member extents (each [Off_i, Off_{i+1}),
	// in stream order) that RestoreStage and List will consume correctly — and, when
	// they can, returns the bytes that cleanly terminate such a stream (GNU tar: the
	// two 512-byte zero blocks of its end-of-archive marker). This is a stronger
	// promise than reporting member offsets: it requires every member's extent to be
	// self-contained and independently restorable, with no cross-member state and no
	// out-of-band directory (a zip-style central directory, a solid-compressed
	// format's shared dictionary would both report offsets yet NOT be spliceable).
	// nil = no such promise; ranged selective restore and the structural samples
	// then fall back to the whole-stream decode, which is always correct. The read
	// side's capability peer of the transforms' Concat and the media's ranged reads.
	SpliceTrailer() []byte
	// RestoreIsCombine declares that a whole-DLE chain restore is a GATHER-THEN-COMBINE:
	// each level's RestoreStage extracts into its own staging directory (inside the
	// destination, so one filesystem) and CombineStage then merges the staged levels into
	// the destination in one step (postgres: pg_combinebackup — an N-input merge, so the
	// levels must exist on disk simultaneously). false = the default additive replay: each
	// level's RestoreStage applies directly into the destination in level order (gnutar's
	// listed-incremental overlay, pipe's consumer). A capability with a graceful default,
	// like SpliceTrailer.
	RestoreIsCombine() bool
	// CombineStage returns the program stage that merges the staged level directories
	// (in chain order, base first) into dest — the finalize step of a combine-shaped
	// restore. It runs on the destination's executor after every level has been staged,
	// and owns removing the staging (which lives under dest). Only called when
	// RestoreIsCombine; a zero Cmd otherwise.
	CombineStage(dest string, stagingDirs []string) programs.Cmd
	// Assembler declares how one logical file's chain versions merge when the browse
	// tree (recover's selection, `nb mount`) reads a file from an incremental chain
	// WITHOUT running the full restore. nil = the default: the newest version of a path
	// is the file (gnutar overlays whole files; pipe has no members). An archiver whose
	// incrementals store per-file DELTAS (postgres: INCREMENTAL.<name> block maps)
	// returns one, so a chain browse assembles correct content instead of showing a
	// stale full beside an unreadable delta.
	Assembler() Assembler
	// Exporter declares that this archiver can MATERIALIZE named units (the
	// inventory's record.Unit identities) into their directly-useful form —
	// postgres: boot a throwaway cluster on the restored tree and pg_dump the
	// table to SQL. nil = no export capability; pointing `--path`/`add` at a
	// unit then errors, and only file selection is available. The read-side
	// answer to "what does a user DO with a table in a backup": point at a
	// file, get a file; point at a thing, get the thing in its useful form.
	Exporter() Exporter
}

// Exporter materializes units from a restored whole-DLE tree — the generic
// layer restores the chain into a scratch directory (the same gather-then-
// combine restore `--all` runs) and then runs Stage there; the archiver owns
// everything inside it. nb's contract stays non-destructive: the output is a
// file the operator imports themselves; no export path ever touches a live
// service.
type Exporter interface {
	// Ext is the filename extension an exported unit lands with (postgres:
	// ".sql"); a unit exports as exactly "<Unit.Path><Ext>" under the
	// destination, so a selection listing can say verbatim what will appear.
	Ext() string
	// Stage returns the program stage that materializes the named units (unit
	// identities, resolved by the caller against the archive's inventory) from
	// the restored tree at dataDir into destDir. source is the DLE's own source
	// string (postgres: the libpq connection reference — the exporter reads the
	// role to connect AS from it; the throwaway cluster's roles are prod's, not
	// the restoring OS user's). It runs on the host that holds dataDir, owns any
	// service it starts there (and must tear it down on every path), and must not
	// touch anything outside dataDir, destDir, and scratch of its own making.
	Stage(dataDir, destDir, source string, units []string) programs.Cmd
}

// Assembler merges one logical file's chain versions for browse-time reads — the
// member-level analog of the chain restore, used where running the archiver's real
// combine (pg_combinebackup) is impossible because only one file is wanted. An archiver
// returning an Assembler also promises the ASSEMBLER CENSUS: the newest chain level's
// member list enumerates every live file (postgres incrementals carry every file as a
// whole copy or a delta stub), so the browse tree takes the newest level as
// authoritative for existence and deletions fall out — where the default census is the
// most-recent-wins union of all levels.
type Assembler interface {
	// Logical maps a stream member path to its logical tree identity and whether the
	// member is a delta needing assembly: postgres "base/16384/INCREMENTAL.2619" →
	// ("base/16384/2619", true); anything stored whole maps to itself with false.
	Logical(path string) (logical string, delta bool)
	// Assemble merges one logical file's chain versions (oldest→newest, one per chain
	// level that holds the member) into the file's content. Each version carries the
	// delta flag Logical reported for its member — the tree knows which stored form it
	// read, so Assemble never guesses from content: a whole version replaces the
	// accumulated result outright, a delta is applied over it.
	Assemble(versions []Version) (io.ReadCloser, error)
}

// Version is one chain level's stored form of a logical file, as handed to
// Assemble: the member's bytes and whether they are a delta (per Logical).
type Version struct {
	R     io.Reader
	Delta bool
}

// DumpErrorInterpreter is an optional Archiver capability: when a dump stage fails, the
// dumper offers the archiver a chance to translate its tool's raw error into a more
// actionable one — a database that alone knows what "WAL summaries are incomplete" means
// can rewrite it into the `nb reset` that fixes it, and strip the generic shell-wrapper
// noise ("bash: exit status 1") so the user sees the tool's own words. It only clarifies;
// it never suppresses the failure. Returning err unchanged (or not implementing this) keeps
// the raw message. dleDisplay is the DLE's host:path identity (what `nb plan`/`nb reset` show).
type DumpErrorInterpreter interface {
	InterpretDumpError(req BackupRequest, dleDisplay string, err error) error
}
