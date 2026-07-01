---
title: Concepts
layout: default
nav_order: 4
description: "The NBackup vocabulary: DLE, Run, Slot, Archive, Cycle, Medium/Volume, Label — and the artifacts you can read."
---

# Concepts
{: .no_toc }

The vocabulary you need to read everything else — and how the concepts nest.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## The vocabulary

| Term | What it is |
|---|---|
| **DLE** | A **backup source**: a `host` + `path` (e.g. `app01:/home`). The thing you choose to back up. (From Amanda: *Disk List Entry*.) |
| **Run** | One planner execution, typically daily. A run decides what to back up and at what level, then dumps it. |
| **Slot** | The **primary artifact**. One run produces exactly one slot — an immutable set of archives. Named `slot-YYYY-MM-DD.NNN`. The addressable unit for copy / restore / list. |
| **Archive** | One **DLE's image at one level** inside a slot. The unit of **retention and pruning**: an old slot can shed one DLE's image while keeping a slot-mate the chain still needs. |
| **Cycle** | The **dump cycle**: the target and hard-max time between full backups of each DLE, and the window retention protects. |
| **Level** | The backup level, 0–9. Level 0 is a **full**; higher levels are **incrementals** relative to a lower level. |
| **Medium** | A named **storage definition** (disk, tape, cloud). Opens as a *Volume*. |
| **Volume** | An ordered sequence of self-describing files addressed by position. Disk, tape, and object stores all map to it. |
| **Label** | The logical identity written on a labeled volume (tape). Address-identified media (disk, cloud) need none. |
| **Slot / Drive** | A tape changer's physical elements: **slots** hold cartridges (each with a scanner-read barcode), **drives** read/write a loaded one. A robot loads slots into drives; a manual drive a human loads. |
| **Catalog** | The local **cache** of slot index + volume registry. It holds no precious state; one media scan (`nb rebuild`) recreates it. |

### How they nest

```text
Run  ──produces──▶  Slot  ──contains──▶  Archive (one per DLE, at one Level)
                      │
                      └── lives on one or more  Volumes  (opened from a Medium)

DLE   = host + path             (what you back up)
Cycle = time between fulls      (the safety/scheduling boundary)
```

A **DLE** is *what* you back up. A **Run** decides and executes. Each run writes one
**Slot**, which groups one **Archive** per DLE. Archives live on **Volumes**, which
you open from a **Medium**. The **Cycle** governs how often each DLE is fulled and
how long its backups are protected.

## Slots and naming

A slot is named for its run's **local calendar date** plus a sequence: the first
run of a day is `slot-2026-06-21.001`; running again the same day gives `.002`,
`.003`, … The sequence is fixed-width, so sorting slot ids as plain text orders
them in time — in an `ls`, a log, or an object-store listing.

Each slot is **immutable**: a committed run is never overwritten. Restores and
pruning order slots by date, then sequence.

## Archives, levels, and chains

Within a slot, each DLE produces one **archive** at one **level**:

- **Level 0** is a full backup.
- **Levels 1–9** are incrementals — a level-`L` dump captures everything changed
  since the level-`L-1` dump.

To restore a DLE you replay its **chain**: the most recent full, then each later
incremental in order. NBackup keeps chains short on purpose (see
[Planning & scheduling](features/planning)) so restores stay simple, and protects
every archive a live chain needs from pruning (see [Pruning](features/pruning)).

## Media and volumes

A **medium** is a named storage definition in your config; it **opens as a
Volume** — an ordered sequence of self-describing files addressed by position. The
three medium types:

- **disk** — a local directory. Address-identified (a path names a file); no
  labels, no swaps.
- **cloud** — an object store (S3/compatible, GCS, Azure Blob) via
  `gocloud.dev/blob`. Also address-identified (a bucket + key names an object).
- **tape** — a changer: drives fed from slots that hold cartridges, whether a
  file-backed library, a robot, or a single drive you change by hand. Tapes carry
  a **label** NBackup verifies before every write.

The **landing** medium (config key `landing:`) is where new slots are created. Any
medium can also be a replication target. See [Storage media](features/media).

## Artifacts you can read

A **volume** is an ordered sequence of self-describing files, each carrying an
identity **header** (slot, DLE, level, scheme, …) and addressed by position. A
**slot** is a run of archives; each **archive** is its payload, followed by a
**member index** (its file list) and a **commit footer** (its identity, sizes, and
checksums). The footer is written **last**, so its presence proves the archive
landed whole. A slot is complete once every archive it planned has committed.

On **disk**, the header is a separate `.hdr` sidecar so the payload stays a clean
archive. One archive is three numbered files:

```text
slots/slot-2026-06-21.001/
  000000-app01-home-L0.tar.zst        # clean compressed tar (payload)
  000000-app01-home-L0.hdr            # JSON header sidecar
  000001-app01-home-L0-index.json.gz  # gzipped member list (browse without extracting)
  000001-app01-home-L0-index.hdr
  000002-app01-home-L0-commit.json    # per-archive footer: identity + sizes + checksums
  000002-app01-home-L0-commit.hdr
  000003-db01-pg-L1.tar.zst           # the next archive continues the numbering
  ...
```

The `NNNNNN` prefix is the file's **position on the volume** — a running counter
that keeps climbing across the slots sharing a volume rather than resetting each
slot. Each archive's **commit footer is its last file**; its **payload is always
the first** of the three — which is all a stock-tool restore needs.

On an **object store** the layout is the disk medium's verbatim — one clean object
per file plus a `.hdr` sidecar — so a slot streams disk↔cloud unchanged. On
**tape**, the header is instead a fixed 32 KB block inline ahead of each payload,
since a tape has no sidecars.

### Recovery never requires NBackup

Archives are produced by **GNU tar** in listed-incremental format, piped through
an external compressor (`zstd` by default; `gzip` or `none` also built in) and,
optionally, an external **encryptor** (`gpg`). NBackup orchestrates these as child
processes rather than reimplementing them — so the tools that wrote an archive are
the tools that read it. A full restores with one pipe:

```bash
zstd -dc 000000-app01-home-L0.tar.zst | tar -xf -
```

Restoring a full + its incrementals replays one archive per level in order,
exactly as `nb recover` does — and `nb drill --tier stock` rehearses that
bare-tools path and prints the commands. The full by-hand procedure is in
[Restore by hand](restore-by-hand).

## The catalog is a cache

NBackup keeps a local **catalog** (default directory `nbackup-catalog`, set with
`workdir:`) that caches the slot index and volume registry, so planning, listing,
locating copies, and pruning never touch a slow or offline volume. But the
**media are the source of truth**: every file is self-describing, every archive is
committed, every labeled volume is identified — so a single scan rebuilds the
catalog (`nb rebuild`). The catalog holds **no** precious state.

The one piece of local state that is *not* on the media is each archiver's
**incremental state** (for GNU tar, the listed-incremental snapshot library). That
belongs to the archiver and lives under a host's `state_dir`, deliberately kept
*beside* the disposable catalog, never inside it. See
[Remote sources](features/remote-sources) and the
[Configuration reference](reference/configuration#incremental-state).

---

Next: browse the [Features](features), or jump to a [Scenario](scenarios) that
matches your setup.
