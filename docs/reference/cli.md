---
title: CLI reference
layout: default
parent: Reference
nav_order: 1
description: "Every nb command, its purpose, and key flags — inspect with a noun, act with a verb."
---

# CLI reference
{: .no_toc }

Every `nb` command, its purpose, and its key flags.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## The convention

NBackup has one binary, `nb`, and a single naming rule:

- **Inspect with a noun.** `nb run`, `nb dle`, and `nb medium` each list with no
  argument and detail one item when given an id — there are no `list`/`show`
  subcommands. `nb run` lists runs; `nb run run-2026-06-21.001` details that
  one. `nb medium` lists media; `nb medium lto` details that one.
- **Act with a flat verb.** Every mutation is a top-level verb — `nb dump`,
  `nb recover`, `nb prune`, `nb verify`, `nb drill`, `nb sync`, … — never nested
  under a noun.

Flags may appear **before or after** the subcommand and its positional
arguments; `nb run --catalog /x lto` and `nb --catalog /x run lto` are
equivalent.

## Commands

| Command | Purpose |
|---|---|
| `nb check` | Verify the config and reach every source host |
| `nb plan` | Show what the next run would do |
| `nb dump` | Execute a run and commit its archives |
| `nb status` | Show progress of the current (or most recent) run |
| `nb report` | Summarize recent runs, or print one dump's per-DLE report |
| `nb run` | List runs, or detail one (`nb run <id>`: archives + copies) |
| `nb dle` | List DLEs, or detail one's archive timeline across runs |
| `nb medium` | List media, or detail one (incl. drives + slots) |
| `nb verify` | Verify run integrity: checksums, or `--deep` structure |
| `nb drill` | Rehearse recovery: prove backups are restorable |
| `nb recover` | Recover as of a date: browse + pick files, or `--all` for a whole DLE |
| `nb copy` | Copy one run between media (`--from`/`--to`, e.g. disk → tape) |
| `nb sync` | Mirror one medium's runs onto another (disk → tape/s3) |
| `nb label` | Label a volume (required for tape before its first dump) |
| `nb load` | Load a slot into a medium's drive (by slot number or `--label`) |
| `nb prune <medium>` | Delete a medium's runs past its cycle/capacity limits |
| `nb reset <dle>` | Schedule a DLE for a full on its next run (fresh chain) |
| `nb rebuild` | Rebuild the local run-index cache from media |

## Global flags

These work with every command and may appear anywhere on the command line —
before or after the subcommand and its arguments.

| Flag | Purpose |
|---|---|
| `-c, --config` | Path to config file (default `nbackup.yaml`) |
| `--catalog` | Catalog directory (overrides config); no short flag |
| `-q, --quiet` | Suppress progress output |

## Planning and dumping

| Command | Key flags | Purpose |
|---|---|---|
| `nb check` | `--offline` | Verify config and reach every source host before a run; warns when `workdir`/`state_dir` resolve to a *relative* path (the cron re-full footgun). `--offline` skips the host probes. |
| `nb plan` | `--days N` | Preview today's plan; `--days N` forecasts the next N daily runs and the `$/month` cost curve. |
| `nb dump` | `--dry-run`, `--date <day>` | Run the backup; `--dry-run` plans without writing, `--date` plans/runs for a specific day. |

```bash
nb check
nb plan
nb plan --days 30
nb dump
nb dump --dry-run --date 2026-07-15
```

See [Planning](../features/planning).

## Inspecting

Each noun lists with no argument and details one item when given an id.

| Command | Key flags | Purpose |
|---|---|---|
| `nb run [id]` | — | List runs (with a COPIES column), or detail one run's archives and every copy's positions. |
| `nb dle [dle]` | — | List DLEs, or detail one DLE's archive timeline across runs. |
| `nb medium [name]` | — | List media, or detail one (its drives + slots). |
| `nb status` | `--watch <interval>` | Show the running (or most recent) run; `--watch` refreshes until it finishes. |

```bash
nb run
nb run run-2026-06-21.001
nb dle app01:/home
nb medium lto
nb status --watch 2s
```

See [Monitoring](../features/monitoring).

## Verifying and drilling

