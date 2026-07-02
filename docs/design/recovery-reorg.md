# Recovery reorganization — one planning package, one extraction path, a read-side fs

Status: IMPLEMENTED (2026-07-02). One deviation from the plan as written: `clerk.ReadArchives` gained open-time copy fail-over (the located copy orders the pass; a copy that will not open falls over through the other eligible copies, still confined to a pinned medium) — the per-step `clerk.Open` restore path had this and the one-pass read must not lose it. Also, decompress now fuses with tar only for a remote target; locally it runs in the filters, so a Sink-role fault stays exactly "tar/composition" — the classification contract the drill relies on.

## Problem

The recovery chain (everything that reads backups back) is split awkwardly:

- **Pure planning is spread over three leaf packages** for one domain: `restore`
  (chain computation), `recovery` (as-of resolution, browse tree, session), `drill`
  (taxonomy, ledger, selection). Four near-identical structs carry
  `Run/DLE/Level/Archiver/Compress/Encrypt` (`restore.Step`, `recovery.Source`,
  `recovery.ExtractStep`, the steps in `drill.Target`).
- **Drill has a private restore path.** `engine/restore.go` claims `extractInto` is
  "the engine's one extraction path", but `engine/drill.go` (`drillChain`)
  hand-composes its own transfer (`decodeFilters` + `SplitTransforms` +
  `NewProgramChain` + `RestoreStage` + `Transfer`). So a green chain drill proves a
  *copy* of the restore path works — not that `nb recover --all` works.
- **Two read-driving idioms.** File-level recover and drill verify use
  `clerk.ReadArchives` (ordered one-pass, mount reuse); the whole-DLE chain restore
  opens each step individually via `clerk.Open` and forgoes mount reuse.
- **Medium pinning done two ways**: the restorer smuggles it through a mutable
  `preferMedium` field (`useMedium`/reset closure); drill passes `medium` explicitly.
- **`engine/drill.go` (≈790 lines) mixes four concerns**: tier execution, unattended
  reachability, the WORM probe, and the ~250-line 3-2-1-1-0 posture audit.
- **The recovery operations hold `*clerk.Clerk` concretely**, though they use exactly
  three methods — the read face is a sharp contract that exists only implicitly.
  The write side already solved this shape: the Author SDK is written once over
  `archiveio.WriteStore`, with `clerk.Session` (serial) and the spool (concurrent)
  behind it.

## Design

### The read-side archive fs (mirror of the write side)

The write side abstracted the **medium end** (`WriteStore`/`Store`/`Ingest`), never
the client end — the client stayed `programs.Executor` + an archiver stage. The read
side mirrors that exactly:

- Medium end: a new `archiveio.ReadStore` interface, implemented by the clerk.
- Client end (the extraction destination): stays concrete — `programs.Executor` for
  the host + `arch.RestoreStage(destDir, members)` as the sink's final stage. No
  `Destination` interface: local-vs-remote is already the executor, and where
  destinations genuinely vary (extract / `tar -t` list / hash) the seam is
  `xfer.Sink`, which already exists. `Host`/`Dest` stay plain data on the request
  because they are *policy inputs* (decrypt placement, feasibility gate, empty-dest
  guard, rollback), not hidable plumbing.

```go
// archiveio — beside WriteStore/Store/Ingest, so store.go reads as the whole
// archive-fs contract, both directions.

// Ref is the logical identity of one archive — the fs "filename".
// (Unifies archiveio.Expect and clerk.Ref, which were field-for-field identical.)
type Ref struct {
    Run   string
    DLE   string
    Level int
}

// ReadStore is the read face of the archive fs: a logical ref resolved to its raw
// on-medium bytes (copy-selected; medium "" = any copy with fail-over), an ordered
// one-pass read over a selection, and an archive's member list. It speaks only refs
// and bytes — no catalog types, no schemes, no transfers (those live in the
// operations, as on the write side).
type ReadStore interface {
    Open(ref Ref, medium string) (io.ReadCloser, error)
    ReadArchives(refs []Ref, medium string,
        fn func(ref Ref, open func() (io.ReadCloser, error)) error) (missing []Ref, err error)
    Members(ref Ref) ([]string, error)
}
```

`Ref` lives in `archiveio` (not `record` — record is on-disk formats only) because
the block layer already needs it: the parts reader asserts identity against each
part's header (today's `Expect`). A later `archivefs` split (fs contracts vs block
mechanics) remains open and **unblocked** by this: fs would sit on block, and the
identity type stays in the lower package either way. Deferred; pure file-move later.

