---
title: Introduction
layout: default
nav_order: 1
description: "Niloen Backup (NBackup) — a run-based backup system with first-class disk, cloud, and tape storage."
permalink: /
---

# Niloen Backup
{: .fs-9 }

A run-based backup system whose design comes from **Amanda** — balanced
multilevel scheduling, immutable daily artifacts, human-readable contents, and
cycle-based safety — with **first-class disk and cloud storage** added on top.
{: .fs-6 .fw-300 }

[Get started](getting-started){: .btn .btn-primary .fs-5 .mb-4 .mb-md-0 .mr-2 }
[Why NBackup?](rationale){: .btn .fs-5 .mb-4 .mb-md-0 }

---

**Niloen Backup** (short: **NBackup**) is an open-source backup system written in
Go. It orchestrates GNU `tar` and a compressor as child processes and produces
**immutable, self-describing artifacts** that you can copy, inspect, and restore
without NBackup installed.

> A backup administrator should be able to reason about backups by looking at a
> sequence of immutable daily backup runs rather than a database of chunks.

Amanda is tape-first. NBackup treats local disk, virtual tape, and object stores
(S3, GCS, Azure Blob) as **equal targets**, and makes the common modern shape —
land fast on disk, then replicate offsite — a first-class operation.

## What makes it different

- **Backups you can read.** Each **run** is exactly one immutable
  directory (or tape span, or set of objects) you can list and understand without
  any NBackup-specific tooling. A full restores with one pipe:

  ```bash
  zstd -dc 000000-app01-home-L0.tar.zst | tar -xf -
  ```

- **Recovery never requires NBackup.** Archives are plain GNU `tar` streams piped
  through a stock compressor (and, optionally, `gpg`). The tools that wrote an
  archive are the tools that read it. NBackup orchestrates; it never invents a
  format only it can open.

- **Disk, cloud, and tape are equal.** The same artifact lives unchanged on any
  medium. `nb copy` moves one run between media; `nb sync` mirrors a whole
  medium offsite. Land on fast local disk, then replicate to S3 or tape.

- **Capacity, not rotation.** You give a medium a storage *capacity* and a *cycle*
  (how often each source is fully backed up). The planner chooses backup levels,
  full frequency, and retention to fit — no balancing knobs to tune.

- **Recoverability is proven, not assumed.** `nb drill` actually restores a
  risk-biased sample of your data and throws it away, delivering the **"0 errors"**
  digit of [3-2-1-1-0](https://www.veeam.com/blog/321-backup-rule.html). An
  unattended failure is loud — it exits non-zero and can email or page you.

- **Encryption that keeps copies interchangeable.** A dump is encrypted once at the
  source (via `gpg`); verifying integrity and replicating offsite never need the
  key.

## How a backup is shaped

A **volume** is an ordered sequence of self-describing files. A **run** is a set
of **archives**; each archive is its payload, a **member index** (its file list),
and a **commit footer** (identity, sizes, checksums) written last — so the footer's
presence proves the archive landed whole. On disk it looks like this:

```text
runs/run-2026-06-21.001/
  000000-app01-home-L0.tar.zst        # clean compressed tar (payload)
  000000-app01-home-L0.hdr            # JSON header sidecar
  000001-app01-home-L0-index.json.gz  # gzipped member list (browse without extracting)
  000002-app01-home-L0-commit.json    # per-archive footer: identity + sizes + checksums
  ...
```

The same shape maps to an object store (one clean object per file) or tape (the
header is a 32 KB block inline ahead of each payload). See [Concepts](concepts)
for the full vocabulary and [Artifacts you can read](concepts#artifacts-you-can-read).

## Where to go next

| If you want to… | Go to |
|---|---|
| Understand *why* NBackup is built this way | [Rationale](rationale) |
| Install it and run your first backup | [Getting Started](getting-started) |
| Learn the vocabulary (run, DLE, cycle, …) | [Concepts](concepts) |
| Read about a specific capability | [Features](features) |
| Copy a working setup for your situation | [Scenarios](scenarios) |
| Look up a command or config key | [Reference](reference) |

---

NBackup is free software under the **GNU General Public License v3.0**.
Copyright © 2026 Niloen AB.
