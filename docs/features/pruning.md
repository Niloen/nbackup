---
title: Pruning & retention
layout: default
parent: Features
nav_order: 9
description: "Per-medium retention with a safety floor that never deletes the last recovery path, then capacity reclamation to fit."
---

# Pruning & retention
{: .no_toc }

Per-medium retention with a safety floor that never deletes the last recovery path, then capacity reclamation to fit.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## Pruning is per-medium

`nb prune [medium]` deletes by default; pass `--dry-run` (`-n`) to preview.
**Retention is per-medium**: name a medium to prune just it (`nb prune disk`,
`nb prune offsite`), or name none to prune **every** configured medium in turn —
the hands-off form for cron, mirroring `nb sync` running every rule. Either way each
store is pruned against its **own** archives, capacity, and `minimum_age`. A copy on
another medium never makes an archive prunable — double storage exists for
redundancy, so each copy is retained on its own terms. Tape recycles whole volumes
by relabel rather than per run, so a fleet-wide `nb prune` only reclaims disk/cloud
and leaves tape untouched.

In the nightly chain, put `nb prune` **after** `nb sync` (`nb dump && nb sync &&
nb prune && …`): a capacity-bound landing must not reclaim a run before it has been
replicated. `minimum_age` (one cycle by default) already keeps a fresh run safe for
the common case, but ordering is the guard when replication is more than a cycle
behind.

The unit pruning reasons about is the **archive** (one DLE's image within a run),
not the whole run. So an old run can shed one DLE's image while keeping a
run-mate the recovery chain still needs.

Pruning has two layers.

## Layer 1: the safety floor

An archive is **protected** — and never reclaimed — if either:

- It is younger than the medium's `minimum_age` (which defaults to one cycle), or
- It belongs to its DLE's **live recovery chain**: that DLE's last full and *every
  later incremental*. A whole-DLE restore replays them in order, so dropping the
  tip loses the latest state and dropping a middle incremental breaks a
  climbing-level chain. A recent dump likewise pins the older base its restore
  needs.

Only a chain **superseded by a newer full** becomes reclaimable. The safety floor
is what guarantees pruning never deletes a DLE's last recovery path.

## Layer 2: capacity reclamation

Among the non-protected archives, the medium's retention strategy reclaims to fit
capacity. How it reclaims depends on the medium:

- **Object stores (disk, S3) reclaim per-archive** — they delete the **oldest
  dead archives until total ≤ capacity**.
- **Tape reclaims whole volumes** by **label rotation** (Amanda's *tapecycle*).
  When a run needs a fresh volume and no blank is loaded, NBackup reuses the
  **oldest tape whose every run is unprotected**, keeping the same label name and
  advancing only its epoch (a reuse, not a rename). It **announces** which tape it
  wants in `nb plan`, the run output, and the swap prompt.

If every tape still holds a protected run, the run **fails loudly** rather than
overwriting one — recoverability outranks capacity. `nb prune` never deletes
individual archives from a tape; `nb label --relabel` is the manual early-recycle
override.

## Sweeping crash leftovers

An interrupted run (a hard kill or power loss before a dump's **commit footer** is
written) can leave **orphans** on a per-file medium: a complete archive part with no
commit footer, or a torn half-written file. They belong to no archive, so retention
above never sees them, yet they keep consuming the store. On disk and S3, `nb prune`
**sweeps** them after the retention pass — `--dry-run` previews them too.

Detection is safe by construction:

- It reads the medium's **own commit footers**, never the catalog cache, so a lost or
  stale catalog can never make a committed archive look like an orphan.
- It honors the medium's `minimum_age` just like retention, so on an immutable bucket
  you set `minimum_age ≥ your Object-Lock retention` and the sweep never even attempts
  a still-locked object; any delete the storage still refuses is logged and left for a
  later prune rather than failing the run.

Tape is untouched by the sweep — its orphans are reclaimed by relabel like everything
else. One S3 case is outside NBackup's reach: a hard-killed upload can leave a *dangling
incomplete multipart upload* that never appears in a bucket listing. Clean those with an
`AbortIncompleteMultipartUpload` **bucket lifecycle rule** (a few days).

## Priority order

The behavior above follows one immovable priority order:

**recoverability > cycle safety > capacity.**

NBackup will never delete the last way to recover a DLE to free space, and will
fail a write before overwriting a still-protected tape. See
[Rationale](../rationale) for why.

---

See also: [Replication & tiered storage](replication),
[Storage media](media), and [Planning](planning).
