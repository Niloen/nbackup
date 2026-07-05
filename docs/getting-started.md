---
title: Getting Started
layout: default
nav_order: 3
description: "Install NBackup, write a minimal config, and run your first backup."
---

# Getting Started
{: .no_toc }

Install NBackup, write a minimal config, and run your first backup.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## Requirements

- **GNU tar** at runtime (`tar` on Linux, `gtar` on macOS/BSD; set a `tar_path`
  option on the archiver to override). NBackup checks the binary is GNU tar
  before running.
- The configured **compressor** on `PATH`: `zstd` (default) or `gzip`; `none`
  needs nothing. NBackup checks it before running. Optional `nice` is used for
  CPU politeness when configured.
- **Go 1.25+** only if you build from source.

{: .note }
> If `zstd` is not installed, set `compress.scheme` to `gzip` or `none` — the
> scheme binary is verified before a dump.

## Install

Every path below ends with the same single `nb` binary.

**Packages (Debian/Ubuntu, RHEL/Fedora)** — the `niloen-backup` deb/rpm from
[GitHub Releases](https://github.com/Niloen/nbackup/releases) installs `nb`
with man pages and shell completions, and declares its dependencies
(GNU tar; `zstd` and `gnupg` recommended):

```bash
# Debian/Ubuntu — pick the .deb for your architecture from the latest release
sudo apt install ./niloen-backup_<version>_amd64.deb

# RHEL/Fedora
sudo dnf install ./niloen-backup-<version>.x86_64.rpm
```

**Prebuilt binary (Linux/macOS, amd64/arm64)** — a tarball per platform on the
same [Releases](https://github.com/Niloen/nbackup/releases) page; unpack and
put `nb` on your `PATH` (man pages and completions ride along in the tarball).

**Container** — `ghcr.io/niloen/nbackup` ships `nb` with GNU tar, zstd, and
gnupg included; the entrypoint is `nb`, and scheduling stays your host's cron.
Mount your config and the catalog/media directories into the container so runs
and state land on the host.

**From source:**

```bash
git clone https://github.com/Niloen/nbackup && cd nbackup
make build          # builds ./bin/nb
# or
go install ./cmd/nb
```

**Upgrading and uninstalling** — upgrades are an in-place binary replacement: bump
the package (`apt install ./niloen-backup_<newer>.deb` / `dnf upgrade`) or drop the
newer `nb` on your `PATH`. To remove, `sudo apt remove niloen-backup` (or `dnf
remove niloen-backup`) for a package install, or just delete the `nb` binary for a
tarball/`go install` one. Your config, catalog, and state directories are yours and
are left untouched either way.

## The command shape

NBackup has one convention you'll lean on constantly:

- **Inspect with a noun** — `nb run`, `nb dle`, `nb medium`. With no argument it
  lists; with an id it details one item (`nb run run-2026-06-21.020000`,
  `nb medium lto`).
- **Act with a flat verb** — `nb dump`, `nb recover`, `nb prune`, `nb sync`, …

Run `nb help <command>` (or `nb <command> --help`) for per-command usage, and
`nb completion <shell>` to generate shell completion. The full list is in the
[CLI reference](reference/cli).

## A minimal config

NBackup reads `nbackup.yaml` from the working directory (override with
`-c/--config`). Start from the shipped example:

```bash
cp nbackup.example.yaml nbackup.yaml   # then edit sources + catalog path
```

The smallest useful config backs up a single machine to local disk:

```yaml
cycle: 7d                            # target & hard-max time between fulls per DLE

compress:
  scheme: zstd                       # zstd | gzip | none

media:
  disk:
    type: disk
    path: /var/lib/nbackup/catalog   # where runs are written
    capacity: 20TB                   # the space NBackup may use here
landing: disk                        # which medium new runs are created on

archivers:
  default:
    type: gnutar
    one-file-system: "true"

dumptypes:
  default:
    archiver: default

sources:
  default:
    localhost: [/home, /etc]         # localhost = backed up locally
```

{: .note }
> Reading all of `/etc` (and most of `/home`) needs **root** — run `nb dump` as
> root (or via cron's root crontab). Without it, unreadable files are omitted and
> the run commits a **PARTIAL** archive with a warning rather than failing.

The four building blocks — **media**, **archivers**, **dumptypes**, **sources** —
are explained in the [Configuration reference](reference/configuration) and the
[Concepts](concepts) page. For now: a **source** is a `host:path` to back up, a
**dumptype** carries per-source policy (excludes, encryption), an **archiver** is
the dump program (GNU tar), and a **medium** is where runs land.

## Your first run

```bash
nb check               # verify the config and reach every source host
nb plan                # preview today's plan, capacity usage, and (for cloud) $/month
nb dump                # run the backup, committing one run's archives
nb status              # progress of the running (or most recent) dump
nb run                 # list runs
```

A first run **fulls everything** (recoverability first); over the next cycle the
planner staggers those fulls apart so daily volume evens out. See
[Planning & scheduling](features/planning) for how that works.

## Inspect what you made

```bash
nb run                         # list runs, with a COPIES column
nb run run-2026-06-21.020000      # archives in one run + every copy's location
nb dle                         # list sources and their archive timelines
nb dle app01:/home             # one source's history across runs
nb verify --all                # re-check every run's archive checksums
```

A **run** is just a directory you can open with `ls`. To prove a restore really
works end-to-end, run a [recovery drill](features/verification):

```bash
nb drill --dry-run     # preview what would be drilled + a recoverability audit
nb drill               # actually restore a risk-biased sample to scratch
```

## Recover a file (or everything)

```bash
# whole-DLE restore as of a date:
nb recover --dle app01:/home --date 2026-06-21 --all --dest /tmp/out

# browse + pick individual files interactively:
nb recover
```

See [Recovery](features/recovery) for both modes.

## A hands-off cron line

Once you add an offsite medium and a `notify:` block, a complete unattended
nightly looks like this:

```sh
nb dump && nb sync && nb prune && nb drill --unattended; nb report --notify
```

`nb dump` lands the backup, `nb sync` mirrors it offsite, `nb prune` trims each
medium back to its cycle/capacity limits, `nb drill` proves a sample restores, and
`nb report --notify` emails the result. `nb prune` with no medium named prunes
every configured medium against its own retention (tape recycles by relabel, so it
is left untouched); it runs **after** `nb sync` so a run is replicated offsite
before disk can reclaim it. Every mutating command exits non-zero on failure and
can alert you — see [Monitoring & reporting](features/monitoring).

## Global flags

These work with every command and may appear anywhere on the command line:

| Flag | Purpose |
|---|---|
| `-c, --config` | path to config file (default `nbackup.yaml`) |
| `--catalog` | catalog directory (overrides config) |
| `-q, --quiet` | suppress progress output |

---

Next: pick a [Scenario](scenarios) that matches your situation, or read about a
specific [Feature](features).
