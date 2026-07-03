# Recovery reorganization — one planning package, one extraction path, a read-side fs

Status: implemented.

For the shipped package boundaries see ARCHITECTURE.md's package map
(`recovery`, `restorer`, `archivefs`, `archiveio`). This doc records *why* the
recovery side is shaped that way and the roads rejected.

## Problem

The recovery chain (everything that reads backups back) was split awkwardly:

- **Pure planning was spread over three leaf packages** for one domain — chain
  computation, as-of resolution / browse / session, and drill taxonomy —
  with four near-identical structs each carrying `Run/DLE/Level/Archiver/
  Compress/Encrypt`.
- **Drill had a private restore path.** The engine claimed one extraction path,
  but the chain-tier drill hand-composed its own transfer. So a green chain drill
  proved a *copy* of the restore path worked — not that `nb recover --all` works.
- **Two read-driving idioms.** File-level recover and drill used an ordered
  one-pass read (mount reuse); the whole-DLE chain restore opened each step
  individually and forwent mount reuse. Medium pinning was likewise done two
  ways (a mutable `preferMedium` field vs. an explicit parameter).
- **The recovery operations held the FS concretely**, though they used exactly
  three methods — the read face was a sharp contract that existed only implicitly.
  The write side had already solved this shape: one writer written over an
  abstract medium end, with a serial and a concurrent implementation behind it.

## The read-side archive fs mirrors the write side

The write side abstracts the **medium end**, never the client end — the client
stays an executor plus an archiver stage. The read side mirrors that exactly:

- **Medium end**: `archivefs.ReadStore` (a logical `archiveio.Ref` → raw
  on-medium bytes with copy selection + fail-over, an ordered one-pass read over
  a selection, and an archive's member list), implemented by `FS`. It speaks only
  refs and bytes — no catalog types, no schemes, no transfers (those live in the
  operations, as on the write side).
- **Client end (the extraction destination) stays concrete**: an executor for
  the host plus the archiver's restore stage as the sink. There is **no
  `Destination` interface** — local-vs-remote is already the executor, and where
  destinations genuinely vary (extract / `tar -t` list / hash) the seam is the
  existing `xfer.Sink`. `Host`/`Dest` stay plain data on the request because they
  are *policy inputs* (decrypt placement, feasibility gate, empty-dest guard,
  rollback), not hidable plumbing.

`Ref` lives in `archiveio`, not `record` (record is on-disk formats only),
because the block layer already needs it: the parts reader asserts identity
against each part's header.

## One pure planning package: `internal/recovery`

`internal/restore` merged into `internal/recovery` (recovery already imported
restore; no cycle). The result is pure, metadata-only "what to extract to get the
data back": chain, as-of resolution, the browse tree + per-archive file
selection, and the browse session. `ExtractStep`/`Source` embed the shared `Step`
(killing the field triplication); `drill.Target.Steps` is `[]recovery.Step`.
`BuildTree` keeps its member-loader **func** — it needs only `Members`, and
handing a pure package an I/O store to use one method would blur the pure/IO line.

Vocabulary settles: **recovery** is the domain; *restore* (whole-DLE) and
*recover* (file-level) are its two user-facing verbs; *drill* rehearses it.

## One execution package: `internal/restorer` (mirror of `dumper`)

The decoder + restorer lifted out of `engine` into `internal/restorer`, written
over `archivefs.ReadStore` plus narrow resolution funcs (so it tests over fakes,
no media fixtures). Everyone converges on one `Extract(Request)` — the read-side
mirror of `archiver.BackupRequest`. A minimal shape of the request:

```go
type Request struct {
    DLE    string // catalog slug
    RunID  string // explicit target run (drill pins one); or ""
    AsOf   string // resolved to a run when RunID == ""
    Dest   string // destination directory (on Host)
    Host   string // "" = extract server-side; else a configured client (--to)
    Medium string // "" = any copy with fail-over; else pinned (--from / drill)
    Force  bool   // allow a non-empty local Dest (skip guard + rollback)
}
```

- `nb recover --all [--to] [--from]` → engine facade → `Extract`. Chain
  resolution, decode placement, known-host validation, empty-dest guard, and
  rollback all live inside.
- The **drill chain tier** calls the same `Extract` into a scratch dir and
  classifies the returned error — no hand-rolled transfer.
- File-level recover keeps `ExtractSelection` (its input is a browse selection,
  not a DLE+date), sharing the same decoder and `ReadStore` underneath.

`Restorer` stays a **concrete struct** — the seam with alternative
implementations is one level down (`ReadStore`: FS vs. test fake); up here the
sharpness is the request type.

`Extract` drives the chain through `ReadArchives` (mount reuse across steps;
per-DLE level order is guaranteed, so a chain can never apply out of order). A
`missing` ref from a chain read is a hard broken-chain error, never a skip.
Medium is a parameter end to end.

### Error contract drives drill classification (the invariant)

Drill classification rides on wrapped errors, not a side channel. `Extract` wraps
with `%w` end to end so `errors.Is`/`errors.As` reach:

- the missing-copy / volume-unavailable sentinels → `drill.ClassMissing`
- an `xfer.Error` with `Role == RoleSink` (tar composition) → `drill.ClassChain`
- anything else in decode/read → `drill.ClassPipeline`

A test asserts each class survives the wrapping. This is what lets drill rehearse
the *actual* restore path and need no privileged API. (Decompress fuses with tar
only for a remote target; locally it runs in the filters, so a Sink-role fault
stays exactly a tar/composition fault — the classification the drill relies on.)

## Verify and drill stay in `engine`, thinner

The **verifier** stays in engine (deeply catalog/placement-integrated) but
consumes the decode primitives exported from `restorer`. `engine/drill.go` splits
by concern: tier execution (`drill.go`) vs. the 3-2-1-1-0 posture audit + WORM
probe (`posture.go`). `drillStock` stays deliberately un-shared — its purpose is
proving recovery works *without* NBackup's code. Engine keeps thin facades so the
CLI is untouched.

## Explicit non-goals / rejected

- **No drill-facing "recovery engine" interface** — drill calls the same concrete
  `Extract` everyone uses; one implementation, one level of policy.
- **No `Destination` interface** — executor + sink already carry the variance that
  exists. If a real second destination kind ever materializes (e.g. `--stdout` à
  la amrestore), the internal `dest` value or an `xfer.Sink` is what graduates;
  nothing pre-abstracted now.
- **No change to `recovery`'s purity** — planning stays metadata-only; execution
  lives in `restorer`.
