# PRD: NBackup (Working Title)

This is the product vision: the goals and their priority order. *How* NBackup
realizes them — the run format, the planner, media, drills — lives in the
[README](README.md) (user-facing) and [ARCHITECTURE.md](ARCHITECTURE.md)
(internal). This document deliberately avoids that detail.

## Vision

A modern backup system inspired by Amanda that preserves its strongest
operational properties while embracing disk and object storage.

It should be understandable without specialized knowledge, produce portable
backup artifacts, treat disk, cloud, and tape as equal targets, and adapt
backup planning to a storage capacity rather than a fixed rotation.

Core philosophy:

> A backup administrator should be able to reason about backups by looking at a
> sequence of immutable daily backup artifacts rather than a database of chunks.

## Audience

The operator NBackup serves values certainty of recovery over storage
efficiency:

- **The small-fleet sysadmin or MSP** — a handful to a few dozen Unix servers,
  one pair of hands, cron and an inbox; wants provable restores without
  operating a daemon constellation or a catalog database.
- **The tape user** — from a homelab LTO drive to a library with a robot;
  underserved by the chunk-store generation, which assumes random-access
  storage.
- **Compliance and archival contexts** — where "readable in fifteen years with
  standard tools" and verifiable immutability beat deduplication ratios.
- **The Amanda operator** — same philosophy, modern storage, less machinery.

Not the target: dedup-heavy fleets (many similar machines, laptop backup),
Windows sources, and anyone for whom storage efficiency is the deciding factor
— a chunk-store tool serves them better, and the docs say so plainly.

---

# Goals

## Preserve Amanda's strengths

- **Balanced scheduling.** Distribute full and incremental backups across days
  to avoid backup spikes. Users should not manually schedule fulls.
- **Immutable daily artifacts.** Each run produces one immutable artifact for
  that day; once written, a day's backup is never overwritten.
- **Human-readable contents.** Backups are stored as normal tar archives a user
  can restore with standard tools. No proprietary chunk store is required for
  recovery.
- **Cycle safety.** Preserve the operational safety of Amanda's tape cycle:
  yesterday's backup must never be able to immediately overwrite a backup still
  inside the recovery window.

## Embrace modern storage

- **Disk, cloud, and tape are equal targets.** None is legacy; none is special.
  Tape remains first-class, and object storage (S3 and compatible, GCS, Azure)
  is a first-class deployment model, not a bolt-on.
- **Land fast, replicate offsite.** The common modern shape — land a backup
  quickly on local disk, then mirror it to an offsite tier — is a first-class
  operation, not something the user has to assemble.
- **The same artifact everywhere.** A backup keeps its format whether it lives
  on disk, in the cloud, or on tape, so copies are interchangeable.

## Prove recoverability

- **Backups that are tested, not assumed.** The system should be able to prove a
  backup is actually restorable — exercising real recovery, not just checksums —
  and make an unattended failure loud. This is its contribution of the "tested"
  guarantee of modern backup practice (3-2-1-1-0).

## Protect the data

- **Optional encryption** that keeps copies interchangeable: a backup is
  encrypted once at the source, and verifying or replicating it offsite never
  needs the key.

---

# Non-Goals

## Global deduplication

The system is not optimized for maximum storage efficiency. Operational
simplicity is preferred over extreme deduplication.

## Chunk-store architecture

The system is not based on content-addressed chunk databases. Intentionally
avoided: Veeam-style block stores, Restic-style chunk repositories, Borg-style
deduplicated archives.

## Storage-class lifecycle modeling

The system does not model cloud storage-class transitions (Glacier / Deep
Archive). Which tier bytes physically sit in is configured operator-side; a flat
cost estimate is the honest one NBackup can deliver.

---

# Priorities

When goals conflict, this order is immovable. Scheduling and retention bend to
it, never the reverse.

## 1. Preserve recoverability

Never delete the last valid recovery path.

## 2. Respect cycle safety

Never retire backups too aggressively; honor the safety window.

## 3. Stay within storage capacity

Adapt scheduling and retention to the capacity the user gives each medium —
warning, rather than silently dropping recovery points, when a complete recovery
set will not fit.

## 4. Balance daily backup volume

Avoid large backup spikes — the last concern, bounded by the three above.

---

# Design Principles

## Backups are the primary abstraction

Not tapes. Not chunks. Not databases. A user reasons in terms of a sequence of
daily backups, and can answer "what happened on June 20?" by inspecting that
day's artifact — without a running backup server or metadata database.

## Storage is secondary

The same backup may live on disk, in the cloud, or on tape without changing its
format. Media are a placement detail, not the unit of thought.

## Capacity, not counts

Users give a medium a storage capacity, not a tape count or rotation schedule.
The system chooses backup levels, full frequency, and retention to fit it.

## Simplicity over optimization

Prefer normal tar archives, immutable artifacts, and simple retention over
global chunk stores, cross-backup deduplication, and opaque repositories.
