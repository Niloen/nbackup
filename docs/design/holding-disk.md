# Design note: holding disk (a marked medium, the orchestrator on the main goroutine)

Status: **implemented.** Amanda's holding disk — a fast scratch disk that absorbs parallel
dumps and feeds one tape drive at disk speed — implemented as a **write-path buffer declared by
marking one or more media `holding: true`**, not as a new sync mode or retention tier. See
[ARCHITECTURE.md](../../ARCHITECTURE.md) ("Holding disk = a marked medium…") for how it sits in
the system.

## The problem

NBackup's default write path is a one-pass stream (`tar → compress → encrypt → meter → volume`).
For a **tape landing** that costs two things: a single drive cannot interleave two archives'
parts, so `landing: <tape>` clamps `workers` to 1 (no dump parallelism); and with no buffer a
source slower than the drive shoe-shines it. Amanda's holding disk fixes both by staging dumps
on a fast disk and draining to the landing continuously. NBackup wants the same with the smallest
possible surface — and the landing it drains to need not be tape (it may be cloud/S3), so the
mechanism is kept medium-neutral.

## The shape

Mark a fast disk/cloud medium **`holding: true`**. It becomes a scratch buffer the dump flows
through; the configured `landing` (tape/S3) stays the authoritative destination. Mark **several**
to spread the dumps across spindles.

```yaml
landing: lto
media:
  lto:     { type: tape, dir: /var/lib/nbackup/vtape, bays: 20, volume_size: 6TB }
  scratch: { type: disk, path: /var/spool/nbackup, capacity: 500GB, holding: true }
parallelism: { workers: 4 }
```

```
dumpers (N goroutines) ── tar→compress→encrypt→meter ──► holding disk(s)   (parallel, committed archives)
        │ holdingPool.acquire picks a disk round-robin; charge bytes on commit; handoff over commitCh
        │ a DLE too big for every disk is size-routed straight to the landing (over directCh)
        ▼
orchestrator (MAIN goroutine, sole catalog writer)
        │ record holding placement ── hand one landing write at a time to the drainer ──┐
        │ ◄── proxy VolumeSink funnels the drainer's NextPart/PlaceRecord back here ─────┤ (control)
        ▼                                                                               ▼
   record landing placement ─► reclaim holding copy ─► release pool        drainer ── CopyArchive ─► landing
                                                                           (byte I/O only, one serial writer)

   back-pressure: every disk full ⇒ next acquire waits ⇒ (landing down) abort ⇒ run fails
```

## Load-bearing decisions (the *why*)

**It is a write-path buffer, not a retention tier.** The landing is the real, authoritative
medium — catalogued, retained, and planned exactly as a normal run. The holding disk is
transient. This is why it needed **no** copy-count retention floor, **no** planner capacity
override, and **no** special `sync` rule: those all came from an earlier design that made the
disk the landing and *streamed* it to tape. Marking a medium and buffering the existing landing
is strictly smaller.

**The orchestrator runs on the main goroutine and is the sole catalog writer.** Catalog
mutations — holding placements, landing placements, and the **volume labels the librarian records
when a landing spans** — must all happen on one goroutine, so the catalog needs **no lock and no
actor** and stays the plain single-threaded store the rest of NBackup already assumes (the workdir
flock still serializes whole *processes*). The dump workers become background goroutines that only
*queue* committed archives over a channel; they never touch the catalog. This is the decision that
kept the concurrency simple.

**Control and data are split by a proxy `VolumeSink` funnel, not a writer hook.** The drainer that
copies a staged archive to the landing does only **catalog-free byte I/O**. The landing
`archiveio.Writer` it drives is built over a **proxy `VolumeSink`** whose `NextPart`/`PlaceRecord`
— the control calls where a volume roll touches the catalog and drives the librarian — **funnel
back to the orchestrator over a channel**; the orchestrator runs the real sink and replies. The
round-trip is synchronous, so the landing drive is never written from two goroutines, yet every
catalog write still lands on the orchestrator. (An earlier sketch handed archives off via an
`OnCommit` writer hook; the funnel is more direct and keeps `archiveio.Writer`/the librarian
unchanged.) The placement record and the holding reclaim are the orchestrator's "control half"
(`finalizeDrain`); the byte copy is the drainer's "data half" (`copyOne`).

