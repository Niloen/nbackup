---
title: Tape with a holding disk
layout: default
parent: Scenarios
nav_order: 4
description: "A fast disk buffers parallel dumps and feeds one tape drive at disk speed — Amanda's classic holding-disk shape."
---

# Tape with a holding disk
{: .no_toc }

A fast scratch disk absorbs parallel dumps, then one drainer feeds them to a single tape drive at disk speed.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## When to use this

Use this when your landing is **tape**. A single drive can't interleave two dumps,
so a tape landing clamps to one worker — and a source slower than the drive
*shoe-shines* it (the tape repeatedly stops, rewinds, and restarts because the data
doesn't arrive fast enough to keep it streaming). Amanda's **holding disk** fixes
both at once: dumps land on a fast scratch disk in **parallel**, then one drainer
copies each finished archive to the tape and frees the disk — so the drive runs at
disk speed and a small disk feeds a much larger tape.

## Configuration

```yaml
cycle: 7d

compress:
  scheme: zstd                     # zstd | gzip | none

# The tape library is the landing (the authoritative copy); the scratch disk is a
# transient buffer the dumps flow THROUGH on the way to tape.
media:
  lto:
    type: tape
    dir: /var/lib/nbackup/vtape    # a file-backed virtual library (no hardware)
    slots: 20                      # storage slots; capacity = slots × volume_size = 120TB
    drives: 1                      # data-transfer drives a robot loads slots into
    volume_size: 6TB
  scratch:
    type: disk
    path: /var/spool/nbackup
    capacity: 500GB
    holding: true                  # mark this disk as the scratch buffer
landing: lto

# Dumps run in parallel onto the holding disk; one drainer copies to tape.
parallelism:
  workers: 4

archivers:
  default:
    type: gnutar
    one-file-system: "true"

dumptypes:
  default:
    archiver: default
  no-logs:
    archiver: default
    exclude: ["*.log", "*.tmp"]

sources:
  default:
    localhost: [/home, /etc]
  no-logs:
    localhost: [/srv/www, /opt/app]
```

## Commands

```bash
nb label lto lto-0001     # label a blank tape before its first dump
nb plan                   # preview the run — announces the tape it expects
nb dump                   # dump in parallel to the holding disk, drain to tape
nb medium lto             # inventory the library: drives (loaded) + slots (barcodes)
nb flush                  # drain a crashed run's staged archives to tape
nb status                 # progress of the running (or most recent) dump
```

A tape must be labeled before its first write, so run `nb label` once per blank
reel (or enable `auto_label` — see [Robotic tape library](tape-library)).

## What happens

1. `nb dump` opens with an estimate pass, then runs up to four DLE dumps **in
   parallel**, each landing on the `scratch` holding disk.
2. As each archive commits on the holding disk, the single **drainer** copies it to
   `lto` and reclaims the disk space it used.
3. The tape drive therefore streams continuously at disk speed instead of waiting on
   any one slow source.

## What to watch

- **The `lto` landing is the authoritative copy.** The holding disk is transient — it
  only buffers the write path. While archives are staged on it they are **visible in
  the catalog**, then removed as they drain.
- **`capacity` back-pressures the dumpers.** A slow tape makes the buffer fill and the
  dumpers wait — it never overfills. A small disk safely feeds a much larger tape.
- **Oversized DLEs skip the buffer.** A DLE estimated larger than the disk dumps
  straight to tape instead of trying to stage through it.
- **A crashed run auto-drains.** Un-flushed archives stay recorded on the holding disk;
  the next `nb dump` drains them automatically, or run `nb flush` to drain explicitly.
- **Several spindles spread the load.** You may mark **several** media `holding: true`;
  the dumpers spread their writes across them (more aggregate write bandwidth and a
  larger combined buffer) and the one drainer copies them all to tape.
- **Tape prunes by whole-volume label rotation** — never per-archive. When a run needs
  a fresh volume and none is blank, the oldest tape whose every run is unprotected is
  recycled (same label, epoch bumped); see [Pruning](../features/pruning).

---

See also: [Holding disk](../features/holding-disk),
[Storage media](../features/media),
[Robotic tape library](tape-library),
[Getting Started](../getting-started).
