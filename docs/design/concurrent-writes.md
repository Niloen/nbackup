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

## Holding disks

When a landing is slow, a dump stages onto a holding disk (still an Author over a routing `WriteStore`),
and a drain goroutine later copies it to the landing and reclaims the staged copy. The disk decouples
dumping from the drive — a stalled or waiting tape blocks only its own drain, never the dumps; a crash
leaves the staged copies recorded, and the next run flushes them.

## Lineage

The shape follows Amanda's driver/taper split — the byte-movers own the data and *report* results; one
coordinator owns media hand-out and the log — adapted to a single process, where the crossings are
shared memory and channels rather than pipes.
