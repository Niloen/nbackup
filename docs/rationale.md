---
title: Rationale
layout: default
nav_order: 2
description: "Why NBackup exists, its Amanda lineage, design philosophy, priority order, and non-goals."
---

# Rationale
{: .no_toc }

Why NBackup is built the way it is — the lineage, the philosophy, and the
priority order that settles every conflict.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## The problem with modern backup tools

Most modern backup systems are **chunk stores**: they slice data into
content-addressed blocks and reassemble it from a database. That buys excellent
deduplication, but it costs you the ability to *reason* about your backups. To
answer "what did we have on June 20th?" you need the tool, its database, and its
exact version — and to restore, you need all of that working at once.

NBackup takes the opposite bet, inherited from **[Amanda](https://www.amanda.org/)**:

> A backup administrator should be able to reason about backups by looking at a
> sequence of immutable daily backup artifacts rather than a database of chunks.

A backup is a thing you can hold: an immutable daily **run**, stored as ordinary
`tar` archives, that you can copy, inspect, and restore with tools that ship in
every Unix. The database (NBackup calls it the *catalog*) is a **cache** — delete
it and one scan of the media rebuilds it. The media are the source of truth.

## The Amanda lineage

NBackup preserves Amanda's strongest operational properties:

- **Balanced multilevel scheduling.** Full and incremental backups are spread
  across days to avoid spikes. You never hand-schedule a full.
- **Immutable daily artifacts.** Each run produces one artifact for that day;
  once written it is never overwritten.
- **Human-readable contents.** Backups are normal `tar` archives. No proprietary
  chunk store stands between you and recovery.
- **Cycle safety.** Yesterday's run can never immediately overwrite a backup still
  inside the recovery window.

## What NBackup adds

Amanda is tape-first; everything else is a bolt-on. NBackup makes **disk, cloud,
and tape equal targets**:

- **Object storage is first-class.** S3 (and any S3-compatible store), Google
  Cloud Storage, and Azure Blob are deployment models, not adapters.
- **Land fast, replicate offsite** is a built-in operation (`nb sync`), not
  something you assemble from cron and `rsync`.
- **The same artifact everywhere.** A run keeps its exact byte layout on disk, in
  a bucket, or on tape — so copies are interchangeable and a restore reads from
  whichever copy is reachable.
- **Capacity-driven planning.** You give a medium a storage *capacity*, not a tape
  count or rotation schedule. The planner adapts levels, full frequency, and
  retention to fit it.
- **Proven recoverability.** `nb drill` actually restores a sample and discards it
  — the "tested" guarantee (the **0** of 3-2-1-1-0) that checksums alone can't give.

## Design principles

**Backups are the primary abstraction.** Not tapes, not chunks, not databases.
You reason in terms of a sequence of daily backups and can answer "what happened
on June 20?" by inspecting that day's run — no running server required.

**Storage is secondary.** The same backup may live on disk, in the cloud, or on
tape without changing format. Media are a placement detail, not the unit of thought.

**Capacity, not counts.** You give a medium a capacity; the system chooses levels,
full frequency, and retention to fit.

**Simplicity over optimization.** Normal `tar` archives, immutable artifacts, and
simple retention beat global chunk stores and opaque repositories — even at the
cost of cross-backup deduplication.

## The priority order

When goals conflict, this order is **immovable**. Scheduling and retention bend to
it, never the reverse:

| Priority | Rule |
|---|---|
| **1. Preserve recoverability** | Never delete the last valid recovery path. |
| **2. Respect cycle safety** | Never retire backups too aggressively; honor the safety window. |
| **3. Stay within capacity** | Adapt scheduling and retention to each medium's capacity — *warn*, rather than silently drop recovery points, when a complete recovery set won't fit. |
| **4. Balance daily volume** | Avoid spikes — the last concern, bounded by the three above. |

This is why, when a complete recovery set won't fit a medium's capacity, NBackup
raises a **warning** and keeps running rather than silently pruning your oldest
recovery points away. Recoverability outranks tidiness.

## Non-goals

NBackup deliberately does **not** do these things:

- **Global deduplication.** Operational simplicity is preferred over extreme
  storage efficiency. There is no cross-backup block dedup.
- **Chunk-store architecture.** No content-addressed chunk database. Veeam-style
  block stores, Restic-style chunk repos, and Borg-style dedup archives are
  intentionally avoided.
- **Storage-class lifecycle modeling.** NBackup does not model cloud storage-class
  transitions (Glacier / Deep Archive). Which tier bytes sit in is configured
  operator-side; a flat [cost estimate](features/cost) is the honest one NBackup
  can deliver.

---

Next: [Getting Started](getting-started) to install and run a backup, or
[Concepts](concepts) for the vocabulary.
