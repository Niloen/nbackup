# Copy and sync over the spool (multi-drive)

Status: implemented.

`nb copy` and `nb sync` re-write archives through the spool — one per target drive,
concurrently — instead of a single drive-0 writer. This gives them drive leasing, the
single orchestrator (catalog single-writer, one robot arm; see concurrent-writes.md and
multi-drive.md), and holding-disk buffering for free later.

## The centralization: `withSpool`

The spool wiring — gather holding disks, build a lane per landing (the
`min(workers, drives)` run math), construct the spool, run the drain/cancel/seal
lifecycle — lives in one place, `Conductor.withSpool(landings, holdingNames, …, run)`,
keyed only on the landings and a producer callback that builds its own route from the
spool. It never sees the producer's item type.

- **Dump**: landings = the plan's distinct landings, producer = the dumper.
- **Copy/sync**: `CopyRun` = `withSpool([target], nil, …)` — one landing, no holding
  buffer (yet), no progress tracker (`withSpool` no-ops phase transitions when the tracker
  is nil).

## The copier producer

A copy is the same shape as a dump — a producer feeding the spool — differing only in the
*source of bytes* and the *writer*:

- **Source**: the `archivefs.ReadStore` returns the archive's raw part stream.
  Concurrency is bounded by the source medium — a worker pool of `workers` for a
  concurrent-read medium (disk/cloud), clamped to 1 for a serial one (tape), whose
  librarian read path is stateful and unsafe to read concurrently.
- **Writer**: `Ingest.NewCopy(arch, est)` leases a target drive and builds an
  `ArchiveWriter` over it that re-writes the archive raw — preserving its identity,
  checksum, and members. The spool's drive semaphore bounds the target side, so the
  effective width is `min(source reads, target drives)`.

## Cross-run sync in one spool

`nb sync` copies **every selected run through one `CopyRun`**, so the drives stay
saturated across run boundaries rather than draining between runs — the win when many runs
each hold few archives. Each run is validated up front (source copy exists; `--force`
reclaims a prior target copy) and all jobs flow through one spool.

This required making the writer **run-per-archive**: the archive's own `Run` drives its
on-volume headers, member-index key, and placement — not the writer's construction-time
run id. For a dump or per-run copy the two are identical; for a cross-run sync each
archive files under its own run.

## Preserved invariants

Catalog single-writer and one-robot-arm come from the spool's orchestrator, exactly as
for dump. A copy records each archive's placement on commit (no run seal). A partial sync
is safe to re-run — runs already on the target are skipped (idempotent).

## Deferred

- **Holding-disk buffering for copy/sync** — `CopyRun` passes no holding disks today;
  wiring them (cloud→holding→tape, reader running ahead of a slow drive) is a later step,
  and `withSpool` already supports it.
- **Progress/`nb status` for copy/sync** — they report through their own logging; a
  tracker would surface the same per-archive `VOLUME` view dumps have.
