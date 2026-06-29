---
title: Storage media
layout: default
parent: Features
nav_order: 3
description: "Disk, tape, and cloud object stores — three medium types behind one self-describing artifact, plus capacity and bandwidth controls."
---

# Storage media
{: .no_toc }

Disk, tape, and cloud object stores — three medium types behind one self-describing artifact.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## The `media:` map

Storage is a map of **named definitions**. Each entry has a `type` and a set of
type-specific parameters; `landing:` names the medium that new slots are written
to:

```yaml
media:
  disk:
    type: disk
    path: /var/lib/nbackup/catalog
    capacity: 20TB
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1
    capacity: 50TB
landing: disk
```

There are three types — **disk**, **tape**, and **cloud** — but they all produce
the same self-describing artifact: an archive restores with stock tools whether
it landed on a filesystem, a tape, or an object store. Adding a new medium type
is a code-level registration; it needs no change to the config structure.

Any medium can also be a **replication target**: dump to one, then mirror sealed
slots onto another with [Replication](replication). Capacity and retention are set
per medium, not globally.

## Disk

```yaml
media:
  disk:
    type: disk
    path: /var/lib/nbackup/catalog   # where slots are written (local dir or NFS)
    capacity: 20TB                   # space NBackup may use here
```

A disk medium is **address-identified**: a file's path names it, so there are no
labels, swaps, or inventory. Each archive lands as a clean payload object plus a
small `.hdr` sidecar, in a human-friendly directory layout you can browse with
`ls` — one slot is a directory, one archive is three numbered files (payload,
member index, commit footer). A plain copy of the payload restores with `tar`.

## Cloud

```yaml
media:
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1
    # prefix: nbackup/   # optional: confine all keys under a folder in the bucket
    capacity: 50TB
```

A cloud medium stores slots in an **object store**. The `url` scheme selects the
backend:

| Scheme | Backend |
|---|---|
| `s3://` | Amazon S3 — and any S3-compatible store (MinIO, Cloudflare R2, Backblaze B2, Wasabi, Synology C2) |
| `gs://` | Google Cloud Storage |
| `azblob://` | Azure Blob Storage |

Like disk, a cloud medium is **address-identified** — no labels, no swap prompts,
nothing to inventory. It just lands and reclaims slots within its `capacity`. The
on-store layout is **disk's, verbatim**: each archive is a clean `.tar.<scheme>`
object plus a header sidecar, so a slot streams disk↔cloud unchanged and a plain
GET yields an archive any stock tool can restore.

### S3-compatible custom endpoints

For stores that speak the S3 protocol at their own URL, add an `endpoint=` query
parameter:

```yaml
media:
  offsite:
    type: cloud
    url: s3://my-bucket?region=eu-005&endpoint=https://s3.example.com
    capacity: 500GB
```

The `endpoint` and `region` parameters are passed straight to the AWS SDK v2
(`V2ConfigFromURLParams`), so any of its URL options work.

### Credentials come from the environment

NBackup never reads cloud credentials from the config file. They come from each
SDK's standard environment:

- S3: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, `~/.aws/credentials`, or an IAM role
- GCS: `GOOGLE_APPLICATION_CREDENTIALS`
- Azure: the `AZURE_*` variables

File-API stores like Google Drive are out of scope — this is an object-store
abstraction, not a generic filesystem driver.

## Tape

```yaml
media:
  lto:
    type: tape
```

A tape medium comes in shapes that differ in **who changes the tape**:

- A **robotic library** — `dir:` (a file-backed virtual library), `bays: N`
  physical positions, a finite `volume_size` per tape. A command moves which bay
  is mounted.
- A **single drive changed by hand** — either a disk-emulated station
  (`mode: manual`, reels that sit on a shelf), or a real drive (`device:` driven
  via `mt`). It shows only the reel currently loaded.

When a backup or restore needs a different tape, NBackup **prompts you to swap it
in and waits**. An unattended run errors instead of hanging.

### Labelling and loading

Tapes are **labelled**, not address-identified. You label a blank, inventory a
medium, and load a tape:

```bash
nb label lto lto-0001     # label a blank
nb medium lto             # inventory: bays (or drive + shelf) and their labels
nb load lto bay-02        # mount a bay (or: nb load --label lto lto-0007)
```

Each tape carries a self-describing label that NBackup **verifies before every
write**, so a foreign or wrong-pool reel is never clobbered.

### Appending and spanning

```yaml
media:
  lto:
    type: tape
    dir: /var/lib/nbackup/vtape
    bays: 20
    volume_size: 6TB
    appendable: true        # default: pack many runs per tape; false = one run per tape
```

`appendable: true` (the default) packs many runs onto one tape; `appendable:
false` writes one run per tape. A run that fills a tape mid-write **spans onto the
next automatically** — a robotic library mounts the next writable bay (auto-labelling
a blank, or recycling the oldest tape past retention), while a manual drive prompts
for a swap. See [Robotic tape library](../scenarios/tape-library) for the full
walkthrough.

A tape medium's capacity derives as `bays × volume_size` (`0` = unbounded).

## Inspecting media

```bash
nb medium          # overview: each medium's type, slots, usage / capacity, volume
nb medium lto      # one medium's volume + slots (incl. bays, or drive + shelf)
```

`nb medium` with no argument lists every medium; with a name it details that one
— a disk or cloud medium's usage, or a tape medium's bays (library) or loaded
drive plus shelf (single drive).

## Capacity (per medium)

Capacity is the headline knob, set **per medium**:

```yaml
media:
  disk: { type: disk, path: /var/lib/nbackup/catalog, capacity: 20TB }
  lto:  { type: tape, dir: /var/lib/nbackup/vtape, bays: 20, volume_size: 6TB }
```

Disk and cloud spell it directly (`capacity: 20TB`); a tape library derives it as
`bays × volume_size`. The planner fills free capacity with promoted fulls, and
pruning reclaims to stay within it. An optional `minimum_age` is a per-medium
safety floor (defaults to one cycle) below which a slot is never retired. See
[Planning](planning) and [Pruning](pruning) for how each consumes capacity.

## Bandwidth politeness (per medium)

```yaml
media:
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1
    capacity: 50TB
    throughput: 50MB/s     # cap to/from this medium (default: uncapped)
```

A medium may declare a `throughput` cap — bytes per second, the `/s` is optional.
It is the network analogue of the `nice` CPU politeness NBackup already applies:
it keeps a dump or sync from saturating the office uplink. The cap is **symmetric
on reads** — a restore or drill download from the same medium honors it too — and
concurrent workers writing one medium **share** the single budget. Set it on the
medium whose link you must protect, typically a cloud or other remote tier.

## The three types at a glance

| Type | Identified by | Labels? | Swaps? |
|---|---|---|---|
| `disk` | Address (path) | No | No |
| `cloud` | Address (URL key) | No | No |
| `tape` | Label | Yes | Robot mounts bays; manual prompts you |

---

See also [Holding disk](holding-disk) for feeding a slow tape or cloud at disk
speed, [Replication](replication) for landing fast then mirroring offsite, and the
tape and cloud [Scenarios](../scenarios) for complete worked setups.
