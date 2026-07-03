# Multi-drive scheduling

Status: implemented within one library; cross-library arm parallelism is future work.

A tape library has several drives; a run should write several archives at once, one per
drive, while the catalog and the robot stay single-writer. The librarian vends N
drive-scoped rolling volumes with collision-free selection; the spool leases a drive per
writer through its per-medium semaphore; `nb status` shows each DLE's landing tape.
Builds directly on concurrent-writes.md — the orchestrator and per-medium writer permits
already bound concurrent writers to a medium; multi-drive is mostly *raising a serial
medium's permit count from 1 to its drive count*, plus one genuinely new piece: per-drive
tape selection.

## The design

**A serial medium's parallelism is its drive count.** A concurrent medium (disk, cloud)
keeps `workers`; a serial medium (tape) gets `min(workers, drives)`, where `drives` comes
from the changer (`len(changer.Drives())`, 1 for a directly-addressed medium). Bumping
the permit alone would make N writers fight over one tape — so the medium must vend N
independent rolling volumes.

**The librarian vends N drive-scoped `PartAllocator`s — the crux.** The librarian becomes
a drive pool: each drive rolls its own tape. Every former drive-0 hardcode becomes a
drive parameter (`Drive(i).Loaded()`, `changer.Load(slot, i)` across advance/select/mount);
each `Allocator` carries its `drive int` and rolls into that drive. Selection is
**collision-free from state already held**: `changer.Drives()` reports each drive's loaded
label and source slot, so the per-write `tried` set gains a librarian-level companion —
the labels/slots any drive currently holds — and two drives never select the same tape or
load the same slot. The robot is one arm: load/roll/select still cross the orchestrator,
so they serialize there with no new lock; byte I/O to each drive's device runs on the
worker, in parallel (each drive owns a persistent device with its own mutex).

**The spool binds one writer per drive.** A serial landing holds N drive-scoped write
handles instead of one shared handle; a writer leases a free drive index — the lease *is*
the reservation — exactly the shape the holding `spool.Pool` already uses for disks. No
new orchestrator message type; a drive is just a landing writer that is a physical drive.

**Finding tapes: a barcode→label cache.** Locating a named tape by linear scan (load each
occupied slot, read its on-tape label) does not survive concurrency — N drives scanning
would fight over slots. A barcode→label cache (a `Barcode` field on the volume record,
filled whenever a labelled tape is mounted and matched against the changer's cheaply-read
barcodes) turns the locate into a lookup and lets each drive target a distinct slot with
no shared scan. Separable from the write-path changes (a single drive works without it),
but it is what makes concurrent selection cheap and safe.

## Invariants preserved

Catalog single-writer (placements still cross the orchestrator's record), one robot arm
(load/roll/select serialize on the orchestrator), and the "everyone above handles
`media.Volume`s" contract (the drive pool is hidden inside the librarian, reached through
the existing `PartAllocator` seam) all hold for free.

## Multiple libraries — one arm each (deferred)

Multiple landings may each be a *separate physical library* with its own robot arm.
Today those arms are not parallelised: one orchestrator goroutine per run serves all
landings, and a roll's physical `Load` runs inside that loop, so arm B's move waits behind
arm A's — and a slow load head-of-lines every landing's control. Correctness is unaffected
(the catalog single-writer is the point); it is only a roll-throughput ceiling, and it
bites only when a run drives 2+ physical libraries (the common mixed-media landing has one
arm plus cheap disk/cloud control ops).

The fix is the same insight as multi-drive — the serial unit is *the arm = the changer*,
not the run — extended across changers: one positioning-serializer per changer (its N
drives serialize on it; two libraries' serializers run concurrently), with the
orchestrator kept only for the one genuinely global single-writer, the catalog record.
The wrinkle: a roll today does robot-move *and* catalog mutation atomically (recording the
accepted label, rewriting a volume record on recycle), so parallel arms need the roll
split into "move + read label" (per-changer, parallel) and "record the label" (global,
serial). Deferred until driving 2+ robots in one run is a real target; the within-one-
library work does not depend on it and is not invalidated by it.
