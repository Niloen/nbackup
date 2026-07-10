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
  subcommands. `nb run` lists runs; `nb run run-2026-06-21.020000` details that
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
| `nb mount <dir>` | FUSE-mount the backups read-only: one directory per run, each a snapshot |
| `nb copy` | Copy one run between media (`--from`/`--to`, e.g. disk → tape) |
| `nb sync` | Mirror one medium's runs onto another (disk → tape/s3) |
| `nb label` | Label a volume (required for tape before its first dump) |
| `nb load` | Load a volume into a medium's drive (by slot number, or `--label`) |
| `nb prune [medium]` | Delete runs past each medium's cycle/capacity limits (all media if none named) |
| `nb flush` | Drain a holding disk's staged archives to the landing |
| `nb reset <dle>` | Schedule a DLE for a full on its next run (fresh chain) |
| `nb rebuild` | Rebuild the catalog from media — additive, so tapes can be fed one at a time |
| `nb init` | Write a starting `nbackup.yaml` (interactive, or via flags) |
| `nb login` | Authenticate a medium needing an interactive credential bootstrap (Google Drive OAuth) |
| `nb web` | Serve a read-only status website |
| `nb version` | Print the `nb` version |
| `nb completion` | Generate a shell completion script |

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
| `nb plan` | `--days N`, `--date <day>` | Preview today's plan; `--days N` forecasts the next N daily runs and the `$/month` cost curve, `--date` plans a specific day. |
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
nb run run-2026-06-21.020000
nb dle app01:/home
nb medium lto
nb status --watch 2s
```

See [Monitoring](../features/monitoring).

## Verifying and drilling

| Command | Key flags | Purpose |
|---|---|---|
| `nb verify` | `--all`, `--deep`, `--dle <dle>` | Re-hash archive checksums; `--all` every run, `--deep` adds a structural decrypt → decompress → `tar -t` check, `--dle` verifies only one DLE's archives across the runs holding it. |
| `nb drill [dle...]` | `--dry-run`, `--from <medium>`, `--tier <tier>`, `--sample N`, `--window <dur>`, `--as-of <date>`, `--worm`, `--unattended` | Rehearse recovery by restoring a risk-biased sample to scratch. `--sample N` sets how many DLEs to drill (default 1), `--window` the coverage window each DLE should fall within, `--worm` probes the medium for WORM/immutability. Naming DLEs re-drills exactly those now (the retry after a failure); a pass clears the DLE's warning. |

```bash
nb verify --all
nb verify --deep
nb verify --dle web01:/home --deep
nb drill
nb drill web01:/home
nb drill --dry-run
nb drill --from cloud --tier structural
nb drill --tier stock
nb drill --unattended
```

Tiers run cheapest to strongest: `sample` (re-hash one part per archive against
its per-part seal — bounded egress, the offsite-friendly default), `checksum`,
`structural`, `chain`, `stock`. See [Verification](../features/verification).

## Recovering

`nb recover` recovers from backups as they stood on a date — a whole DLE with
`--all`, or browse-and-pick individual files without it.

| Flag | Purpose |
|---|---|
| `--dle <host:path>` | The DLE to recover (omit with `--all` to recover every DLE). |
| `--date <day>` | Recover as of this date (`YYYY-MM-DD`, default today; resolves to the most recent run on or before it). |
| `--time <ts>` | As-of point-in-time `YYYY-MM-DD HH[:MM[:SS]]` (UTC) — reaches an earlier same-day run. Mutually exclusive with `--date`. |
| `--all` | Whole-DLE restore (replays full + later incrementals, deletion-faithful). |
| `--dest <dir>` | Destination directory (must be empty unless `--force`). |
| `--force` | With `--all`, restore into a populated `--dest`, pruning its contents to match. |
| `--path <p>` | Restrict to a path, or name an inventory unit to export (e.g. `public.users` → `<unit>.sql`); repeatable. |
| `--list` | List matching paths instead of extracting. |
| `--inventory` | Print the DLE's content inventory as of the date (the units the archiver reported, e.g. postgres tables) and exit. |
| `--from <medium>` | With `--all`, read from this medium's copy specifically (e.g. the offsite tape) instead of auto-selecting. |
| `--to host:path` | With `--all`, restore onto a remote client (`host` must be in `hosts:`). |
| `--yes` | Skip the egress-cost confirmation when reading from a cloud/cold medium. |

```bash
nb recover                                                    # interactive shell
nb recover --dle app01:/home --date 2026-06-21 --all --dest /tmp/out
nb recover --dle app01:/home --date 2026-06-20 --list --path /etc
nb recover --dle app01:/home --date 2026-06-20 \
    --path /etc/hosts --path /etc/nginx --dest /tmp/out
