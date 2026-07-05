# Concurrent writes

Status: implemented.

The run's concurrent write seam (`spool`). See ARCHITECTURE.md, "A run may write
several landings at once" and "Holding disk = a marked medium" for how it sits in the
system; this note records the concurrency model and the roads not taken.

## Why

Per-DLE landing lets one run write several media at once — a large DLE withheld from an
expensive medium while cheap DLEs stream to a cheaper one. So a run writes many archives
concurrently, to different media, while the catalog and each medium's changer must stay
single-writer.

## The model

A run's write end is an `archiveio.Writer`, which produces one `ArchiveWriter` per
archive and does *all* the byte work — part headers, payload, footer, member index,
checksum — on its own goroutine. A Writer binds two seams with different owners,
separately:

- `archiveio.PartAllocator` — volume alloc/roll (the `librarian.Allocator` behind the
  opened `depot.WriteMedium`; the device side).
- `archiveio.Recorder` — the commit crossing (`archivefs.Session`; the fs side).

Concurrency is nothing but *where those two seams point*:

- **Serial** — the seams point straight at the medium/session; calls run inline
  (`CopyRun`, `Flush`, any single-writer path).
- **Concurrent** — the spool routes *both* seams through one orchestrator goroutine, so
  a volume roll and a catalog record hop to it while bulk bytes stay on the worker
  (dumps).

`archivefs.Ingest` (`NewArchive → ArchiveSink`) is the dumper's *source* of write
handles for a landing route — it hands out sinks, it is not written to. Only the spool
implements it. `ArchiveSink` is the five-method surface a producer drives (the
`xfer.Sink` pair plus Close/Committed/Meter): a single-landing route gets the bare
`*archiveio.ArchiveWriter`; a fan-out route (`landing: [s3, gdrive]`) gets an
`archiveio.Tee` fanning one stream into a writer per lane, all cutting parts at the SAME
boundaries (the minimum cap), so every copy carries identical per-part seals and no
re-parting or per-lane goroutine exists. On the staged path the fan-out is N parallel
drains per archive instead, with the holding copy reclaimed by the last one; failure is
any-lane-suffices (a failed landing trips for the rest of the run, warned with the
`nb sync` repair; only a route with no survivor aborts).

## The orchestrator

One goroutine per run. It serves exactly the operations that must be single-writer — a
volume alloc/roll and a catalog record (plus a drain's reclaim) — each as a small typed
message, and nothing else. No bulk bytes cross it, so a slow drive's payload can't block
it. Per-medium **writer permits** (a semaphore) bound concurrent writers to a medium: 1
for a serial tape, N for a concurrent medium like cloud. Because every catalog write
lands on this one goroutine, the catalog needs no lock and no per-run actor — it stays
the single-threaded store "One mutating `nb`" already assumes.

## Reads: per-medium window ownership

Ownership inside a run window is **per medium**: at window-open every medium gets exactly
one owner. The media the run writes (landings and holding disks) transfer to the spool —
its orchestrator owns them together with the live catalog. The caller keeps the rest
read-only (for a copy/sync, the source medium). `withSpool` is the handover: it checks
the two sets are disjoint (a medium cannot be both read and written in one run) and hands
the run closure a read-only `archivefs.ReadStore` (via `Depot`) built over a snapshot of
the catalog's placements taken at that moment — one goroutine, before any writer exists —
and **pinned to the kept media**, so a read cannot even resolve onto a spool-owned medium
via the `""` any-copy fail-over. The restriction lives in the data, not a check.

The snapshot leans on an invariant: **a session never reads its own writes through the
catalog.** Everything written inside the window belongs to this run; everything read (a
copy's source placements) was recorded by a previous run — so the point-in-time view is
never stale for its reader. The one same-run read-back — a drain reopening a staged
archive — travels by value, not through the catalog.

**The layer below media — a shared changer (a tape library's robot) — is not covered by
per-medium ownership.** In-process the robot's serialization is the read-clamp, not the
orchestrator: a reader needs the robot only when the source is tape, and a tape source
clamps the transfer to one worker, whose reads and routed writes strictly alternate.
Cross-process it is the config lock (internal/lock): any command touching a medium takes
it, so a verify or recover never drives a robot mid-dump. If concurrent tape reads are
ever built (a multi-drive library reading and writing at once), the robot needs its own
per-device owner at the changer boundary — Amanda's chg-robot lockfile is the precedent —
acquired as a leaf inside the changer op, never via the orchestrator (wrong scope:
per-run vs per-device; a minutes-long mount must not starve catalog writes).

## Lineage

The shape follows Amanda's driver/taper split — the byte-movers own the data and
*report* results; one coordinator owns media hand-out and the log — adapted to a single
process, where the crossings are shared memory and channels rather than pipes.
