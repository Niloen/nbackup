# Multi-drive scheduling

> **Status: implemented (within one library).** The librarian vends N drive-scoped lazy
> sinks with collision-free selection; the spool leases a drive per writer through its
> semaphore; `nb status` shows each DLE's landing tape. Validated on the 4-drive mtx
> emulator (concurrent 4-drive dump from empty, cross-drive recover byte-identical) and by
> a hermetic file-backed test (`TestMultiDriveTapeConcurrentWrite`). Cross-library arm
> parallelism remains deferred (see below). Two pre-existing tape-layer bugs the mixed
> emulator surfaced were fixed alongside: (1) `count()` returned the st driver's `fileno`
> -1 for a blank tape after MTEOM, panicking `make([]FileInfo, 0, n)` — now floored at 0;
> (2) `tapeChanger.Load` left `d.dev` bound after a *failed* load (a wrong-generation reel
> that mtx unloads-then-fails-to-load), so a later open hit a phantom tape ("no medium") —
> now the binding is cleared on load failure.


## Why

A tape library has several drives; a run should write several archives at once, one per drive, while the
catalog and the robot stay single-writer. Today the librarian schedules **drive 0 only** — every
`Load`, every `loaded()`, every roll hardcodes drive 0 — so an N-drive library writes at 1/N speed. This
is that seam.

It builds directly on [concurrent-writes.md](concurrent-writes.md): that landed the orchestrator, the
routed `WriteStore`, and a per-backing semaphore that already bounds concurrent writers to a medium.
Multi-drive is almost entirely *filling in a number that is currently pinned to 1* — plus one genuinely
new piece (per-drive tape selection).

## What already exists (do not rebuild)

- **The orchestrator** — one goroutine per run serving volume roll + catalog `Record` + drain reclaim as
  typed messages; all byte I/O stays on the worker (`spool`).
- **The per-backing semaphore** — `spool/spool.go`, `backing.free` (a channel sized to `Backing.Writers`).
  A writer leases a store index to write, releases it on close. Set in `conductor/run.go`:
  ```go
  writers := 1
  if !buffering && !pw.Serial { writers = workers }   // serial (tape) → 1
  ```
- **The holding Pool** already leases a resource per writer (`pool.Acquire(est)` → a disk index, author
  over `pool.Storage(idx)`). Leasing a **drive** is structurally identical.

So the pre-spool plan's big chunks — generalise the orchestrator to N drainers, add a librarian-facing
`AcquireWriter`/`Release`, split byte-I/O from control — are **already done**. Deleted from this plan.

## The one gap

`VolumeSink.NextPart()` (`archiveio/writer.go`) returns **one** `media.Volume` — the medium's single
loaded tape (the librarian's drive-0 proxy). The orchestrator serves *every* concurrent writer for a
backing from that one sink. Bumping `writers` to N alone would make N writers fight over one tape. The fix
is: **the medium must vend N independent rolling volumes, one per drive.**

## The design — three changes

### 1. A serial medium's parallelism is its drive count

- `conductor.PreparedWriter` gains `Drives int` (1 for a single-drive changer, disk, or cloud) beside
  the existing `Serial bool`.
- `conductor/run.go`: `if pw.Serial { writers = min(workers, pw.Drives) }` (concurrent media stay
  `workers`; buffered runs stay 1 for now — see *Deferred*).
- `engine/conduct.go` fills `Drives` from the changer (`len(changer.Drives())`, 1 for `directChanger`).

### 2. The librarian vends N drive-scoped `VolumeSink`s — the crux

Today one librarian owns one medium and drives slot 0. It becomes a **drive pool**: `Drives() int` and a
drive-scoped sink per drive, each rolling *its own* tape. Three internal reworks:

- **Every drive-0 hardcode becomes a parameter.** `loaded()` → `Drive(i).Loaded()`;
  `changer.Load(slot, 0)` → `changer.Load(slot, i)` in `advanceViaLibrary`, `findSlot`, `recycleVia*`,
  `chooseSlot`, `mount`. A `WriteSink` carries its `drive int`; its roll loads into that drive.
- **Collision-free selection.** The selection loops (`advanceViaLibrary`, `oldestReusable`) must exclude
  tapes **loaded or reserved on another drive**. This is *free from state we already have*:
  `changer.Drives()` reports each drive's loaded `Volume.Label` and `FromSlot`. The per-write `tried`
  set gains a librarian-level companion: the set of labels/slots any drive currently holds. Two drives
  never select the same tape, never load the same slot.
- **The robot is one arm.** Load/roll/select still cross the orchestrator, so they serialise there — no
  new lock. Byte I/O to each drive's `mtDevice` runs on the worker, in parallel (each drive already owns
  a persistent `mtDevice` with its own mutex).

