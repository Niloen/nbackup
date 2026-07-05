---
title: Recovery
layout: default
parent: Features
nav_order: 7
description: "Recover backups as they stood on a date — a whole DLE, or browse and pick individual files. No index server needed."
---

# Recovery
{: .no_toc }

Recover backups as they stood on a date — rebuild a whole DLE, or browse and pick individual files, with no index server in the loop.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

`nb recover` recovers from backups **as they stood on a date**, in two modes:
rebuild an entire DLE, or browse its filesystem and pull back individual files.
A DLE is identified by `host:path` (e.g. `app01:/home`) — that is what the tables
show and what `--dle` (and the interactive `setdisk`) accept.

## Whole-DLE restore (`--all`)

```bash
nb recover --dle app01:/home --date 2026-06-21 --all --dest /tmp/out
```

This rebuilds an entire DLE: it replays the most recent full at or before the
date, then every later incremental up to it, **in run order**, using GNU tar's
incremental extraction.

Because the incrementals carry directory census data, **deletions are applied** —
a file removed between the full and the date is absent after the restore — and
extraction **prunes the destination to match the backup**. So `--dest` must be
empty, or pass `--force` to restore into a populated directory, replacing its
contents.

Omit `--dle` to restore **every** DLE, each into its own subdirectory of `--dest`:

```bash
nb recover --date 2026-06-21 --all --dest /tmp/out
```

## File-level recovery (browse + pick)

Without `--all`, recover browses a DLE's filesystem and pulls back individual
files or directories. The browse view **merges the restore chain** — the full
plus every later incremental up to the date — so each path shows its newest
version on or before the date, recovered from the archive that holds it.

There is **no separate index server**: browsing reads the **member index** every
archive already records, so it touches only the catalog and reaches media only
when you extract.

### One-shot, scriptable

```bash
nb recover --dle app01:/home --date 2026-06-20 --list --path /etc
nb recover --dle app01:/home --date 2026-06-20 \
    --path /etc/hosts --path /etc/nginx --dest /tmp/out
```

### Interactive shell

`nb recover` with no arguments opens a shell that tracks a current DLE and date;
set those, then navigate and select:

```text
recover> setdisk app01:/home
recover> setdate 2026-06-20
recover app01:/home:/> cd etc
recover app01:/home:/etc> ls
  hosts   nginx/   passwd
recover app01:/home:/etc> add hosts nginx
recover app01:/home:/etc> extract /tmp/out
recovered 12 file(s) from 2 archive(s) into /tmp/out
```

Paths are relative to the DLE's backed-up root. Selecting a directory pulls its
**whole subtree** (each file from the archive that last changed it).

{: .note }
Selected-file recovery **never deletes** — you get exactly what you ask for, and
nothing in `--dest` is pruned. One fidelity consequence: GNU tar records
deletions in its snapshot, not the member index, so a file deleted at a later
incremental still shows in the browse view. Recover the **whole DLE** with
`--all` when you need deletion-accurate state.

## Efficient partial reads (archive shapes)

Pulling one file out of a large archive normally costs the *whole* archive:
decompression is stateful, so reaching a late member means streaming every byte
before it through the decoder — real egress on a cloud store. NBackup records
enough metadata at write time (**decode-restart points**) that a selective recover
fetches **only the bytes it needs**, without changing what the archive looks like to
a whole-stream reader or to the [stock-tools one-liner](../restore-by-hand).

Which optimization applies falls out of the pipeline — the archive's **shape**,
stamped in its commit footer so a reader decodes it with no config:

| Pipeline | Shape | Selective read | Whole restore |
|----------|-------|----------------|---------------|
| Server-side compress, no encryption (`zstd`/`gzip`/`none`) | **framed** | ranged GET of the covering frames — a validated single-file extract cost 0.3% of the archive | reads it all |
| Encrypted (`gpg`) | **atomic** | fetch only the atoms (sealed `.pNNN` gpg messages) covering the members | reads it all |
| Client-placed transform, or a non-restartable scheme | **stream** | whole archive, as before | reads it all |

A **framed** archive is byte-for-byte identical to a plain stream — the compressor
just restarts every `frame_size` of input, an invisible restart point — so `nb copy`,
`nb verify`, and the stock one-liner are all unchanged; the frame table rides in the
per-archive index. An **atomic** archive stores each part as one complete gpg message
(a concatenation of gpg messages can't be re-decrypted as one stream, so encryption
can't be frame-invisible), and selective restore decrypts only the covering atoms.

{: .note }
Tape streams whole regardless — it has no egress meter, and a within-file skip buys
little — so ranged reads pay off on **cloud** (and, less, local disk). A **whole-DLE**
restore reads every member whatever the shape; the win is in single-file recovery.

### Tuning encrypted-restore granularity

For an encrypted (atomic) dumptype, `part_size` sets the **atom size** — the unit a
selective restore and a key-proving drill fetch:

```yaml
part_size: 10GiB              # global default atom size (matches the cloud slice size)
dumptypes:
  vm-images:
    encrypt: { scheme: gpg, recipient: backups@example.com }
    part_size: 2GiB           # smaller atoms → finer restore, cheaper drills, more objects
```

Smaller atoms mean finer selective-restore granularity and a cheaper key-proving drill
sample, at the cost of more objects on the medium. Because a sealed atom can't shrink
without the key, it also can't land on a medium whose per-part **ceiling** is below it —
flagged at `nb plan` time and refused per-archive by `nb sync`, never a silent failure.
Unencrypted framed archives carry no atom size; they are re-sliced freely and take each
medium's own `part_size`. `frame_size` (default 256 MiB) is the framed shape's
restart interval — an advanced knob that is right for almost everyone as-is. The full
design is in [docs/design/archive-shapes.md](https://github.com/Niloen/nbackup/blob/main/docs/design/archive-shapes.md).

## Egress on a cloud restore

Pulling from a cloud store costs egress. `nb recover` estimates the **egress $**
before it reads and warns — and, interactively, confirms — when the amount is
material. See [Cost forecasting](cost).

---

See also [Verification & drills](verification) to prove a restore will work
before you need it, [Restore by hand](../restore-by-hand) for the
stock-tools path that needs no NBackup, and [Replication](replication) to
restage an offsite copy back to disk first.