### One pure planning package: `internal/recovery`

Merge `internal/restore` into `internal/recovery` (recovery already imports restore;
importers are only `drill` and `engine`; no cycle). Result — pure, metadata-only,
"what to extract to get the data back":

```
internal/recovery/
  chain.go     Chain, Step                      (was package restore)
  asof.go      AsOf, parseAsOf                  (exported enough for the CLI's --time validation)
  tree.go      Tree, BuildTree, Collect, ExtractStep, Source
  session.go   Session
```

`ExtractStep` and `Source` embed `Step` (`ExtractStep{Step; Members []string}`,
`Source{Step; Member string}`), killing the field triplication. `drill.Target.Steps`
becomes `[]recovery.Step`. `BuildTree` keeps its member-loader **func** (it only
needs `Members`; handing a pure package an I/O store to use one method would blur
the pure/IO line). Vocabulary settles: **recovery** is the domain; *restore*
(whole-DLE) and *recover* (file-level) are its two user-facing verbs; *drill*
rehearses it.

### One execution package: `internal/restorer` (mirror of `dumper`)

Lift the decoder + restorer out of `engine` into `internal/restorer`, written over
`ReadStore` plus narrow func deps (the pattern the decoder already uses):

```go
// Deps: ReadStore; Archives func() []record.Archive (for Chain/AsOf);
// Exec func(host) programs.Executor; ArchiverFor func(type, host);
// EncryptionFor func(dle) config.EncryptConfig; compress/crypt default opts;
// DisplayDLE func(slug) string.

// Request: reconstruct one DLE as of a point in time, at a destination —
// the read-side mirror of archiver.BackupRequest.
type Request struct {
    DLE    string // catalog slug
    RunID  string // explicit target run (drill pins one); or ""
    AsOf   string // "YYYY-MM-DD[ HH[:MM[:SS]]]", resolved to a run when RunID == ""
    Dest   string // destination directory (on Host)
    Host   string // "" = extract server-side; else a configured client (--to)
    Medium string // "" = any copy with fail-over; else pinned (--from / drill source)
    Force  bool   // allow a non-empty local Dest (skip guard + rollback)
}

func (r *Restorer) Extract(req Request, logf Logf) error
func (r *Restorer) ExtractSelection(steps []recovery.ExtractStep, destDir string, logf Logf) (int, error)
func (r *Restorer) OpenRecover(dle, asOf string) (*recovery.Tree, error)
```

Everyone converges on `Extract`:

- `nb recover --all [--to] [--from]` → engine facade → `Extract{DLE, AsOf, Dest,
  Host, Medium, Force}`. Chain resolution, decode placement, known-host validation,
  empty-dest guard, rollback all inside.
- **drill chain tier** → `Extract{DLE, RunID, Dest: scratchDir, Medium: medium}`,
  then classify the returned error. `drillChain`'s hand-rolled transfer is deleted.
- File-level recover keeps `ExtractSelection` (its input is a browse selection, not
  a DLE+date) sharing the same decoder and ReadStore underneath.

`Restorer` stays a **concrete struct** — the seam with alternative implementations
is one level down (`ReadStore`: clerk vs test fake); up here the sharpness is the
request type.

Internals:

- **One-pass chain reads**: `Extract` drives the chain through
  `ReadStore.ReadArchives` (mount reuse across steps; `OrderForOnePass` already
  guarantees per-DLE level order, so a chain can never apply out of order). A
  behavior improvement over today's per-step `Open`. A `missing` ref from a chain
  read is a hard broken-chain error (wrapping `ErrMissingCopy`), never a skip.
- **`preferMedium` field, `useMedium`, and its reset closure are deleted** — medium
  is a parameter end to end.
- **Destination as one internal value**: resolve `Request` once into
  `dest{exec programs.Executor; host, dir string}` and pass that down instead of
  threading `(destDir, targetHost)` string pairs through five signatures. If a real
  second destination kind ever materializes (e.g. `--stdout` à la amrestore), this
  struct — or an `xfer.Sink` — is what graduates; nothing pre-abstracted now.
- **One `encFor(dle) config.EncryptConfig` helper** replaces the three inline
  `dleByName` + `EncryptionFor` lookups.
- `decryptHint` and `errDestSetup` move with the decoder.

**Error contract (drill classification rides on errors, not a side channel).**
`Extract` wraps with `%w` end to end so `errors.Is`/`errors.As` reach:

