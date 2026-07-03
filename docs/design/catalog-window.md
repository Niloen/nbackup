# Two separations: catalog read/write, media faces opened at the depot

Status: implemented 2026-07-03 (this doc is the decision record; see Dropped/Deferred
for the roads not taken). Builds on docs/design/concurrent-writes.md.

## Problem

The per-medium window ownership merged in 57fb63d was sound but implicit, and it
conflated two different separations into one mechanism (the kept-media catalog
filter in the old `openReader`):

1. **Catalog consistency** — readers need stable state while a run mutates the
   catalog. It was served by `SnapshotPlacements` + `snapshotMap`, a clerk Map
   whose write methods existed only to error ("read-only by panic").
2. **Media access** — a reader must not mount a medium whose drives the spool is
   using. It was served by filtering the snapshot's placements to a pre-declared
   `kept` list — a hardware-ownership rule encoded in a data structure, and
   mostly a backstop anyway: the one in-window reader (copy/sync) pins every
   read to its source medium explicitly.

## What was built

### Catalog: View / Window (catalog/window.go)

`cat.OpenWindow()` — no media lists — returns:

- **`View`**: a point-in-time deep copy of every run's placements, read-only **by
  type** (`PlacementsFor` only). The window's read clerk is built over it
  (`clerk.ReadMap`), so `snapshotMap`'s panicking stubs are gone: there is no
  write method to stub.
- **`Window`**: the marker that a run window is open (one at a time), closed
  unconditionally when `withSpool` returns.

Durability and mutation semantics are UNCHANGED: every mutator persists per op
(atomic tmp+rename), the librarian's mid-window reads and writes (recycle,
relabel, labeling, barcodes) hit the live catalog exactly as before — which is
what makes them correct: they see this run's own placements when picking a
reusable volume. All mutation stays on the run's single writer goroutine (the
spool orchestrator and the librarians it drives). Readers never see mid-window
state because they read the View's copy.

The clerk's catalog seam split accordingly (`clerk.Map` → `ReadMap` + `Writer`):
the Clerk holds only `ReadMap`; a `Session` records through an explicit
`clerk.WriteMap` passed to `OpenRun` (the live catalog). `ReclaimStaged` takes the
Writer too. `archiveio` did not change at all.

### Media: typed faces, opened at the depot (librarian/faces.go, engine/depot.go)

Opening a medium is the acquisition. The depot mints the librarian's three
faces — the ONLY ways to reach a medium — and each face's method set IS the
access rule:

- `OpenForRead` → `librarian.ReadMedium` (ReadFileAt, MountForRead, Close):
  archive-data reads. The clerk's `MounterFor` is exactly this open. Cannot
  label, advance, or author — the methods don't exist on the type.
- `OpenForWrite` → `librarian.WriteMedium` (Name, Volume, Parallel,
  PrepareWrite, WriteSink, LazyDriveSinks, Close): run authoring. Opening takes
  the window's exclusive claim in the depot's `writeHeld` table; `Close`
  releases it (idempotent). A `clerk.Session` carries this handle (the clerk's
  `Medium` role: Name+Volume), so "which medium a write records against" has
  one source of truth — the loose medium string through the write path is gone.
- `OpenAdmin` → `librarian.AdminMedium` (Label, Load, View, Labeled,
  AppendOnly, Volume, Close): the operator face plus the passive introspection
  that posture/ledger/drill reporting needs.

`OpenForRead`/`OpenAdmin` are refused while a window write-holds the medium;
read opens are otherwise untracked (many readers share). The claim's lifecycle
is the handle's: `openWriter` wires `WriteMedium.Close` into
`PreparedWriter.Release`, and `withSpool` defers the releases to window end —
after the drain joins, so the claim spans every write. So:

- The `kept` list is GONE from `withSpool`/`OpenReader`. Nothing declares a read
  set; conflicts surface where media are touched. Copy/sync still pins its reads
  to the source medium as before; `CopyRun` rejects source == target up front so
  the operator gets a direct message rather than a failed-over read.
- The old disjointness loops are gone: read-vs-write exclusion IS the claim
  table; a double claim (one medium as landing and holding disk, or two windows)
  fails the claim.
- No lock on the table: claims happen before the window's producers start,
  release after they join, and the process runs one command.

**The Flush exception (found during implementation):** flush legitimately reads
AND writes the same holding medium — it drains staged archives off it and
reclaims them. "One owner per medium" means one *owner*, not one access mode;
flush is that owner. Flush therefore takes no claim on its holding disks (its
reclaim writes go through the raw volume, not the librarian) and shares one
`WriteMedium` per landing across the crashed runs it drains (`conductor.Flush`
opens a writer per run×landing, but the claim is per medium — the engine
memoizes the handle and authors each run's Session over it via
`prepareWriterOn`). The in-window drain reads staged archives via
`Session.OpenArchive` (by value through `CommitResult`), bypassing `MounterFor`,
so a window's own holding claim never blocks it.

## Dropped

- **Journal / batched commit.** Written, then removed on review. It bought no
  crash-safety (per-op persist is already atomic and durable per archive — the
  archive is the commit unit either way) and its performance win is negligible
  at catalog scale (a few MB rewritten per archive vs the archive itself).
  Amanda's trace log is not the precedent it appeared to be: Amanda journals for
  *reporting and history* (amreport, amstatus, amadmin find, amcleanup's
  post-crash report), which NBackup's run log already covers — not for catalog
  durability. If a huge catalog ever makes rewrite-per-archive measurable,
  batching can return behind the mutators without touching any caller.
- **One catalog per medium.** Breaks the read side: restore fail-over, run
  identity, "held anywhere else" reclaim bookkeeping, and label uniqueness are
  cross-medium questions the single Entry+Placements catalog answers from one
  file, offline.
- **Per-medium catalog write handles (`tx.On(medium)` → Recorder).** The medium
  on a placement is data, not an access right; medium separation belongs at the
  media layer.
- **A standalone MediaGuard / reservation registry.** A lock table kept in sync
  with the real resources by hand; superseded by claims taken where the window
  opens.

## Deferred (possible follow-ups, deliberately not built)

- **`OpenExclusive` (read+write in one handle)** — sketched for Flush's holding
  disks, then found unnecessary: Flush's holding writes go through the raw
  volume, never the librarian, so no combined face exists to mint.
- **Read-refusal caching in the clerk** — refusals are a map lookup, so per-read
  probing of a claimed medium costs nothing today.

## Invariants (unchanged, now with owners)

- A session never reads its own writes through the catalog; the drain's
  read-back travels by value in `CommitResult`. (Why the View copy is sound.)
- The archive is the commit unit; there is no run-level rollback. Every archive
  persisted when recorded; a failed or canceled run keeps what committed.
- One catalog writer: the spool orchestrator's goroutine (plus the librarians it
  drives inside `NextPart`).
- One owner per medium per window: writes via the claim, reads via fail-over
  past claimed media. Cross-process exclusion stays with the run lock.
