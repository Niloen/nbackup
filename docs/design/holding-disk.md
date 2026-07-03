# Holding disk

Status: implemented.

Amanda's holding disk — a fast scratch disk that absorbs parallel dumps and feeds one
tape drive at disk speed — as a **write-path buffer declared by marking one or more media
`holding: true`**, not a new sync mode or retention tier. See ARCHITECTURE.md, "Holding
disk = a marked medium…" for the concept and its place in the system; this note records
the design decisions and the roads not taken.

## The problem

The default write path is a one-pass stream (`tar → compress → encrypt → meter →
volume`). For a **tape landing** that costs two things: a single drive cannot interleave
two archives' parts, so the landing clamps `workers` to 1 (no dump parallelism); and with
no buffer a source slower than the drive shoe-shines it. Amanda's holding disk fixes both
by staging dumps on a fast disk and draining to the landing continuously — and the landing
drained to need not be tape (it may be cloud/S3), so the mechanism stays medium-neutral.

## The shape

Mark a fast disk/cloud medium `holding: true`; mark several to spread dumps across
spindles. It becomes a scratch buffer the dump flows through, while the configured
`landing` (tape/S3) stays the authoritative, catalogued/retained/planned destination.

```yaml
landing: lto
media:
  lto:     { type: tape, dir: /var/lib/nbackup/vtape, slots: 20, volume_size: 6TB }
  scratch: { type: disk, path: /var/spool/nbackup, capacity: 500GB, holding: true }
parallelism: { workers: 4 }
```

```
dumpers (N goroutines) ── tar→compress→encrypt→meter ──► holding disk(s)   (parallel, committed archives)
        │ spool.Pool.acquire picks a disk round-robin; charge bytes on commit; handoff over commitCh
        │ a DLE too big for every disk is size-routed straight to the landing (over directCh)
        ▼
orchestrator (MAIN goroutine, sole catalog writer)
        │ record holding placement ── hand one landing write at a time to the drainer ──┐
        │ ◄── the drainer's PartAllocator/Recorder seams route back here ───────────────┤ (control)
        ▼                                                                               ▼
   record landing placement ─► reclaim holding copy ─► release pool        drainer ── CopyArchive ─► landing
                                             (byte I/O only, the landing's `writers` permits at a time)

   back-pressure: every disk full ⇒ next acquire waits ⇒ (landing down) abort ⇒ run fails
```

## Decisions worth remembering

ARCHITECTURE.md already carries the shape (buffer not retention tier; the orchestrator as
sole catalog writer; the round-robin pool; live cataloguing + `nb flush` crash recovery).
The non-obvious *why* behind those choices:

**Why marking a medium beats streaming to tape.** An earlier design made the disk the
landing and streamed it to tape, which needed a copy-count retention floor, a planner
capacity override, and a special `sync` rule. Keeping the existing landing authoritative
and merely buffering it in front needs *none* of those — strictly smaller.

**Why the drainer does only byte I/O.** The landing Writer the drainer drives has its
`PartAllocator` and `Recorder` seams routed back to the orchestrator (concurrent-writes.md),
so a volume roll's catalog + librarian work still happens on the one goroutine while the
drive is never written from two. This is what lets the catalog stay lock-free.

**Why `holding: true` requires disk/cloud.** Parallel dumpers each write an *unbounded*
fslike sink — the only kind safe for concurrent `WriteArchive`: it never rolls volumes, so
it never touches the librarian's shared rolling state. A spanning sink (tape) is unsafe,
which is why a tape landing clamps to 1 worker and each drainer drives it serially.
Enforced via the `media.ConcurrentWrite` capability, not a hardcoded type list.

**Why round-robin, not pick-the-emptiest.** Bytes are charged only *after* the long dump
commits, so an emptiest-first policy would make all cold-start workers read the same
all-equal free space and herd onto one disk. A round-robin cursor spreads writes across
spindles regardless of charge timing. More disks buy dump-side write bandwidth and a
larger buffer (burst absorption); they do not raise sustained landing throughput
(`writers` on the landing is that lever).

**Why size-route an oversized DLE direct.** `capacity` is a soft back-pressure budget, not
a hard limit (the sink is unbounded), so a DLE physically larger than the disk would stage
fully and stall the gate for everyone. A DLE whose estimate meets or exceeds the largest
disk's capacity (with no unbounded holding disk) bypasses the buffer straight to the
landing. The estimate is the uncompressed chosen-level size — an upper bound — so routing
is conservative (safe to route a DLE that would have fit compressed).

## Constraints & acceptance

- **Default unchanged:** with no holding medium the run, catalog (record-at-`Finish`,
  single-writer), retention, and planner behave byte-for-byte as before.
- **Invariants preserved:** one-pass dump, server-side meter/seal, keyless
  verify/copy/sync, `Entry`/`Placement` identity. The scoped dumper/drainer split (a
  `flushing` progress state) exists only in this opt-in mode.
- **Acceptance:** one or more `holding: true` disks buffering a tape landing dump N DLEs
  in parallel; dumpers spread across disks; each disk's high-water stays at in-flight +
  un-drained; an oversized DLE lands direct; each archive reaches the landing and is
  reclaimed from its disk; a landing outage fails the run with nothing dropped; a crashed
  run's leftovers (possibly split across disks) drain via `nb flush`; the chains restore
  from the landing after the disks are reclaimed.