### 3. The spool binds one writer per drive

A serial backing holds **N drive-`Store`s** (N `clerk.OpenRun`s, one over each drive-sink) instead of
one shared store. `newArchive`'s direct path leases a free drive index — the leased index *is* the
drive reservation — and authors over `b.stores[idx]`. Exactly the shape `pool.Acquire` already uses for
holding disks. No new orchestrator message type; a drive is just a backing writer that is a physical drive.

## The robot and finding tapes (barcode↔label)

Today there is **no label→barcode map**. `findSlot` locates a named tape by a **linear scan** — load
each occupied slot into drive 0, read its on-tape label. Barcodes are read cheaply from `mtx status` and
carried on `SlotStatus`/`VolumeStatus`, but used only for operator display (`room`, `View`,
`nb medium`), never selection; the catalog stores only `Label`, no barcode.

That scan does not survive concurrency: N drives each scanning would fight over slots and could load the
same slot. So a **barcode→label cache** (a `Barcode` field on `catalog.VolumeRecord`, filled whenever a
labelled tape is mounted, and matched against `mtx status` barcodes) turns `findSlot` into a lookup and
lets each drive target a *distinct* slot with no shared scan. It is **separable** from changes 1–3 (a
single drive works without it) but is what makes concurrent selection cheap and safe — so it lands with,
or just before, the write-path work rather than after.

## Invariants preserved (all for free)

- **Catalog single-writer** — placements still cross the orchestrator's `Record`.
- **One robot arm** — load/roll/select serialise on the orchestrator.
- **The "everyone above handles Volumes" contract** — callers still see `media.Volume`s; the drive pool
  is hidden inside the librarian, reached through the existing `WriteStore` seam. No new engine-facing
  API — the pre-spool `AcquireWriter`/`Release`/`Drives()` idea collapses into "vend N sinks; the spool
  leases one per writer."

## Multiple libraries — one arm each (deferred)

Multiple landings may each be a *separate physical library* with its own robot arm. Today those arms
are **not** parallelised: there is one orchestrator goroutine per run serving all backings, and a roll's
physical `Load` runs *inside* that loop (`NextPart → advance → Advance → changer.Load`), so arm B's move
waits behind arm A's — and a slow load on A head-of-lines every backing's control (`Record`/`PlaceRecord`).
Correctness is unaffected (the catalog single-writer is the point); it is only a roll-throughput ceiling.

This is fine for the common mixed-media landing (disk + cloud + *one* tape library): only one arm exists,
and the other landings' control ops are cheap. It bites only when a run drives **2+ physical libraries**.

The fix is the same insight as multi-drive — the serial unit is *the arm = the changer*, not the run —
extended across changers: **one positioning-serializer per changer** (its N drives serialise on it; two
libraries' serialisers run concurrently), with the orchestrator kept for the one genuinely global
single-writer, the catalog `Record`. The wrinkle that makes it non-trivial: a roll today does robot-move
*and* catalog mutation atomically (`verifyWritable` records the accepted label, `recycle` rewrites a
volume record), so parallel arms need the roll split into "move + read label" (per-changer, parallel) and
"record the label" (global, serial). Deferred until driving 2+ robots in one run is a real target; the
within-one-library work below does not depend on it and is not invalidated by it.

## Order of work

1. Barcode↔label cache (change in *The robot* above) — unblocks safe concurrent `findSlot`.
2. Librarian drive pool + drive-scoped sinks + collision-free selection (change 2) — the crux; test on
   the emulator (4 drives) with a single writer first (drive parameter = 0 must stay a no-op change).
3. `PreparedWriter.Drives` + `conductor` writer bound (change 1) and the spool per-drive stores (change 3)
   — wire concurrency on; validate two archives rolling on two drives in parallel, cross-drive recover
   byte-identical.

## Deferred

- **Parallel drains** — a buffered run still drains serially (`writers=1` when buffering). Multi-drive can
  drain to N drives with the same `min(workers, drives)` lever; separable, same mechanism.
- **Parallel restore reads** — `MountForRead` stays on one drive; correctness of parallel *writes*
  first.
- **`nb plan` expected-tape** — generalises from one tape to a set of ≤N; a display nuance, not blocking.