- `clerk.ErrMissingCopy` / `librarian.ErrVolumeUnavailable` → `drill.ClassMissing`
- `xfer.Error` with `Role == RoleSink` (tar composition) → `drill.ClassChain`
- anything else in decode/read → `drill.ClassPipeline`

A test asserts each class survives the wrapping — this is the invariant that lets
drill need no privileged API.

### Verify and drill stay in `engine`, thinner

- The **verifier** stays in engine (it is deeply catalog/placement-integrated) but
  consumes the decoder primitives now exported from `restorer`
  (checksum / structural list). Possible follow-up: move it into `restorer` if the
  engine residue gets thin; not part of this change.
- **`engine/drill.go` splits by concern**:
  - `drill.go` — options/result/report, `Drill`, `drillTarget`, tier executors
    (checksum/structural → verifier; chain → `restorer.Extract`; stock),
    `unattendedReachable`.
  - `posture.go` — `Posture` + 3-2-1-1-0 checks, `WormResult` + probe.
- **`drillStock` stays deliberately un-shared** — its entire purpose is proving
  recovery works *without* NBackup's code. Its comment says so explicitly.
- Engine keeps thin facades (`Restore`, `RestoreAsOf[To]`, `OpenRecover`,
  `ExtractSelection`, `Verify`, `Drill`) so the CLI is untouched except where noted.

### CLI

- Split the interactive shell (`recoverShell` + friends, ~400 lines) out of
  `cli/recover.go` into `cli/recover_shell.go`. File split only.
- `validateAsOfTime`'s duplicated layouts collapse onto the parse `recovery`
  exports.

## Explicit non-goals / rejected

- **No drill-facing "recovery engine" interface** — drill calls the same concrete
  `Extract` everyone uses; one implementation, one level of policy.
- **No `Destination` interface** — see above; executor + sink already carry the
  variance that exists.
- **No `archivefs` package split now** — real layering, deferred as a later pure
  file-move; nothing here blocks it (`Ref` lands in `archiveio`, correct home
  either way).
- **No change to `recovery`'s purity** — planning stays metadata-only; execution
  lives in `restorer`.
- **No back-compat shims** (greenfield): `Expect` and `clerk.Ref` are replaced at
  all use sites, not aliased.

## Steps (each compiles and passes `gofmt -l`, `go vet ./...`, `go test -race ./...`)

1. **`archiveio.Ref`** — rename `Expect` → `Ref`, delete `clerk.Ref`, update the
   clerk internals and the ~12 engine use sites. Mechanical.
2. **`archiveio.ReadStore`** — declare beside the write-side contracts; add a
   compile-time `var _ archiveio.ReadStore = (*clerk.Clerk)(nil)`; switch the
   read-side operations' fields from `*clerk.Clerk` to `archiveio.ReadStore`.
3. **Merge `internal/restore` → `internal/recovery`** — `chain.go`/`asof.go`/
   `tree.go`/`session.go`; embed `Step` in `ExtractStep`/`Source`; retarget
   `drill`, `engine/cost.go`, `engine/restore.go`, `engine/drill.go`; move tests.
4. **`internal/restorer`** — lift decoder + restorer; `Request`/`Extract`/
   `ExtractSelection`/`OpenRecover`; one-pass chain reads; delete `preferMedium`;
   internal `dest` value; `encFor` helper; error-contract test; fake-ReadStore unit
   tests (new capability — no media fixtures needed). Engine facades delegate.
5. **Drill on the unified API** — chain tier calls `Extract` into scratch and
   classifies via the error contract; delete `drillChain`'s private transfer and
   `classifyOpenErr`'s duplication where subsumed; verifier tiers unchanged; stock
   unchanged.
6. **File splits + docs** — `engine/posture.go`; `cli/recover_shell.go`;
   ARCHITECTURE.md package table (drop the `restore` row, reword `recovery`, add
   `restorer`, fix the stale clerk row that still claims the clerk owns
   Extract/ListMembers/VerifyChecksum/DecodeFilters), and state the invariants:
   *one extraction path; drills rehearse it; classification rides on wrapped
   sentinel errors*.

## Behavior changes (intended, to verify explicitly)

- Whole-DLE chain restore gains one-pass ordered reads (mount reuse across chain
  steps on the same volume). Level order is preserved by `OrderForOnePass`.
- A chain drill now exercises the *actual* `nb recover --all` code path.
- Everything else is behavior-preserving; CLI flags and output unchanged.