| Command | Key flags | Purpose |
|---|---|---|
| `nb verify` | `--all`, `--deep` | Re-hash archive checksums; `--all` every run, `--deep` adds a structural decrypt → decompress → `tar -t` check. |
| `nb drill` | `--dry-run`, `--from <medium>`, `--tier <tier>`, `--as-of <date>`, `--unattended` | Rehearse recovery by restoring a risk-biased sample to scratch. |

```bash
nb verify --all
nb verify --deep
nb drill
nb drill --dry-run
nb drill --from cloud --tier structural
nb drill --tier stock
nb drill --unattended
```

Tiers run weakest to strongest: `checksum`, `structural`, `chain`, `stock`. See
[Verification](../features/verification).

## Recovering

`nb recover` recovers from backups as they stood on a date — a whole DLE with
`--all`, or browse-and-pick individual files without it.

| Flag | Purpose |
|---|---|
| `--dle <host:path>` | The DLE to recover (omit with `--all` to recover every DLE). |
| `--date <day>` | Recover as of this date. |
| `--all` | Whole-DLE restore (replays full + later incrementals, deletion-faithful). |
| `--dest <dir>` | Destination directory (must be empty unless `--force`). |
| `--force` | Restore into a populated `--dest`, replacing its contents. |
| `--path <p>` | Restrict to a path; repeatable for several. |
| `--list` | List matching paths instead of extracting. |
| `--to host:path` | Restore onto a remote client. |

```bash
nb recover                                                    # interactive shell
nb recover --dle app01:/home --date 2026-06-21 --all --dest /tmp/out
nb recover --dle app01:/home --date 2026-06-20 --list --path /etc
nb recover --dle app01:/home --date 2026-06-20 \
    --path /etc/hosts --path /etc/nginx --dest /tmp/out
```

See [Recovery](../features/recovery).

## Replicating

| Command | Key flags | Purpose |
|---|---|---|
| `nb copy` | `--from <medium>`, `--to <medium>` | Copy one run between media (e.g. disk → tape). |
| `nb sync` | `--to <medium>`, `--from <medium>`, `--last N`, `--dry-run` | Mirror one medium's runs onto another, oldest-first; no `--to` runs every config `sync:` rule. |

```bash
nb sync --to lto --dry-run
nb sync --to lto
nb sync --to glacier --last 4
nb sync                       # run every rule in the config's sync: block
nb sync --from lto --to disk  # un-vault: restage tape back to disk
```

The source defaults to the landing medium; `--from` overrides it. See
[Replication](../features/replication).

## Retention and media

| Command | Key flags | Purpose |
|---|---|---|
| `nb prune <medium>` | `-n, --dry-run` | Delete the named medium's runs past its cycle/capacity limits; `-n` previews. |
| `nb label` | `--relabel` | Label a volume (required for tape before its first dump); `--relabel` recycles an aged-out tape. |
| `nb load` | — | Load a slot into a medium's drive (by slot number, or `--label`). |
| `nb reset <dle>` | — | Schedule a DLE for a full on its next run (fresh chain). |
| `nb rebuild` | — | Rebuild the local run-index cache from media. |
| `nb flush` | — | Drain a holding disk's staged archives to the landing. |

```bash
nb prune disk -n
nb prune disk
nb label lto lto-0001
nb label --relabel lto lto-0042
nb load lto 2
nb reset app01:/home
nb rebuild
```

The medium is named explicitly because retention is per-medium. See
[Pruning](../features/pruning) and [Media](../features/media).

## Reporting

| Command | Key flags | Purpose |
|---|---|---|
| `nb report` | `--last N`, `--json`, `--dump`, `--run <id>`, `--notify` | Summarize recent runs and recovery health; `--dump` prints one dump's per-DLE report. |

```bash
nb report
nb report --last 30
nb report --json
nb report --dump
nb report --dump --run run-2026-06-21.001
nb report --notify
```

See [Monitoring](../features/monitoring).

## Help and completion

- `nb help <command>` (or `nb <command> --help`) prints per-command usage and
  examples.
- `nb completion <shell>` generates shell completion.

---

For the full config surface every command reads, see the
[Configuration reference](configuration). For the concepts behind the
commands, see [Concepts](../concepts) and [Features](../features).
