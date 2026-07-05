---
title: Archivers (tar, PostgreSQL, pipe)
layout: default
parent: Features
nav_order: 3
description: "The pluggable dump programs that produce the backup stream: GNU tar (default), PostgreSQL 17 incremental base backups, and bring-your-own-command pipes."
---

# Archivers
{: .no_toc }

The pluggable dump programs that produce the backup stream: GNU tar (default),
PostgreSQL 17 incremental base backups, and bring-your-own-command pipes.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## What an archiver is

An **archiver** is the program that turns a source into the backup stream — the
Amanda "application". Everything downstream of that stream (compression,
encryption, splitting, media, catalog, verification, drills) is identical for
every archiver; the archiver owns only how the stream is produced, how its
incremental levels chain, and how the stream restores.

Three types ship:

| Type | Source | Incrementals | Restores with |
|---|---|---|---|
| `gnutar` (default) | a filesystem path | GNU tar listed-incremental (`.snar`) | stock `tar` |
| `postgres` | a live PostgreSQL 17+ cluster | native incremental base backups | `pg_combinebackup` |
| `pipe` | anything your command understands | full-only | your own command |

The config split is always the same: `archivers:` names a type plus its
tool-behavior options, a **dumptype** references an archiver and carries per-DLE
policy, and each `sources:` entry contributes a **source string** the archiver
interprets — a path for gnutar, a libpq connection reference for postgres, an
opaque token for pipe. Locality details (a host's odd binary path) go in
`hosts.<h>.archivers.<type>` overrides, exactly like per-host `ssh:` fields.

Every archiver's incremental state lives under the host's `state_dir` — a host
property, not an archiver option. See the
[configuration reference](../reference/configuration#archivers).

## gnutar — filesystem trees (the default)

The default archiver needs no configuration at all: `archiver: gnutar` (or
nothing — an empty dumptype resolves to it) dumps a filesystem tree with GNU
tar in listed-incremental format, so every level restores with stock `tar` and
deletions are replayed on restore.

```yaml
archivers:
  default:
    type: gnutar
    one-file-system: "true"   # don't cross mount points
    sparse: "true"            # store sparse files efficiently
    # tar_path: gtar          # GNU tar binary (use "gtar" on macOS/BSD)
```

Its incremental state is the `.snar` snapshot library under the host's
`state_dir`, promoted atomically only when the archive commits. Excludes are a
dumptype option (skipping `*.log` is a decision about the data, not about how
tar runs).

## postgres — live PostgreSQL clusters

The `postgres` archiver backs up a **live cluster** with PostgreSQL 17's native
incremental base backups — no snapshot-to-file staging, no WAL spool, nothing
written on the database host but a small manifest per level:

- **Level 0** is a full `pg_basebackup --format=tar` streamed straight to
  NBackup's pipeline (compression, encryption, splitting, and media handling
  all apply as usual).
- **Level N** is `pg_basebackup --incremental` against the manifest the
  previous level left behind — changed relation files are stored as block
  deltas, so a busy cluster's daily incremental is small. By default a DLE
  *sits* at level 1 (each incremental re-captures everything changed since the
  full, so any one restores as L0 + a single L1), and the
  [planner](planning) deepens the level only when a climb saves enough — so a
  chain is usually just two levels.
- The manifest rides *inside* the dump's own tar stream and is teed out in
  flight, then promoted atomically on commit — the same discipline as gnutar's
  `.snar` library, stored under the same `state_dir`.

```yaml
archivers:
  pg:
    type: postgres
    bin_dir: /usr/lib/postgresql/17/bin   # if the v17 tools are off PATH

dumptypes:
  db:
    archiver: pg

sources:
  db:
    localhost:
      - app_prod                                   # a database name (libpq defaults: local socket, peer auth)
      - service=legacy                             # a service from ~/.pg_service.conf
      - "host=10.0.0.12 port=5433 dbname=postgres" # a full conninfo string
```

### The source string is a libpq connection reference

The DLE's "path" is anything `psql -d` accepts: a bare database name, a
`service=` reference, or a full conninfo string. Authentication is entirely the
client's own libpq configuration — peer auth as the identity running the dump,
`~/.pgpass`, `~/.pg_service.conf`. **NBackup's config carries no connection
secrets**, the same doctrine as SSH (agent) and gpg (keyring).

Two things follow from the string being the DLE's identity:

- **One DLE per cluster.** A base backup images the *whole cluster*; the
  database in the source string only names the connection. Two DLEs reaching
  the same cluster through different strings would back it up twice.
- **Prefer a stable string.** Changing the string mints a new DLE (fresh
  level-0, history continues under the old ID until retention retires it). Use
  a bare dbname or `service=` for anything long-lived — the service file can
  then change host, port, and credentials freely without touching the DLE.

### Requirements, proven by `nb check`

- **PostgreSQL 17+** server, and the v17+ client tools (`pg_basebackup`,
  `pg_combinebackup`) runnable on the host that dumps — set `bin_dir` if they
  are off `PATH` (Debian: `/usr/lib/postgresql/17/bin`).
- `summarize_wal = on` on the server (incrementals need WAL summaries).
- A role with `REPLICATION` (or superuser) for `pg_basebackup`.

`nb check` proves the whole chain live: it runs the tools, verifies their
version, actually connects as the configured identity (failing rather than
prompting for a password), and prints the `ALTER SYSTEM SET summarize_wal`
line if the server isn't ready.

### Restore, browse, and table export

A whole-DLE restore (`nb recover --all`) stages every level of the chain and
runs `pg_combinebackup` once — the database's own tool stays authoritative —
leaving a data directory ready for `pg_ctl start`. Browsing (`nb recover`'s
shell, `nb mount`) assembles a file's chain of block deltas in-process, so the
view is correct without a full restore.

Backups also report their **contents**: tables with sizes, captured at dump
time.

```console
$ nb recover --dle localhost:app_prod --date 2026-07-01 --inventory
  table.app_prod.public.orders   1.8 GB  (14 file(s))
  table.app_prod.public.users    312 MB  (6 file(s))
2 units · run run-2026-07-01.020000
```

Pointing `--path` (or the shell's `add`) at a unit name exports the table as
**ready-to-import `pg_dump` SQL**: NBackup restores the cluster to scratch,
boots a throwaway postmaster that cannot reach anything (sockets only, every
prod-reaching knob overridden), dumps the table, and tears it all down. The
cost of the scratch restore is priced and confirmed like `--all`; loading the
SQL into your database stays your own, explicit act.

```console
$ nb recover --dle localhost:app_prod --date 2026-07-01 \
    --path public.users --dest /tmp/out
$ ls /tmp/out
table.app_prod.public.users.sql
```

### Options

| Option | Meaning |
|---|---|
| `mode` | backup strategy; `incr` (the default and currently the only mode) — PG17 native incremental base backups |
| `bin_dir` | directory holding the PostgreSQL client tools; empty = `PATH` |

`bin_dir` on a specific host is the classic per-host override:
`hosts.<h>.archivers.postgres.bin_dir`.

## pipe — bring your own command

The `pipe` archiver wraps any pair of user commands that produce and consume a
stream — the escape hatch for sources NBackup has no native archiver for:

```yaml
archivers:
  sqlite:
    type: pipe
    backup_command: "sqlite3 {source} '.backup /dev/stdout'"
    restore_command: "sqlite3 {dest}"        # consumes the stream on stdin
    # estimate_command: "stat -c%s {source}" # optional: prints a byte count
```

- `{source}` and `{dest}` substitute the DLE's source string and the restore
  destination, shell-quoted; the commands run via `sh -c` on the DLE's host,
  so a remote DLE's producer runs on the client exactly like tar does.
- **Full-only**: the planner simply schedules fulls for pipe DLEs.
- The stock recovery recipe is your own `restore_command` — recovery needs
  only the tool that made the stream.

One named pipe archiver serves many DLEs: the command is the definition, the
source string parameterizes each entry.

## Where each knob lives

| You want to vary… | Put it in… |
|---|---|
| how the tool behaves (sparse, mode, commands) | the named `archivers:` definition |
| where the tool lives on one host (`tar_path`, `bin_dir`) | `hosts.<h>.archivers.<type>` |
| which object is backed up (path, cluster, database file) | the DLE's source string |
| what to skip, encryption, compression, landing | the dumptype |

---

See also: [Configuration reference](../reference/configuration#archivers),
[Recovery](recovery), [Remote sources](remote-sources),
[Concepts](../concepts).
