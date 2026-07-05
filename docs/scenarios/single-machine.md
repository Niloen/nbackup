---
title: Single machine → local disk
layout: default
parent: Scenarios
nav_order: 1
description: "One host backing up to a local disk — the simplest useful NBackup setup."
---

# Single machine → local disk
{: .no_toc }

One server backing itself up to an attached disk or NAS path — the smallest setup that is still worth running.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## When to use this

You have a single machine and somewhere to put the backups: a second internal
disk, an external drive, or a mounted NAS path. Everything runs locally — no SSH,
no cloud, no tapes. This is the natural next step after [Getting Started](../getting-started),
and a fine place to stay until you want an offsite copy (then move on to
[Disk → S3 offsite](disk-to-s3)).

## Config

Save this as `nbackup.yaml`. Point `path` at your backup disk and adjust the
`sources` paths to what you actually want protected.

```yaml
# Target — and hard maximum — time between fulls for every DLE.
cycle: 7d

# Pipe each archive through zstd (must be on PATH). Use `gzip` if zstd is
# unavailable, or `none` to skip compression entirely.
compress:
  scheme: zstd
  level: 3

# One disk medium: a directory NBackup writes runs into, with a capacity it
# stays within (pruning reclaims the oldest to fit).
media:
  disk:
    type: disk
    path: /mnt/backup/nbackup        # your backup disk or NAS mount
    capacity: 2TB                    # space NBackup may use here

# Runs are created on the disk medium.
landing: disk

# One gnutar archiver. one-file-system keeps a dump from wandering across mount
# points into the backup disk itself or into /proc, /sys, etc.
archivers:
  default:
    type: gnutar
    one-file-system: "true"
    sparse: "true"

# Two dumptypes: the plain default, and one that drops logs and temp files so
# noisy, low-value data doesn't bloat every incremental.
dumptypes:
  default:
    archiver: default
  no-logs:
    archiver: default
    exclude: ["*.log", "*.tmp"]

# The disklist: grouped by dumptype, then host, then paths. Everything is
# localhost, so it all runs locally. Reading all of /etc and /var/log needs root,
# so run `nb dump` as root — otherwise unreadable files are omitted and the run
# commits a PARTIAL archive with a warning.
sources:
  default:
    localhost: [/home, /etc]
  no-logs:
    localhost: [/srv/www, /var/log]
```

## Commands

```bash
nb check                 # validate the config and confirm the disk is reachable
nb plan                  # preview today's run: levels per DLE, capacity usage
nb dump                  # run the backup, committing one run

nb status                # progress of the running (or most recent) dump
nb run                   # list the runs on disk

nb verify --all          # re-hash every run's archives against their checksums
nb drill                 # actually restore a risk-biased sample and discard it

# Whole-DLE restore as of a date, into an empty directory:
nb recover --dle localhost:/home --date 2026-06-21 --all --dest /tmp/restore
```

## What happens

- **First run fulls everything.** With no history, every DLE is due a level-0
  full, so day one is your largest run — recoverability comes first.
- **After that, the planner staggers fulls across the cycle.** It pulls future
  fulls forward onto lighter days so the lock-step of day one spreads out, while
  taking incrementals in between. You don't tune this; see
  [Planning](../features/planning).
- **The disk fills, then holds steady.** Once runs reach the `minimum_age`
  floor (one cycle by default) and a newer full supersedes them, `nb prune disk`
  reclaims the oldest to stay within `capacity`.

## What to watch

- **Prove restores actually work.** Checksums confirm the bytes are intact;
  `nb drill` confirms they're *restorable* — it catches a broken incremental
  chain or a scheme drift that `nb verify` can't. See
  [Verification & drills](../features/verification).
- **Keep `nb dump` in cron.** A nightly line is all this scenario needs:

  ```cron
  0 2 * * *  cd /etc/nbackup && nb dump
  ```

  Run `nb` from the directory holding `nbackup.yaml` (or pass `-c`), and set
  absolute `workdir`/`state_dir` paths if cron runs it from elsewhere — otherwise
  it can silently re-full.
- **One disk is one copy.** This setup has no offsite protection. When you're
  ready, add a second medium and replicate to it — that's
  [Disk → S3 offsite](disk-to-s3).
