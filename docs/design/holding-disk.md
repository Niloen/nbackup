# Design note: holding disk (a marked medium, taper on the main goroutine)

Status: **implemented.** Amanda's holding disk — a fast scratch disk that absorbs parallel
dumps and feeds one tape drive at disk speed — implemented as a **write-path buffer declared by
marking a medium `holding: true`**, not as a new sync mode or retention tier. See
[ARCHITECTURE.md](../../ARCHITECTURE.md) ("Holding disk = a marked medium…") for how it sits in
the system.

## The problem

NBackup's default write path is a one-pass stream (`tar → compress → encrypt → meter → volume`).
For a **tape landing** that costs two things: a single drive cannot interleave two archives'
parts, so `landing: <tape>` clamps `workers` to 1 (no dump parallelism); and with no buffer a
source slower than the drive shoe-shines it. Amanda's holding disk fixes both by staging dumps
on a fast disk and draining to tape continuously. NBackup wants the same with the smallest
possible surface.

## The shape

Mark a fast disk/cloud medium **`holding: true`**. It becomes a scratch buffer the dump flows
through; the configured `landing` (tape/S3) stays the authoritative destination.

```yaml
landing: lto
media:
  lto:     { type: tape, dir: /var/lib/nbackup/vtape, bays: 20, volume_size: 6TB }
  scratch: { type: disk, path: /var/spool/nbackup, capacity: 500GB, holding: true }
parallelism: { workers: 4 }
```

```
dumpers (N goroutines) ── tar→compress→encrypt→meter ──► holding disk (parallel, committed archives)
                                                              │  OnCommit hands off (arch + positions)
                                                              ▼
taper (MAIN goroutine) ── record holding placement ─► CopyArchive ─► landing (tape) ─► record landing ─► reclaim holding
                                                              ▲
            capacity back-pressure: disk full ⇒ dumpers wait ⇒ (landing down) abort ⇒ run fails
```

## Load-bearing decisions (the *why*)

**It is a write-path buffer, not a retention tier.** The landing is the real, authoritative
medium — catalogued, retained, and planned exactly as a normal run. The holding disk is
transient. This is why it needed **no** copy-count retention floor, **no** planner capacity
override, and **no** special `sync` rule: those all came from an earlier design that made the
disk the landing and *streamed* it to tape. Marking a medium and buffering the existing landing
is strictly smaller.

**The taper runs on the main goroutine and is the sole catalog writer.** The taper does not
merely emit placements — it drives the librarian, which **records volume labels in the catalog
when a tape spans**. So it must own the catalog. The dump workers become background goroutines
that only *queue* committed archives (via the writer's `OnCommit`); they never touch the
catalog. With a single catalog writer the catalog needs **no lock and no actor** — it stays the
plain single-threaded store the rest of NBackup already assumes (the workdir flock still
serializes whole *processes*). This is the decision that kept the concurrency simple.

**Concurrency safety is the existing model, not a new one.** Parallel dumpers share one holding
`WriteSink`, and only an **unbounded** fslike sink (disk/cloud) is safe for concurrent
`WriteArchive` — it never rolls volumes, so it never touches the librarian's shared rolling
state; positions come from a mutex-guarded counter. A spanning sink (tape) is *not* safe, which
is why tape landings clamp to 1 worker. The taper drives the spanning landing **serially**. So
the two combinations used — dumpers→unbounded-disk, taper→tape-serial — are the two the
`archiveio.Writer` already documents. Hence `holding: true` requires a disk/cloud medium.

**Back-pressure is a byte gate; degraded mode refuses to proceed.** A `byteGate` sized to the
holding disk's `capacity` is charged when a dumper commits and released when the taper reclaims;
a dumper over capacity waits. If the landing fails, the taper aborts the gate — blocked dumpers
wake, the dumpers stop, and the run fails with the landing error. It never overfills the disk or
drops un-replicated data.

**The holding disk is catalogued live; crash recovery needs no scan.** The taper records each
archive's holding placement as it commits and removes it on flush, so `nb slot`/`nb dle` show
the disk's contents during a run, and a crash leaves the un-flushed archives recorded on the
holding medium in the catalog. `nb flush` (Amanda's `amflush`) — and an auto-flush at the start
of the next `nb dump` — reads those placements (no media scan), copies the archives to the
landing, and reclaims the disk. The self-describing disk remains the `nb rebuild` backstop.

## Constraints & acceptance

- **Default unchanged:** with no holding medium, the run, catalog (record-at-`Finish`,
  single-writer), retention, and planner behave byte-for-byte as before.
- **Invariants preserved:** one-pass dump, server-side meter/seal, keyless verify/copy/sync,
  `Entry`/`Placement` identity. The taper re-adds a scoped dumper/taper split (a `flushing`
  progress state), but only in this opt-in mode.
- **Acceptance:** a `holding: true` disk buffering a tape landing dumps N DLEs in parallel; the
  disk high-water stays at in-flight + un-flushed; each archive lands on tape and is reclaimed
  from disk; a landing outage fails the run with nothing dropped; a crashed run's leftovers
  drain to tape via `nb flush`; the chain restores from tape after the disk is reclaimed.
- **Verify** per repo conventions: `gofmt -l`, `go vet ./...`, `go test -race ./...`; tests use
  scheme `none` and the `dir:`-backed virtual tape library.
