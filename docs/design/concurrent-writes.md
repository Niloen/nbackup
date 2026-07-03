# Concurrent writes

## Why

Per-DLE landing lets one run write several media at once — a large DLE withheld from an expensive
medium while cheap DLEs stream to it. So a run authors many archives concurrently, to different media,
while the catalog and each medium's changer must stay single-writer. This is that seam.

## The model

Everything hangs off one interface. `archiveio.WriteStore` is what an archive is written *to*: hand out
volumes (`NextPart`/`PlaceRecord`) and take the finished record (`Record`). An `archiveio.Author` — the
SDK — is built once over a `WriteStore` and authors archives (an `ArchiveWriter` per archive). The
Author does *all* the byte work — part headers, payload, footer, member index, checksum — on its own
goroutine.

Concurrency is nothing but **which `WriteStore` the Author was handed**:

- **Serial** — the Author is built straight over a `clerk.Session`; its calls run inline. (`CopyRun`,
  `Flush`, and any single-writer path.)
- **Concurrent** — the spool builds the Author over a *routing* `WriteStore` that wraps a `Session`; its
  three calls hop to one orchestrator goroutine. (Dumps.)

Two wider roles sit beside `WriteStore`, each just an addition to the one before:

```
VolumeSink   —  hand out volumes / roll                 (the librarian's changer view)
  WriteStore —  + Record                                ← what an Author writes to
    Store    —  + OpenArchive + Reclaim                 ← a holding disk (drain reads a staged
                                                           archive back, then reclaims it)
```

- **`Store`** is the full medium end; a holding disk is one.
- **`Ingest`** (`NewArchive → *ArchiveWriter`) is the dumper's *source* of writers for a landing — it
  hands out writers, it is not itself written to. Only the spool implements it.

## The orchestrator

One goroutine per run. It serves exactly the operations that must be single-writer — a volume
alloc/roll and a catalog `Record` (plus the drain's reclaim) — each as a small typed message, and
nothing else. No bulk bytes ever cross it, so a slow drive's payload can't block it; that flows on the
worker. Per-medium **writer permits** (a semaphore) bound concurrent writers to a medium: 1 for a serial tape, N
for a concurrent medium like cloud.

## Reads

Ownership inside a window is **per medium**: at window-open every medium gets exactly one owner.
The media the run writes (landings and holding disks) transfer to the spool — its orchestrator
owns them together with the live catalog, and may do what it likes with them (a holding disk is
both staged onto and drained back by its one owner). The caller keeps the rest — for a copy/sync,
the source medium — read-only. `withSpool` is the handover: it checks the two sets are disjoint
(a medium cannot be both read and written in one run) and hands the run closure a read-only
archive fs (`archiveio.ReadStore`, via `Deps.OpenReader`) built over a snapshot of the catalog's
placements taken at that moment (one goroutine, before any writer exists) and **pinned to the kept
media** — the view holds only their placements, so a read cannot resolve onto a spool-owned medium
at all, not even via the `""` any-copy fail-over; the restriction is in the data, not a check. A
dump keeps nothing (it writes media and reads only the host fs). So a reader and the orchestrator
never share the live entries — even when both touch the same run's entry (a copy reads a run's
source placement while writing its target placement) — and never share a medium either.

The snapshot leans on an invariant: **a session never reads its own writes through the catalog.**
Everything written inside the window belongs to this run; everything read (a copy's source
placements) was recorded by a previous run — so the point-in-time view is never stale for its
reader. The one same-run read-back — a drain reopening a staged archive — travels by value in
`CommitResult`, not through the catalog.

Per-medium ownership cannot cover the layer below media: two media may share one changer device
(a tape library's robot). In-process the robot's serialization is the read-clamp, not the
orchestrator — a reader needs the robot only when the source is tape, and a tape source clamps
the transfer to one worker, whose reads and routed writes strictly alternate (the write call
blocks its caller until the orchestrator replies). Cross-process it is the config lock: any
command that accesses a medium takes it (see internal/lock), so a verify or recover never drives
a robot mid-dump. If concurrent tape reads are ever built (a multi-drive library reading and
writing at once), the robot needs its own per-device owner at the changer boundary — Amanda's
chg-robot lockfile is the precedent — acquired as a leaf inside the changer op, never via the
orchestrator (wrong scope: per-run vs per-device; and a minutes-long mount must not starve
catalog writes). Note the read path is not purely reading either: loading a tape learns barcodes
into the catalog, safe only under the same clamp.

## Holding disks

When a landing is slow, a dump stages onto a holding disk (still an Author over a routing `WriteStore`),
and a drain goroutine later copies it to the landing and reclaims the staged copy. The disk decouples
dumping from the drive — a stalled or waiting tape blocks only its own drain, never the dumps; a crash
leaves the staged copies recorded, and the next run flushes them. The drains share the landing's writer
permits with direct writes: the medium's `writers` cap (defaulting to its natural width — a serial
library's drive count, else the worker count) — so a concurrent-write landing absorbs several drains at
once while a single tape drive still takes one archive at a time. The same cap gates staging onto a
holding disk (the pool skips a disk at its cap), so `writers` is one lever for a medium's write
concurrency wherever it sits in the flow.

## Lineage

The shape follows Amanda's driver/taper split — the byte-movers own the data and *report* results; one
coordinator owns media hand-out and the log — adapted to a single process, where the crossings are
shared memory and channels rather than pipes.

## Amendment (2026-07-03, fs-restructure)

The single glued `WriteStore` interface the Author was built over is gone. The
writer (`Author` → `archiveio.Writer`) is now bound to its two seams separately —
`archiveio.PartAllocator` (volume alloc/roll, from the opened `depot.WriteMedium`'s
`librarian.Allocator`) and `archiveio.Recorder` (the commit crossing, the
`archivefs.Session`) — because the glue joined seams with different owners: the
device side allocates, the fs side records. The invariant this doc establishes is
untouched: the spool routes *both* seams through its single orchestrator goroutine,
which remains the sole owner of rolls and catalog writes. `clerk` is `archivefs`,
`Session` no longer proxies allocation, and `Store` is `archivefs.WriteStore`.
See docs/design/fs-restructure.md for the full rename map.
