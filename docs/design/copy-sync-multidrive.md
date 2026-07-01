# Copy and sync over the spool (multi-drive)

> **Status: implemented.** `nb copy` and `nb sync` re-author archives through the spool — one per
> target drive, concurrently — instead of a single drive-0 writer. Validated by hermetic tests
> (`TestMultiDriveCopyToTape`, `TestMultiDriveSyncCrossRun`) and the existing copy/sync suite.

## Why

Only `nb dump` used the spool (and so multi-drive); `CopySlot` re-authored each archive serially on
one writer. A disk→tape sync of many runs therefore wrote one archive at a time to drive 0 even on an
N-drive library. Routing copy/sync through the spool gives them drive leasing, the single orchestrator
(catalog single-writer, one robot arm), and — for free — holding-disk buffering later.

## The centralization: `withSpool`

The spool wiring — gather holding disks, build a backing per landing (the `min(workers, drives)` run
math), `spool.New`, the drain/cancel/seal lifecycle — was inline in `runOrchestrated`. It is now
`Conductor.withSpool(landings, holdingNames, spec, workers, tr, run)`: the one place that machinery
lives, keyed only on the landings and a producer callback (`run(sp)`, which builds its own route from
`sp`). It never sees the producer's item type.

- **Dump**: `landings = distinctLandings(plan)`, producer = the dumper. (unchanged behaviour)
- **Copy/sync**: `Conductor.CopyRun(target, spec, workers, run)` = `withSpool([target], nil, …)` — one
  landing, no holding buffer (yet), no progress tracker (`withSpool` no-ops phase transitions when `tr`
  is nil).

## The copier producer

A copy is the same shape as a dump — a producer feeding the spool — differing only in the *source of
bytes* and the *writer*:

- **Source**: `clerk.Open(ref, from)` returns the archive's raw part stream. Concurrency is bounded by
  the source medium: a worker pool of `workers` for a concurrent-read medium (disk/cloud), clamped to
  1 for a serial one (tape) — the librarian read path is stateful, so concurrent reads are unsafe there.
- **Writer**: `Ingest.NewCopy(arch, est)` (added beside `NewArchive`) leases a target drive and builds
  an Author over it that re-authors the archive raw — preserving its identity, checksum, and members.
  The spool's drive semaphore bounds the target side, so the effective width is
  `min(source reads, target drives)`. `Transfer` commits the writer; the caller `Close`s it to release
  the drive (the same acquire/close symmetry the dumper uses).

## Cross-run sync in one spool

`SyncTo` copies **every selected run through one `CopyRun`** (`copier.CopyRuns`), so the drives stay
saturated across run boundaries rather than draining between runs — the win when many runs each hold
few archives. Each run is validated (source copy exists; `--force` reclaims a prior target copy) and
its jobs gathered up front; then all jobs flow through one spool.

This required making the Author **run-per-archive**: the archive's own `Run` now drives its on-volume
headers, its member-index key, and its placement — not the writer's construction-time run id. For a
dump or a per-run copy the two are identical (no change); for a cross-run sync each archive files
under its own run. `NewCopy` no longer re-stamps `arch.Run` with the writer's id.

## Preserved invariants

Catalog single-writer and one-robot-arm come from the spool's orchestrator, exactly as for dump. A copy
records each archive's placement on commit (no run seal). A partial sync is safe to re-run — it is
idempotent (runs already on the target are skipped).

## Deferred

- **Holding-disk buffering for copy/sync** — `CopyRun` passes no holding disks today; wiring them
  (cloud→holding→tape, reader runs ahead of a slow drive) is a later step, and `withSpool` already
  supports it.
- **Progress/`nb status` for copy/sync** — they report through their own report/logf; giving them a
  tracker would surface the same per-archive `VOLUME` view dumps have.
