---
title: Robotic tape library
layout: default
parent: Scenarios
nav_order: 6
description: "A changer with drives fed from slots, automatic labeling, label rotation, and cross-tape spanning."
---

# Robotic tape library
{: .no_toc }

A tape library — drives fed from slots, where a robot loads whichever slot it needs — with automatic labeling, whole-volume label rotation, and cross-tape spanning.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## When to use this

Use this when your tape is a **robotic library**: a set of drives fed from many
storage slots, where a robot loads whichever slot it needs. NBackup addresses the
slots, reads each tape's label after loading it into a drive, and rolls from one
tape to the next on its own — so runs pack onto tapes, fill them, span across them,
and recycle aged-out volumes without an operator standing by.

## Configuration

```yaml
cycle: 7d

compress:
  scheme: zstd                     # zstd | gzip | none

media:
  lto:
    type: tape
    dir: /var/lib/nbackup/vtape    # a virtual library (no hardware); also takes a
                                   # bucket URL (s3://…) to put the library in a store
    slots: 20                      # storage slots; capacity = slots × volume_size = 120TB
    drives: 1                      # data-transfer drives a robot loads slots into
    volume_size: 6TB               # a write past this hits end-of-tape and spans
    appendable: true               # pack many runs per tape (false = one run per tape)
landing: lto

# Let a dump label a blank tape itself instead of requiring `nb label` first.
auto_label: true

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

To feed the drive at full speed with parallel dumps, add a scratch buffer — see
[Tape with a holding disk](tape-holding-disk).

## Commands

```bash
nb label lto lto-0001        # label a blank slot (or rely on auto_label)
nb medium lto                # inventory the library: drives (what's loaded) + slots (barcodes)
nb load lto 2                # load slot 2 (or: nb load --label lto lto-0007)
nb plan                      # preview the run — announces the expected/next tape
nb dump                      # dump; rolls across tapes automatically as they fill
nb prune lto                 # reclaim — whole volumes only, by label rotation
```

## Key behaviors

- **Labels are verified before every write.** Each tape carries a self-describing
  identity label. NBackup reads it after loading a slot into a drive and checks it before
  writing, so a foreign or wrong-pool reel is **never clobbered**.
- **`appendable: true` packs many runs per tape**; `appendable: false` writes one run per
  tape (Amanda-style).
- **A run that fills a tape mid-write spans onto the next automatically** — splitting even
  a single large archive. The robot loads the next writable slot: a blank (auto-labeled),
  or, when none is blank, the oldest tape past retention recycled in place.
- **Whole-volume recycle / label rotation (Amanda's *tapecycle*).** When a run needs a
  fresh volume and none is blank, NBackup reuses the **oldest tape whose every run is
  unprotected** — keeping the same label name and advancing only its epoch — and
  **announces** which tape it wants (in `nb plan`, the run output, and any swap prompt).
- **If every tape still holds a protected run, the run FAILS LOUDLY** rather than
  overwriting one — recoverability outranks capacity. `nb label --relabel lto <name>` is
  the manual early-recycle override.
- **Tape reclaims whole volumes, never per-archive.** Capacity is `slots × volume_size`;
  `nb prune` rotates labels rather than deleting individual archives from a tape. See
  [Pruning](../features/pruning).

## Restore

Restore loads whichever tape holds the copy it needs. A **spanned** archive reassembles
by loading its tapes in order.

## Real hardware: a SCSI changer

The config above uses a **virtual** library (`dir:`) so it runs with no hardware. A
real robotic library is the same medium with two fields swapped in: `changer:` (the
library's SCSI control node) and `device:` (its tape drive nodes), driven via
`mtx(1)`. Everything else — labeling, spanning, recycle, restore — is identical.

```yaml
media:
  lto:
    type: tape
    changer: /dev/sg0              # the robot's control (sg) node — mtx talks to this
    device: /dev/nst0,/dev/nst1    # the drive nodes, IN THE LIBRARY'S DRIVE ORDER
    part_size: 6TB                 # bound parts; a real drive can't see its own fill
    minimum_age: 180d
    appendable: true
landing: lto
```

`slots:`/`drives:`/`volume_size:` do not apply here — the library reports its own
slots, drives, and barcodes (`nb medium lto` shows them). `mtx` must be on `PATH`.

**Drive order is load-bearing.** `device:` lists the drive nodes in the changer's
own **drive order**: the first node is drive 0 (the robot's first data-transfer
element), the second is drive 1, and so on. This is **not** the numeric `/dev/nstN`
order — a library's drive 0 is often `/dev/nst7`. Get it wrong and a load puts the
tape in one drive while NBackup reads another; it fails fast with *"no tape loaded …
check `device:` order"* rather than hanging, but it won't run. To find the order,
load a slot into drive 0 and see which node comes online:

```bash
mtx -f /dev/sg0 load 1 0
for d in /dev/nst*; do mt -f $d status 2>/dev/null | grep -q ONLINE && echo "drive 0 = $d"; done
mtx -f /dev/sg0 unload 1 0
# repeat for `load 1 1`, `load 1 2`, … to map every drive, then list them in that order.
nb medium lto     # confirms: each drive row shows its NODE
```

**Multiple drives run in parallel.** List two or more nodes and NBackup schedules
`parallelism.workers` dumps across the drives at once — each worker gets its own
drive and its own tape. (To instead feed a *single* slow drive at full speed, put a
[holding disk](tape-holding-disk) in front of it.)

## Single-drive variants

A single drive you change by hand — a file-backed manual drive (`manual: true`) or a real
device (`device: /dev/nst0`) — shows only the reel currently loaded and **prompts you to
swap** a tape when a run or restore needs a different one (an unattended run errors
instead of hanging). See [Storage media](../features/media).

---

See also: [Storage media](../features/media),
[Pruning](../features/pruning),
[Tape with a holding disk](tape-holding-disk),
[Getting Started](../getting-started).
