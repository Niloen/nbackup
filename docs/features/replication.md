---
title: Replication & tiered storage
layout: default
parent: Features
nav_order: 5
description: "Land fast on disk, replicate offsite. nb copy moves one run between media; nb sync mirrors a whole medium."
---

# Replication & tiered storage
{: .no_toc }

Land fast on local disk, then replicate offsite to tape or the cloud.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## Land fast, replicate offsite

The common operational shape is **land fast, replicate offsite**: dump to local
disk (cheap, fast, online), then mirror the committed runs to tape or S3 for the
offsite copy. NBackup has two commands for moving runs between media:

- `nb copy` copies **one** run between media, choosing the endpoints with
  `--from`/`--to` (e.g. disk → tape).
- `nb sync` is the **batch** form of `nb copy`: it copies every run the target
  medium is missing.

When the second copy should exist from day one, prefer a **multi-landing route**
over syncing: `landing: [s3, gdrive]` writes every new archive to both media from
local data, so nothing is downloaded from one cloud to feed the other (see
[Storage media](media)). `nb sync` then remains the tool for **history** — seeding
a newly added medium with the runs that predate it — and for repairing the gap a
failed landing left behind (`nb sync --to gdrive`).

`nb sync` works **oldest first**, so an interrupted sync makes contiguous,
replayable progress and a full always lands before its incrementals.

## Syncing

```bash
nb sync --to lto --dry-run      # preview: what disk has that tape doesn't
nb sync --to lto                # copy the backlog
nb sync --to glacier --last 4   # only the 4 most recent runs
nb sync                         # run every rule in the config's sync: block
nb sync --from lto --to disk    # un-vault: restage tape back to disk
```

The source defaults to the **landing** medium; **`--from` overrides it**, so the
same command both pushes offsite (disk → tape/S3) and pulls back (tape → disk).
Reading a tape source mounts the volume holding each run, just like a restore.

`nb sync` **copies by default** — pass `--dry-run` (`-n`) to preview without
writing. It is **idempotent**: each run copies atomically and records a second
placement, so re-running resumes where an interrupted sync left off, and a
fully-mirrored target reports "up to date". On a hard error (target full or
offline) it stops and reports its progress.

## Declaring recurring targets

Declare recurring replication in the config's `sync:` block so a cron line is just
`nb dump && nb sync`:

```yaml
sync:
  - to: glacier        # mirror everything to the object store
  - to: lto
    last: 4            # copy only the 4 most recent runs (does not remove older
                       # ones already on tape — `nb prune` trims)
  - from: lto          # second tier: tape -> deep-archive (source need not be landing)
    to: deep-archive
```

`nb sync` with no `--to` runs every rule in the block; `nb sync --to lto` syncs a
single ad-hoc target.

## Replication and pruning are independent

Replication and pruning are **independent**. Each medium prunes against its own
retention, so a run leaves disk when **disk's** capacity and cycle say so — never
merely because a copy reached S3 or tape. Both copies are kept, each retained on
its own terms.

To use a cheap offsite tier as bulk retention while disk stays lean, give disk a
tighter `capacity` (or a shorter `minimum_age`) than the tier: `nb sync` mirrors
runs offsite and `nb prune` independently trims disk back to its budget. A
restore reads from whichever copy is available.

---

See also: [Pruning & retention](pruning), [Storage media](media),
[Holding disk](holding-disk), and the [disk → S3](../scenarios/disk-to-s3)
scenario.
