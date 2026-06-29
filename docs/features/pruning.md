---
title: Pruning & retention
layout: default
parent: Features
nav_order: 8
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

`nb prune <medium>` deletes by default; pass `--dry-run` (`-n`) to preview.
**Retention is per-medium**, so the medium is named explicitly (`nb prune disk`,
`nb prune offsite`): each store is pruned against its **own** archives, capacity,
and `minimum_age`. A copy on another medium never makes an archive prunable —
double storage exists for redundancy, so each copy is retained on its own terms.

The unit pruning reasons about is the **archive** (one DLE's image within a slot),
not the whole slot. So an old slot can shed one DLE's image while keeping a
slot-mate the recovery chain still needs.

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

## Priority order

The behavior above follows one immovable priority order:

**recoverability > cycle safety > capacity.**

NBackup will never delete the last way to recover a DLE to free space, and will
fail a write before overwriting a still-protected tape. See
[Rationale](../rationale) for why.

---

See also: [Replication & tiered storage](replication),
[Storage media](media), and [Planning](planning).
