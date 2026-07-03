# Two separations: catalog read/write, media faces opened at the depot

Status: implemented. Decision record; builds on docs/design/concurrent-writes.md.

## Problem

Per-medium window ownership conflated two different separations into one
mechanism (a kept-media catalog filter):

1. **Catalog consistency** — readers need stable state while a run mutates the
   catalog. It was served by a snapshot map whose write methods existed only to
   panic ("read-only by panic").
2. **Media access** — a reader must not mount a medium whose drives the spool is
   using. It was served by filtering the snapshot's placements to a pre-declared
   `kept` list — a hardware-ownership rule encoded in a data structure, and
   mostly a backstop: the one in-window reader (copy/sync) pins every read to its
   source medium explicitly.

The catalog-is-a-cache and read-snapshot invariants themselves live in
ARCHITECTURE.md ("The catalog is a cache"). This doc records how those two
separations were teased apart and the roads rejected.

## What was built

### Catalog: View / Window

`cat.OpenWindow()` — no media lists — returns:

- **`View`**: a point-in-time deep copy of every run's placements, read-only by
  type (`PlacementsFor` only). The window's `ReadMap` is built over it, so there
  is no write method left to stub.
- **`Window`**: the marker that a run window is open (one at a time), closed
  unconditionally when the run ends.

Durability is unchanged: every mutator persists per op (atomic tmp+rename). The
librarian's mid-window reads and writes (recycle, relabel, labeling, barcodes)
hit the live catalog as before — which is what makes them correct: they see this
run's own placements when picking a reusable volume. Readers never see mid-window
state because they read the View's copy. The catalog seam split into `ReadMap`
(over the View, held by readers) + `WriteMap` (the live catalog, recorded through
by a `Session`). `archiveio` did not change.

### Media: typed faces, opened at the depot

Opening a medium is the acquisition. The depot mints the three faces — the ONLY
ways to reach a medium — and each face's method set IS the access rule (see
ARCHITECTURE.md, `depot` package map row):

- `ReadMedium` (ReadFileAt, MountForRead, Close): archive-data reads. Cannot
  label, advance, or author — the methods don't exist on the type.
- `WriteMedium` (Name, Volume, Parallel, PrepareWrite, Allocator,
  LazyDriveAllocators, Close): run authoring. Opening takes the window's
  exclusive claim in the depot's `writeHeld` table; `Close` releases it. A
  `Session` carries this handle, so "which medium a write records against" has
  one source of truth.
- `AdminMedium` (Label, Load, View, Labeled, AppendOnly, Volume, Close): the
  operator face plus the passive introspection posture/ledger/drill need.

Read/admin opens are refused while a window write-holds the medium; read opens
are otherwise untracked (many readers share). The claim's lifecycle is the
handle's: `WriteMedium.Close` releases it, deferred to window end (after the
drain joins), so the claim spans every write. Consequences:

- The `kept` list is gone. Nothing declares a read set; conflicts surface where
  media are touched. Copy/sync still pins its reads to the source medium;
  `CopyRun` rejects source == target up front for a direct message.
- The old disjointness loops are gone: read-vs-write exclusion IS the claim
  table; a double claim (one medium as both landing and holding disk, or two
  windows) fails the claim.
- No lock on the table: claims happen before the window's producers start,
  release after they join, and the process runs one command.

**The Flush exception:** flush legitimately reads AND writes the same holding
medium — it drains staged archives off it and reclaims them. "One owner per
medium" means one *owner*, not one access mode; flush is that owner. It takes no
claim on its holding disks (its reclaim writes go through the raw volume, not the
librarian) and shares one `WriteMedium` per landing across the crashed runs it
drains (claim is per medium; the engine memoizes the handle and authors each
run's Session over it). The in-window drain reads staged archives by value
through `CommitResult`, bypassing `MounterFor`, so a window's own holding claim
never blocks it.

## Dropped

- **Journal / batched commit.** Written, then removed. It bought no crash-safety
  (per-op persist is already atomic and durable per archive — the archive is the
  commit unit either way) and its performance win is negligible at catalog scale.
  Amanda's trace log is not the precedent it appeared to be: Amanda journals for
  *reporting and history* (amreport, amstatus, amcleanup), which NBackup's run
  log already covers — not for catalog durability. If a huge catalog ever makes
  rewrite-per-archive measurable, batching can return behind the mutators without
  touching any caller.
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
- **`OpenExclusive` (read+write in one handle).** Sketched for Flush's holding
  disks, then found unnecessary: Flush's holding writes go through the raw
  volume, never the librarian, so no combined face exists to mint.
- **Read-refusal caching in the FS.** Refusals are a map lookup, so per-read
  probing of a claimed medium costs nothing.

## Invariants (with owners)

- A session never reads its own writes through the catalog; the drain's
  read-back travels by value in `CommitResult`. (Why the View copy is sound.)
- The archive is the commit unit; there is no run-level rollback.
- One catalog writer: the spool orchestrator's goroutine (plus the librarians it
  drives inside `NextPart`).
- One owner per medium per window: writes via the claim, reads via fail-over past
  claimed media. Cross-process exclusion stays with the run lock.