```

See [Recovery](../features/recovery).

## Mounting

`nb mount <dir>` serves the backups as a read-only FUSE filesystem: the top
level lists runs, and inside each run is every DLE's snapshot as of that run —
the same view `nb recover` browses for a date, pinned to a run. Browsing reads
only the member indexes; a file's content is recovered from the archives on its
first open and cached for the mount's lifetime, so an unopened file lists with
size 0 until read (`cat`/`cp` see the full content). Like file-level recovery
the view is a union: a file deleted before the run may still appear. Unmount
with Ctrl-C or `fusermount -u <dir>`.

| Flag | Purpose |
|---|---|
| `--cache-dir <dir>` | Keep the recovered-file cache here (default: a temp dir, removed on unmount). |

```bash
nb mount /mnt/backups
ls /mnt/backups
cat '/mnt/backups/<run>/<dle>/etc/hosts'
fusermount -u /mnt/backups
```

## Replicating

| Command | Key flags | Purpose |
|---|---|---|
| `nb copy` | `--from <medium>`, `--to <medium>`, `--dry-run`, `--force` | Copy one run between media (e.g. disk → tape); `--force` re-copies a run already on the target. |
| `nb sync` | `--to <medium>`, `--from <medium>`, `--run <id>`, `--last N`, `--since <day>`, `--dry-run`, `--force` | Mirror runs onto a medium, oldest-first; no `--to` runs every config `sync:` rule. Without `--from` the source is resolved per run (the landing, else whichever medium holds the missing archives). `--run` repairs just that run (repeatable); `--since` bounds by date (intersects `--last`); `--force` re-copies runs already on the target. |

```bash
nb sync --to lto --dry-run
nb sync --to lto
nb sync --to glacier --last 4
nb sync                       # run every rule in the config's sync: block
nb sync --from lto --to disk  # un-vault: restage tape back to disk
nb sync --run run-2026-07-08.010001 --to c2   # repair one run (e.g. after a tripped landing)
```

The source defaults to the landing medium; `--from` overrides it. See
[Replication](../features/replication).

## Retention and media

| Command | Key flags | Purpose |
|---|---|---|
| `nb prune [medium]` | `-n, --dry-run`, `--date <day>` | Delete runs past the cycle/capacity limits — one named medium, or every medium if none named; `-n` previews, `--date` sets the reference "now". |
| `nb label` | `--relabel` | Label a volume (required for tape before its first dump); `--relabel` recycles an aged-out tape. |
| `nb load` | `--label` | Load a volume into a medium's drive — by slot number, or `--label` to match a volume label. |
| `nb reset <dle>` | — | Schedule a DLE for a full on its next run (fresh chain). |
| `nb rebuild` | `--full` | Rebuild the local run-index cache from media. Additive: each pass merges what is readable now and prints the tapes still missing — after losing the catalog, insert tapes one at a time and re-run until the worklist is empty. `--full` wipes the cache first. |
| `nb flush` | — | Drain a holding disk's staged archives to the landing. |

```bash
nb prune disk -n
nb prune disk
nb prune                     # manually trim every configured medium (writes make their own room; not needed in cron)
nb label lto lto-0001
nb label --relabel lto lto-0042
nb load lto 3
nb load --label lto DAILY-01
nb reset app01:/home
nb rebuild
```

Retention is per-medium: name a medium to prune just it, or name none to prune
every configured medium in turn (tape recycles by relabel, so a fleet-wide prune
only reclaims disk/cloud). See [Pruning](../features/pruning) and
[Media](../features/media).

## Logging in a medium

Most media need no login: disk and tape have no credentials, and a cloud bucket
or a **service-account** Google Drive authenticate straight from the ambient
environment (`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`, …). `nb login <medium>`
exists for the one case that needs a one-time interactive consent — a **personal
Google Drive**, whose OAuth grant mints a reusable token.

| Command | Key flags | Purpose |
|---|---|---|
| `nb login <medium>` | `--client <json>`, `--out <path>` | Run a medium's credential bootstrap. Flags after the medium name are that type's own; see `nb login <medium> -h`. |

The gdrive flow adapts to the OAuth client you registered in the Google Cloud
Console (NBackup ships none — you bring your own, so no shared app or quota sits
between you and Google): a **"TVs and Limited Input devices"** client uses a
headless device code (prints a short code + URL, no browser or open port), while
a **"Desktop app"** client opens a browser on this machine and captures the
redirect itself. Pass the client-secret JSON with `--client` (or set
`GOOGLE_OAUTH_CLIENT`). The minted token is written to a default per-medium path
under the [`secrets_dir`](configuration#secrets_dir) (`<secrets_dir>/gdrive.json`)
that the medium then reads automatically — no environment variable to set.
Override the path with `--out`.

```bash
nb login gdrive
nb login gdrive --client ~/client_secret.json
```

See [Backing up to Google Drive](../scenarios/gdrive).

## Reporting

| Command | Key flags | Purpose |
|---|---|---|
| `nb report` | `--last N`, `--json`, `--dump`, `--run <id>`, `--notify` | Summarize recent runs and recovery health; `--dump` prints one dump's per-DLE report. |

```bash
nb report
nb report --last 30
nb report --json
nb report --dump
nb report --dump --run run-2026-06-21.020000
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
