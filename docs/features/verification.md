---
title: Verification & drills
layout: default
parent: Features
nav_order: 7
description: "nb verify checks integrity; nb drill actually restores a risk-biased sample to prove recoverability — the 0 of 3-2-1-1-0."
---

# Verification & recovery drills
{: .no_toc }

Two layers that prove backups are good — checksum integrity, and an actual restore that delivers the "0" of 3-2-1-1-0.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## `nb verify` — the integrity check

`nb verify` is the atomic integrity check. By default it re-hashes each archive's
payload against the checksum recorded when the archive was committed, catching
bit-rot or a corrupted copy. It is **stateless and keyless**: it reads nothing but
the bytes on the medium and the checksum beside them, so it needs no decryption key
and keeps no ledger.

```bash
nb verify run-2026-06-21.020000   # re-hash one run's archives
nb verify --all                # re-check every run
```

Because a run can have several copies, `nb verify` audits **every** copy and tells
you that an intact copy still exists when one is damaged — a damaged offsite reel
does not condemn the run if disk still holds a good copy.

Integrity is also enforced on **every read, not just on demand**: each part is
re-hashed against its recorded seal as it streams, so a restore of a damaged copy
fails loudly — naming the corrupt part and pointing at the intact copies — instead
of silently writing a bit-flipped file (tar's own checksums cover only headers, so
data corruption would otherwise pass through unnoticed).

### `--deep` — prove it is a restorable stream

A checksum proves the bytes are unchanged; it does not prove they are *readable*.
`nb verify --deep` adds a **structural** check: it streams the archive through the
real read pipeline — decrypt, decompress, then `tar -t` to **list** (not extract)
the members — and asserts that the pipeline completes and that the members match the
recorded index.

```bash
nb verify --deep run-2026-06-21.020000
```

This proves the bytes are a valid *restorable stream* and exercises the encryption
key and compression scheme, while still **writing nothing** to disk. It is the
bridge from "the bytes are intact" to "the bytes can be read back".

## `nb drill` — the recoverability rehearsal

A checksum cannot catch a lost key, a drifted compression or `tar` version, a broken
incremental chain, or an offsite copy nobody can actually read. The only way to know
a backup restores is to restore it. `nb drill` is the recoverability rehearsal
layered on top of verify: it **actually restores** a risk-biased sample of DLEs —
full plus incrementals, faithful to deletions — into a scratch directory, then
discards it.

This is NBackup's contribution of the **"0 errors"** digit of 3-2-1-1-0: not just
that copies exist, but that they come back.

```bash
nb drill                                  # drill the riskiest sample on the landing copy
nb drill --dry-run                        # preview: what would be drilled, plus the posture audit
nb drill --from cloud --tier structural   # routine offsite check (no-write)
nb drill --tier stock                     # restore via the documented gpg/zstd/tar one-liner
```

The hands-off nightly form drills after the dump, sync, and prune, then reports:

```bash
nb dump && nb sync && nb prune && nb drill --unattended; nb report --notify
```

### Selection is risk-first

A drill does not test everything every night — it tests what is most likely to be
broken or most expensive to lose. Selection:

- **rotates** DLEs so each one is drilled within a window (nothing goes untested
  indefinitely);
- **prioritizes** the longest incremental chains and the oldest fulls still relied
  upon (the targets where drift or a missing base hurts most);
- drills a **point-in-time** (`--as-of`), not merely the latest run — so a restore
  *as it would have stood* on an earlier date is exercised too.

### Tiers

Each target is exercised at a tier, from cheapest to most thorough:

| Tier         | What it does                                                        |
|--------------|--------------------------------------------------------------------|
| `sample`     | re-hash ONE part per archive against its per-part seal — bounded egress on a cloud copy; successive drills rotate through the parts, so coverage accumulates |
| `checksum`   | re-hash the payload against the recorded checksum                  |
| `structural` | stream through decrypt → decompress → `tar -t` (list, no write)    |
| `chain`      | a real restore of the full + incrementals to scratch              |
| `stock`      | restore via the documented `gpg`/`zstd`/`tar` one-liner           |

```bash
nb drill --tier stock   # rehearse the bare-tools restore and print the commands
```

Outcomes append to an inspectable **ledger** (`drill-ledger.json`) in the catalog
workdir: per DLE its last drill, the tier, the source medium, and pass/fail. The
ledger is what `nb report` reads to flag DLEs whose drills are failing or overdue.

### Failure classification

When a drill fails, the failure is **classified** — because each class implies a
different fix:

- **integrity** — corruption; a copy's bytes no longer match the checksum.
- **pipeline** — the archive would not decrypt/decompress/untar; a lost key or a
  drifted scheme/`tar`.
- **chain** — the incremental composition is broken; a base run is missing or a
  level does not replay.
- **missing-copy** — no readable copy of the target could be reached.

The command **exits non-zero** on any failure, so a nightly drill can page you.

### Attended vs unattended

A drill runs in one of two modes:

- **Attended** (interactive) may prompt you to load a tape to reach a target.
- **Unattended** (`--unattended`, auto-detected when stdin is not a terminal — the
  cron mode) never prompts and **skips** any target that would need a tape swap.

A skip is a **coverage warning, not a failure**: a nightly unattended drill stays
green while it rotates through the fleet over many nights, instead of going red the
moment a target lives on a tape that is not loaded.

## The 3-2-1-1-0 posture audit

Every drill run prints a recoverability posture audit against the
[3-2-1-1-0](https://www.veeam.com/blog/321-backup-rule.html) rule: copies, media,
offsite presence, immutability, and 0 errors — plus key-reachable, incremental-state,
and capacity checks. `nb drill --dry-run` prints the same audit without restoring
anything.

The **immutability** line comes from a WORM probe. NBackup keeps one fixed probe
object on the `--from` medium and checks that deleting it is **refused** (S3 Object
Lock, LTO WORM). A refused delete proves the storage enforces immutability; NBackup
only *detects* this — you configure it operator-side on the storage, and least
privilege keeps NBackup unable to turn it off. Append-only media such as tape are
immutable by construction and reported without a probe.

## Honest limits

{: .note }
A `chain` drill restores a whole DLE, so it reads every member and spends the **full
bytes** whatever the archive's shape. For routine **offsite** drills use the cheap
`sample` tier (one part per archive, rotating through the parts over successive drills)
or the no-write `structural` tier, and watch the forecast egress the dry-run prints
(see [Cost forecasting](cost)). Ranged reads cut the cost of *selective* (single-file)
recovery, not of a whole-DLE restore — see
[Recovery → Efficient partial reads](recovery#efficient-partial-reads-archive-shapes).
Drills restore only to scratch and **never** touch real data or the incremental
snapshot library.

## See also

- [Recovery](recovery) — restoring a whole DLE or browsing and picking files.
- [Encryption](encryption) — why verify and sync stay keyless, and what `--deep` and
  a `chain` drill need the key for.
- [Monitoring & reporting](monitoring) — how drill outcomes surface in `nb report`
  and notifications.
- [Concepts](../concepts) and the [Rationale](../rationale) behind verify-as-primitive,
  drill-as-orchestration.
