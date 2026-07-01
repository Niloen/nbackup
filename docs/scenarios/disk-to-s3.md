---
title: Disk → S3 offsite
layout: default
parent: Scenarios
nav_order: 2
description: "Land fast on local disk, then replicate every run to S3 for the offsite copy. No holding disk."
---

# Disk → S3 offsite
{: .no_toc }

Dump fast to a local disk, then mirror every run to S3 so there's always an offsite copy.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## When to use this

This is the common modern shape: a fast, online local copy for everyday restores
plus a cheap, durable offsite copy for disaster recovery. Because S3 replication
is **asynchronous** — `nb sync` copies committed runs after the dump finishes —
you do **not** need a holding disk here. The dump lands on local disk at disk
speed; the slow uplink is paid off afterward in the background.

## Config

Save this as `nbackup.yaml`. The disk medium is the landing (kept deliberately
lean); the cloud medium is the offsite mirror.

```yaml
cycle: 7d

compress:
  scheme: zstd
  level: 3

media:
  # Fast local landing. A tighter capacity keeps disk lean — old runs leave
  # disk on disk's own budget, independent of what S3 holds.
  disk:
    type: disk
    path: /var/lib/nbackup/disk
    capacity: 2TB

  # Offsite mirror. The backend is chosen by the URL scheme; throughput caps the
  # uplink so a sync doesn't saturate the office line.
  offsite:
    type: cloud
    url: s3://company-backups?region=eu-north-1
    capacity: 50TB
    throughput: 50MB/s

# Runs are created on disk first.
landing: disk

# Replication rule: mirror the landing's sealed runs to S3. A cron `nb sync`
# (no --to) runs every rule here.
sync:
  - to: offsite

archivers:
  default:
    type: gnutar
    one-file-system: "true"
    sparse: "true"

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
    localhost: [/srv/www, /var/log]
```

**Credentials never live in the config.** The `s3://` backend reads them from the
standard AWS SDK environment — `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`,
`~/.aws/credentials`, or an IAM role. NBackup stores no secret.

## Commands

```bash
nb dump                          # land a run on disk
nb sync --to offsite --dry-run   # preview: which runs disk has that S3 doesn't
nb sync --to offsite             # copy the backlog to S3, oldest first

# Hands-off cron line: dump, push offsite, prove a restore, then mail the digest.
nb dump && nb sync && nb drill --unattended; nb report --notify
```

A routine offsite drill should limit egress — drill the *structural* tier (decrypt
+ decompress + list, no extract) directly from the S3 copy:

```bash
nb drill --from offsite --tier structural
```

## What happens

- `nb dump` writes the run to disk at local speed.
- `nb sync` copies each missing run to S3 atomically, oldest first, and records a
  second placement — so an interrupted sync resumes where it stopped.
- A restore reads from **whichever copy is reachable**: disk when it's online, S3
  when disk is gone.

## What to watch

- **Disk and S3 prune independently.** A run leaves disk when *disk's* capacity
  and cycle say so — never merely because a copy reached S3. Both copies are kept,
  each retained on its own terms. Give disk the tighter `capacity` and let S3 be
  the deep retention tier. See [Pruning](../features/pruning) and
  [Replication](../features/replication).
- **Mind the bill.** `nb plan` prints the S3 footprint's storage `$/month` and the
  marginal cost the next run adds; `nb plan --days 30` projects the curve. Restores
  from S3 surface an egress `$` estimate before pulling. See
  [Cost forecasting](../features/cost).
- **Untrusted destination?** If you don't fully trust the bucket, add an `encrypt`
  block so only ciphertext leaves the host — vaulting with `nb sync` never needs
  the key. See [Encryption](../features/encryption).
- **Cap the uplink.** The `throughput: 50MB/s` on the cloud medium keeps both
  `nb sync` and any restore/drill download from monopolizing the office line.