**Concurrency safety is the existing model, not a new one.** Parallel dumpers each write an
*unbounded* fslike sink (disk/cloud) — the only kind safe for concurrent `WriteArchive`: it never
rolls volumes, so it never touches the librarian's shared rolling state; positions come from a
mutex-guarded counter. A spanning sink (tape) is *not* safe, which is why a tape landing clamps to
1 worker. The single drainer drives the spanning landing **serially**. So the two combinations
used — dumpers→unbounded-disk, drainer→landing-serial — are the two the `archiveio.Writer` already
documents. Hence `holding: true` requires a disk/cloud medium (checked via the
`media.ConcurrentWrite` capability, not a hardcoded type list).

**Several holding disks share a `holdingPool`; allocation is round-robin.** Each disk is a
`{writer, session, volume, capacity, used}`; the pool (one mutex+cond) hands a worker a disk per
dump. Allocation is **round-robin, skipping full or too-small disks** — chosen over "pick the
emptiest" because bytes are charged only *after* the (long) dump commits, so an emptiest-first
policy would make all cold-start workers read the same all-equal free space and herd onto one
disk; a round-robin cursor spreads writes across spindles regardless of charge timing. More disks
buy **dump-side write bandwidth** and a **larger combined buffer** (burst absorption); they do not
raise sustained landing throughput (the drain stays one serial writer). The handoff carries an
**int disk index**, so the drainer reads, reclaims, and releases the disk the archive landed on.

**Back-pressure is the pool; degraded mode refuses to proceed.** A disk's bytes are charged when a
dumper commits and released when the orchestrator reclaims the drained copy (actual compressed
bytes — *no* up-front reservation, so a bad estimate is still corrected by the real charge). The
**next** `acquire` is the gate: it blocks while every eligible disk is over capacity. If the
landing fails, the orchestrator aborts the pool — blocked dumpers wake, the dumpers stop, and the
run fails with the landing error. It never overfills a disk or drops un-replicated data.

**A DLE too big for every disk is size-routed direct.** The holding sink is unbounded, so
`capacity` is a soft back-pressure budget, not a hard limit — a DLE physically larger than the
disk would stage fully and stall the gate for everyone while it drained. So a DLE whose estimate
meets or exceeds the largest disk's capacity (and there is no unbounded holding disk) **bypasses
the buffer**: the worker routes it over `directCh` and the orchestrator dumps it straight to the
landing through the drainer. The estimate is the uncompressed chosen-level size — an upper bound on
the stored bytes — so routing is conservative (it may route a DLE direct that would have fit
compressed, which is always safe). An unbounded holding disk in the pool ⇒ nothing ever routes
direct.

**The holding disk is catalogued live; crash recovery needs no scan.** The orchestrator records
each archive's holding placement as it commits and removes it on drain, so `nb slot`/`nb dle` show
the disks' contents during a run, and a crash leaves the un-flushed archives recorded on the
holding media in the catalog. `nb flush` (Amanda's `amflush`) — and an auto-flush at the start of
the next `nb dump` — gathers the staged slots across **every** holding disk (no media scan), opens
the landing once per slot, copies each disk's archives to the landing, reclaims the disks, and
seals. The self-describing disks remain the `nb rebuild` backstop.

## Constraints & acceptance

- **Default unchanged:** with no holding medium, the run, catalog (record-at-`Finish`,
  single-writer), retention, and planner behave byte-for-byte as before.
- **Invariants preserved:** one-pass dump, server-side meter/seal, keyless verify/copy/sync,
  `Entry`/`Placement` identity. The orchestrator re-adds a scoped dumper/drainer split (a
  `flushing` progress state), but only in this opt-in mode.
- **Acceptance:** one or more `holding: true` disks buffering a tape landing dump N DLEs in
  parallel; the dumpers spread across the disks; each disk's high-water stays at in-flight +
  un-drained; an oversized DLE lands direct on the landing; each archive lands on the landing and
  is reclaimed from its disk; a landing outage fails the run with nothing dropped; a crashed run's
  leftovers (possibly split across disks) drain via `nb flush`; the chains restore from the
  landing after the disks are reclaimed.
- **Verify** per repo conventions: `gofmt -l`, `go vet ./...`, `go test -race ./...`; tests use
  scheme `none` and the `dir:`-backed virtual tape library.
