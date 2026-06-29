---
title: Holding disk
layout: default
parent: Features
nav_order: 9
description: "A fast scratch disk that absorbs parallel dumps and feeds one slow tape drive (or a bandwidth-capped cloud) at disk speed."
---

# Holding disk
{: .no_toc }

A fast scratch disk that absorbs parallel dumps and feeds one slow tape drive (or a bandwidth-capped cloud) at disk speed.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## The problem

A tape landing normally clamps to a single worker: one drive cannot interleave two
dumps, so everything funnels through it one at a time. Worse, a source slower than the
drive *shoe-shines* it — the tape repeatedly stops, rewinds, and restarts because the
data does not arrive fast enough to keep it streaming.

Amanda's **holding disk** fixes both. Mark a fast disk (or cloud) medium with
`holding: true` and it becomes a scratch buffer the dump flows *through*. Dumps land on
the buffer in **parallel**, then one drainer copies each finished archive to the landing
and frees the disk. The drive runs at disk speed, and a small disk can feed a much
larger tape.

## Configuration

```yaml
landing: lto
media:
  lto:     { type: tape, dir: /var/lib/nbackup/vtape, bays: 20, volume_size: 6TB }
  scratch: { type: disk, path: /var/spool/nbackup, capacity: 500GB, holding: true }
parallelism: { workers: 4 }
```

Here four workers dump in parallel onto the `scratch` disk while one drainer copies each
completed archive onto `lto`.

## How it behaves

- **The landing stays authoritative.** `lto` remains the real, retained copy. The
  holding disk is transient — it only buffers the write path. While archives are staged
  on it they are **visible in the catalog**, then removed as they drain.
- **`capacity` back-pressures the dumpers.** When the tape is slow, the buffer fills and
  the dumpers wait — it never overfills. A small disk safely feeds a much larger tape.
- **Oversized DLEs skip the buffer.** A DLE estimated larger than the disk dumps
  straight to the landing instead of trying to stage through it.
- **No data is dropped on failure.** If the landing is unreachable the run fails rather
  than silently discarding what it buffered.
- **Crash recovery is automatic.** A crashed run's un-flushed archives stay recorded on
  the holding disk. The next `nb dump` auto-drains them, or run `nb flush` to drain them
  explicitly:

  ```bash
  nb flush
  ```

## Several holding disks

A holding disk must be a disk or cloud medium — never the landing. You may mark
**several** media `holding: true`:

```yaml
media:
  lto:      { type: tape, dir: /var/lib/nbackup/vtape, bays: 20, volume_size: 6TB }
  scratch1: { type: disk, path: /mnt/ssd1, capacity: 500GB, holding: true }
  scratch2: { type: disk, path: /mnt/ssd2, capacity: 500GB, holding: true }
```

The dumpers spread their writes across all of them — more spindles means more aggregate
write bandwidth and a larger combined buffer — and the one drainer copies them all to
the landing.

## When you need it

- A **tape landing** where you want parallel dumps feeding a single drive at full speed.
- A **cloud landing** where you want parallel local dumps that then drain to a
  bandwidth-capped tier.

---

See also: [Storage media](media), [Replication](replication),
[Tape with a holding disk](../scenarios/tape-holding-disk),
[S3 with a holding disk](../scenarios/s3-holding-disk).
