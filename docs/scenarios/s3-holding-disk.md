---
title: S3 with a holding disk
layout: default
parent: Scenarios
nav_order: 5
description: "A local buffer absorbs parallel dumps, then one drainer streams them to a bandwidth-capped cloud landing."
---

# S3 with a holding disk
{: .no_toc }

A local scratch disk absorbs parallel dumps at full speed, then one drainer streams them to a throughput-capped cloud landing.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## When to use this

Use this when your landing is a **cloud bucket** but you want fast **parallel** local
dumps decoupled from a slow or bandwidth-capped uplink. The dumpers fill a local disk
at full speed while one drainer streams each finished archive to S3 under a throughput
cap that protects the office link.

This differs from [Disk → S3](disk-to-s3): there, S3 is a *replication target* and the
local disk is the authoritative landing. **Here the cloud bucket is the landing** (the
authoritative copy) and the local disk is a transient holding buffer the dumps flow
through.

## Configuration

```yaml
cycle: 7d

compress:
  scheme: zstd                     # zstd | gzip | none

# Encryption is recommended when the landing is a cloud bucket.
# encrypt:
#   scheme: gpg
#   recipient: backups@example.com

# The cloud bucket is the landing (the authoritative copy); the scratch disk is a
# transient buffer the dumps flow THROUGH on the way to the cloud.
media:
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1   # or gs://bucket, azblob://container
    capacity: 50TB
    throughput: 50MB/s             # cap the uplink — the drainer paces to this budget
  scratch:
    type: disk
    path: /var/spool/nbackup
    capacity: 500GB
    holding: true                  # mark this disk as the scratch buffer
landing: offsite

# Dumps run in parallel onto the holding disk; one drainer streams to the cloud.
parallelism:
  workers: 4

archivers:
  default:
    type: gnutar
    one-file-system: "true"

dumptypes:
  default:
    archiver: default
  no-logs:
    archiver: default
    exclude: ["*.log", "*.tmp"]

sources:
  default:
    localhost: [/home, /etc]
  no-logs:
    localhost: [/srv/www, /opt/app]
```

Cloud **credentials never live in the config** — they come from the standard AWS SDK
environment (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`, `~/.aws/credentials`, or an
IAM role).

## Commands

```bash
nb plan                       # preview the run + the storage $/month it adds
nb dump                       # dump in parallel to disk, drain to the cloud
nb flush                      # drain a crashed run's staged archives explicitly
nb status                     # progress of the running (or most recent) dump
nb drill --tier structural    # routine no-write recoverability check on the cloud copy
```

## What happens

1. `nb dump` runs up to four DLE dumps **in parallel**, each landing on the `scratch`
   holding disk at full local speed.
2. As each archive commits on the holding disk, the single **drainer** streams it to
   `offsite`, paced to the medium's `throughput` budget, and reclaims the disk space.
3. The uplink stays inside its cap while local dumps never wait on the network.

## What to watch

- **The cloud landing is the authoritative copy.** The holding disk is transient — it
  only buffers the write path and is visible in the catalog while archives stage on it.
- **`capacity` back-pressures the dumpers.** A slow uplink makes the buffer fill and the
  dumpers wait — it never overfills.
- **The throughput budget paces the drain.** The cap is symmetric on reads too, so a
  later restore or drill download honors the same budget; see the bandwidth section of
  [Storage media](../features/media).
- **Oversized DLEs skip the buffer.** A DLE estimated larger than the disk streams
  straight to the landing instead of staging through it.
- **A crashed run auto-drains.** Un-flushed archives stay recorded on the holding disk;
  the next `nb dump` drains them automatically, or run `nb flush` to drain explicitly.
- **Egress costs on restore.** Pulling from the cloud transfers bytes out; `nb recover`
  estimates the egress `$` before it downloads — see [Cost forecasting](../features/cost).
- **Encrypt the cloud copy.** Because the authoritative copy lives offsite, pipe each
  archive through gpg — see [Encryption](../features/encryption).

---

See also: [Holding disk](../features/holding-disk),
[Storage media](../features/media),
[Disk → S3](disk-to-s3),
[Getting Started](../getting-started).
